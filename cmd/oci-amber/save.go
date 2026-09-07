package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v2"

	"github.com/draganm/oci-amber/dockerarchive"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/tui"
)

// saveConfig is everything `save` needs. saveConfigFromCLI fills it from
// flags; tests construct it directly and call runSave.
type saveConfig struct {
	Store  string
	Output string   // "" or "-" means stdout
	Refs   []string // repo, repo:tag or repo@digest, validated
	// Platform is the platform whose child an index's manifest.json entry
	// describes; nil means hostPlatform().
	Platform *oci.Platform
	Progress string    // "auto" | "tui" | "plain"; "" means auto
	Stdin    io.Reader // nil means os.Stdin; the TUI reads its keys here
	Stdout   io.Writer // nil means os.Stdout
	Stderr   io.Writer // nil means os.Stderr; progress goes here
}

func saveFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write the archive to `path` instead of stdout"},
		&cli.StringFlag{Name: "progress", Value: "auto", Usage: "progress display on stderr: auto, tui or plain", EnvVars: envVar("progress")},
	}
}

func saveConfigFromCLI(c *cli.Context) (saveConfig, error) {
	if c.NArg() < 1 {
		return saveConfig{}, errors.New("save takes at least one reference (repo, repo:tag or repo@digest)")
	}
	cfg := saveConfig{Store: c.String("store"), Output: c.String("output"), Refs: c.Args().Slice(), Progress: c.String("progress")}
	if cfg.Store == "" {
		return saveConfig{}, errors.New("--store must not be empty")
	}
	switch cfg.Progress {
	case "auto", "tui", "plain":
	default:
		return saveConfig{}, fmt.Errorf("--progress must be auto, tui or plain, got %q", cfg.Progress)
	}
	for _, r := range cfg.Refs {
		if _, err := parseSaveRef(r); err != nil {
			return saveConfig{}, err
		}
	}
	return cfg, nil
}

// saveRef is a parsed reference: a tag, a digest, or with neither every
// tag of the repository.
type saveRef struct {
	repo   string
	tag    string
	digest oci.Digest
}

func (r saveRef) String() string {
	switch {
	case r.digest != "":
		return r.repo + "@" + r.digest.String()
	case r.tag != "":
		return r.repo + ":" + r.tag
	default:
		return r.repo
	}
}

// parseSaveRef parses and validates repo, repo:tag or repo@digest.
func parseSaveRef(s string) (saveRef, error) {
	repo, reference := oci.SplitReference(s)
	if err := oci.ValidateRepository(repo); err != nil {
		return saveRef{}, fmt.Errorf("%q: %v", s, err)
	}
	r := saveRef{repo: repo}
	switch {
	case reference == "" && len(s) > len(repo):
		return saveRef{}, fmt.Errorf("%q: nothing after the %q", s, s[len(repo):])
	case reference == "":
	case oci.IsDigest(reference):
		d, err := oci.ParseDigest(reference)
		if err != nil {
			return saveRef{}, fmt.Errorf("%q: %v", s, err)
		}
		r.digest = d
	default:
		if err := oci.ValidateTag(reference); err != nil {
			return saveRef{}, fmt.Errorf("%q: %v", s, err)
		}
		r.tag = reference
	}
	return r, nil
}

// hostPlatform is the platform docker would pick on this machine: its
// architecture, on linux unless the host is windows, since a darwin
// client's daemon runs in a linux VM.
func hostPlatform() *oci.Platform {
	os := "linux"
	if runtime.GOOS == "windows" {
		os = "windows"
	}
	return &oci.Platform{OS: os, Architecture: runtime.GOARCH}
}

