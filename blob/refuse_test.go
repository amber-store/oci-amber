package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	zrecipe "github.com/draganm/zrecipe"
	"github.com/jobs-build/amber-store-core/fstree"

	"github.com/amber-store/oci-amber/oci"
)

func TestRawRefusedErrorMessage(t *testing.T) {
	d := oci.DigestOfBytes([]byte("layer"))
	detail := errors.New("zrecipe: not reproducible")
	with := &RawRefusedError{Digest: d, Format: "gzip", Reason: ReasonNotReproducible, Err: detail}
	want := "blob: " + d.String() + " cannot be stored as a prism (not-reproducible: zrecipe: not reproducible); raw layers are refused, start with --allow-raw to store them"
	if got := with.Error(); got != want {
		t.Errorf("Error() =\n%s\nwant\n%s", got, want)
	}
	if !errors.Is(with, detail) {
		t.Error("the refusal does not unwrap to its detail")
	}
	without := &RawRefusedError{Digest: d, Format: "gzip", Reason: ReasonDecomposeFailed}
	want = "blob: " + d.String() + " cannot be stored as a prism (decompose-failed); raw layers are refused, start with --allow-raw to store them"
	if got := without.Error(); got != want {
		t.Errorf("Error() without detail =\n%s\nwant\n%s", got, want)
	}
	if without.Unwrap() != nil {
		t.Error("Unwrap of a refusal without detail is not nil")
	}
}

// putRefused runs Put on data against b, which must refuse it, and checks
// what every refusal has in common: the error is a *RawRefusedError naming
// the blob, reason and format, its message tells the operator about
// --allow-raw, nothing is published or recorded as recent, the spool stays
// in place as after any other failed Put, the spool directory is clean and
// the "layer refused" line is logged at error level with the reason, with
// no "blob stored" line next to it. It returns the error for
// reason-specific checks.
func putRefused(t *testing.T, b *Store, logs *logBuffer, data []byte, reason RawReason, format string) *RawRefusedError {
	t.Helper()
	d := oci.DigestOfBytes(data)
	sp := spoolOf(data)
	_, err := b.Put(context.Background(), sp)
	var refused *RawRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Put = %v, want a *RawRefusedError", err)
	}
	if refused.Digest != d || refused.Reason != reason || refused.Format != format {
		t.Fatalf("refused = {%s %s %s}, want {%s %s %s}", refused.Digest, refused.Reason, refused.Format, d, reason, format)
	}
	for _, want := range []string{d.String(), string(reason), "cannot be stored as a prism", "--allow-raw"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, err := b.Open(d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after a refusal = %v, want ErrNotFound", err)
	}
	if ok, err := b.Exists(d); err != nil || ok {
		t.Fatalf("Exists after a refusal = %v, %v", ok, err)
	}
	if _, ok := b.TakeRecent(d); ok {
		t.Error("a refusal recorded recent stats")
	}
	if r, err := sp.Open(); err != nil {
		t.Errorf("spool discarded after a refusal: %v", err)
	} else if c, ok := r.(io.Closer); ok {
		c.Close()
	}
	assertSpoolDirEmpty(t, b)
	out := logs.String()
	for _, want := range []string{"level=ERROR", `msg="layer refused"`, "digest=" + d.String(), "format=" + format, "reason=" + string(reason)} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "blob stored") {
		t.Errorf("a refused blob was logged as stored:\n%s", out)
	}
	return refused
}

// TestPutRefusesNonReproducibleByDefault: without AllowRaw a gzip zrecipe
// cannot reproduce is refused rather than stored raw, and the speculative
// staging leaves nothing behind either, exactly as in the allowed case.
func TestPutRefusesNonReproducibleByDefault(t *testing.T) {
	b, st, logs := newTestStore(t, Options{})
	content := textBytes(4096, 21)
	tarData := tarBytes(t, "etc/motd", content)
	data := twoLevelGzip(t, tarData[:len(tarData)/2], tarData[len(tarData)/2:])

	refused := putRefused(t, b, logs, data, ReasonNotReproducible, "gzip")
	if !errors.Is(refused, zrecipe.ErrNotReproducible) {
		t.Errorf("refusal does not carry zrecipe's error: %v", refused.Err)
	}
	// The 4 KiB file is one chunk, so its content key is EncodeBlob's.
	obj, err := fstree.EncodeBlob(content)
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := st.Has(obj.Key); has {
		t.Fatal("the staged file content reached the store although the blob was refused")
	}
}

