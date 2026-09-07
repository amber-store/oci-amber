package upload

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

const testID = "0123456789abcdef0123456789abcdef"

// newTestSession returns a session that spills into a fresh temp dir.
func newTestSession(t *testing.T, maxInMemory int64) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	s := newSession(testID, filepath.Join(dir, testID), maxInMemory, time.Now())
	t.Cleanup(func() { s.close() })
	return s, dir
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func appendBytes(t *testing.T, s *Session, data []byte) int64 {
	t.Helper()
	off, err := s.Append(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return off
}

func TestAppendBelowThresholdStaysInMemory(t *testing.T) {
	s, dir := newTestSession(t, 1024)
	first := pattern(500, 1)
	if off := appendBytes(t, s, first); off != 500 {
		t.Fatalf("offset after first append = %d, want 500", off)
	}
	if s.Offset() != 500 {
		t.Fatalf("Offset = %d, want 500", s.Offset())
	}
	// Exactly the threshold still stays in memory.
	second := pattern(524, 2)
	if off := appendBytes(t, s, second); off != 1024 {
		t.Fatalf("offset after second append = %d, want 1024", off)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("session below threshold created files %v", names)
	}
	all := append(append([]byte{}, first...), second...)
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if sp.Size() != 1024 {
		t.Fatalf("Spool.Size = %d, want 1024", sp.Size())
	}
	if sp.Digest().String() != sha256Hex(all) {
		t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), sha256Hex(all))
	}
	checkSpoolReads(t, sp, all)
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("taking a memory spool created files %v", names)
	}
}

func TestAppendAboveThresholdSpillsToOneFile(t *testing.T) {
	s, dir := newTestSession(t, 1024)
	first := pattern(700, 1)
	if off := appendBytes(t, s, first); off != 700 {
		t.Fatalf("offset = %d, want 700", off)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("premature spill: %v", names)
	}

	second := pattern(700, 2)
	if off := appendBytes(t, s, second); off != 1400 {
		t.Fatalf("offset = %d, want 1400", off)
	}
	names := dirNames(t, dir)
	if len(names) != 1 || names[0] != testID {
		t.Fatalf("dir entries after spill = %v, want [%s]", names, testID)
	}
	all := append(append([]byte{}, first...), second...)
	onDisk, err := os.ReadFile(filepath.Join(dir, testID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, all) {
		t.Fatal("spilled file does not hold the buffered bytes followed by the new ones")
	}
	if s.buf.Len() != 0 {
		t.Fatalf("memory buffer still holds %d bytes after spill", s.buf.Len())
	}

	third := pattern(300, 3)
	if off := appendBytes(t, s, third); off != 1700 {
		t.Fatalf("offset = %d, want 1700", off)
	}
	if s.Offset() != 1700 {
		t.Fatalf("Offset = %d, want 1700", s.Offset())
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("dir entries = %v, want exactly one", names)
	}
	all = append(all, third...)
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if sp.Size() != 1700 {
		t.Fatalf("Spool.Size = %d, want 1700", sp.Size())
	}
	if sp.Digest().String() != sha256Hex(all) {
		t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), sha256Hex(all))
	}
	checkSpoolReads(t, sp, all)
}

func TestAppendSingleWriteLargerThanThreshold(t *testing.T) {
	data := pattern(5000, 4)
	cases := []struct {
		name string
		src  func() io.Reader
	}{
		// bytes.Reader implements io.WriterTo, so io.Copy hands the whole
		// slice to one Write.
		{"one write", func() io.Reader { return bytes.NewReader(data) }},
		// OneByteReader forces io.Copy through its read loop, one byte at a time.
		{"byte at a time", func() io.Reader { return iotest.OneByteReader(bytes.NewReader(data)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := newTestSession(t, 100)
			off, err := s.Append(tc.src())
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if off != 5000 {
				t.Fatalf("offset = %d, want 5000", off)
			}
			if names := dirNames(t, dir); len(names) != 1 {
				t.Fatalf("dir entries = %v, want exactly one", names)
			}
			if s.buf.Len() != 0 {
				t.Fatalf("memory buffer holds %d bytes after spill", s.buf.Len())
			}
			sp, err := s.Spool()
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			if sp.Digest() != oci.DigestOfBytes(data) {
				t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), oci.DigestOfBytes(data))
			}
			checkSpoolReads(t, sp, data)
		})
	}
}

func TestAppendZeroThresholdSpillsImmediately(t *testing.T) {
	s, dir := newTestSession(t, 0)
	data := pattern(10, 5)
	if off := appendBytes(t, s, data); off != 10 {
		t.Fatalf("offset = %d, want 10", off)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("dir entries = %v, want exactly one", names)
	}
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	checkSpoolReads(t, sp, data)
}

