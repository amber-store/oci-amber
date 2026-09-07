package store_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"
	"github.com/jobs-build/amber-store-core/packstore"

	"github.com/amber-store/oci-amber/store"
)

// wantConfigJSON is the exact parameter record the spec pins.
const wantConfigJSON = `{"version":1,"chunking":{"minSize":32768,"normalSize":524288,"maxSize":1048576},"segmentSize":2147483648}` + "\n"

// openStore opens a fresh store under t.TempDir() and closes it at cleanup.
func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

// putBlob stores data as a single Blob object through the packstore directly
// (fstree.EncodeBlob + WriteBatch) so the tests do not depend on the writer.
func putBlob(t *testing.T, st *store.Store, data []byte) key.Key {
	t.Helper()
	obj, err := fstree.EncodeBlob(data)
	if err != nil {
		t.Fatalf("EncodeBlob: %v", err)
	}
	seq := func(yield func(packstore.Object, error) bool) {
		yield(packstore.Object{Key: obj.Key, Data: obj.Bytes}, nil)
	}
	if err := st.Objects.WriteBatch(seq); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	return obj.Key
}

func TestOpenCreatesConfigAndLayout(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	got, err := os.ReadFile(filepath.Join(dir, store.ConfigFile))
	if err != nil {
		t.Fatalf("reading %s: %v", store.ConfigFile, err)
	}
	if string(got) != wantConfigJSON {
		t.Fatalf("config file = %q, want %q", got, wantConfigJSON)
	}
	for _, sub := range []string{"packstore", "refs", "gc"} {
		fi, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Fatalf("stat %s: %v", sub, err)
		}
		if !fi.IsDir() {
			t.Fatalf("%s is not a directory", sub)
		}
	}
	if st.Config() != store.DefaultConfig() {
		t.Fatalf("Config() = %+v, want %+v", st.Config(), store.DefaultConfig())
	}
	if st.Objects == nil || st.Refs == nil || st.GC == nil {
		t.Fatal("Objects, Refs and GC must all be set")
	}
	if !strings.Contains(logBuf.String(), "store opened") {
		t.Fatalf("log = %q, want a 'store opened' line", logBuf.String())
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := store.DefaultConfig()
	if cfg.Version != 1 {
		t.Fatalf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Chunking != (store.Chunking{MinSize: 32768, NormalSize: 524288, MaxSize: 1048576}) {
		t.Fatalf("Chunking = %+v", cfg.Chunking)
	}
	if cfg.SegmentSize != 2<<30 {
		t.Fatalf("SegmentSize = %d, want %d", cfg.SegmentSize, 2<<30)
	}
	opts := cfg.Chunking.ByteOpts()
	if opts.MinSize != 32768 || opts.NormalSize != 524288 || opts.MaxSize != 1048576 {
		t.Fatalf("ByteOpts = %+v", *opts)
	}
	if store.ItemBits != 7 || store.ModeFile != 0o100644 || store.ModeDir != 0o040755 {
		t.Fatal("ItemBits/ModeFile/ModeDir differ from the spec")
	}
}

func TestReopenWithSameParameters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	k := putBlob(t, st, []byte("persisted across reopen"))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err = store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	got, err := os.ReadFile(filepath.Join(dir, store.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantConfigJSON {
		t.Fatalf("config file after reopen = %q, want %q", got, wantConfigJSON)
	}
	data, err := st.Get(k)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(data) != "persisted across reopen" {
		t.Fatalf("Get = %q", data)
	}
}

func TestOpenIsSingleOwner(t *testing.T) {
	st, dir := openStore(t)
	if _, err := store.Open(dir, store.Options{}); err == nil {
		t.Fatal("second Open of an open store succeeded, want an error")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	again.Close()
}

func TestOpenRefusesDifferentParameters(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"minSize", `{"version":1,"chunking":{"minSize":16384,"normalSize":524288,"maxSize":1048576},"segmentSize":2147483648}` + "\n"},
		{"segmentSize", `{"version":1,"chunking":{"minSize":32768,"normalSize":524288,"maxSize":1048576},"segmentSize":268435456}` + "\n"},
		{"version", `{"version":2,"chunking":{"minSize":32768,"normalSize":524288,"maxSize":1048576},"segmentSize":2147483648}` + "\n"},
		{"unknownField", `{"version":1,"chunking":{"minSize":32768,"normalSize":524288,"maxSize":1048576},"segmentSize":2147483648,"itemBits":8}` + "\n"},
		{"garbage", "not json\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "store")
			st, err := store.Open(dir, store.Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, store.ConfigFile), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			st, err = store.Open(dir, store.Options{})
			if err == nil {
				st.Close()
				t.Fatal("Open accepted a store with different parameters")
			}
			if !strings.Contains(err.Error(), "store parameters") {
				t.Fatalf("error = %q, want it to mention store parameters", err)
			}
			// The refused open must not have rewritten the record.
			got, rerr := os.ReadFile(filepath.Join(dir, store.ConfigFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != tc.body {
				t.Fatalf("config file was rewritten to %q", got)
			}
		})
	}
}

func TestGetAndHas(t *testing.T) {
	st, _ := openStore(t)
	data := []byte("hello, amber")
	k := putBlob(t, st, data)

	got, err := st.Get(k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}
	has, err := st.Has(k)
	if err != nil || !has {
		t.Fatalf("Has = %v, %v; want true, nil", has, err)
	}

	missing, err := fstree.EncodeBlob([]byte("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(missing.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	has, err = st.Has(missing.Key)
	if err != nil || has {
		t.Fatalf("Has(missing) = %v, %v; want false, nil", has, err)
	}
}

func TestOpenWithGCIntervalCloses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{GCInterval: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- st.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return; the background collector was not stopped")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenWhileOpenReportsInUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	_, err = store.Open(dir, store.Options{})
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("second Open returned %v, want ErrInUse", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q does not name the directory", err)
	}
}
