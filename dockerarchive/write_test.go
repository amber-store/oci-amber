package dockerarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/draganm/oci-amber/oci"
)

// memSource is a Source over maps: the manifests and blobs of the images a
// test saves.
type memSource struct {
	manifests map[oci.Digest][]byte
	blobs     map[oci.Digest][]byte
	blobErr   error // returned by every Blob call when set
	blobCalls int
}

func newMemSource() *memSource {
	return &memSource{manifests: map[oci.Digest][]byte{}, blobs: map[oci.Digest][]byte{}}
}

func (m *memSource) Manifest(ctx context.Context, repo string, d oci.Digest) ([]byte, error) {
	b, ok := m.manifests[d]
	if !ok {
		return nil, fmt.Errorf("manifest %s not in %s", d, repo)
	}
	return b, nil
}

func (m *memSource) Blob(ctx context.Context, d oci.Digest, w io.Writer) error {
	m.blobCalls++
	if m.blobErr != nil {
		return m.blobErr
	}
	b, ok := m.blobs[d]
	if !ok {
		return fmt.Errorf("blob %s not stored", d)
	}
	_, err := w.Write(b)
	return err
}

func (m *memSource) addBlob(mediaType string, data []byte) oci.Descriptor {
	d := oci.DigestOfBytes(data)
	m.blobs[d] = data
	return oci.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}
}

// addImage stores a config, its layers and an OCI manifest over them, and
// returns the manifest's descriptor with platform and annotations.
func (m *memSource) addImage(config string, layers []string, platform *oci.Platform, annotations map[string]string) oci.Descriptor {
	man := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest}
	c := m.addBlob(oci.MediaTypeOCIConfig, []byte(config))
	man.Config = &c
	for _, l := range layers {
		man.Layers = append(man.Layers, m.addBlob("application/vnd.oci.image.layer.v1.tar+gzip", []byte(l)))
	}
	body, _ := json.Marshal(man)
	d := oci.DigestOfBytes(body)
	m.manifests[d] = body
	return oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: d, Size: int64(len(body)), Platform: platform, Annotations: annotations}
}

func (m *memSource) addIndex(children ...oci.Descriptor) oci.Descriptor {
	body, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: children})
	d := oci.DigestOfBytes(body)
	m.manifests[d] = body
	return oci.Descriptor{MediaType: oci.MediaTypeOCIIndex, Digest: d, Size: int64(len(body))}
}

// write saves images from src and returns the archive bytes and an
// Archive opened over them.
func write(t *testing.T, src Source, opts WriteOptions, images ...Export) ([]byte, *Archive) {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, src, images, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(t.TempDir(), "saved.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open(saved): %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return buf.Bytes(), a
}

// entries lists the tar's entries as "name mode type mtime".
func entries(t *testing.T, archive []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %04o %c %d", h.Name, h.Mode, h.Typeflag, h.ModTime.Unix()))
	}
}

