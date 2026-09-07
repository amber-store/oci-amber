package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/tui"
)

// lsConfig is everything `ls` needs. lsConfigFromCLI fills it from flags;
// tests construct it directly and call runLs.
type lsConfig struct {
	Store  string
	All    bool      // untagged manifests too
	Repo   string    // "" lists every repository
	Stdout io.Writer // nil means os.Stdout
	Stderr io.Writer // nil means os.Stderr
}

func lsFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.BoolFlag{Name: "all", Aliases: []string{"a"}, Usage: "list the manifests no tag points at as well (as <none>), the children of an index excepted"},
	}
}

func lsConfigFromCLI(c *cli.Context) (lsConfig, error) {
	if c.NArg() > 1 {
		return lsConfig{}, errors.New("ls takes at most one repository")
	}
	cfg := lsConfig{Store: c.String("store"), All: c.Bool("all"), Repo: c.Args().First()}
	if cfg.Store == "" {
		return lsConfig{}, errors.New("--store must not be empty")
	}
	if cfg.Repo != "" {
		if err := oci.ValidateRepository(cfg.Repo); err != nil {
			return lsConfig{}, err
		}
	}
	return cfg, nil
}

// lsRow is one line of the listing: an image and, for an index, what it
// holds.
type lsRow struct {
	repo, tag    string // tag is "" for an untagged manifest
	meta         image.Meta
	platforms    int          // index children that are not attestations
	attestations int          // index children that are
	children     []oci.Digest // index children
}

// runLs opens the store read-only, collects the images and prints them as
// a table: one row per tag, sorted by repository and tag; with All, the
// manifests no tag points at follow each repository's tags, sorted by
// digest, except the children of an index, which its row accounts for.
func runLs(ctx context.Context, cfg lsConfig) error {
	stdout, stderr := cfg.Stdout, cfg.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ro, err := openReadOnly(cfg.Store, log)
	if err != nil {
		return err
	}
	rows, err := listImages(ctx, ro.images, cfg.Repo, cfg.All)
	if err := errors.Join(err, ro.Close()); err != nil {
		return err
	}
	return writeLsTable(stdout, rows)
}

// listImages collects the rows of repo, or of every repository when repo
// is "".
func listImages(ctx context.Context, images *image.Store, repo string, all bool) ([]lsRow, error) {
	repos := []string{repo}
	if repo == "" {
		var err error
		if repos, err = images.Repositories(); err != nil {
			return nil, err
		}
	}
	var rows []lsRow
	for _, repo := range repos {
		tags, err := images.Tags(repo)
		if errors.Is(err, image.ErrNotFound) {
			return nil, fmt.Errorf("no repository %s", repo)
		}
		if err != nil {
			return nil, err
		}
		tagged := map[oci.Digest]bool{}
		children := map[oci.Digest]bool{}
		for _, tag := range tags {
			row, err := newLsRow(ctx, images, repo, tag)
			if err != nil {
				return nil, err
			}
			tagged[row.meta.Digest] = true
			for _, c := range row.children {
				children[c] = true
			}
			rows = append(rows, row)
		}
		if !all {
			continue
		}
		digests, err := images.Manifests(repo)
		if err != nil && !errors.Is(err, image.ErrNotFound) {
			return nil, err
		}
		var untagged []lsRow
		for _, d := range digests {
			if tagged[d] {
				continue
			}
			row, err := newLsRow(ctx, images, repo, d.String())
			if err != nil {
				return nil, err
			}
			for _, c := range row.children {
				children[c] = true
			}
			untagged = append(untagged, row)
		}
		for _, row := range untagged {
			if !children[row.meta.Digest] {
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

// newLsRow opens reference (a tag or a digest) in repo. An index is read
// and parsed for what it holds.
func newLsRow(ctx context.Context, images *image.Store, repo, reference string) (lsRow, error) {
	im, err := images.Open(repo, reference)
	if err != nil {
		return lsRow{}, fmt.Errorf("opening %s %s: %w", repo, reference, err)
	}
	row := lsRow{repo: repo, meta: im.Meta}
	if !oci.IsDigest(reference) {
		row.tag = reference
	}
	if im.Meta.Kind != image.KindIndex {
		return row, nil
	}
	var buf bytes.Buffer
	if err := im.WriteTo(ctx, &buf); err != nil {
		return lsRow{}, fmt.Errorf("reading index %s of %s: %w", im.Meta.Digest, repo, err)
	}
	m, err := oci.ParseManifest(buf.Bytes())
	if err != nil {
		return lsRow{}, fmt.Errorf("index %s of %s: %w", im.Meta.Digest, repo, err)
	}
	for _, c := range m.Manifests {
		row.children = append(row.children, c.Digest)
		if c.IsAttestation() {
			row.attestations++
		} else {
			row.platforms++
		}
	}
	return row, nil
}

// writeLsTable prints rows under a header, columns aligned.
func writeLsTable(w io.Writer, rows []lsRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tDIGEST\tKIND\tSIZE\tROOTFS\tPUSHED")
	for _, r := range rows {
		tag := r.tag
		if tag == "" {
			tag = "<none>"
		}
		kind, rootfs := string(r.meta.Kind), "-"
		if r.meta.Kind == image.KindIndex {
			kind = "index (" + plural(r.platforms, "platform")
			if r.attestations > 0 {
				kind += " + " + plural(r.attestations, "attestation")
			}
			kind += ")"
		} else if r.meta.Rootfs != nil {
			rootfs = string(r.meta.Rootfs.Status)
		}
		hex := r.meta.Digest.Hex()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.repo, tag, hex[:min(12, len(hex))], kind, tui.FormatBytes(r.meta.Stats.TotalBytes), rootfs,
			r.meta.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

// plural renders n with word, pluralized ("1 platform", "2 platforms").
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