func TestAppendReaderErrorKeepsPrefix(t *testing.T) {
	s, _ := newTestSession(t, 1<<20)
	data := pattern(100, 6)
	boom := errors.New("boom")
	off, err := s.Append(io.MultiReader(bytes.NewReader(data), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatalf("Append error = %v, want %v", err, boom)
	}
	if off != 100 {
		t.Fatalf("offset = %d, want 100", off)
	}
	if s.Offset() != 100 {
		t.Fatalf("Offset = %d, want 100", s.Offset())
	}
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if sp.Digest().String() != sha256Hex(data) {
		t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), sha256Hex(data))
	}
	checkSpoolReads(t, sp, data)
}

func TestSpoolDigestEqualsSha256OfConcatenation(t *testing.T) {
	cases := []struct {
		name        string
		maxInMemory int64
	}{
		{"memory", 1 << 20},
		{"file", 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestSession(t, tc.maxInMemory)
			var all []byte
			for i, n := range []int{7, 3000, 1} {
				piece := pattern(n, byte(i+10))
				appendBytes(t, s, piece)
				all = append(all, piece...)
			}
			sp, err := s.Spool()
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			if sp.Size() != int64(len(all)) {
				t.Fatalf("Spool.Size = %d, want %d", sp.Size(), len(all))
			}
			if sp.Digest().String() != sha256Hex(all) {
				t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), sha256Hex(all))
			}
			if sp.Digest() != oci.DigestOfBytes(all) {
				t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), oci.DigestOfBytes(all))
			}
		})
	}
}

func TestSessionSpoolOpenReadsFullData(t *testing.T) {
	cases := []struct {
		name        string
		maxInMemory int64
	}{
		{"memory", 1 << 20},
		{"file", 10},
	}
	data := pattern(20_000, 20)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestSession(t, tc.maxInMemory)
			appendBytes(t, s, data[:9_000])
			appendBytes(t, s, data[9_000:])
			sp, err := s.Spool()
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			checkSpoolReads(t, sp, data)
			// A second Open works too.
			checkSpoolReads(t, sp, data)
		})
	}
}

func TestEmptySessionSpool(t *testing.T) {
	s, dir := newTestSession(t, 1<<20)
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if sp.Size() != 0 {
		t.Fatalf("Spool.Size = %d, want 0", sp.Size())
	}
	if sp.Digest().String() != sha256Hex(nil) {
		t.Fatalf("Spool.Digest = %s, want %s", sp.Digest(), sha256Hex(nil))
	}
	checkSpoolReads(t, sp, nil)
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("empty session created files %v", names)
	}
}

func TestSpoolIsSnapshotOfSession(t *testing.T) {
	cases := []struct {
		name        string
		maxInMemory int64
	}{
		{"memory", 1 << 20},
		{"file", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestSession(t, tc.maxInMemory)
			appendBytes(t, s, []byte("abc"))
			sp1, err := s.Spool()
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			appendBytes(t, s, []byte("def"))
			sp2, err := s.Spool()
			if err != nil {
				t.Fatalf("Spool: %v", err)
			}
			if sp1.Size() != 3 || sp1.Digest() != oci.DigestOfBytes([]byte("abc")) {
				t.Fatalf("first spool changed: size %d digest %s", sp1.Size(), sp1.Digest())
			}
			checkSpoolReads(t, sp1, []byte("abc"))
			if sp2.Size() != 6 || sp2.Digest() != oci.DigestOfBytes([]byte("abcdef")) {
				t.Fatalf("second spool: size %d digest %s", sp2.Size(), sp2.Digest())
			}
			checkSpoolReads(t, sp2, []byte("abcdef"))
		})
	}
}

func TestSessionCloseRemovesFileAndRejectsUse(t *testing.T) {
	s, dir := newTestSession(t, 10)
	appendBytes(t, s, pattern(100, 7))
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("dir entries = %v, want exactly one", names)
	}
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("file left after close: %v", names)
	}
	if _, err := s.Append(bytes.NewReader([]byte("x"))); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Append after close = %v, want ErrUnknown", err)
	}
	if _, err := s.Spool(); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Spool after close = %v, want ErrUnknown", err)
	}
	if err := s.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestConcurrentAppendsAreSerialized(t *testing.T) {
	const (
		goroutines = 8
		perRoutine = 8
		chunk      = 1000
	)
	s, dir := newTestSession(t, 4096)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perRoutine)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perRoutine {
				if _, err := s.Append(bytes.NewReader(pattern(chunk, byte(g*perRoutine+i)))); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}
	total := int64(goroutines * perRoutine * chunk)
	if s.Offset() != total {
		t.Fatalf("Offset = %d, want %d", s.Offset(), total)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("dir entries = %v, want exactly one", names)
	}
	sp, err := s.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if sp.Size() != total {
		t.Fatalf("Spool.Size = %d, want %d", sp.Size(), total)
	}
	// The running hash and the stored bytes agree whatever the interleaving.
	r, err := sp.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.(io.Closer).Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if oci.DigestOfBytes(got) != sp.Digest() {
		t.Fatalf("stored bytes hash to %s, spool digest is %s", oci.DigestOfBytes(got), sp.Digest())
	}
}