func TestWriteSingleImage(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{"architecture":"amd64","os":"linux"}`, []string{"layer one", "layer two"}, nil, nil)
	archive, a := write(t, src, WriteOptions{}, Export{Repo: "demo/app", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"})

	// index.json: the stored descriptor plus the names docker records.
	if len(a.Index.Manifests) != 1 {
		t.Fatalf("index.json has %d manifests, want 1", len(a.Index.Manifests))
	}
	got := a.Index.Manifests[0]
	want := oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: img.Digest, Size: img.Size, Annotations: map[string]string{
		"org.opencontainers.image.ref.name": "v1",
		"io.containerd.image.name":          "docker.io/demo/app:v1",
	}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("index.json entry:\n got %+v\nwant %+v", got, want)
	}
	if a.LayoutVersion != "1.0.0" {
		t.Errorf("oci-layout version %q", a.LayoutVersion)
	}
	// manifest.json: Config, RepoTags, Layers as blob paths.
	m, _ := oci.ParseManifest(src.manifests[img.Digest])
	wantLegacy := LegacyEntry{Config: "blobs/sha256/" + m.Config.Digest.Hex(), RepoTags: []string{"demo/app:v1"},
		Layers: []string{"blobs/sha256/" + m.Layers[0].Digest.Hex(), "blobs/sha256/" + m.Layers[1].Digest.Hex()}}
	if len(a.Legacy) != 1 || fmt.Sprint(a.Legacy[0]) != fmt.Sprint(wantLegacy) {
		t.Errorf("manifest.json:\n got %+v\nwant %+v", a.Legacy, wantLegacy)
	}
	// Every blob is there, byte for byte, the manifest included.
	for d, data := range src.blobs {
		if b, err := a.ReadBlob(d); err != nil || !bytes.Equal(b, data) {
			t.Errorf("blob %s: %v", d, err)
		}
	}
	if b, err := a.ReadBlob(img.Digest); err != nil || !bytes.Equal(b, src.manifests[img.Digest]) {
		t.Errorf("manifest blob: %v", err)
	}
	// The archive imports under the saved name.
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Entries) != 1 || fmt.Sprint(p.Entries[0].Names) != "[demo/app:v1]" {
		t.Errorf("plan entries %+v", p.Entries)
	}
	// Layout as containerd's exporter writes it: directories, blobs in
	// digest order, then the three top-level files; 0444 blobs, epoch mtimes.
	var hexes []string
	for d := range src.blobs {
		hexes = append(hexes, d.Hex())
	}
	hexes = append(hexes, img.Digest.Hex())
	slices.Sort(hexes)
	wantEntries := []string{"blobs/ 0755 5 0", "blobs/sha256/ 0755 5 0"}
	for _, h := range hexes {
		wantEntries = append(wantEntries, "blobs/sha256/"+h+" 0444 0 0")
	}
	wantEntries = append(wantEntries, "index.json 0644 0 0", "manifest.json 0644 0 0", "oci-layout 0444 0 0")
	if got := entries(t, archive); !slices.Equal(got, wantEntries) {
		t.Errorf("entries:\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(wantEntries, "\n     "))
	}
}

func TestWriteIndex(t *testing.T) {
	src := newMemSource()
	amd := src.addImage(`{"os":"linux","architecture":"amd64"}`, []string{"amd layer"}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	arm := src.addImage(`{"os":"linux","architecture":"arm64"}`, []string{"arm layer"}, &oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, nil)
	att := src.addImage(`{"os":"unknown","architecture":"unknown"}`, []string{"in-toto"}, &oci.Platform{OS: "unknown", Architecture: "unknown"},
		map[string]string{oci.AnnotationReferenceType: oci.AttestationManifest, "vnd.docker.reference.digest": amd.Digest.String()})
	idx := src.addIndex(amd, arm, att)
	export := Export{Repo: "multi", Digest: idx.Digest, MediaType: idx.MediaType, Tag: "latest"}

	legacyConfig := func(a *Archive) string {
		t.Helper()
		if len(a.Legacy) != 1 {
			t.Fatalf("manifest.json has %d entries, want 1", len(a.Legacy))
		}
		return a.Legacy[0].Config
	}
	configOf := func(d oci.Descriptor) string {
		m, _ := oci.ParseManifest(src.manifests[d.Digest])
		return "blobs/sha256/" + m.Config.Digest.Hex()
	}

	// The preferred platform picks the manifest.json entry.
	_, a := write(t, src, WriteOptions{Platform: &oci.Platform{OS: "linux", Architecture: "arm64"}}, export)
	if got := legacyConfig(a); got != configOf(arm) {
		t.Errorf("manifest.json config = %s, want arm64's %s", got, configOf(arm))
	}
	if !slices.Equal(a.Legacy[0].RepoTags, []string{"multi:latest"}) {
		t.Errorf("RepoTags = %v", a.Legacy[0].RepoTags)
	}
	// Everything under the index is saved: the index, all children, the
	// attestation, every blob.
	for d := range src.blobs {
		if _, err := a.ReadBlob(d); err != nil {
			t.Errorf("blob %s: %v", d, err)
		}
	}
	for d := range src.manifests {
		if _, err := a.ReadBlob(d); err != nil {
			t.Errorf("manifest %s: %v", d, err)
		}
	}
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if e := p.Entries[0]; !e.IsIndex || e.Platforms != 2 || e.Attestations != 1 || fmt.Sprint(e.Names) != "[multi:latest]" {
		t.Errorf("plan entry %+v", e)
	}

	// No preference, or one nothing matches: the first child with a real
	// platform, never the attestation.
	_, a = write(t, src, WriteOptions{}, export)
	if got := legacyConfig(a); got != configOf(amd) {
		t.Errorf("no preference: manifest.json config = %s, want amd64's", got)
	}
	_, a = write(t, src, WriteOptions{Platform: &oci.Platform{OS: "linux", Architecture: "s390x"}}, export)
	if got := legacyConfig(a); got != configOf(amd) {
		t.Errorf("unmatched preference: manifest.json config = %s, want amd64's", got)
	}
}

func TestWriteIndexWithoutRunnableChild(t *testing.T) {
	src := newMemSource()
	att := src.addImage(`{}`, nil, &oci.Platform{OS: "unknown", Architecture: "unknown"},
		map[string]string{oci.AnnotationReferenceType: oci.AttestationManifest})
	idx := src.addIndex(att)
	archive, a := write(t, src, WriteOptions{}, Export{Repo: "att", Digest: idx.Digest, MediaType: idx.MediaType, Tag: "v1"})
	if a.Legacy != nil {
		t.Errorf("manifest.json should be absent, got %+v", a.Legacy)
	}
	for _, e := range entries(t, archive) {
		if strings.HasPrefix(e, "manifest.json") {
			t.Errorf("manifest.json written: %s", e)
		}
	}
	if len(a.Index.Manifests) != 1 {
		t.Errorf("index.json entries %+v", a.Index.Manifests)
	}
}

func TestWriteDigestReference(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{}`, []string{"l"}, nil, nil)
	archive, a := write(t, src, WriteOptions{}, Export{Repo: "demo/app", Digest: img.Digest, MediaType: img.MediaType})
	if d := a.Index.Manifests[0]; d.Annotations != nil {
		t.Errorf("digest reference must carry no name annotations, got %v", d.Annotations)
	}
	if len(a.Legacy) != 1 || a.Legacy[0].RepoTags != nil {
		t.Errorf("manifest.json = %+v, want one entry without RepoTags", a.Legacy)
	}
	if !bytes.Contains(archive, []byte(`"RepoTags":null`)) {
		t.Error(`manifest.json must spell "RepoTags":null, as docker does`)
	}
	if _, err := a.Plan(PlanOptions{Names: []string{"demo/app:restored"}}); err != nil {
		t.Errorf("import with --name: %v", err)
	}
}

