package browse

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

const (
	layerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	layerTar  = "application/vnd.oci.image.layer.v1.tar"
)

// fixture is a store holding what the browser must be able to show:
//
//	library/app:v1        manifest; layers A (gzip, prism) and B (tar, prism); rootfs ok
//	library/app:latest    index over v1 (linux/amd64) and m2 (linux/arm64: layers A and C)
//	library/app@m2, @m3   manifests no tag points at
//	tools/rawimg:r1       manifest whose only layer is random bytes: raw not-tar, no rootfs
//	library/app/sub:x     a nested repository name
type fixture struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	blobs  *blob.Store // read-only
	images *image.Store
	b      *Browser

	sizes                                  map[oci.Digest]int64
	tarA, tarB, tarC, bigBinary            []byte
	layerA, layerB, layerC, rawLayer, conf oci.Digest
	m1, m2, m3, idx, rawM                  oci.Digest
}

// fixEntry is one tar entry of a fixture layer.
type fixEntry struct {
	name    string
	dir     bool
	data    []byte
	symlink string
	mode    int64
}

// fixTar builds a tar with a fixed mtime so listings are stable.
func fixTar(t *testing.T, entries []fixEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, ModTime: mtime}
		switch {
		case e.dir:
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case e.symlink != "":
			h.Typeflag, h.Mode, h.Linkname = tar.TypeSymlink, 0o777, e.symlink
		default:
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o644, int64(len(e.data))
		}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("tar write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// fixGzip compresses with compress/gzip at the default level, which the
// go-flate engine reproduces, so the layer becomes a prism.
func fixGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFixture(t *testing.T) *fixture { return newFixtureIn(t, t.TempDir()) }

// TestWriteFixture builds the fixture store under $OCI_AMBER_BROWSE_FIXTURE
// so the real binary can be pointed at it; it is skipped otherwise.
func TestWriteFixture(t *testing.T) {
	dir := os.Getenv("OCI_AMBER_BROWSE_FIXTURE")
	if dir == "" {
		t.Skip("set OCI_AMBER_BROWSE_FIXTURE to a directory to write the fixture store there")
	}
	newFixtureIn(t, dir)
}

// newFixtureIn builds the fixture in <dir>/store with <dir>/work as the
// ingest work directory.
func newFixtureIn(t *testing.T, dir string) *fixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	rw, err := blob.New(st, blob.Options{
		WorkDir:               filepath.Join(dir, "work"),
		MaxInMemory:           8 << 20,
		AnalyzeParallelism:    1,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 1,
		VerifyRoundTrip:       true,
		RecentTTL:             time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	f := &fixture{t: t, ctx: t.Context(), st: st, images: image.New(st, rw, log), sizes: map[oci.Digest]int64{}}

	rnd := rand.New(rand.NewPCG(1, 2))
	f.bigBinary = make([]byte, 300<<10)
	for i := range f.bigBinary {
		f.bigBinary[i] = byte(rnd.IntN(256))
	}
	copy(f.bigBinary, "\x7fELF")

	f.tarA = fixTar(t, []fixEntry{
		{name: "bin/", dir: true},
		{name: "bin/app", data: f.bigBinary, mode: 0o755},
		{name: "bin/sbin", symlink: "../usr/bin"},
		{name: "etc/", dir: true},
		{name: "etc/os-release", data: []byte("PRETTY_NAME=\"Fixture Linux\"\nID=fixture\nVERSION_ID=1\n")},
		{name: "etc/config.json", data: []byte(`{"listen":":8080","workers":4,"tags":["a","b"]}`)},
		{name: "etc/link-to-os", symlink: "os-release"},
		{name: "etc/abs", symlink: "/etc/os-release"},
		{name: "etc/dangling", symlink: "nowhere"},
		{name: "usr/", dir: true},
		{name: "usr/bin/", dir: true},
		{name: "usr/bin/tool.sh", data: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
		{name: "var/", dir: true},
		{name: "var/cache/", dir: true},
		{name: "var/cache/x", data: []byte("cached\n")},
	})
	f.tarB = fixTar(t, []fixEntry{
		{name: "etc/", dir: true},
		{name: "etc/hostname", data: []byte("fixture\n")},
		{name: ".wh.var"},
	})
	f.tarC = fixTar(t, []fixEntry{
		{name: "etc/", dir: true},
		{name: "etc/arch", data: []byte("arm64\n")},
	})
	f.layerA = f.putBlob(rw, fixGzip(t, f.tarA))
	f.layerB = f.putBlob(rw, f.tarB)
	f.layerC = f.putBlob(rw, fixGzip(t, f.tarC))
	raw := make([]byte, 4096)
	for i := range raw {
		raw[i] = byte(rnd.IntN(256))
	}
	f.rawLayer = f.putBlob(rw, raw)
	f.conf = f.putBlob(rw, []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"config":{"Cmd":["/bin/app"]}}`))

	f.m1 = f.putImage("library/app", "v1", f.manifest(map[string]string{"fixture": "m1"}, f.layerA, f.layerB))
	f.m2 = f.putImage("library/app", "", f.manifest(map[string]string{"fixture": "m2"}, f.layerA, f.layerC))
	index := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: []oci.Descriptor{
		{MediaType: oci.MediaTypeOCIManifest, Digest: f.m1, Size: f.sizes[f.m1], Platform: &oci.Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: oci.MediaTypeOCIManifest, Digest: f.m2, Size: f.sizes[f.m2], Platform: &oci.Platform{OS: "linux", Architecture: "arm64"}},
	}}
	f.idx = f.putImage("library/app", "latest", index)
	f.m3 = f.putImage("library/app", "", f.manifest(map[string]string{"fixture": "m3"}, f.layerB))
	f.rawM = f.putImage("tools/rawimg", "r1", f.manifest(map[string]string{"fixture": "raw"}, f.rawLayer))
	f.putImage("library/app/sub", "x", f.manifest(map[string]string{"fixture": "sub"}, f.layerB))

	f.blobs = blob.NewReadOnly(st, log)
	f.images = image.New(st, f.blobs, log)
	f.b = New(st, f.blobs, f.images)
	return f
}

func (f *fixture) putBlob(rw *blob.Store, data []byte) oci.Digest {
	f.t.Helper()
	m, err := rw.Put(f.ctx, upload.NewMemorySpool(data))
	if err != nil {
		f.t.Fatalf("blob.Put: %v", err)
	}
	f.sizes[m.Digest] = m.Size
	return m.Digest
}

// manifest is an image manifest over the shared config and layers; gzip
// layers get the +gzip media type, the rest the plain tar one.
func (f *fixture) manifest(annotations map[string]string, layers ...oci.Digest) oci.Manifest {
	m := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: f.conf, Size: f.sizes[f.conf]},
		Annotations:   annotations,
	}
	for _, d := range layers {
		mt := layerTar
		if d == f.layerA || d == f.layerC {
			mt = layerGzip
		}
		m.Layers = append(m.Layers, oci.Descriptor{MediaType: mt, Digest: d, Size: f.sizes[d]})
	}
	return m
}

// putImage pushes m to repo under tag, or by digest when tag is "".
func (f *fixture) putImage(repo, tag string, m oci.Manifest) oci.Digest {
	f.t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	ref := tag
	if ref == "" {
		ref = oci.DigestOfBytes(body).String()
	}
	meta, err := f.images.Put(f.ctx, repo, ref, m.MediaType, body)
	if err != nil {
		f.t.Fatalf("image.Put(%s, %s): %v", repo, ref, err)
	}
	f.sizes[meta.Digest] = meta.Size
	return meta.Digest
}

func (f *fixture) openBlob(d oci.Digest) *blob.Blob {
	f.t.Helper()
	bl, err := f.blobs.Open(d)
	if err != nil {
		f.t.Fatalf("blob.Open(%s): %v", d, err)
	}
	return bl
}

func (f *fixture) openImage(repo, reference string) *image.Image {
	f.t.Helper()
	im, err := f.images.Open(repo, reference)
	if err != nil {
		f.t.Fatalf("image.Open(%s, %s): %v", repo, reference, err)
	}
	return im
}

// lookupKey is the content key of name inside the directory object dir.
func (f *fixture) lookupKey(dir key.Key, name string) key.Key {
	f.t.Helper()
	k, err := f.st.LookupKey(dir, name)
	if err != nil {
		f.t.Fatalf("LookupKey(%s): %v", name, err)
	}
	return k
}

// mustList lists n or fails the test.
func mustList(t *testing.T, n Lister) []Row {
	t.Helper()
	rows, err := n.List()
	if err != nil {
		t.Fatalf("List(%s): %v", n.Crumb(), err)
	}
	return rows
}

func rowNames(rows []Row) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names
}

// rowNamed returns the row called name or fails the test.
func rowNamed(t *testing.T, rows []Row, name string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row %q among %v", name, rowNames(rows))
	return Row{}
}

// childList lists the Child of the row called name.
func childList(t *testing.T, rows []Row, name string) []Row {
	t.Helper()
	r := rowNamed(t, rows, name)
	l, ok := r.Child.(Lister)
	if !ok {
		t.Fatalf("row %q has Child %T, want a Lister", name, r.Child)
	}
	return mustList(t, l)
}

// readAll opens the row's Child as a file and reads it whole.
func readAll(t *testing.T, r Row) []byte {
	t.Helper()
	o, ok := r.Child.(Opener)
	if !ok {
		t.Fatalf("row %q has Child %T, want an Opener", r.Name, r.Child)
	}
	f, err := o.Open()
	if err != nil {
		t.Fatalf("Open(%s): %v", r.Name, err)
	}
	rd := f.Open()
	defer rd.Close()
	data, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read %s: %v", r.Name, err)
	}
	if int64(len(data)) != f.Size {
		t.Fatalf("%s: read %d bytes, File.Size is %d", r.Name, len(data), f.Size)
	}
	return data
}

// assertNames fails unless rows are named exactly want, in order.
func assertNames(t *testing.T, rows []Row, want ...string) {
	t.Helper()
	if got := rowNames(rows); !slices.Equal(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
}
