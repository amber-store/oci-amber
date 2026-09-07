package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/store"
)

// runBrowseApp runs the browse command with args and returns the config
// the action received, or the error the app returned.
func runBrowseApp(t *testing.T, args ...string) (browseConfig, error) {
	t.Helper()
	var got browseConfig
	called := false
	app := newApp(commands{Browse: func(_ context.Context, cfg browseConfig) error {
		called = true
		got = cfg
		return nil
	}})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "browse"}, args...))
	if err == nil && !called {
		t.Fatal("browse action was not called")
	}
	return got, err
}

func TestBrowseFlags(t *testing.T) {
	clearEnv(t)
	cfg, err := runBrowseApp(t, "--store", "/srv/amber")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if want := (browseConfig{Store: "/srv/amber"}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	cfg, err = runBrowseApp(t, "--store", "/srv/amber", "--log-file", "/tmp/browse.log", "library/app:v1")
	if err != nil {
		t.Fatalf("browse with args: %v", err)
	}
	if cfg.Start != "library/app:v1" || cfg.LogFile != "/tmp/browse.log" {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := runBrowseApp(t, "--store", "/srv/amber", "a", "b"); err == nil || !strings.Contains(err.Error(), "at most one reference") {
		t.Errorf("two references: %v", err)
	}
	if _, err := runBrowseApp(t); err == nil {
		t.Error("--store is required")
	}
	t.Setenv("OCI_AMBER_STORE", "/env/store")
	if cfg, err := runBrowseApp(t); err != nil || cfg.Store != "/env/store" {
		t.Errorf("store from the environment: %+v, %v", cfg, err)
	}
}

func TestRunBrowseRefusesMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := runBrowse(context.Background(), browseConfig{Store: dir, Stdout: &bytes.Buffer{}, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "no oci-amber store at "+dir) {
		t.Fatalf("missing store: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("browse must not create a store directory")
	}
}

func TestRunBrowseNeedsTerminal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	err = runBrowse(context.Background(), browseConfig{Store: dir, Stdout: &bytes.Buffer{}, Stderr: io.Discard})
	if err == nil || err.Error() != "browse needs a terminal" {
		t.Fatalf("not a terminal: %v", err)
	}
}
