package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// runLsApp runs the ls command with args and returns the config the action
// received, or the error the app returned.
func runLsApp(t *testing.T, args ...string) (lsConfig, error) {
	t.Helper()
	var got lsConfig
	called := false
	app := newApp(commands{Ls: func(_ context.Context, cfg lsConfig) error {
		called = true
		got = cfg
		return nil
	}})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "ls"}, args...))
	if err == nil && !called {
		t.Fatal("ls action was not called")
	}
	return got, err
}

func TestLsFlags(t *testing.T) {
	clearEnv(t)
	cfg, err := runLsApp(t, "--store", "/srv/amber")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if want := (lsConfig{Store: "/srv/amber"}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	cfg, err = runLsApp(t, "--store", "/srv/amber", "-a", "library/app")
	if err != nil {
		t.Fatalf("ls -a repo: %v", err)
	}
	if !cfg.All || cfg.Repo != "library/app" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg, err := runLsApp(t, "--store", "/srv/amber", "--all"); err != nil || !cfg.All {
		t.Errorf("--all: %+v, %v", cfg, err)
	}
	if _, err := runLsApp(t, "--store", "/srv/amber", "a", "b"); err == nil || !strings.Contains(err.Error(), "at most one repository") {
		t.Errorf("two repositories: %v", err)
	}
	if _, err := runLsApp(t, "--store", "/srv/amber", "not:a/repo"); err == nil {
		t.Error("an invalid repository name must be rejected")
	}
	if _, err := runLsApp(t); err == nil {
		t.Error("--store is required")
	}
	t.Setenv("OCI_AMBER_STORE", "/env/store")
	if cfg, err := runLsApp(t); err != nil || cfg.Store != "/env/store" {
		t.Errorf("store from the environment: %+v, %v", cfg, err)
	}
}

// lsRows runs ls and returns the header and the rows, each split into
// columns at runs of two or more spaces.
func lsRows(t *testing.T, cfg lsConfig) (header []string, rows [][]string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cfg.Stdout, cfg.Stderr = &stdout, &stderr
	if err := runLs(context.Background(), cfg); err != nil {
		t.Fatalf("runLs: %v\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("ls wrote to stderr:\n%s", stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	split := regexp.MustCompile(`\s{2,}`)
	for i, l := range lines {
		cols := split.Split(strings.TrimSpace(l), -1)
		if i == 0 {
			header = cols
		} else {
			rows = append(rows, cols)
		}
	}
	return header, rows
}

var pushedRe = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d$`)

func TestRunLs(t *testing.T) {
	f := newFixture(t)
	header, rows := lsRows(t, lsConfig{Store: f.store})
	if want := []string{"REPOSITORY", "TAG", "DIGEST", "KIND", "SIZE", "ROOTFS", "PUSHED"}; !reflect.DeepEqual(header, want) {
		t.Fatalf("header = %q, want %q", header, want)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3:\n%q", len(rows), rows)
	}
	// Sorted by repository, then tag; digest is the first twelve hex
	// characters; the size is the image's total as the manifest describes it.
	appSize, indexSize := imageSize(t, f.store, "demo/app", "v1"), imageSize(t, f.store, "multi", "latest")
	for i, want := range [][]string{
		{"demo/app", "latest", f.app.Digest.Hex()[:12], "manifest", appSize, "ok"},
		{"demo/app", "v1", f.app.Digest.Hex()[:12], "manifest", appSize, "ok"},
		{"multi", "latest", f.index.Digest.Hex()[:12], "index (2 platforms + 1 attestation)", indexSize, "-"},
	} {
		if got := rows[i][:6]; !reflect.DeepEqual(got, want) {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
		if !pushedRe.MatchString(rows[i][6]) {
			t.Errorf("row %d pushed = %q", i, rows[i][6])
		}
	}

	// A repository argument filters; an unknown one is an error.
	if _, rows := lsRows(t, lsConfig{Store: f.store, Repo: "multi"}); len(rows) != 1 || rows[0][0] != "multi" {
		t.Errorf("filtered rows = %q", rows)
	}
	if err := runLs(context.Background(), lsConfig{Store: f.store, Repo: "nobody", Stdout: io.Discard, Stderr: io.Discard}); err == nil || !strings.Contains(err.Error(), "no repository nobody") {
		t.Errorf("unknown repository: %v", err)
	}
}

// TestRunLsUntagged: without -a only tags are listed; with it, manifests
// no tag points at appear as <none>, except the children of an index.
func TestRunLsUntagged(t *testing.T) {
	f := newFixture(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(f.store, store.Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	images := image.New(st, nil, quiet)
	for _, tag := range []string{"v1", "latest"} {
		if err := images.Delete("demo/app", tag); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, rows := lsRows(t, lsConfig{Store: f.store}); len(rows) != 1 || rows[0][0] != "multi" {
		t.Errorf("without -a: rows = %q", rows)
	}
	_, rows := lsRows(t, lsConfig{Store: f.store, All: true})
	if len(rows) != 2 {
		t.Fatalf("with -a: %d rows, want the untagged demo/app and multi:latest:\n%q", len(rows), rows)
	}
	if got := rows[0][:4]; !reflect.DeepEqual(got, []string{"demo/app", "<none>", f.app.Digest.Hex()[:12], "manifest"}) {
		t.Errorf("untagged row = %q", got)
	}
	if rows[1][0] != "multi" || rows[1][1] != "latest" {
		t.Errorf("second row = %q", rows[1])
	}
}

func TestRunLsEmptyStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	header, rows := lsRows(t, lsConfig{Store: dir})
	if header[0] != "REPOSITORY" || len(rows) != 0 {
		t.Errorf("empty store: header %q, rows %q", header, rows)
	}
}

func TestRunLsRefusesMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := runLs(context.Background(), lsConfig{Store: dir, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "no oci-amber store at "+dir) {
		t.Fatalf("missing store: %v", err)
	}
}

// imageSize is how ls should print the size of repo:tag.
func imageSize(t *testing.T, storeDir, repo, tag string) string {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(storeDir, store.Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im, err := image.New(st, nil, quiet).Open(repo, tag)
	if err != nil {
		t.Fatal(err)
	}
	return tui.FormatBytes(im.Meta.Stats.TotalBytes)
}
