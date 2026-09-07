package image_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// listEnv is an image store over a temporary amber store with one config
// blob already uploaded, so that test manifests have something to reference.
// Names are prefixed with "list" so they cannot collide with helpers in the
// package's other test files.
type listEnv struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
	config oci.Descriptor
}

func newListEnv(t *testing.T) *listEnv {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               filepath.Join(dir, "work"),
		MaxInMemory:           1 << 20,
		AnalyzeParallelism:    1,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 1,
		VerifyRoundTrip:       true,
		RecentTTL:             time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	env := &listEnv{t: t, ctx: t.Context(), st: st, blobs: blobs, images: image.New(st, blobs, log)}
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	meta, err := blobs.Put(env.ctx, upload.NewMemorySpool(cfg))
	if err != nil {
		t.Fatalf("blob.Put(config): %v", err)
	}
	env.config = oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: meta.Digest, Size: meta.Size}
	return env
}

// manifest returns an OCI image manifest over the shared config blob.
// Different annotations make otherwise identical manifests distinct.
func (e *listEnv) manifest(artifactType string, subject *oci.Descriptor, annotations map[string]string) oci.Manifest {
	cfg := e.config
	return oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		ArtifactType:  artifactType,
		Config:        &cfg,
		Subject:       subject,
		Annotations:   annotations,
	}
}

// put marshals m and pushes it to repo under tag, or by digest when tag is
// empty, with m.MediaType as the Content-Type. It returns the stored
// metadata and the exact bytes pushed.
func (e *listEnv) put(repo, tag string, m oci.Manifest) (*image.Meta, []byte) {
	e.t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		e.t.Fatalf("marshal manifest: %v", err)
	}
	reference := tag
	if reference == "" {
		reference = string(oci.DigestOfBytes(body))
	}
	meta, err := e.images.Put(e.ctx, repo, reference, m.MediaType, body)
	if err != nil {
		e.t.Fatalf("image.Put(%s, %s): %v", repo, reference, err)
	}
	if meta.Digest != oci.DigestOfBytes(body) || meta.Size != int64(len(body)) {
		e.t.Fatalf("image.Put(%s, %s) returned digest %s size %d, want %s %d",
			repo, reference, meta.Digest, meta.Size, oci.DigestOfBytes(body), len(body))
	}
	return meta, body
}

// descriptorOf is the descriptor a parent (an index's manifests entry or a
// referrer's subject field) uses for a stored manifest.
func descriptorOf(m *image.Meta) oci.Descriptor {
	return oci.Descriptor{MediaType: m.MediaType, Digest: m.Digest, Size: m.Size}
}

func TestTagsSorted(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	for _, tag := range []string{"v2", "latest", "1.0", "Beta", "v10"} {
		env.put(repo, tag, env.manifest("", nil, map[string]string{"tag": tag}))
	}
	// Repositories whose names extend repo share its ref prefix up to the
	// ':' and must not leak into its tag list.
	env.put("library/app2", "zzz", env.manifest("", nil, nil))
	env.put("library/app/sub", "sub", env.manifest("", nil, nil))
	// Moving a tag to another manifest keeps a single entry for it.
	env.put(repo, "latest", env.manifest("", nil, map[string]string{"tag": "latest-moved"}))

	tags, err := env.images.Tags(repo)
	if err != nil {
		t.Fatalf("Tags(%q): %v", repo, err)
	}
	// Bytewise order: digits < uppercase < lowercase, and "v10" < "v2".
	want := []string{"1.0", "Beta", "latest", "v10", "v2"}
	if !slices.Equal(tags, want) {
		t.Fatalf("Tags(%q) = %q, want %q", repo, tags, want)
	}
}

func TestTagsUnknownRepo(t *testing.T) {
	env := newListEnv(t)
	env.put("library/app", "v1", env.manifest("", nil, nil))
	// "library" is a prefix of library/app's manifest ref namespace and
	// "library/ap" a prefix of its tag ref; neither is a repository.
	for _, repo := range []string{"nope", "library", "library/ap", "library/app/sub"} {
		tags, err := env.images.Tags(repo)
		if !errors.Is(err, image.ErrNotFound) {
			t.Fatalf("Tags(%q) = %q, %v; want ErrNotFound", repo, tags, err)
		}
	}
}

func TestTagsDigestOnlyRepo(t *testing.T) {
	env := newListEnv(t)
	env.put("digest/only", "", env.manifest("", nil, nil))
	tags, err := env.images.Tags("digest/only")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if tags == nil || len(tags) != 0 {
		t.Fatalf("Tags = %#v, want an empty non-nil slice", tags)
	}
}

func TestTagsAfterDelete(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	meta, _ := env.put(repo, "v1", env.manifest("", nil, nil))

	// Deleting by tag drops only the tag ref: the repository still exists
	// through its manifest ref and answers an empty list.
	if err := env.images.Delete(repo, "v1"); err != nil {
		t.Fatalf("Delete(v1): %v", err)
	}
	tags, err := env.images.Tags(repo)
	if err != nil || tags == nil || len(tags) != 0 {
		t.Fatalf("after deleting the tag: Tags = %#v, %v; want [], nil", tags, err)
	}

	// Deleting by digest drops the manifest ref, the repository's last one.
	if err := env.images.Delete(repo, string(meta.Digest)); err != nil {
		t.Fatalf("Delete(%s): %v", meta.Digest, err)
	}
	if _, err := env.images.Tags(repo); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("after deleting the manifest: Tags error = %v, want ErrNotFound", err)
	}
}

func TestManifestsListsTaggedAndUntagged(t *testing.T) {
	e := newListEnv(t)
	tagged, _ := e.put("library/app", "v1", e.manifest("", nil, map[string]string{"n": "1"}))
	untagged, _ := e.put("library/app", "", e.manifest("", nil, map[string]string{"n": "2"}))
	// A nested repository shares the ref prefix "oci/manifest/library/app/".
	e.put("library/app/sub", "x", e.manifest("", nil, map[string]string{"n": "3"}))

	got, err := e.images.Manifests("library/app")
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	want := []oci.Digest{tagged.Digest, untagged.Digest}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Manifests = %v, want %v", got, want)
	}
	if _, err := e.images.Manifests("nobody/here"); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("unknown repository: %v, want ErrNotFound", err)
	}
}
