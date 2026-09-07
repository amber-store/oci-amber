package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/core/fstree"
	tarprism "github.com/draganm/tar-prism"
	zrecipe "github.com/draganm/zrecipe"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// countFiles counts the regular files under dir and logs each one.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
			t.Logf("file under work dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return n
}

func TestPutDedupsSharedFiles(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	shared := make([]prismFile, 9)
	for i := range shared {
		shared[i] = prismFile{name: fmt.Sprintf("app/lib%d.so", i), data: randomBytes(t, 512<<10)}
	}
	first := append(append([]prismFile{}, shared...), prismFile{name: "app/version-1.txt", data: randomBytes(t, 32<<10)})
	second := append(append([]prismFile{}, shared...), prismFile{name: "app/version-2.txt", data: randomBytes(t, 32<<10)})
	dataA := gzipBytes(t, prismTar(t, first), gzip.DefaultCompression)
	dataB := gzipBytes(t, prismTar(t, second), gzip.DefaultCompression)

	metaA := putPrism(t, b, dataA)
	if metaA.Kind != KindPrism {
		t.Fatalf("first layer: kind = %q (reason %q), want prism", metaA.Kind, metaA.RawReason)
	}
	if metaA.Stats.DedupedBytes > metaA.Stats.LogicalBytes/10 {
		t.Errorf("first layer into an empty store deduped %d of %d bytes", metaA.Stats.DedupedBytes, metaA.Stats.LogicalBytes)
	}

	metaB := putPrism(t, b, dataB)
	if metaB.Kind != KindPrism {
		t.Fatalf("second layer: kind = %q (reason %q), want prism", metaB.Kind, metaB.RawReason)
	}
	t.Logf("second layer: %+v (size %d)", metaB.Stats, metaB.Size)
	if float64(metaB.Stats.DedupedBytes) <= 0.9*float64(metaB.Stats.LogicalBytes) {
		t.Errorf("dedupedBytes = %d, want > 90%% of logicalBytes %d", metaB.Stats.DedupedBytes, metaB.Stats.LogicalBytes)
	}
	if float64(metaB.Stats.DiskBytes) >= 0.2*float64(metaB.Size) {
		t.Errorf("diskBytes = %d, want < 20%% of size %d", metaB.Stats.DiskBytes, metaB.Size)
	}
	if metaB.Stats.LogicalBytes != metaB.Stats.NewLogicalBytes+metaB.Stats.DedupedBytes {
		t.Errorf("stats do not add up: %+v", metaB.Stats)
	}
	if metaB.Stats.ObjectsDeduped == 0 || metaB.Stats.ObjectsNew == 0 {
		t.Errorf("objects new/deduped = %d/%d, want both > 0", metaB.Stats.ObjectsNew, metaB.Stats.ObjectsDeduped)
	}
	for _, c := range []struct {
		d    oci.Digest
		want []byte
	}{{metaA.Digest, dataA}, {metaB.Digest, dataB}} {
		got, _ := pullPrism(t, b, c.d)
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s: pulled bytes differ", c.d)
		}
	}

	again := putPrism(t, b, dataA)
	wantSkipped := store.Stats{LogicalBytes: int64(len(dataA)), DedupedBytes: int64(len(dataA))}
	if again.Kind != KindPrism || again.Digest != metaA.Digest || again.Stats != wantSkipped {
		t.Errorf("re-push returned %+v, want kind prism with stats %+v", *again, wantSkipped)
	}
	if stats, ok := b.TakeRecent(metaA.Digest); !ok || stats != wantSkipped {
		t.Errorf("TakeRecent after re-push = %+v, %v; want %+v, true", stats, ok, wantSkipped)
	}
	if _, ok := b.TakeRecent(metaA.Digest); ok {
		t.Error("TakeRecent did not consume the entry")
	}
	if stats, ok := b.TakeRecent(metaB.Digest); !ok || stats != metaB.Stats {
		t.Errorf("TakeRecent(second) = %+v, %v; want %+v, true", stats, ok, metaB.Stats)
	}
}