// runSave opens the store read-only, resolves every reference, then writes
// one `docker image save` archive of them to the output, showing progress
// on stderr and ending with a summary line there. References are resolved
// before the output is created, so an unknown one leaves nothing behind; a
// failure while writing removes the output file.
func runSave(ctx context.Context, cfg saveConfig) error {
	stdin, stdout, stderr := cfg.Stdin, cfg.Stdout, cfg.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	toFile := cfg.Output != "" && cfg.Output != "-"
	if !toFile && isTerminal(stdout) {
		return errors.New("cowardly refusing to save to a terminal. Use the -o flag or redirect.")
	}
	// The archive may be on stdout, so the display lives on stderr and
	// the TUI needs both it and the keyboard to be terminals.
	mode := cfg.Progress
	if mode == "" || mode == "auto" {
		mode = "plain"
		if isTerminal(stdin) && isTerminal(stderr) {
			mode = "tui"
		}
	}
	// The TUI keeps warnings for after the screen is gone.
	var deferred bytes.Buffer
	var logOut io.Writer = stderr
	if mode == "tui" {
		logOut = &deferred
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ro, err := openReadOnly(cfg.Store, log)
	if err != nil {
		return err
	}
	output := "stdout"
	if toFile {
		output = cfg.Output
	}
	what := strings.Join(cfg.Refs, ", ") + " → " + output
	tr := tui.NewSaveTracker(nil)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := func() error { return writeArchive(ctx, ro, cfg, stdout, toFile, tr.Progress) }
	if mode == "tui" {
		err = tui.RunSave(tr, "Saving "+what, cancel, stdin, stderr, run)
	} else {
		err = tui.RunSavePlain(stderr, tr, plainStatusInterval, run)
	}
	if deferred.Len() > 0 {
		io.Copy(stderr, &deferred)
	}
	var terr *tui.TerminalError
	if errors.Is(err, context.Canceled) && !errors.As(err, &terr) {
		err = errors.New("save cancelled")
	}
	if err == nil {
		s := tr.Snapshot()
		fmt.Fprintf(stderr, "Saved %s: %s in %s\n", what, tui.FormatBytes(s.Total), tui.FormatShort(s.Elapsed))
	}
	return errors.Join(err, ro.Close())
}

// isTerminal reports whether v is an *os.File on a terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// writeArchive resolves the references and writes the archive to the
// output file, or to stdout when toFile is false, reporting progress.
func writeArchive(ctx context.Context, ro *readOnlyStore, cfg saveConfig, stdout io.Writer, toFile bool, progress func(dockerarchive.WriteProgress)) error {
	exports, err := resolveExports(ro.images, cfg.Refs)
	if err != nil {
		return err
	}
	platform := cfg.Platform
	if platform == nil {
		platform = hostPlatform()
	}

	out := stdout
	var f *os.File
	if toFile {
		if f, err = os.Create(cfg.Output); err != nil {
			return fmt.Errorf("creating %s: %w", cfg.Output, err)
		}
		out = f
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	err = dockerarchive.Write(ctx, bw, storeSource{ro}, exports, dockerarchive.WriteOptions{Platform: platform, Progress: progress})
	if err == nil {
		err = bw.Flush()
	}
	if f != nil {
		// Only a regular file is removed after a failure: -o /dev/stdout
		// or a fifo is not ours to unlink.
		regular := false
		if st, serr := f.Stat(); serr == nil && st.Mode().IsRegular() {
			regular = true
		}
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("writing %s: %w", cfg.Output, cerr)
		}
		if err != nil && regular {
			os.Remove(cfg.Output)
		}
	}
	return err
}

// resolveExports turns references into the images to save: a bare
// repository contributes every tag, in tag order; an image named twice is
// saved once. A reference that does not exist is an error.
func resolveExports(images *image.Store, refs []string) ([]dockerarchive.Export, error) {
	var exports []dockerarchive.Export
	seen := map[dockerarchive.Export]bool{}
	// add opens one tag or digest reference; r names it in errors.
	add := func(r saveRef) error {
		reference := r.tag
		if r.digest != "" {
			reference = r.digest.String()
		}
		im, err := images.Open(r.repo, reference)
		if errors.Is(err, image.ErrNotFound) {
			return fmt.Errorf("%s: not found", r)
		}
		if err != nil {
			return err
		}
		e := dockerarchive.Export{Repo: r.repo, Digest: im.Meta.Digest, MediaType: im.Meta.MediaType, Tag: r.tag}
		if !seen[e] {
			seen[e] = true
			exports = append(exports, e)
		}
		return nil
	}
	for _, s := range refs {
		r, err := parseSaveRef(s)
		if err != nil {
			return nil, err
		}
		if r.tag != "" || r.digest != "" {
			if err := add(r); err != nil {
				return nil, err
			}
			continue
		}
		tags, err := images.Tags(r.repo)
		if errors.Is(err, image.ErrNotFound) || (err == nil && len(tags) == 0) {
			return nil, fmt.Errorf("%s: not found", r)
		}
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			if err := add(saveRef{repo: r.repo, tag: tag}); err != nil {
				return nil, err
			}
		}
	}
	return exports, nil
}

// storeSource is the dockerarchive.Source over a read-only store.
type storeSource struct{ ro *readOnlyStore }

func (s storeSource) Manifest(ctx context.Context, repo string, d oci.Digest) ([]byte, error) {
	im, err := s.ro.images.Open(repo, d.String())
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := im.WriteTo(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s storeSource) Blob(ctx context.Context, d oci.Digest, w io.Writer) error {
	bl, err := s.ro.blobs.Open(d)
	if err != nil {
		return err
	}
	return bl.WriteTo(ctx, w)
}
