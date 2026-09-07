package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/store"
)

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testConfig is a small, fast configuration for an in-process server.
func testConfig(storeDir string, logs io.Writer) config {
	return config{
		Store:                 storeDir,
		Listen:                "127.0.0.1:0",
		MaxInMemory:           1 << 20,
		AnalyzeParallelism:    1,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 1,
		VerifyRoundTrip:       true,
		UploadTimeout:         time.Hour,
		GCInterval:            0,
		LogLevel:              slog.LevelDebug,
		LogOutput:             logs,
	}
}

func writeLeftover(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunServesV2(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	workDir := filepath.Join(storeDir, "work")
	ownDir := filepath.Join(workDir, workSubdir)
	spoolLeftover := filepath.Join(ownDir, "spool", "leftover")
	uploadLeftover := filepath.Join(ownDir, "uploads", "leftover")
	writeLeftover(t, spoolLeftover)
	writeLeftover(t, uploadLeftover)
	// --work-dir may be a directory the operator also uses for other
	// things (a shared scratch disk). The registry owns exactly
	// <work-dir>/oci-amber and must not delete anything beside it, not even
	// under the names it used to occupy directly.
	operatorFiles := []string{
		filepath.Join(workDir, "operator-data"),
		filepath.Join(workDir, "spool", "operator-data"),
		filepath.Join(workDir, "uploads", "operator-data"),
	}
	for _, p := range operatorFiles {
		writeLeftover(t, p)
	}

	logs := &syncBuffer{}
	cfg := testConfig(storeDir, logs)
	addrCh := make(chan net.Addr, 1)
	cfg.OnListen = func(a net.Addr) { addrCh <- a }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("run exited before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the server to listen")
	}

	resp, err := http.Get("http://" + addr.String() + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading /v2/ body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v2/ status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("Docker-Distribution-API-Version = %q, want %q", got, "registry/2.0")
	}
	if got := strings.TrimSpace(string(body)); got != "{}" {
		t.Fatalf("GET /v2/ body = %q, want {}", got)
	}

	if _, err := os.Stat(filepath.Join(storeDir, store.ConfigFile)); err != nil {
		t.Fatalf("store config file: %v", err)
	}
	for _, p := range []string{spoolLeftover, uploadLeftover} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed at startup, stat err = %v", p, err)
		}
	}
	for _, p := range operatorFiles {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s is not the registry's to delete: %v", p, err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancel, want nil", err)
		}
	case <-time.After(shutdownTimeout + 10*time.Second):
		t.Fatal("run did not return after cancel")
	}

	if conn, err := net.DialTimeout("tcp", addr.String(), time.Second); err == nil {
		conn.Close()
		t.Fatalf("listener %s still accepts connections after shutdown", addr)
	}

	out := logs.String()
	for _, want := range []string{`msg="oci-amber listening"`, `addr=` + addr.String(), `msg="shutting down"`, `msg="oci-amber stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output lacks %q:\n%s", want, out)
		}
	}

	// A clean shutdown released the store: it can be opened again.
	st, err := store.Open(storeDir, store.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("reopening store after shutdown: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRunCallsStopAtShutdownStart covers the second-SIGINT fix: run must
// call cfg.Stop (main wires it to signal.NotifyContext's stop) the moment
// ctx.Done fires, before the shutdown drain, not after run returns. A nil
// Stop (every other test in this file) must remain a silent no-op.
func TestRunCallsStopAtShutdownStart(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	logs := &syncBuffer{}
	cfg := testConfig(storeDir, logs)
	addrCh := make(chan net.Addr, 1)
	cfg.OnListen = func(a net.Addr) { addrCh <- a }
	stopped := make(chan struct{})
	cfg.Stop = func() { close(stopped) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()

	select {
	case <-addrCh:
	case err := <-done:
		t.Fatalf("run exited before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the server to listen")
	}

	cancel()
	select {
	case <-stopped:
	case err := <-done:
		t.Fatalf("run returned (%v) before calling Stop", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop was not called promptly after ctx.Done")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancel, want nil", err)
		}
	case <-time.After(shutdownTimeout + 10*time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRunFailsWhenListenAddressIsBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	storeDir := filepath.Join(t.TempDir(), "store")
	cfg := testConfig(storeDir, io.Discard)
	cfg.Listen = ln.Addr().String()
	listened := false
	cfg.OnListen = func(net.Addr) { listened = true }

	err = run(context.Background(), cfg)
	if err == nil {
		t.Fatal("run succeeded on a busy address")
	}
	if !strings.Contains(err.Error(), "listening on "+ln.Addr().String()) {
		t.Fatalf("error %q does not name the listen address", err)
	}
	if listened {
		t.Fatal("OnListen was called although listening failed")
	}

	// The store was closed on the failure path: it can be opened again.
	st, err := store.Open(storeDir, store.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("reopening store after failed run: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailsWhenStoreCannotBeOpened(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(notADir, io.Discard)
	cfg.WorkDir = filepath.Join(t.TempDir(), "work")
	cfg.OnListen = func(net.Addr) { t.Error("OnListen called although the store could not be opened") }

	err := run(context.Background(), cfg)
	if err == nil {
		t.Fatal("run succeeded with a regular file as the store directory")
	}
	if !strings.Contains(err.Error(), "opening store") {
		t.Fatalf("error %q does not mention opening the store", err)
	}
}

func TestRunRejectsEmptyConfig(t *testing.T) {
	if err := run(context.Background(), config{}); err == nil {
		t.Fatal("run accepted an empty config")
	}
	if err := run(context.Background(), config{Store: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "listen address") {
		t.Fatalf("run without a listen address: got %v", err)
	}
}
