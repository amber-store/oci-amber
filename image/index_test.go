package image

import (
	"bytes"
	"context"
	"testing"

	"github.com/amber-store/core/key"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// pushChild pushes a one-layer image manifest by digest and returns its body,
// digest and meta.
func (e *env) pushChild(repo, seed string) ([]byte, oci.Digest, *Meta) {
	e.t.Helper()
	cfg, _ := e.configBlob(seed)
	l, _ := e.layerBlob(150 << 10)
	body := manifestBody(e.t, imageManifest(cfg, l))
	d := oci.DigestOfBytes(body)
	return body, d, e.put(repo, string(d), oci.MediaTypeOCIManifest, body)
}

func TestPutIndex(t *testing.T) {
	e := newEnv(t)
	bodyA, dA, metaA := e.pushChild("library/app", "child-a")
	bodyB, dB, metaB := e.pushChild("library/app", "child-b")

	idx := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIIndex,
		Manifests: []oci.Descriptor{
			{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA)), Annotations: map[string]string{"os": "linux"}},
			{MediaType: oci.MediaTypeOCIManifest, Digest: dB, Size: int64(len(bodyB))},
			{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA))}, // duplicate child
		},
		Annotations: map[string]string{"org.opencontainers.image.ref.name": "latest"},
	}
	body := manifestBody(t, idx)
	m := e.put("library/app", "latest", oci.MediaTypeOCIIndex, body)

	if m.Kind != KindIndex || m.MediaType != oci.MediaTypeOCIIndex || m.ArtifactType != "" || m.Subject != nil {
		t.Fatalf("Meta = %+v", *m)
	}
	if m.Annotations["org.opencontainers.image.ref.name"] != "latest" {
		t.Fatalf("Annotations = %v", m.Annotations)
	}
	if want := int64(len(body)) + metaA.Stats.TotalBytes + metaB.Stats.TotalBytes; m.Stats.TotalBytes != want {
		t.Fatalf("TotalBytes = %d, want index bytes + children totals %d", m.Stats.TotalBytes, want)
	}
	if m.Stats.LogicalBytes < metaA.Stats.LogicalBytes+metaB.Stats.LogicalBytes+int64(len(body)) {
		t.Fatalf("LogicalBytes = %d, want at least children's %d + index length", m.Stats.LogicalBytes, metaA.Stats.LogicalBytes+metaB.Stats.LogicalBytes)
	}
	if m.Stats.DiskBytes <= metaA.Stats.DiskBytes+metaB.Stats.DiskBytes {
		t.Fatalf("DiskBytes = %d, want more than children's %d (the index objects are new)", m.Stats.DiskBytes, metaA.Stats.DiskBytes+metaB.Stats.DiskBytes)
	}
	if m.Stats.DedupedBytes < metaA.Stats.DedupedBytes+metaB.Stats.DedupedBytes {
		t.Fatalf("DedupedBytes = %d, want at least children's %d", m.Stats.DedupedBytes, metaA.Stats.DedupedBytes+metaB.Stats.DedupedBytes)
	}

	root := e.resolve(ManifestRef("library/app", m.Digest))
	if e.resolve(TagRef("library/app", "latest")) != root {
		t.Fatal("tag ref does not point at the index root")
	}
	mansKey, err := e.st.LookupKey(root, ManifestsDir)
	if err != nil {
		t.Fatalf("manifests/: %v", err)
	}
	entries, more, err := e.st.ListDir(mansKey, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(entries) != 2 {
		t.Fatalf("manifests/ has %d entries (more=%v), want 2", len(entries), more)
	}
	for _, ent := range entries {
		if ent.Mode != store.ModeDir {
			t.Fatalf("manifests/%s mode = %o, want %o", ent.Name, ent.Mode, store.ModeDir)
		}
		d, err := oci.ParseDigest(string(ent.Name))
		if err != nil {
			t.Fatal(err)
		}
		ck, err := key.Parse(ent.ContentKey)
		if err != nil {
			t.Fatal(err)
		}
		if want := e.resolve(ManifestRef("library/app", d)); ck != want {
			t.Fatalf("manifests/%s -> %s, want child root %s", d, ck, want)
		}
	}
	if string(entries[0].Name) != string(min(dA, dB)) || string(entries[1].Name) != string(max(dA, dB)) {
		t.Fatalf("manifests/ entries %q, %q are not the two children in order", entries[0].Name, entries[1].Name)
	}
	blobsKey, err := e.st.LookupKey(root, BlobsDir)
	if err != nil {
		t.Fatalf("blobs/: %v", err)
	}
	entries, more, err = e.st.ListDir(blobsKey, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(entries) != 0 {
		t.Fatalf("an index root's blobs/ has %d entries, want 0", len(entries))
	}

	im, err := e.images.Open("library/app", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if im.Meta.Kind != KindIndex {
		t.Fatalf("Open kind = %s", im.Meta.Kind)
	}
	var buf bytes.Buffer
	if err := im.WriteTo(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatal("index bytes differ after round trip")
	}
}

func TestPutIndexMissingChild(t *testing.T) {
	e := newEnv(t)
	bodyA, dA, _ := e.pushChild("library/app", "present")
	// A child that exists only in another repository is unknown here.
	bodyO, dO, _ := e.pushChild("other/repo", "elsewhere")
	missing := oci.DigestOfBytes([]byte("no such manifest"))

	for _, c := range []struct {
		name string
		d    oci.Digest
		size int64
	}{
		{"never pushed", missing, 10},
		{"other repository", dO, int64(len(bodyO))},
	} {
		idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: []oci.Descriptor{
			{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA))},
			{MediaType: oci.MediaTypeOCIManifest, Digest: c.d, Size: c.size},
		}}
		body := manifestBody(t, idx)
		_, err := e.images.Put(context.Background(), "library/app", "latest", oci.MediaTypeOCIIndex, body)
		oe := ociErr(t, err, oci.CodeManifestBlobUnknown)
		detail, ok := oe.Detail.(map[string]string)
		if !ok || detail["digest"] != string(c.d) {
			t.Fatalf("%s: Detail = %#v, want digest %s", c.name, oe.Detail, c.d)
		}
		e.absent(ManifestRef("library/app", oci.DigestOfBytes(body)))
		e.absent(TagRef("library/app", "latest"))
	}
}

