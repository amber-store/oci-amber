package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

func TestPutConfigBlobRaw(t *testing.T) {
	b, st, logs := newTestStore(t, Options{})
	data := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	d := oci.DigestOfBytes(data)
	before := time.Now()
	m, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := Meta{Version: MetaVersion, Digest: d, Size: int64(len(data)), Kind: KindRaw, Format: "none", RawReason: ReasonNotTar, UploadedAt: m.UploadedAt, Stats: m.Stats}
	metaEqual(t, want, *m)
	if m.UploadedAt.Before(before.Add(-time.Second)) || m.UploadedAt.Location() != time.UTC {
		t.Fatalf("uploadedAt = %v", m.UploadedAt)
	}
	s := m.Stats
	if s.LogicalBytes < int64(len(data)) || s.NewLogicalBytes != s.LogicalBytes || s.DedupedBytes != 0 || s.DiskBytes <= 0 || s.ObjectsNew == 0 || s.ObjectsDeduped != 0 {
		t.Fatalf("first-write stats = %+v", s)
	}

	ok, err := b.Exists(d)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	bl, err := b.Open(d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	metaEqual(t, *m, bl.Meta)
	if !bl.SupportsRange() {
		t.Fatal("raw blob must support ranges")
	}
	got := pullAll(t, bl)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %q, want %q", got, data)
	}
	if oci.DigestOfBytes(got) != d {
		t.Fatal("pulled digest differs")
	}
	if _, err := st.Lookup(bl.Root(), RawFile); err != nil {
		t.Fatalf("raw entry: %v", err)
	}
	if _, err := st.Lookup(bl.Root(), CompFile); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("comp.json on a raw root: %v", err)
	}
	assertSpoolDirEmpty(t, b)

	line := logs.String()
	for _, want := range []string{"blob stored", "digest=" + d.String(), "kind=raw", "format=none", "raw_reason=not-tar", "logical_bytes=", "deduped_bytes=0", "disk_bytes=", "duration="} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q:\n%s", want, line)
		}
	}
}

func TestPutSameBlobTwiceIsDeduplicated(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	data := textBytes(50000, 3)
	d := oci.DigestOfBytes(data)
	size := int64(len(data))

	m1, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if s, ok := b.TakeRecent(d); !ok || s != m1.Stats {
		t.Fatalf("TakeRecent after first Put = %+v, %v; want %+v", s, ok, m1.Stats)
	}

	sp := spoolOf(data)
	m2, err := b.Put(context.Background(), sp)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	wantStats := store.Stats{LogicalBytes: size, DedupedBytes: size}
	if m2.Stats != wantStats {
		t.Fatalf("dedup stats = %+v, want %+v", m2.Stats, wantStats)
	}
	m1.Stats = wantStats
	metaEqual(t, *m1, *m2)
	if s, ok := b.TakeRecent(d); !ok || s != wantStats {
		t.Fatalf("TakeRecent after dedup = %+v, %v; want %+v", s, ok, wantStats)
	}
	if _, ok := b.TakeRecent(d); ok {
		t.Fatal("TakeRecent must consume the entry")
	}
	if _, err := sp.Open(); err == nil {
		t.Fatal("a deduplicated spool must be discarded")
	}
}

func TestReuseKnownDigest(t *testing.T) {
	b, _, logs := newTestStore(t, Options{})
	data := textBytes(50000, 3)
	d := oci.DigestOfBytes(data)
	size := int64(len(data))

	// A miss changes nothing.
	if _, err := b.Reuse(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reuse of an unknown digest: %v, want ErrNotFound", err)
	}
	if _, ok := b.TakeRecent(d); ok {
		t.Fatal("a miss must not record a recent entry")
	}
	if strings.Contains(logs.String(), "blob already present") {
		t.Fatal("a miss must not log a dedup hit")
	}

	m1, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := b.TakeRecent(d); !ok {
		t.Fatal("TakeRecent after Put")
	}

	// A hit behaves like Put's whole-blob dedup, without a spool.
	m2, err := b.Reuse(d)
	if err != nil {
		t.Fatalf("Reuse: %v", err)
	}
	wantStats := store.Stats{LogicalBytes: size, DedupedBytes: size}
	if m2.Stats != wantStats {
		t.Fatalf("dedup stats = %+v, want %+v", m2.Stats, wantStats)
	}
	m1.Stats = wantStats
	metaEqual(t, *m1, *m2)
	if s, ok := b.TakeRecent(d); !ok || s != wantStats {
		t.Fatalf("TakeRecent after Reuse = %+v, %v; want %+v", s, ok, wantStats)
	}
	line := logs.String()
	for _, want := range []string{"blob already present", "digest=" + d.String(), "size=50000"} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q:\n%s", want, line)
		}
	}
	if strings.Count(line, "blob stored") != 1 {
		t.Errorf("Reuse must not ingest:\n%s", line)
	}
}

