package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v2"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// importRecentTTL keeps every blob's accounting in the recent-uploads table
// for the whole run: the image store reads it when the manifest is
// published, which can be hours after a layer was stored.
const importRecentTTL = 365 * 24 * time.Hour

// plainStatusInterval is how often plain mode prints a status line.
const plainStatusInterval = 5 * time.Second

// importConfig is everything `import` needs. importConfigFromCLI fills it
// from flags; tests construct it directly and call runImport.
type importConfig struct {
	Store                 string
	WorkDir               string // "" means <Store>/work
	MaxInMemory           int64
	AnalyzeParallelism    int
	AnalyzeTimeout        time.Duration
	MaxConcurrentFinalize int
	VerifyLimit           int64
	VerifyRoundTrip       bool
	AllowRaw              bool
	LogLevel              slog.Level
	LogFile               string
	Archive               string   // path, or "-" for stdin
	Names                 []string // --name overrides
	Progress              string   // "auto" | "tui" | "plain"
	Stdin                 io.Reader
	Stdout, Stderr        io.Writer
}

func importFlags() []cli.Flag {
	return append(storeFlags(),
		&cli.StringSliceFlag{Name: "name", Usage: "publish under `repo:tag` instead of the archive's RepoTags (repeatable; single-image archives only)"},
		&cli.StringFlag{Name: "progress", Value: "auto", Usage: "progress display: auto, tui or plain", EnvVars: envVar("progress")},
		&cli.StringFlag{Name: "log-file", Usage: "write the full log to `path`", EnvVars: envVar("log-file")},
	)
}

func importConfigFromCLI(c *cli.Context) (importConfig, error) {
	if c.NArg() != 1 {
		return importConfig{}, errors.New("import takes exactly one archive path (or - for stdin)")
	}
	cfg := importConfig{
		Store:                 c.String("store"),
		WorkDir:               c.String("work-dir"),
		AnalyzeParallelism:    c.Int("analyze-parallelism"),
		AnalyzeTimeout:        c.Duration("analyze-timeout"),
		MaxConcurrentFinalize: c.Int("max-concurrent-finalize"),
		VerifyRoundTrip:       c.Bool("verify-roundtrip"),
		AllowRaw:              c.Bool("allow-raw"),
		LogFile:               c.String("log-file"),
		Archive:               c.Args().First(),
		Names:                 c.StringSlice("name"),
		Progress:              c.String("progress"),
	}
	if cfg.Store == "" {
		return importConfig{}, errors.New("--store must not be empty")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.Store, "work")
	}
	size, err := parseSize(c.String("max-in-memory"))
	if err != nil {
		return importConfig{}, fmt.Errorf("--max-in-memory: %w", err)
	}
	cfg.MaxInMemory = size
	limit, err := parseSize(c.String("verify-limit"))
	if err != nil {
		return importConfig{}, fmt.Errorf("--verify-limit: %w", err)
	}
	cfg.VerifyLimit = limit
	if cfg.AnalyzeParallelism < 1 {
		return importConfig{}, fmt.Errorf("--analyze-parallelism must be at least 1, got %d", cfg.AnalyzeParallelism)
	}
	if cfg.AnalyzeTimeout <= 0 {
		return importConfig{}, fmt.Errorf("--analyze-timeout must be positive, got %s", cfg.AnalyzeTimeout)
	}
	if cfg.MaxConcurrentFinalize < 1 {
		return importConfig{}, fmt.Errorf("--max-concurrent-finalize must be at least 1, got %d", cfg.MaxConcurrentFinalize)
	}
	switch cfg.Progress {
	case "auto", "tui", "plain":
	default:
		return importConfig{}, fmt.Errorf("--progress must be auto, tui or plain, got %q", cfg.Progress)
	}
	for _, n := range cfg.Names {
		if _, err := dockerarchive.ParseName(n); err != nil {
			return importConfig{}, fmt.Errorf("--name %v", err)
		}
	}
	level, err := parseLogLevel(c.String("log-level"))
	if err != nil {
		return importConfig{}, fmt.Errorf("--log-level: %w", err)
	}
	cfg.LogLevel = level
	if len(cfg.Names) == 0 {
		cfg.Names = nil
	}
	return cfg, nil
}

