package image

import (
	"errors"
	"testing"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
)

func TestDeleteByTag(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("delete-tag")
	l, _ := e.layerBlob(64 << 10)
	body := manifestBody(t, imageManifest(cfg, l))
	digest := oci.DigestOfBytes(body)
	e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	e.put("library/app", "v2", oci.MediaTypeOCIManifest, body)

	if err := e.images.Delete("library/app", "v1"); err != nil {
		t.Fatalf("Delete(v1): %v", err)
	}
	e.absent(TagRef("library/app", "v1"))
	e.resolve(TagRef("library/app", "v2"))
	e.resolve(ManifestRef("library/app", digest))
	if _, err := e.images.Open("library/app", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(v1) after delete = %v, want ErrNotFound", err)
	}
	if _, err := e.images.Open("library/app", "v2"); err != nil {
		t.Fatalf("Open(v2) after deleting v1: %v", err)
	}
	if _, err := e.images.Open("library/app", string(digest)); err != nil {
		t.Fatalf("Open(digest) after deleting v1: %v", err)
	}

	for _, c := range []struct{ repo, ref string }{
		{"library/app", "v1"},   // already deleted
		{"library/app", "nope"}, // never existed
		{"library/app", "-bad"}, // not a tag
		{"other/repo", "v2"},    // wrong repo
		{"Library/App", "v2"},   // invalid repo
	} {
		if err := e.images.Delete(c.repo, c.ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%s, %s) = %v, want ErrNotFound", c.repo, c.ref, err)
		}
	}
}

func TestDeleteByDigest(t *testing.T) {
	e := newEnv(t)
	subject := oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes([]byte("the subject")), Size: 11}
	cfg, _ := e.configBlob("delete-digest")
	l, _ := e.layerBlob(64 << 10)
	m := imageManifest(cfg, l)
	m.Subject = &subject
	body := manifestBody(t, m)
	digest := oci.DigestOfBytes(body)
	e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	e.put("library/app", "v2", oci.MediaTypeOCIManifest, body)

	// Another manifest in the same repository, and the same manifest in
	// another repository, must survive.
	cfgKeep, _ := e.configBlob("keep")
	keepBody := manifestBody(t, imageManifest(cfgKeep, l))
	keepDigest := oci.DigestOfBytes(keepBody)
	e.put("library/app", "keep", oci.MediaTypeOCIManifest, keepBody)
	e.put("other/repo", "v1", oci.MediaTypeOCIManifest, body)

	if err := e.images.Delete("library/app", string(digest)); err != nil {
		t.Fatalf("Delete(digest): %v", err)
	}
	e.absent(ManifestRef("library/app", digest))
	e.absent(TagRef("library/app", "v1"))
	e.absent(TagRef("library/app", "v2"))
	e.absent(ReferrerRef("library/app", subject.Digest, digest))
	e.resolve(TagRef("library/app", "keep"))
	e.resolve(ManifestRef("library/app", keepDigest))
	e.resolve(TagRef("other/repo", "v1"))
	e.resolve(ManifestRef("other/repo", digest))
	e.resolve(ReferrerRef("other/repo", subject.Digest, digest))
	// Blob refs are never touched by a manifest delete.
	e.resolve(blob.RefName(cfg.Digest))
	e.resolve(blob.RefName(l.Digest))

	if _, err := e.images.Open("library/app", string(digest)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
	if err := e.images.Delete("library/app", string(digest)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
	if err := e.images.Delete("library/app", string(oci.DigestOfBytes([]byte("unknown")))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(unknown digest) = %v, want ErrNotFound", err)
	}
	err := e.images.Delete("library/app", "sha512:"+hexA+hexA)
	ociErr(t, err, oci.CodeDigestInvalid)
}

func TestDeleteByDigestAfterRepush(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("repush")
	body := manifestBody(t, imageManifest(cfg))
	digest := oci.DigestOfBytes(body)
	e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	oldRoot := e.resolve(ManifestRef("library/app", digest))

	// Re-push by digest with a different Content-Type: a new root (its
	// meta.json differs) while the tag still points at the old one.
	e.put("library/app", string(digest), oci.MediaTypeDockerManifest, body)
	newRoot := e.resolve(ManifestRef("library/app", digest))
	if newRoot == oldRoot {
		t.Fatal("re-push with a different media type produced the same root")
	}
	if e.resolve(TagRef("library/app", "v1")) != oldRoot {
		t.Fatal("re-push by digest moved the tag")
	}

	if err := e.images.Delete("library/app", string(digest)); err != nil {
		t.Fatal(err)
	}
	e.absent(ManifestRef("library/app", digest))
	e.absent(TagRef("library/app", "v1"))
}

func TestDeleteConcurrentSameRepo(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("concurrent")
	body := manifestBody(t, imageManifest(cfg))
	digest := oci.DigestOfBytes(body)
	e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- e.images.Delete("library/app", string(digest)) }()
	}
	var ok, notFound int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotFound):
			notFound++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || notFound != 1 {
		t.Fatalf("concurrent deletes: %d succeeded, %d not found; want 1 and 1", ok, notFound)
	}
	e.absent(TagRef("library/app", "v1"))
}