func TestPutLargeLayerSpillsAndCleansUp(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	b, _, _ := newTestStore(t, Options{WorkDir: work, MaxInMemory: 1 << 20, VerifyRoundTrip: true})
	files := []prismFile{
		{name: "big/one.bin", data: randomBytes(t, 4<<20)},
		{name: "big/two.txt", data: textBytes(1<<20, 7)},
	}
	tarData := prismTar(t, files)
	data := gzipBytes(t, tarData, gzip.DefaultCompression)
	if len(data) <= 1<<20 {
		t.Fatalf("fixture is %d bytes, too small to spill", len(data))
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := upload.NewManager(filepath.Join(work, "uploads"), 1<<20, time.Hour, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	sess, err := m.Create()
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
	if sp.Size() != int64(len(data)) || sp.Digest() != oci.DigestOfBytes(data) {
		t.Fatalf("spool = %d/%s, want %d/%s", sp.Size(), sp.Digest(), len(data), oci.DigestOfBytes(data))
	}
	if n := countFiles(t, work); n == 0 {
		t.Fatal("upload above the memory threshold did not spill to a file")
	}

	meta, err := b.Put(context.Background(), sp)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Kind != KindPrism || meta.Entries != len(files) || meta.DiffID != oci.DigestOfBytes(tarData) {
		t.Fatalf("meta = %+v, want a prism with %d entries", *meta, len(files))
	}
	if n := countFiles(t, work); n != 0 {
		t.Errorf("%d files left under the work directory after Put", n)
	}
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
}

func TestPutRoundTripFailureStoresRaw(t *testing.T) {
	orig := roundTripCheck
	roundTripCheck = func(ctx context.Context, b *Store, src *Prism, params *zrecipe.Params, want oci.Digest) error {
		return errors.New("forced round-trip failure")
	}
	t.Cleanup(func() { roundTripCheck = orig })

	b, st, logs := newTestStore(t, Options{VerifyRoundTrip: true, AllowRaw: true})
	data := gzipBytes(t, prismTar(t, prismFixtureFiles(t)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonRoundTripFailed {
		t.Fatalf("kind/reason = %q/%q, want raw/%s", meta.Kind, meta.RawReason, ReasonRoundTripFailed)
	}
	if meta.Format != "gzip" {
		t.Errorf("format = %q, want gzip (the detected format is kept for raw blobs)", meta.Format)
	}
	if meta.DiffID != "" || meta.UncompressedSize != 0 || meta.Entries != 0 || meta.Engine != "" || meta.EngineVersion != "" {
		t.Errorf("raw blob carries prism-only fields: %+v", *meta)
	}
	if meta.Stats.LogicalBytes < int64(len(data)) {
		t.Errorf("logicalBytes = %d, want at least the blob size %d", meta.Stats.LogicalBytes, len(data))
	}
	assertSpoolDirEmpty(t, b)

	got, bl := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	if !bl.SupportsRange() {
		t.Error("raw fallback does not support ranges")
	}
	if part := pullRange(t, bl, 10, 19); !bytes.Equal(part, data[10:20]) {
		t.Errorf("WriteRange(10, 19) = %x, want %x", part, data[10:20])
	}
	if _, err := st.Lookup(bl.Root(), RawFile); err != nil {
		t.Errorf("raw fallback root lacks %s: %v", RawFile, err)
	}
	for _, name := range []string{tarprism.RecipeFile, tarprism.IndexFile, tarprism.BlobsDir, CompFile} {
		if _, err := st.Lookup(bl.Root(), name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("raw fallback root has %s (err %v)", name, err)
		}
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "round-trip verification failed") || !strings.Contains(out, "raw_reason=roundtrip-failed") {
		t.Errorf("log output lacks the error-level round-trip line or the raw reason:\n%s", out)
	}
}

func TestRoundTripCheckObeysVerifyOption(t *testing.T) {
	calls := 0
	orig := roundTripCheck
	roundTripCheck = func(ctx context.Context, b *Store, src *Prism, params *zrecipe.Params, want oci.Digest) error {
		calls++
		return orig(ctx, b, src, params, want)
	}
	t.Cleanup(func() { roundTripCheck = orig })
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)

	on, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	if meta := putPrism(t, on, data); meta.Kind != KindPrism {
		t.Fatalf("verify on: kind = %q (reason %q)", meta.Kind, meta.RawReason)
	}
	if calls != 1 {
		t.Fatalf("round-trip check ran %d times with VerifyRoundTrip on, want 1", calls)
	}

	off, _, _ := newTestStore(t, Options{VerifyRoundTrip: false})
	if meta := putPrism(t, off, data); meta.Kind != KindPrism {
		t.Fatalf("verify off: kind = %q (reason %q)", meta.Kind, meta.RawReason)
	}
	if calls != 1 {
		t.Fatalf("round-trip check ran %d times in total, want still 1 with VerifyRoundTrip off", calls)
	}
	got, _ := pullPrism(t, off, oci.DigestOfBytes(data))
	if !bytes.Equal(got, data) {
		t.Fatal("pulled bytes differ with VerifyRoundTrip off")
	}
}

// TestRoundTripCheckUsesStoredCompParams covers I5: finalizePrism must feed
// the round-trip check the params it reads back from the stored comp.json,
// not the *Params pass one built in memory, so the check exercises exactly
// the bytes a real pull will use. Injecting a divergence between the two is
// impractical (finalizePrism only ever writes what pass one produced), so
// this instead captures what the hook receives and compares it against an
// independent ReadParams decode of the comp.json that Put actually left in
// the store.
func TestRoundTripCheckUsesStoredCompParams(t *testing.T) {
	var got *zrecipe.Params
	orig := roundTripCheck
	roundTripCheck = func(ctx context.Context, b *Store, src *Prism, params *zrecipe.Params, want oci.Digest) error {
		got = params
		return orig(ctx, b, src, params, want)
	}
	t.Cleanup(func() { roundTripCheck = orig })

	b, st, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want prism", meta.Kind, meta.RawReason)
	}
	if got == nil {
		t.Fatal("roundTripCheck was never invoked")
	}

	root, err := st.Resolve(RefName(meta.Digest))
	if err != nil {
		t.Fatal(err)
	}
	want, err := b.readParams(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roundTripCheck received %+v, want the stored comp.json decoded: %+v", got, want)
	}
}

func TestPutContextCancelledDuringPrismFails(t *testing.T) {
	// The round-trip hook stands in for the request going away in the
	// round-trip check after the commit: the upload must fail with the
	// context's error, nothing may be published and the spool must stay
	// usable for a retry.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := roundTripCheck
	roundTripCheck = func(context.Context, *Store, *Prism, *zrecipe.Params, oci.Digest) error {
		cancel()
		return errors.New("client went away")
	}
	t.Cleanup(func() { roundTripCheck = orig })

	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	sp := spoolOf(data)
	_, err := b.Put(ctx, sp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
		t.Fatal("blob published despite the cancelled context")
	}
	if _, err := sp.Open(); err != nil {
		t.Fatalf("spool removed after a failed Put: %v", err)
	}
	if _, ok := b.TakeRecent(oci.DigestOfBytes(data)); ok {
		t.Fatal("failed Put recorded recent stats")
	}
}

func TestPutPrismAnalyzeTimeoutStoresRaw(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, AnalyzeTimeout: time.Nanosecond, AllowRaw: true})
	data := gzipBytes(t, prismTar(t, prismFixtureFiles(t)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonAnalyzeTimeout || meta.Format != "gzip" {
		t.Fatalf("kind/reason/format = %q/%q/%q, want raw/%s/gzip", meta.Kind, meta.RawReason, meta.Format, ReasonAnalyzeTimeout)
	}
	got, bl := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	if !bl.SupportsRange() {
		t.Error("raw blob does not support ranges")
	}
}

// TestPutCompressedNonTarStoresRawNotTar covers the tar probe that runs
// before Analyze: a compressed blob that is not a tar (an oras-pushed SBOM,
// a gzipped config, a model shard) can never become a prism, so it is
// classified raw with reason not-tar straight away instead of paying the
// full candidate search, a doomed staging and an error-level log line.
func TestPutCompressedNonTarStoresRawNotTar(t *testing.T) {
	payload := bytes.Repeat([]byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"]}}`+"\n"), 64)
	for _, c := range []struct {
		name   string
		data   []byte
		format string
	}{
		{"gzip", gzipBytes(t, payload, gzip.DefaultCompression), "gzip"},
		{"zstd", prismZstd(t, payload), "zstd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true})
			meta := putPrism(t, b, c.data)
			if meta.Kind != KindRaw || meta.RawReason != ReasonNotTar {
				t.Fatalf("kind/reason = %q/%q, want raw/%s", meta.Kind, meta.RawReason, ReasonNotTar)
			}
			if meta.Format != c.format {
				t.Errorf("format = %q, want %s", meta.Format, c.format)
			}
			if meta.DiffID != "" || meta.Entries != 0 || meta.Engine != "" || meta.EngineVersion != "" {
				t.Errorf("raw blob carries prism-only fields: %+v", *meta)
			}
			got, _ := pullPrism(t, b, meta.Digest)
			if !bytes.Equal(got, c.data) {
				t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(c.data))
			}
			assertSpoolDirEmpty(t, b)
			out := logs.String()
			if strings.Contains(out, "level=ERROR") {
				t.Errorf("a non-tar artifact was logged at error level:\n%s", out)
			}
			if strings.Contains(out, "decompose failed") {
				t.Errorf("staging ran on a non-tar artifact:\n%s", out)
			}
			if !strings.Contains(out, "raw_reason=not-tar") {
				t.Errorf("log output lacks the raw reason:\n%s", out)
			}

			// The probe runs before Analyze: with a one-nanosecond analyze
			// deadline the blob would otherwise come out analyze-timeout.
			quick, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, AnalyzeTimeout: time.Nanosecond})
			if m := putPrism(t, quick, c.data); m.Kind != KindRaw || m.RawReason != ReasonNotTar {
				t.Fatalf("with a 1 ns analyze deadline: kind/reason = %q/%q, want raw/%s (Analyze must not run)", m.Kind, m.RawReason, ReasonNotTar)
			}
		})
	}
}