func TestWriteSameImageTwoTags(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{}`, []string{"l"}, nil, nil)
	_, a := write(t, src, WriteOptions{},
		Export{Repo: "demo/app", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"},
		Export{Repo: "demo/app", Digest: img.Digest, MediaType: img.MediaType, Tag: "latest"})
	if len(a.Index.Manifests) != 2 {
		t.Fatalf("index.json has %d entries, want one per tag", len(a.Index.Manifests))
	}
	if r0, r1 := a.Index.Manifests[0].Annotations["org.opencontainers.image.ref.name"], a.Index.Manifests[1].Annotations["org.opencontainers.image.ref.name"]; r0 != "v1" || r1 != "latest" {
		t.Errorf("ref names %q, %q", r0, r1)
	}
	if len(a.Legacy) != 1 || !slices.Equal(a.Legacy[0].RepoTags, []string{"demo/app:v1", "demo/app:latest"}) {
		t.Errorf("manifest.json = %+v", a.Legacy)
	}
	if src.blobCalls != 2 {
		t.Errorf("blobs streamed %d times, want once each (config, layer)", src.blobCalls)
	}
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Entries) != 1 || fmt.Sprint(p.Entries[0].Names) != "[demo/app:v1 demo/app:latest]" {
		t.Errorf("plan entries %+v", p.Entries)
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{}`, []string{"b", "a"}, nil, nil)
	export := Export{Repo: "demo/app", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"}
	first, _ := write(t, src, WriteOptions{}, export)
	second, _ := write(t, src, WriteOptions{}, export)
	if !bytes.Equal(first, second) {
		t.Error("two saves of the same image differ")
	}
}