func TestPutWithSubject(t *testing.T) {
	e := newEnv(t)
	// The subject does not have to exist.
	subject := oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes([]byte("subject never pushed")), Size: 20}
	cfg, _ := e.putBlob("application/vnd.example.config.v1+json", []byte(`{"signature":"yes"}`))
	sig := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		ArtifactType:  "application/vnd.example.signature.v1",
		Config:        &cfg,
		Subject:       &subject,
		Annotations:   map[string]string{"io.example.created": "2026-09-03"},
	}
	body := manifestBody(t, sig)
	digest := oci.DigestOfBytes(body)
	m := e.put("library/app", string(digest), oci.MediaTypeOCIManifest, body)

	if m.ArtifactType != "application/vnd.example.signature.v1" {
		t.Fatalf("ArtifactType = %q, want the explicit artifactType", m.ArtifactType)
	}
	if m.Subject == nil || m.Subject.MediaType != subject.MediaType || m.Subject.Digest != subject.Digest || m.Subject.Size != subject.Size {
		t.Fatalf("Subject = %+v, want %+v", m.Subject, subject)
	}
	if m.Annotations["io.example.created"] != "2026-09-03" {
		t.Fatalf("Annotations = %v", m.Annotations)
	}
	root := e.resolve(ManifestRef("library/app", digest))
	if got := e.resolve(ReferrerRef("library/app", subject.Digest, digest)); got != root {
		t.Fatalf("referrer ref -> %s, want the image root %s", got, root)
	}
	im, err := e.images.Open("library/app", string(digest))
	if err != nil {
		t.Fatal(err)
	}
	if im.Meta.Subject == nil || im.Meta.Subject.MediaType != subject.MediaType || im.Meta.Subject.Digest != subject.Digest || im.Meta.Subject.Size != subject.Size || im.Meta.ArtifactType != m.ArtifactType || im.Meta.Annotations["io.example.created"] != "2026-09-03" {
		t.Fatalf("stored meta lost subject/artifactType/annotations: %+v", im.Meta)
	}

	// An index with a subject records a referrer ref too.
	idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, ArtifactType: "application/vnd.example.sbom.v1", Subject: &subject}
	idxBody := manifestBody(t, idx)
	idxDigest := oci.DigestOfBytes(idxBody)
	im2 := e.put("library/app", string(idxDigest), oci.MediaTypeOCIIndex, idxBody)
	if im2.Kind != KindIndex || im2.ArtifactType != "application/vnd.example.sbom.v1" {
		t.Fatalf("index meta = %+v", *im2)
	}
	e.resolve(ReferrerRef("library/app", subject.Digest, idxDigest))
}