// TestPutTruncatedTarStoresRawDecomposeFailed keeps the decompose-failure
// path covered now that a compressed non-tar never reaches it: this stream
// does start with a valid tar header, so it passes the probe and Analyze,
// and only tar-prism finds it broken part way through.
func TestPutTruncatedTarStoresRawDecomposeFailed(t *testing.T) {
	full := tarBytes(t, "usr/lib/app", textBytes(8<<10, 3))
	truncated := full[:tarHeaderSize+1024] // the header plus part of the content
	if !isTarHeader(truncated[:tarHeaderSize]) {
		t.Fatal("the fixture must start with a valid tar header, or it would be classified not-tar")
	}
	data := gzipBytes(t, truncated, gzip.DefaultCompression)

	b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true, AllowRaw: true})
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonDecomposeFailed {
		t.Fatalf("kind/reason = %q/%q, want raw/%s", meta.Kind, meta.RawReason, ReasonDecomposeFailed)
	}
	if meta.Format != "gzip" {
		t.Errorf("format = %q, want gzip", meta.Format)
	}
	if meta.DiffID != "" || meta.Entries != 0 || meta.Engine != "" {
		t.Errorf("raw blob carries prism-only fields: %+v", *meta)
	}
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes differ from the %d pushed", len(got), len(data))
	}
	assertSpoolDirEmpty(t, b)
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "decompose failed") || !strings.Contains(out, "raw_reason=decompose-failed") {
		t.Errorf("log output lacks the error-level decompose line or the raw reason:\n%s", out)
	}
}

