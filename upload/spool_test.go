package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

// pattern returns n deterministic bytes that differ between seeds.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ seed
	}
	return b
}

// sha256Hex computes the digest string independently of package oci.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// checkSpoolReads opens sp and checks that the reader yields want from
// offset 0, seeks back to the start, reads at an offset and reports its
// size when seeking to the end.
func checkSpoolReads(t *testing.T, sp *Spool, want []byte) {
	t.Helper()
	r, err := sp.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, want %d bytes with identical content", len(got), len(want))
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek(0): %v", err)
	}
	got, err = io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll after Seek: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("second read differs from input")
	}
	if len(want) >= 8 {
		buf := make([]byte, 4)
		if _, err := r.ReadAt(buf, 3); err != nil {
			t.Fatalf("ReadAt(3): %v", err)
		}
		if !bytes.Equal(buf, want[3:7]) {
			t.Fatalf("ReadAt(3) = %q, want %q", buf, want[3:7])
		}
	}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek(end): %v", err)
	}
	if end != int64(len(want)) {
		t.Fatalf("Seek(end) = %d, want %d", end, len(want))
	}
}

func TestMemorySpool(t *testing.T) {
	data := []byte("hello, upload spool")
	sp := NewMemorySpool(data)
	if sp.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", sp.Size(), len(data))
	}
	if sp.Digest().String() != sha256Hex(data) {
		t.Fatalf("Digest = %s, want %s", sp.Digest(), sha256Hex(data))
	}
	if sp.Digest() != oci.DigestOfBytes(data) {
		t.Fatalf("Digest = %s, want %s", sp.Digest(), oci.DigestOfBytes(data))
	}
	checkSpoolReads(t, sp, data)
	if err := sp.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := sp.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := sp.Open(); err == nil {
		t.Fatal("Open after Remove succeeded")
	}
}

func TestEmptyMemorySpool(t *testing.T) {
	sp := NewMemorySpool(nil)
	if sp.Size() != 0 {
		t.Fatalf("Size = %d, want 0", sp.Size())
	}
	if sp.Digest().String() != sha256Hex(nil) {
		t.Fatalf("Digest = %s, want %s", sp.Digest(), sha256Hex(nil))
	}
	checkSpoolReads(t, sp, nil)
}

func TestFileSpool(t *testing.T) {
	dir := t.TempDir()
	data := pattern(10_000, 1)
	path := filepath.Join(dir, "spool")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sp := &Spool{size: int64(len(data)), digest: oci.DigestOfBytes(data), path: path}
	checkSpoolReads(t, sp, data)

	// Two readers may be open at once and are independent.
	r1, err := sp.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r2, err := sp.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c1, ok := r1.(io.Closer)
	if !ok {
		t.Fatal("file spool reader does not implement io.Closer")
	}
	c2 := r2.(io.Closer)
	head := make([]byte, 100)
	if _, err := io.ReadFull(r1, head); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got2, data) {
		t.Fatal("second reader did not read the full data")
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Remove deletes the file and is idempotent.
	if err := sp.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists after Remove: %v", err)
	}
	if err := sp.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := sp.Open(); err == nil {
		t.Fatal("Open after Remove succeeded")
	}
}

func TestFileSpoolIsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool")
	first := pattern(100, 2)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	sp := &Spool{size: int64(len(first)), digest: oci.DigestOfBytes(first), path: path}

	// The file grows after the snapshot was taken.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(pattern(50, 3)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	checkSpoolReads(t, sp, first)
}

func TestSectionSpoolReadsOnlyItsWindow(t *testing.T) {
	data := []byte("0123456789abcdef")
	window := data[4:12]
	sp := NewSectionSpool(bytes.NewReader(data), 4, int64(len(window)), oci.DigestOfBytes(window))
	if sp.Size() != 8 {
		t.Fatalf("Size = %d, want 8", sp.Size())
	}
	if sp.Digest() != oci.DigestOfBytes(window) {
		t.Fatalf("Digest = %s", sp.Digest())
	}
	r, err := sp.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789ab" {
		t.Fatalf("read %q, want %q", got, "456789ab")
	}
	buf := make([]byte, 3)
	if _, err := r.ReadAt(buf, 5); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "9ab" {
		t.Fatalf("ReadAt(5) = %q, want %q", buf, "9ab")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(r)
	if string(again) != "456789ab" {
		t.Fatalf("after Seek read %q", again)
	}
}

func TestSectionSpoolRemoveLeavesSourceAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sp := NewSectionSpool(f, 6, 5, oci.DigestOfBytes([]byte("world")))
	if err := sp.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Remove must not touch the source file: %v", err)
	}
	if _, err := sp.Open(); err == nil {
		t.Fatal("Open after Remove must fail")
	}
}