func TestRecentTableExpires(t *testing.T) {
	b, _, _ := newTestStore(t, Options{RecentTTL: time.Nanosecond})
	one := []byte(`{"one":1}`)
	two := []byte(`{"two":2}`)
	d1, d2 := oci.DigestOfBytes(one), oci.DigestOfBytes(two)
	if _, err := b.Put(context.Background(), spoolOf(one)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := b.TakeRecent(d1); ok {
		t.Fatal("expired entry must not be returned")
	}
	// Recording purges expired rows so the table does not grow without bound.
	if _, err := b.Put(context.Background(), spoolOf(one)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := b.Put(context.Background(), spoolOf(two)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	b.recentMu.Lock()
	n := len(b.recent)
	_, hasTwo := b.recent[d2]
	b.recentMu.Unlock()
	if n != 1 || !hasTwo {
		t.Fatalf("recent table has %d rows (two present: %v); want only the latest", n, hasTwo)
	}
}

func TestPutEmptyBlob(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	m, err := b.Put(context.Background(), spoolOf(nil))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.Kind != KindRaw || m.RawReason != ReasonNotTar || m.Size != 0 || m.Digest != oci.DigestOfBytes(nil) {
		t.Fatalf("meta = %+v", m)
	}
	bl, err := b.Open(m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := pullAll(t, bl); len(got) != 0 {
		t.Fatalf("empty blob pulled %d bytes", len(got))
	}
	var buf bytes.Buffer
	if err := bl.WriteRange(context.Background(), &buf, 0, 0); err == nil {
		t.Fatal("no range is satisfiable on an empty blob")
	}
}

func TestPutLargeRawBlobRanges(t *testing.T) {
	b, _, _ := newTestStore(t, Options{MaxInMemory: 1 << 20})
	data := randomBytes(t, 3<<20)
	size := int64(len(data))
	d := oci.DigestOfBytes(data)

	// A file-backed spool from an upload session, as the registry produces
	// for blobs above --max-in-memory.
	upDir := t.TempDir()
	up, err := upload.NewManager(upDir, 1<<20, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })
	sess, err := up.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	sp, err := sess.Spool()
	if err != nil {
		t.Fatal(err)
	}
	if sp.Digest() != d || sp.Size() != size {
		t.Fatalf("spool = %s/%d", sp.Digest(), sp.Size())
	}

	m, err := b.Put(context.Background(), sp)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.Kind != KindRaw || m.RawReason != ReasonNotTar || m.Size != size {
		t.Fatalf("meta = %+v", m)
	}
	files, err := os.ReadDir(upDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !f.IsDir() {
			t.Fatalf("spool file %s left after Put", filepath.Join(upDir, f.Name()))
		}
	}
	assertSpoolDirEmpty(t, b)

	bl, err := b.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := pullAll(t, bl); !bytes.Equal(got, data) {
		t.Fatal("full pull differs")
	}
	ranges := [][2]int64{
		{0, 9},
		{5, size - 1},
		{size - 1, size - 1},
		{600000, 1600000}, // crosses at least one chunk boundary (chunks are at most 1 MiB)
		{1<<20 - 1, 1 << 20},
		{0, size - 1},
	}
	for _, r := range ranges {
		got := pullRange(t, bl, r[0], r[1])
		if !bytes.Equal(got, data[r[0]:r[1]+1]) {
			t.Errorf("range %d-%d: got %d bytes, mismatch", r[0], r[1], len(got))
		}
	}
	var buf bytes.Buffer
	if err := bl.WriteRange(context.Background(), &buf, 0, size); err == nil {
		t.Fatal("end == size must be rejected")
	}
}

func TestDeleteBlob(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	data := textBytes(20000, 9)
	d := oci.DigestOfBytes(data)
	if _, err := b.Put(context.Background(), spoolOf(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(d); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, err := b.Exists(d); err != nil || ok {
		t.Fatalf("Exists after Delete = %v, %v", ok, err)
	}
	if _, err := b.Open(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete = %v, want ErrNotFound", err)
	}
	if err := b.Delete(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
	// Re-pushing after a delete ingests again; the objects are still in
	// the pack, so everything deduplicates at the object level.
	m, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put after Delete: %v", err)
	}
	if m.Stats.NewLogicalBytes != 0 || m.Stats.DedupedBytes != m.Stats.LogicalBytes || m.Stats.DiskBytes != 0 {
		t.Fatalf("re-put stats = %+v", m.Stats)
	}
	if ok, _ := b.Exists(d); !ok {
		t.Fatal("blob missing after re-put")
	}
}

func putRawGzipCase(t *testing.T, name string, data []byte, reason RawReason) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		b, _, logs := newTestStore(t, Options{AllowRaw: true})
		d := oci.DigestOfBytes(data)
		m, err := b.Put(context.Background(), spoolOf(data))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if m.Kind != KindRaw || m.Format != "gzip" || m.RawReason != reason || m.Engine != "" || m.DiffID != "" || m.Entries != 0 {
			t.Fatalf("meta = %+v, want raw gzip %s", m, reason)
		}
		bl, err := b.Open(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := pullAll(t, bl); !bytes.Equal(got, data) {
			t.Fatal("round trip differs")
		}
		assertSpoolDirEmpty(t, b)
		if line := logs.String(); !strings.Contains(line, "raw_reason="+string(reason)) {
			t.Errorf("log lacks raw_reason=%s:\n%s", reason, line)
		}
	})
}

func TestPutGzipFallbacks(t *testing.T) {
	// The decompressed stream must start with a valid tar header, or the
	// blob/analyze.go tar probe now classifies it not-tar before these
	// fixtures ever reach the Analyze failure they are meant to exercise
	// (see TestPutTruncatedTarStoresRawDecomposeFailed for the same fix
	// applied to the decompose-failed case).
	tarData := tarBytes(t, "etc/motd", textBytes(4096, 11))
	tp1, tp2 := tarData[:len(tarData)/2], tarData[len(tarData)/2:]
	putRawGzipCase(t, "non-reproducible", twoLevelGzip(t, tp1, tp2), ReasonNotReproducible)
	putRawGzipCase(t, "corrupt trailer", corruptGzipCRC(gzipBytes(t, tarData, gzip.DefaultCompression)), ReasonCorrupt)
	putRawGzipCase(t, "multi-member", slices.Concat(gzipBytes(t, tp1, gzip.BestSpeed), gzipBytes(t, tp2, gzip.BestSpeed)), ReasonUnsupported)
}

func TestPutAnalyzeTimeoutStoresRaw(t *testing.T) {
	b, _, _ := newTestStore(t, Options{AnalyzeTimeout: time.Nanosecond, AllowRaw: true})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(3000, 5)), gzip.DefaultCompression)
	m, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.Kind != KindRaw || m.RawReason != ReasonAnalyzeTimeout || m.Format != "gzip" {
		t.Fatalf("meta = %+v", m)
	}
	bl, err := b.Open(m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := pullAll(t, bl); !bytes.Equal(got, data) {
		t.Fatal("round trip differs")
	}
}

func TestPutCancelledContext(t *testing.T) {
	b, _, _ := newTestStore(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, data := range map[string][]byte{
		"json": []byte(`{"architecture":"amd64"}`),
		"gzip": gzipBytes(t, tarBytes(t, "a", textBytes(100, 1)), gzip.BestSpeed),
	} {
		sp := spoolOf(data)
		_, err := b.Put(ctx, sp)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: Put = %v, want context.Canceled", name, err)
		}
		if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
			t.Errorf("%s: blob published despite cancelled context", name)
		}
		// The spool stays so the registry can keep the session for a retry.
		r, err := sp.Open()
		if err != nil {
			t.Errorf("%s: spool discarded after a failed Put: %v", name, err)
		} else if c, ok := r.(io.Closer); ok {
			c.Close()
		}
	}
}

func TestPutConcurrentSameDigest(t *testing.T) {
	b, _, _ := newTestStore(t, Options{MaxConcurrentFinalize: 4})
	data := textBytes(40000, 21)
	size := int64(len(data))
	const n = 4
	metas := make([]*Meta, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			metas[i], errs[i] = b.Put(context.Background(), spoolOf(data))
		}()
	}
	wg.Wait()
	dedup := store.Stats{LogicalBytes: size, DedupedBytes: size}
	hits, ingests := 0, 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Put %d: %v", i, errs[i])
		}
		switch {
		case metas[i].Stats == dedup:
			hits++
		case metas[i].Stats.NewLogicalBytes > 0:
			ingests++
		default:
			t.Fatalf("Put %d: unexpected stats %+v", i, metas[i].Stats)
		}
	}
	if ingests != 1 || hits != n-1 {
		t.Fatalf("ingests = %d, dedup hits = %d; want 1 and %d", ingests, hits, n-1)
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); !ok {
		t.Fatal("blob missing")
	}
}