// TestPutNonReproducibleLeavesNoStagedObjects: a gzip zrecipe cannot
// reproduce is staged speculatively while the search runs, then dropped.
// The raw blob's objects are the only ones that land; the load-bearing
// assertion is that the file content the tar carried is absent from the
// store. The spool-dir check says nothing about the pack, which is
// unlinked the moment it is created and could never appear there: it only
// shows zrecipe cleaned up after itself.
func TestPutNonReproducibleLeavesNoStagedObjects(t *testing.T) {
	b, st, _ := newTestStore(t, Options{VerifyRoundTrip: true, AllowRaw: true})
	content := textBytes(4096, 21)
	tarData := tarBytes(t, "etc/motd", content)
	data := twoLevelGzip(t, tarData[:len(tarData)/2], tarData[len(tarData)/2:])

	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotReproducible {
		t.Fatalf("kind/reason = %s/%s, want raw/not-reproducible", meta.Kind, meta.RawReason)
	}
	// The 4 KiB file is one chunk, so its content key is EncodeBlob's.
	obj, err := fstree.EncodeBlob(content)
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := st.Has(obj.Key); has {
		t.Fatal("the staged file content reached the store although the blob is raw")
	}
	assertSpoolDirEmpty(t, b)
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatal("pulled bytes differ")
	}
}

