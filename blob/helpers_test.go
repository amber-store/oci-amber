package blob

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// logBuffer collects slog output. The handler serializes its own writes; the
// mutex guards String() against handlers still writing.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// newTestStore opens a fresh amber store in a temp dir and a blob store
// over it. A zero WorkDir gets its own temp dir.
func newTestStore(t *testing.T, opts Options) (*Store, *store.Store, *logBuffer) {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if opts.WorkDir == "" {
		opts.WorkDir = t.TempDir()
	}
	logs := &logBuffer{}
	b, err := New(st, opts, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, st, logs
}

func spoolOf(data []byte) *upload.Spool { return upload.NewMemorySpool(data) }

// spoolDirOf is the zrecipe temp directory of b.
func spoolDirOf(b *Store) string { return filepath.Join(b.opts.WorkDir, spoolDirName) }

// assertSpoolDirEmpty fails when zrecipe left a file under the work dir.
func assertSpoolDirEmpty(t *testing.T, b *Store) {
	t.Helper()
	entries, err := os.ReadDir(spoolDirOf(b))
	if err != nil {
		t.Fatalf("spool dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("files left under %s: %v", spoolDirOf(b), entries)
	}
}

// textBytes returns n bytes of compressible pseudo-text.
func textBytes(n int, seed int64) []byte {
	r := mrand.New(mrand.NewSource(seed))
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	var sb strings.Builder
	for sb.Len() < n {
		sb.WriteString(words[r.Intn(len(words))])
		sb.WriteByte(' ')
		if r.Intn(12) == 0 {
			sb.WriteByte('\n')
		}
	}
	return []byte(sb.String())[:n]
}

// randomBytes returns n incompressible bytes.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// tarBytes builds an uncompressed tar holding one regular file.
func tarBytes(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// gzipBytes compresses data with compress/gzip at level.
func gzipBytes(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// twoLevelGzip builds one gzip member whose deflate payload comes from two
// compress/flate writers at different levels joined at a sync-flush
// boundary. It inflates fine, but no single-pass encoder emits an empty
// stored block followed by a restarted compressor at another level, so
// zrecipe cannot reproduce it.
func twoLevelGzip(t *testing.T, part1, part2 []byte) []byte {
	t.Helper()
	var payload bytes.Buffer
	w1, err := flate.NewWriter(&payload, flate.BestSpeed)
	if err != nil {
		t.Fatalf("flate: %v", err)
	}
	if _, err := w1.Write(part1); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w1.Flush(); err != nil { // non-final blocks, byte aligned
		t.Fatalf("flate flush: %v", err)
	}
	w2, err := flate.NewWriter(&payload, flate.BestCompression)
	if err != nil {
		t.Fatalf("flate: %v", err)
	}
	if _, err := w2.Write(part2); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w2.Close(); err != nil { // final block
		t.Fatalf("flate close: %v", err)
	}
	var out bytes.Buffer
	out.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff})
	out.Write(payload.Bytes())
	crc := crc32.NewIEEE()
	crc.Write(part1)
	crc.Write(part2)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc.Sum32())
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(len(part1)+len(part2)))
	out.Write(trailer[:])
	return out.Bytes()
}

// corruptGzipCRC flips the first byte of the gzip trailer's CRC32.
func corruptGzipCRC(gz []byte) []byte {
	bad := append([]byte{}, gz...)
	bad[len(bad)-8] ^= 0xff
	return bad
}