// TestPutRefusesDecomposeFailedByDefault: a reproducible gzip of a tar that
// breaks part way through passes the probe and Analyze and fails in
// tar-prism; without AllowRaw that downgrade becomes a refusal, and the
// "storing raw" line of the allowed case is not logged.
func TestPutRefusesDecomposeFailedByDefault(t *testing.T) {
	full := tarBytes(t, "usr/lib/app", textBytes(8<<10, 3))
	truncated := full[:tarHeaderSize+1024]
	if !isTarHeader(truncated[:tarHeaderSize]) {
		t.Fatal("the fixture must start with a valid tar header, or it would be classified not-tar")
	}
	data := gzipBytes(t, truncated, gzip.DefaultCompression)

	b, _, logs := newTestStore(t, Options{})
	refused := putRefused(t, b, logs, data, ReasonDecomposeFailed, "gzip")
	if refused.Err == nil {
		t.Error("refusal lacks tar-prism's error")
	}
	if out := logs.String(); strings.Contains(out, "storing raw") {
		t.Errorf("a refusal was logged as a fallback:\n%s", out)
	}
}

// TestPutRefusesRoundTripFailureByDefault: the prism was committed before
// the round-trip check failed; without AllowRaw its objects are left to GC
// and the upload is refused instead of being downgraded.
func TestPutRefusesRoundTripFailureByDefault(t *testing.T) {
	orig := roundTripCheck
	roundTripCheck = func(context.Context, *Store, *Prism, *zrecipe.Params, oci.Digest) error {
		return errors.New("forced round-trip failure")
	}
	t.Cleanup(func() { roundTripCheck = orig })

	b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true})
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	refused := putRefused(t, b, logs, data, ReasonRoundTripFailed, "gzip")
	if refused.Err == nil || !strings.Contains(refused.Err.Error(), "forced round-trip failure") {
		t.Errorf("refusal lacks the round-trip error: %v", refused.Err)
	}
	if out := logs.String(); strings.Contains(out, "storing raw") {
		t.Errorf("a refusal was logged as a fallback:\n%s", out)
	}
}

func TestPutRefusesAnalyzeTimeoutByDefault(t *testing.T) {
	b, _, logs := newTestStore(t, Options{AnalyzeTimeout: time.Nanosecond})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(3000, 5)), gzip.DefaultCompression)
	refused := putRefused(t, b, logs, data, ReasonAnalyzeTimeout, "gzip")
	if !errors.Is(refused, context.DeadlineExceeded) {
		t.Errorf("refusal does not carry the deadline error: %v", refused.Err)
	}
}

// TestPutConfigBlobRawWithoutAllowRaw: not-tar is the one reason AllowRaw
// does not govern. A config, plain or gzipped, can never be a prism, so it
// is stored raw by a store that refuses raw layers, quietly.
func TestPutConfigBlobRawWithoutAllowRaw(t *testing.T) {
	payload := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	for _, c := range []struct {
		name   string
		data   []byte
		format string
	}{
		{"json", payload, "none"},
		{"gzipped json", gzipBytes(t, bytes.Repeat(payload, 64), gzip.DefaultCompression), "gzip"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _, logs := newTestStore(t, Options{})
			m, err := b.Put(context.Background(), spoolOf(c.data))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if m.Kind != KindRaw || m.RawReason != ReasonNotTar || m.Format != c.format {
				t.Fatalf("meta = %+v, want raw/not-tar/%s", *m, c.format)
			}
			bl, err := b.Open(m.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if got := pullAll(t, bl); !bytes.Equal(got, c.data) {
				t.Fatal("pulled bytes differ")
			}
			out := logs.String()
			if strings.Contains(out, "layer refused") || strings.Contains(out, "level=ERROR") {
				t.Errorf("a not-tar blob was refused or logged at error level:\n%s", out)
			}
			if !strings.Contains(out, "raw_reason=not-tar") {
				t.Errorf("log lacks the raw reason:\n%s", out)
			}
		})
	}
}