// cancelOnProgress is an Observer that cancels a context once the analyze
// stage reports having read more than after bytes of the spool, standing
// in for a client that goes away while pass one is inflating the blob.
type cancelOnProgress struct {
	cancel context.CancelFunc
	after  int64
	once   sync.Once
}

func (c *cancelOnProgress) BlobStage(oci.Digest, Stage) {}
func (c *cancelOnProgress) BlobProgress(_ oci.Digest, n int64) {
	if n > c.after {
		c.once.Do(c.cancel)
	}
}

// TestPutCancelledDuringAnalyzeLeavesNothing cancels the request while the
// speculative decompose is under way, not before it starts: zrecipe reads
// the spool through a 64 KiB bufio, so a cancellation that waits for the
// 64 KiB mark lands after Detect and the tar probe, with Analyze inside
// its inflate loop and the stager consuming the pipe. The fixture is
// incompressible, so its gzip is several times that threshold. The upload
// must fail with the context's error, publish nothing, leave the spool
// usable for a retry and leave no file under the work directory.
func TestPutCancelledDuringAnalyzeLeavesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := filepath.Join(t.TempDir(), "work")
	obs := &cancelOnProgress{cancel: cancel, after: 64 << 10}
	b, _, _ := newTestStore(t, Options{WorkDir: work, VerifyRoundTrip: true, Observer: obs})
	data := gzipBytes(t, prismTar(t, []prismFile{
		{name: "usr/lib/file-0.txt", data: textBytes(4096, 31)},
		{name: "usr/lib/file-1.txt", data: textBytes(4096, 32)},
		{name: "usr/lib/big.bin", data: randomBytes(t, 512<<10)},
	}), gzip.DefaultCompression)
	if int64(len(data)) < 4*obs.after {
		t.Fatalf("fixture is %d compressed bytes, too small to cancel mid-stream", len(data))
	}
	sp := spoolOf(data)

	_, err := b.Put(ctx, sp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
		t.Fatal("blob published despite the cancelled context")
	}
	if _, err := sp.Open(); err != nil {
		t.Fatalf("spool removed after a failed Put: %v", err)
	}
	if n := countFiles(t, work); n != 0 {
		t.Fatalf("%d files left under the work directory", n)
	}
}

func TestPutUnwritableSpoolDirFailsUpload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	dir := spoolDirOf(b)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	sp := spoolOf(data)

	_, err := b.Put(context.Background(), sp)
	if err == nil {
		t.Fatal("Put succeeded although the pack file could not be created")
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
		t.Fatal("blob published although the upload failed")
	}
	if _, err := sp.Open(); err != nil {
		t.Fatalf("spool removed after a failed Put: %v", err)
	}
}

func TestPutUncompressedNonTarNeverStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := []byte(`{"architecture":"arm64","os":"linux"}`) // a config blob
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotTar || meta.Format != "none" {
		t.Fatalf("meta = %+v, want raw/not-tar/none", *meta)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageRaw}, StageRaw)
	assertSpoolDirEmpty(t, b)
}
