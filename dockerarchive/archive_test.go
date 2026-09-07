package dockerarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/oci"
)

func openBuilder(t *testing.T, b *archivetest.Builder) *Archive {
	t.Helper()
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestOpenIndexesBlobsAndReadsTopFiles(t *testing.T) {
	b := archivetest.New()
	layer := []byte("layer bytes that are long enough to matter")
	img := b.AddImage([]byte(`{"os":"linux"}`), []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar", Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes([]byte(`{"os":"linux"}`)), RepoTags: []string{"app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	a := openBuilder(t, b)

	if len(a.Index.Manifests) != 1 || a.Index.Manifests[0].Digest != img.Digest {
		t.Fatalf("index = %+v", a.Index)
	}
	if len(a.Legacy) != 1 || a.Legacy[0].RepoTags[0] != "app:v1" || a.Legacy[0].Config != "blobs/sha256/"+oci.DigestOfBytes([]byte(`{"os":"linux"}`)).Hex() {
		t.Fatalf("legacy = %+v", a.Legacy)
	}
	if a.LayoutVersion != "1.0.0" {
		t.Fatalf("LayoutVersion = %q, want 1.0.0", a.LayoutVersion)
	}
	ld := oci.DigestOfBytes(layer)
	if !a.Has(ld) {
		t.Fatal("layer not indexed")
	}
	if n, ok := a.Size(ld); !ok || n != int64(len(layer)) {
		t.Fatalf("Size = %d,%v", n, ok)
	}
	sec, err := a.Section(ld)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(sec)
	if string(got) != string(layer) {
		t.Fatalf("section read %q", got)
	}
	body, err := a.ReadBlob(img.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if oci.DigestOfBytes(body) != img.Digest {
		t.Fatal("ReadBlob returned other bytes")
	}
	if a.Has(oci.DigestOfBytes([]byte("nope"))) {
		t.Fatal("Has reports an absent blob")
	}
	if _, err := a.Section(oci.DigestOfBytes([]byte("nope"))); !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("Section of an absent blob: %v", err)
	}
}

func TestOpenRejectsMissingIndex(t *testing.T) {
	b := archivetest.New()
	b.AddBlob([]byte("x"))
	b.NoIndex()
	path, _ := b.WriteFile(t.TempDir(), "legacy.tar")
	_, err := Open(path)
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
	if !strings.Contains(err.Error(), "25") {
		t.Fatalf("message should point at Docker 25+: %v", err)
	}
}

func TestReadBlobVerifiesDigest(t *testing.T) {
	b := archivetest.New()
	wrong := oci.DigestOfBytes([]byte("what the name claims"))
	b.AddBlobAs(wrong, []byte("what is actually there"))
	b.Top()
	a := openBuilder(t, b)
	if _, err := a.ReadBlob(wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ReadBlob on a corrupt blob: %v", err)
	}
	var seen []int64
	err := a.Verify(context.Background(), wrong, func(n int64) { seen = append(seen, n) })
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Verify on a corrupt blob: %v", err)
	}
	if len(seen) == 0 || seen[len(seen)-1] != int64(len("what is actually there")) {
		t.Fatalf("Verify progress = %v", seen)
	}
}

func TestVerifyAcceptsGoodBlobAndObeysContext(t *testing.T) {
	b := archivetest.New()
	d := b.AddBlob([]byte("good bytes"))
	b.Top()
	a := openBuilder(t, b)
	if err := a.Verify(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Verify(ctx, d, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Verify: %v", err)
	}
}

func TestOpenNormalisesDotSlashPrefix(t *testing.T) {
	// Some tar writers prefix names with "./"; the reader must normalise.
	b := archivetest.New()
	d := b.AddBlob([]byte("payload"))
	b.Top()
	raw := b.Bytes()
	rewritten := rewriteNames(t, raw, func(name string) string { return "./" + name })
	path := writeTemp(t, rewritten)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.Has(d) {
		t.Fatal("./-prefixed blob not indexed")
	}
}

func TestOpenIgnoresUnrelatedFilesAndNonHexBlobNames(t *testing.T) {
	b := archivetest.New()
	d := b.AddBlob([]byte("payload"))
	b.Top()
	raw := b.Bytes()
	extended := appendEntries(t, raw, []namedEntry{
		{name: "README", data: []byte("hello")},
		{name: "blobs/sha256/not-hex", data: []byte("junk")},
	})
	path := writeTemp(t, extended)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.Has(d) {
		t.Fatal("real blob not indexed")
	}
}

func TestOpenReportsTruncatedArchive(t *testing.T) {
	b := archivetest.New()
	// A blob large enough that the cut below lands well inside its content,
	// with plenty of bytes still missing before the next header.
	b.AddBlob(bytes.Repeat([]byte("x"), 4096))
	b.Top()
	raw := b.Bytes()

	off, size := blobContentSpan(t, raw)
	if size < 2 {
		t.Fatal("blob too small to truncate meaningfully")
	}
	truncated := raw[:off+size/2]
	path := writeTemp(t, truncated)

	_, err := Open(path)
	if err == nil {
		t.Fatal("Open on a truncated archive should fail")
	}
	if !strings.Contains(err.Error(), "reading tar") {
		t.Fatalf("err = %v, want it to mention reading tar", err)
	}
}

func TestOpenRejectsOversizedTopFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// A header claiming more bytes than actually follow: readTop must
	// reject it by size before ever trying to read the body.
	if err := tw.WriteHeader(&tar.Header{
		Name:     IndexFile,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     maxTopFile + 1,
	}); err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, buf.Bytes())

	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("Open on an oversized index.json: %v", err)
	}
}

// rewriteNames re-encodes a tar with every entry name mapped through f.
func rewriteNames(t *testing.T, data []byte, f func(string) string) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h.Name = f(h.Name)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return out.Bytes()
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// namedEntry is one extra regular-file entry for appendEntries.
type namedEntry struct {
	name string
	data []byte
}

// appendEntries re-encodes data's tar, unchanged, then appends extra as
// further regular-file entries.
func appendEntries(t *testing.T, data []byte, extra []namedEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range extra {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return out.Bytes()
}

// blobContentSpan returns the offset and size of the first blobs/sha256
// entry's content in raw, using the same reader-offset trick Open uses
// (archive/tar seeks past content when the reader is an io.Seeker, so the
// reader's offset right after Next is the entry's first content byte).
func blobContentSpan(t *testing.T, raw []byte) (off, size int64) {
	t.Helper()
	r := bytes.NewReader(raw)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && strings.HasPrefix(h.Name, blobPrefix) {
			pos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			return pos, h.Size
		}
	}
}