func TestWriteBlobError(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{}`, []string{"l"}, nil, nil)
	src.blobErr = errors.New("recompress failed")
	err := Write(context.Background(), io.Discard, src, []Export{{Repo: "x", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"}}, WriteOptions{})
	if err == nil || !errors.Is(err, src.blobErr) {
		t.Fatalf("Write = %v, want the blob error", err)
	}
}

func TestWriteUnknownManifest(t *testing.T) {
	src := newMemSource()
	err := Write(context.Background(), io.Discard, src, []Export{{Repo: "x", Digest: oci.DigestOfBytes([]byte("nope")), MediaType: oci.MediaTypeOCIManifest, Tag: "v1"}}, WriteOptions{})
	if err == nil {
		t.Fatal("Write must fail for a manifest the source does not have")
	}
}

func TestDockerReference(t *testing.T) {
	for in, want := range map[string]string{
		"busybox:1.37":                    "docker.io/library/busybox:1.37",
		"team/app:v1":                     "docker.io/team/app:v1",
		"localhost:5000/app:v1":           "localhost:5000/app:v1",
		"registry.example.ch/team/app:v1": "registry.example.ch/team/app:v1",
	} {
		repo, tag := oci.SplitReference(in)
		if got := dockerReference(Name{Repo: repo, Tag: tag}); got != want {
			t.Errorf("dockerReference(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestWriteManifestWithoutConfig: an artifact manifest has no config, so it
// gets no manifest.json entry; as the first child of an index it is passed
// over for one that has a config.
func TestWriteManifestWithoutConfig(t *testing.T) {
	src := newMemSource()
	layer := src.addBlob("application/vnd.example.data", []byte("artifact payload"))
	body, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest, ArtifactType: "application/vnd.example", Layers: []oci.Descriptor{layer}})
	art := oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(body), Size: int64(len(body))}
	src.manifests[art.Digest] = body

	archive, a := write(t, src, WriteOptions{}, Export{Repo: "art", Digest: art.Digest, MediaType: art.MediaType, Tag: "v1"})
	if a.Legacy != nil {
		t.Errorf("manifest.json should be absent for an artifact, got %+v", a.Legacy)
	}
	for _, e := range entries(t, archive) {
		if strings.HasPrefix(e, "manifest.json") {
			t.Errorf("manifest.json written: %s", e)
		}
	}
	if b, err := a.ReadBlob(layer.Digest); err != nil || !bytes.Equal(b, src.blobs[layer.Digest]) {
		t.Errorf("artifact layer: %v", err)
	}

	img := src.addImage(`{}`, []string{"l"}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	idx := src.addIndex(art, img)
	_, a = write(t, src, WriteOptions{}, Export{Repo: "mixed", Digest: idx.Digest, MediaType: idx.MediaType, Tag: "v1"})
	m, _ := oci.ParseManifest(src.manifests[img.Digest])
	if len(a.Legacy) != 1 || a.Legacy[0].Config != "blobs/sha256/"+m.Config.Digest.Hex() {
		t.Errorf("manifest.json = %+v, want the image child's config", a.Legacy)
	}
}

// sizedSource wraps a Source and writes delta bytes more (or fewer) than
// the blob holds.
type sizedSource struct {
	Source
	delta int
}

func (s sizedSource) Blob(ctx context.Context, d oci.Digest, w io.Writer) error {
	var buf bytes.Buffer
	if err := s.Source.Blob(ctx, d, &buf); err != nil {
		return err
	}
	b := buf.Bytes()
	if s.delta < 0 {
		b = b[:len(b)+s.delta]
	} else {
		b = append(b, bytes.Repeat([]byte{0}, s.delta)...)
	}
	_, err := w.Write(b)
	return err
}

func TestWriteBlobSizeMismatch(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{}`, []string{"layer bytes"}, nil, nil)
	export := []Export{{Repo: "x", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"}}
	err := Write(context.Background(), io.Discard, sizedSource{src, -1}, export, WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "its descriptor says") {
		t.Errorf("short blob: %v", err)
	}
	err = Write(context.Background(), io.Discard, sizedSource{src, 1}, export, WriteOptions{})
	if err == nil || !errors.Is(err, tar.ErrWriteTooLong) {
		t.Errorf("long blob: %v", err)
	}
}

func TestWriteReportsProgress(t *testing.T) {
	src := newMemSource()
	img := src.addImage(`{"os":"linux"}`, []string{"layer-one", "layer-two-is-longer"}, nil, nil)
	var got []WriteProgress
	write(t, src, WriteOptions{Progress: func(p WriteProgress) { got = append(got, p) }}, Export{Repo: "x", Digest: img.Digest, MediaType: img.MediaType, Tag: "v1"})

	// Every blobs/sha256 entry, manifest included, in the order they are
	// written: each is announced before its first byte, reported after
	// every write, and counted done afterwards.
	sizes := map[oci.Digest]int64{img.Digest: img.Size}
	for d, b := range src.blobs {
		sizes[d] = int64(len(b))
	}
	digests := slices.Sorted(maps.Keys(sizes))
	var total int64
	for _, s := range sizes {
		total += s
	}
	want := []WriteProgress{{Count: len(digests), Total: total}}
	var written int64
	for i, d := range digests {
		want = append(want, WriteProgress{Count: len(digests), Total: total, Written: written, Done: i, Blob: d, Size: sizes[d]})
		written += sizes[d]
		want = append(want, WriteProgress{Count: len(digests), Total: total, Written: written, Done: i, Blob: d, Size: sizes[d], BlobWritten: sizes[d]})
		want = append(want, WriteProgress{Count: len(digests), Total: total, Written: written, Done: i + 1})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("progress calls:\n got %+v\nwant %+v", got, want)
	}
}