// runImport opens the archive and the store, plans, runs the import under
// the chosen progress display and prints the report.
func runImport(ctx context.Context, cfg importConfig) error {
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
	mode := cfg.Progress
	if mode == "auto" {
		mode = "plain"
		if f, ok := stdout.(*os.File); ok && term.IsTerminal(f.Fd()) {
			mode = "tui"
		}
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Join(cfg.Store, "work")
	}
	ownDir := filepath.Join(workDir, workSubdir)
	if err := os.MkdirAll(ownDir, 0o755); err != nil {
		return fmt.Errorf("creating work directory %s: %w", ownDir, err)
	}

	// Logging: a file gets everything; plain mode logs to stderr; the TUI
	// keeps warnings and errors for after the screen is gone.
	var deferred bytes.Buffer
	var logOut io.Writer = stderr
	level := cfg.LogLevel
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		logOut = f
	} else if mode == "tui" {
		logOut = &deferred
		level = max(level, slog.LevelWarn)
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	defer func() {
		if deferred.Len() > 0 {
			io.Copy(stderr, &deferred)
		}
	}()

	// A killed earlier run can leave its spooled stdin behind; nothing
	// reads it again, so sweep it before it accumulates across runs.
	if stale, err := filepath.Glob(filepath.Join(ownDir, "import-*.tar")); err == nil {
		for _, f := range stale {
			if err := os.Remove(f); err != nil {
				log.Debug("removing stale spooled archive", "path", f, "error", err)
			}
		}
	}

	// The store is opened before spooling stdin, so a store locked by a
	// running serve is reported before a multi-GB stream is copied.
	st, err := store.Open(cfg.Store, store.Options{Logger: log})
	if err != nil {
		return fmt.Errorf("opening store %s: %w", cfg.Store, err)
	}
	defer st.Close()

	path := cfg.Archive
	if path == "-" {
		tmp := filepath.Join(ownDir, fmt.Sprintf("import-%d.tar", os.Getpid()))
		if err := spoolStdin(stdin, tmp); err != nil {
			return err
		}
		defer os.Remove(tmp)
		path = tmp
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer arch.Close()

	tr := importer.NewTracker(importer.TrackerOptions{Verify: cfg.VerifyRoundTrip})
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               ownDir,
		MaxInMemory:           cfg.MaxInMemory,
		AnalyzeParallelism:    cfg.AnalyzeParallelism,
		AnalyzeTimeout:        cfg.AnalyzeTimeout,
		MaxConcurrentFinalize: cfg.MaxConcurrentFinalize,
		VerifyLimit:           cfg.VerifyLimit,
		VerifyRoundTrip:       cfg.VerifyRoundTrip,
		AllowRaw:              cfg.AllowRaw,
		RecentTTL:             importRecentTTL,
		Observer:              tr,
	}, log)
	if err != nil {
		return fmt.Errorf("creating blob store: %w", err)
	}
	images := image.New(st, blobs, log)

	plan, err := arch.Plan(dockerarchive.PlanOptions{Names: cfg.Names, Present: blobs.Exists})
	if err != nil {
		return fmt.Errorf("planning import: %w", err)
	}
	im := importer.New(blobs, images, arch, plan, tr, importer.Options{Workers: cfg.MaxConcurrentFinalize})

	label := cfg.Archive
	if label == "-" {
		label = "stdin"
	} else {
		label = filepath.Base(label)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := func() (*importer.Report, error) { return im.Run(ctx) }
	var rep *importer.Report
	if mode == "tui" {
		rep, err = tui.Run(tr, importTitle(label, plan), cancel, run)
	} else {
		rep, err = tui.RunPlain(stderr, tr, plainStatusInterval, run)
	}
	if err != nil {
		var terr *tui.TerminalError
		if errors.As(err, &terr) {
			return err
		}
		if errors.Is(err, context.Canceled) {
			return errors.New("import cancelled")
		}
		return err
	}
	// Close the store explicitly and check the error before reporting
	// success: Close is a real flush (GC, refs, objects) that can fail, and
	// a failed flush must not be masked by a success report. The deferred
	// st.Close() above is an idempotent backstop (store.Close uses
	// sync.Once) in case a path above this point returns early.
	if err := st.Close(); err != nil {
		return fmt.Errorf("closing store: %w", err)
	}
	fmt.Fprint(stdout, tui.RenderReport(rep, label))
	return nil
}

// importTitle is the TUI's first line: the archive's label and the names.
func importTitle(label string, plan *dockerarchive.Plan) string {
	var names []string
	for _, e := range plan.Entries {
		for _, n := range e.Names {
			names = append(names, n.String())
		}
	}
	return fmt.Sprintf("Importing %s → %s", label, strings.Join(names, ", "))
}

// spoolStdin copies r to path so the archive can be read in place.
func spoolStdin(r io.Reader, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("spooling stdin: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("spooling stdin: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("spooling stdin: %w", err)
	}
	return nil
}
