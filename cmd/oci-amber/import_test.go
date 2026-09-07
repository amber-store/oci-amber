package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/oci"
)

// runImportApp runs the import command with args and returns the config
// the action received, or the error the app returned.
func runImportApp(t *testing.T, args ...string) (importConfig, error) {
	t.Helper()
	var got importConfig
	called := false
	app := newApp(commands{Import: func(_ context.Context, cfg importConfig) error {
		called = true
		got = cfg
		return nil
	}})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "import"}, args...))
	if err == nil && !called {
		t.Fatal("import action was not called")
	}
	return got, err
}

func TestImportFlagDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := runImportApp(t, "--store", "/srv/amber", "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	want := importConfig{
		Store:                 "/srv/amber",
		WorkDir:               filepath.Join("/srv/amber", "work"),
		MaxInMemory:           64 << 20,
		AnalyzeParallelism:    2,
		AnalyzeTimeout:        15 * time.Minute,
		MaxConcurrentFinalize: max(1, runtime.NumCPU()/2),
		VerifyLimit:           150 << 20,
		VerifyRoundTrip:       false,
		AllowRaw:              false,
		LogLevel:              slog.LevelInfo,
		Archive:               "image.tar",
		Progress:              "auto",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestImportFlagsExplicit(t *testing.T) {
	clearEnv(t)
	cfg, err := runImportApp(t, "--store", "/s", "--name", "a:1", "--name", "b:2", "--progress", "plain", "--log-file", "/tmp/x.log", "--verify-limit", "0", "--verify-roundtrip", "--allow-raw", "-")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Archive != "-" || !reflect.DeepEqual(cfg.Names, []string{"a:1", "b:2"}) || cfg.Progress != "plain" || cfg.LogFile != "/tmp/x.log" || cfg.VerifyLimit != 0 || !cfg.VerifyRoundTrip || !cfg.AllowRaw {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestImportFlagsFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("OCI_AMBER_STORE", "/srv/env-store")
	t.Setenv("OCI_AMBER_PROGRESS", "plain")
	t.Setenv("OCI_AMBER_LOG_FILE", "/tmp/env.log")
	t.Setenv("OCI_AMBER_VERIFY_ROUNDTRIP", "true")
	t.Setenv("OCI_AMBER_ALLOW_RAW", "true")

	cfg, err := runImportApp(t, "image.tar")
	if err != nil {
		t.Fatalf("import from env: %v", err)
	}
	if cfg.Store != "/srv/env-store" || cfg.Progress != "plain" || cfg.LogFile != "/tmp/env.log" || !cfg.VerifyRoundTrip || !cfg.AllowRaw {
		t.Fatalf("env flags: got %+v", cfg)
	}

	// A command-line flag wins over the environment.
	cfg, err = runImportApp(t, "--progress", "tui", "image.tar")
	if err != nil {
		t.Fatalf("import with override: %v", err)
	}
	if cfg.Progress != "tui" {
		t.Fatalf("progress = %q, want %q (flag must override env)", cfg.Progress, "tui")
	}
}

func TestImportRejectsBadValues(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{
		{"--store", "/s"},                                    // no archive
		{"--store", "/s", "a.tar", "b.tar"},                  // two archives
		{"--store", "/s", "--progress", "fancy", "a.tar"},    // bad progress mode
		{"--store", "/s", "--verify-limit", "lots", "a.tar"}, // bad verify limit
		{"--store", "/s", "--name", "not valid", "a.tar"},    // bad name
		{"a.tar"}, // no store
	} {
		if _, err := runImportApp(t, args...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

// TestRunImportPlain imports a synthesized archive in plain mode and checks
// the report and the store.
func TestRunImportPlain(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte(strings.Repeat("plain layer bytes ", 64))
	img := b.AddImage(config, []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"demo/app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	tmp := t.TempDir()
	path, err := b.WriteFile(tmp, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelInfo,
		Archive: path, Progress: "plain", Stdout: &stdout, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("runImport: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Imported image.tar in", "demo/app:v1", "Compressed", "Added to CAS", "Dedup ratio"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "blob stored") {
		t.Errorf("plain mode must log to stderr:\n%s", stderr.String())
	}
	// The tag is there: open the store again the way serve would.
	cfg2 := cfg
	cfg2.Stdout, cfg2.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := runImport(context.Background(), cfg2); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !strings.Contains(cfg2.Stdout.(*bytes.Buffer).String(), "everything already present") {
		t.Errorf("second run report:\n%s", cfg2.Stdout.(*bytes.Buffer).String())
	}
}

func TestRunImportFromStdin(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	img := b.AddImage(config, nil, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"empty:1"}})
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelWarn,
		Archive: "-", Progress: "plain", Stdin: bytes.NewReader(b.Bytes()), Stdout: &stdout, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("runImport: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "empty:1") {
		t.Errorf("report:\n%s", stdout.String())
	}
	entries, _ := filepath.Glob(filepath.Join(tmp, "store", "work", "oci-amber", "import-*.tar"))
	if len(entries) != 0 {
		t.Errorf("stdin copy left behind: %v", entries)
	}
}

// TestRunImportRemovesStaleSpoolFiles sweeps a spooled-stdin file left
// behind by a killed earlier run: nothing reads it again, and it would
// otherwise accumulate in the work directory across restarts.
func TestRunImportRemovesStaleSpoolFiles(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	img := b.AddImage(config, nil, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"stale:1"}})
	tmp := t.TempDir()
	path, err := b.WriteFile(tmp, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	ownDir := filepath.Join(tmp, "store", "work", "oci-amber")
	if err := os.MkdirAll(ownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ownDir, "import-99999.tar")
	if err := os.WriteFile(stale, []byte("leftover from a killed run"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelWarn,
		Archive: path, Progress: "plain", Stdout: &stdout, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("runImport: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale spool file survived the run: %v", err)
	}
}
