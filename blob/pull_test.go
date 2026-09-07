package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amber-store/core/key"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

func rawMeta(data []byte) Meta {
	return Meta{Version: MetaVersion, Digest: oci.DigestOfBytes(data), Size: int64(len(data)), Kind: KindRaw, Format: "none", RawReason: ReasonNotTar, UploadedAt: time.Now().UTC()}
}

// pullAll streams bl through WriteTo and returns the bytes.
func pullAll(t *testing.T, bl *Blob) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bl.WriteTo(context.Background(), &buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return buf.Bytes()
}

// pullRange streams start..end of bl through WriteRange.
func pullRange(t *testing.T, bl *Blob, start, end int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bl.WriteRange(context.Background(), &buf, start, end); err != nil {
		t.Fatalf("WriteRange(%d, %d): %v", start, end, err)
	}
	return buf.Bytes()
}

// putRawRoot stores data as a raw blob root (ingestRaw + buildRoot) and
// publishes it under meta.Digest, bypassing Put so tests can plant a Meta
// of their choosing.
func putRawRoot(t *testing.T, b *Store, st *store.Store, data []byte, meta Meta) key.Key {
	t.Helper()
	ctx := context.Background()
	w := st.NewWriter(ctx)
	rawKey, err := b.ingestRaw(ctx, w, spoolOf(data))
	if err != nil {
		t.Fatalf("ingestRaw: %v", err)
	}
	meta.Stats, err = w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	rw := st.NewWriter(ctx)
	root, err := b.buildRoot(rw, meta, map[string]key.Key{RawFile: rawKey}, key.Key{})
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	if _, err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Publish(RefName(meta.Digest), root); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return root
}

func TestWriteToRaw(t *testing.T) {
	b, st, _ := newTestStore(t, Options{})
	data := textBytes(70000, 7)
	putRawRoot(t, b, st, data, rawMeta(data))
	bl, err := b.Open(oci.DigestOfBytes(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := pullAll(t, bl); !bytes.Equal(got, data) {
		t.Fatalf("WriteTo returned %d bytes, want %d identical bytes", len(got), len(data))
	}
	if got := pullRange(t, bl, 0, int64(len(data))-1); !bytes.Equal(got, data) {
		t.Fatal("full range differs from data")
	}
}

func TestWriteToDetectsDigestMismatch(t *testing.T) {
	b, st, _ := newTestStore(t, Options{})
	data := []byte("the bytes")
	meta := rawMeta([]byte("other bytes")) // digest of something else
	meta.Size = int64(len(data))
	putRawRoot(t, b, st, data, meta)
	bl, err := b.Open(meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = bl.WriteTo(context.Background(), &buf)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("WriteTo = %v, want ErrDigestMismatch", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("bytes must still have been streamed before the mismatch is reported")
	}
}

func TestWriteRangeValidation(t *testing.T) {
	b, st, _ := newTestStore(t, Options{})
	data := []byte("0123456789")
	putRawRoot(t, b, st, data, rawMeta(data))
	bl, err := b.Open(oci.DigestOfBytes(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]int64{{-1, 0}, {5, 4}, {0, 10}, {10, 10}, {3, 11}} {
		var buf bytes.Buffer
		if err := bl.WriteRange(context.Background(), &buf, r[0], r[1]); err == nil {
			t.Errorf("WriteRange(%d, %d) succeeded, want an error", r[0], r[1])
		}
		if buf.Len() != 0 {
			t.Errorf("WriteRange(%d, %d) wrote %d bytes before failing", r[0], r[1], buf.Len())
		}
	}
	if got := pullRange(t, bl, 3, 5); string(got) != "345" {
		t.Fatalf("WriteRange(3,5) = %q", got)
	}
	if got := pullRange(t, bl, 9, 9); string(got) != "9" {
		t.Fatalf("WriteRange(9,9) = %q", got)
	}
	if got := pullRange(t, bl, 0, 0); string(got) != "0" {
		t.Fatalf("WriteRange(0,0) = %q", got)
	}
}

func TestPrismRangeUnsupported(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(3000, 5)), gzip.DefaultCompression)
	m, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if m.Kind != KindPrism {
		t.Fatalf("kind = %q (reason %q), want prism", m.Kind, m.RawReason)
	}
	bl, err := b.Open(m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if bl.SupportsRange() {
		t.Fatal("prism blobs must not advertise ranges")
	}
	var buf bytes.Buffer
	if err := bl.WriteRange(context.Background(), &buf, 0, 1); err == nil {
		t.Fatal("WriteRange on a prism must fail")
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteRange wrote %d bytes before refusing", buf.Len())
	}
	if got := pullAll(t, bl); !bytes.Equal(got, data) {
		t.Fatal("WriteTo on prism differs from the pushed bytes")
	}
}

func TestWriteToObservesContext(t *testing.T) {
	b, st, _ := newTestStore(t, Options{})
	data := randomBytes(t, 3<<20)
	putRawRoot(t, b, st, data, rawMeta(data))
	bl, err := b.Open(oci.DigestOfBytes(data))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := bl.WriteTo(ctx, &buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes written after cancellation", buf.Len())
	}
	if err := bl.WriteRange(ctx, &buf, 0, 9); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteRange = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes written by WriteRange after cancellation", buf.Len())
	}
}
