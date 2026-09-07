package image

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/core/key"
	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

const layerMediaType = "application/vnd.oci.image.layer.v1.tar+gzip"

// syncBuffer is a goroutine-safe bytes.Buffer for capturing log output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// env is one temporary store with a blob store and an image store over it.
type env struct {
	t      *testing.T
	st     *store.Store
	blobs  *blob.Store
	images *Store
	logs   *syncBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bs, err := blob.New(st, blob.Options{
		WorkDir:               filepath.Join(t.TempDir(), "work"),
		MaxInMemory:           8 << 20,
		AnalyzeParallelism:    1,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 1,
		VerifyRoundTrip:       true,
		RecentTTL:             time.Hour,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, blobs: bs, images: New(st, bs, log), logs: logs}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// putBlob pushes data through the blob store and returns a descriptor for it
// and the blob's meta (which carries the per-blob stats).
func (e *env) putBlob(mediaType string, data []byte) (oci.Descriptor, *blob.Meta) {
	e.t.Helper()
	m, err := e.blobs.Put(context.Background(), upload.NewMemorySpool(data))
	if err != nil {
		e.t.Fatal(err)
	}
	return oci.Descriptor{MediaType: mediaType, Digest: m.Digest, Size: m.Size}, m
}

// configBlob pushes a small JSON image config made unique by seed.
func (e *env) configBlob(seed string) (oci.Descriptor, *blob.Meta) {
	e.t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"config":{"Labels":{"seed":"` + seed + `"}}}`)
	return e.putBlob(oci.MediaTypeOCIConfig, cfg)
}

// layerBlob pushes n random bytes as a layer; it is stored raw, which is all
// the image tests need.
func (e *env) layerBlob(n int) (oci.Descriptor, *blob.Meta) {
	e.t.Helper()
	return e.putBlob(layerMediaType, randomBytes(e.t, n))
}

func manifestBody(t *testing.T, m oci.Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func imageManifest(config oci.Descriptor, layers ...oci.Descriptor) oci.Manifest {
	return oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest, Config: &config, Layers: layers}
}

func (e *env) put(repo, reference, contentType string, body []byte) *Meta {
	e.t.Helper()
	m, err := e.images.Put(context.Background(), repo, reference, contentType, body)
	if err != nil {
		e.t.Fatalf("Put(%s, %s): %v", repo, reference, err)
	}
	return m
}

func (e *env) resolve(name string) key.Key {
	e.t.Helper()
	k, err := e.st.Resolve(name)
	if err != nil {
		e.t.Fatalf("Resolve(%s): %v", name, err)
	}
	return k
}

func (e *env) absent(name string) {
	e.t.Helper()
	if _, err := e.st.Resolve(name); !errors.Is(err, store.ErrNotFound) {
		e.t.Fatalf("Resolve(%s) = %v, want ErrNotFound", name, err)
	}
}

func ociErr(t *testing.T, err error, code oci.ErrorCode) *oci.Error {
	t.Helper()
	var e *oci.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v (%T) is not *oci.Error", err, err)
	}
	if e.Code != code {
		t.Fatalf("error code = %s, want %s (%v)", e.Code, code, err)
	}
	return e
}

func TestPutMissingBlob(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("missing-blob")
	missing := oci.Descriptor{MediaType: layerMediaType, Digest: oci.DigestOfBytes([]byte("never uploaded")), Size: 14}
	body := manifestBody(t, imageManifest(cfg, missing))

	_, err := e.images.Put(context.Background(), "library/app", "v1", oci.MediaTypeOCIManifest, body)
	oe := ociErr(t, err, oci.CodeManifestBlobUnknown)
	detail, ok := oe.Detail.(map[string]string)
	if !ok || detail["digest"] != string(missing.Digest) {
		t.Fatalf("Detail = %#v, want {digest: %s}", oe.Detail, missing.Digest)
	}
	e.absent(ManifestRef("library/app", oci.DigestOfBytes(body)))
	e.absent(TagRef("library/app", "v1"))
}

func TestPutByTag(t *testing.T) {
	e := newEnv(t)
	cfg, cfgMeta := e.configBlob("by-tag")
	l1, l1Meta := e.layerBlob(200 << 10)
	l2, l2Meta := e.layerBlob(300 << 10)
	body := manifestBody(t, imageManifest(cfg, l1, l2))
	before := time.Now().Add(-time.Second)

	// Content-Type parameters are ignored; the media type is the bare type.
	m := e.put("library/app", "v1.0", oci.MediaTypeOCIManifest+"; charset=utf-8", body)

	digest := oci.DigestOfBytes(body)
	if m.Version != 1 || m.Kind != KindManifest || m.MediaType != oci.MediaTypeOCIManifest || m.Digest != digest || m.Size != int64(len(body)) {
		t.Fatalf("Meta = %+v", *m)
	}
	if m.ArtifactType != oci.MediaTypeOCIConfig {
		t.Fatalf("ArtifactType = %q, want the config media type fallback", m.ArtifactType)
	}
	if m.Subject != nil || m.Annotations != nil {
		t.Fatalf("Subject/Annotations should be absent: %+v", *m)
	}
	if m.CreatedAt.Before(before) || m.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("CreatedAt = %v", m.CreatedAt)
	}
	wantTotal := cfgMeta.Size + l1Meta.Size + l2Meta.Size + int64(len(body))
	if m.Stats.TotalBytes != wantTotal {
		t.Fatalf("TotalBytes = %d, want %d", m.Stats.TotalBytes, wantTotal)
	}
	tagKey := e.resolve(TagRef("library/app", "v1.0"))
	manKey := e.resolve(ManifestRef("library/app", digest))
	if tagKey != manKey {
		t.Fatalf("tag ref -> %s but manifest ref -> %s", tagKey, manKey)
	}
	refs, err := e.st.ListRefs(ReferrerPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("referrer refs created without a subject: %v", refs)
	}
}

func TestPutMediaTypeFallbacks(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("media-type")

	// No Content-Type: the manifest's own mediaType field wins.
	docker := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeDockerManifest, Config: &cfg}
	body := manifestBody(t, docker)
	if m := e.put("app", "docker", "", body); m.MediaType != oci.MediaTypeDockerManifest {
		t.Fatalf("MediaType = %q, want body mediaType", m.MediaType)
	}
	// Neither: the OCI manifest type is the last resort.
	bare := oci.Manifest{SchemaVersion: 2, Config: &cfg}
	body = manifestBody(t, bare)
	if m := e.put("app", "bare", "", body); m.MediaType != oci.MediaTypeOCIManifest {
		t.Fatalf("MediaType = %q, want OCI manifest default", m.MediaType)
	}
	// Content-Type wins over the body.
	body = manifestBody(t, docker)
	if m := e.put("app", "ct", oci.MediaTypeOCIManifest, body); m.MediaType != oci.MediaTypeOCIManifest {
		t.Fatalf("MediaType = %q, want Content-Type", m.MediaType)
	}
}

func TestOpenByTagAndDigest(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("open")
	l1, _ := e.layerBlob(64 << 10)
	body := manifestBody(t, imageManifest(cfg, l1))
	putMeta := e.put("library/app", "latest", oci.MediaTypeOCIManifest, body)
	digest := oci.DigestOfBytes(body)

	for _, ref := range []string{"latest", string(digest)} {
		im, err := e.images.Open("library/app", ref)
		if err != nil {
			t.Fatalf("Open(%s): %v", ref, err)
		}
		if im.Meta.Digest != digest || im.Meta.Size != int64(len(body)) || im.Meta.MediaType != oci.MediaTypeOCIManifest ||
			im.Meta.Kind != KindManifest || im.Meta.Stats != putMeta.Stats || !im.Meta.CreatedAt.Equal(putMeta.CreatedAt) {
			t.Fatalf("Open(%s).Meta = %+v, want %+v", ref, im.Meta, *putMeta)
		}
		if im.Root() != e.resolve(ManifestRef("library/app", digest)) {
			t.Fatalf("Open(%s).Root() = %s, want the manifest ref's key", ref, im.Root())
		}
		var buf bytes.Buffer
		if err := im.WriteTo(context.Background(), &buf); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), body) {
			t.Fatalf("WriteTo produced %d bytes that differ from the %d pushed", buf.Len(), len(body))
		}
	}

	for _, c := range []struct{ repo, ref string }{
		{"library/app", "nope"},
		{"library/app", string(oci.DigestOfBytes([]byte("other")))},
		{"other/repo", "latest"},
		{"other/repo", string(digest)},
		{"library/app", "-bad"},
		{"Library/App", "latest"},
	} {
		if _, err := e.images.Open(c.repo, c.ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%s, %s) = %v, want ErrNotFound", c.repo, c.ref, err)
		}
	}
	_, err := e.images.Open("library/app", "sha512:"+hexA+hexA)
	ociErr(t, err, oci.CodeDigestInvalid)

	im, err := e.images.Open("library/app", "latest")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := im.WriteTo(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo with cancelled ctx = %v, want context.Canceled", err)
	}
	// A stored digest that does not match the bytes is reported after the body.
	im.Meta.Digest = oci.Digest("sha256:" + hexA)
	var buf bytes.Buffer
	if err := im.WriteTo(context.Background(), &buf); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("WriteTo with wrong digest = %v, want ErrDigestMismatch", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatal("WriteTo must still write the body before reporting the mismatch")
	}
}

func TestPutByDigest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg, _ := e.configBlob("by-digest")
	body := manifestBody(t, imageManifest(cfg))
	digest := oci.DigestOfBytes(body)

	m := e.put("app", string(digest), oci.MediaTypeOCIManifest, body)
	if m.Digest != digest {
		t.Fatalf("Digest = %s, want %s", m.Digest, digest)
	}
	e.resolve(ManifestRef("app", digest))
	refs, err := e.st.ListRefs(TagPrefix + "app:")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("push by digest created tag refs: %v", refs)
	}

	wrong := oci.DigestOfBytes([]byte("something else"))
	_, err = e.images.Put(ctx, "app", string(wrong), oci.MediaTypeOCIManifest, body)
	ociErr(t, err, oci.CodeDigestInvalid)
	e.absent(ManifestRef("app", wrong))

	_, err = e.images.Put(ctx, "app", "sha512:"+hexA+hexA, oci.MediaTypeOCIManifest, body)
	ociErr(t, err, oci.CodeDigestInvalid)
}

func TestPutInvalidTag(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("bad-tag")
	body := manifestBody(t, imageManifest(cfg))
	for _, tag := range []string{"-bad", ".dot", strings.Repeat("a", 129), "with space", "slash/tag"} {
		_, err := e.images.Put(context.Background(), "app", tag, oci.MediaTypeOCIManifest, body)
		oe := ociErr(t, err, oci.CodeManifestInvalid)
		if !strings.Contains(oe.Message, "invalid tag") {
			t.Errorf("tag %q: message %q does not mention invalid tag", tag, oe.Message)
		}
	}
	e.absent(ManifestRef("app", oci.DigestOfBytes(body)))
}

func TestPutInvalidManifest(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`not json`,
		`{"schemaVersion":1,"layers":[]}`,
		`{"schemaVersion":2,"layers":[{"mediaType":"x","digest":"sha256:zz","size":1}]}`,
		`{"schemaVersion":2,"config":{"mediaType":"x","digest":"sha256:` + hexA + `","size":1},"subject":{"mediaType":"x","digest":"sha256:short","size":1}}`,
	} {
		_, err := e.images.Put(context.Background(), "app", "v1", oci.MediaTypeOCIManifest, []byte(body))
		ociErr(t, err, oci.CodeManifestInvalid)
	}
	refs, err := e.st.ListRefs(ManifestPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("invalid manifests were published: %v", refs)
	}
}

func TestPutInvalidRepository(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("bad-repo")
	body := manifestBody(t, imageManifest(cfg))
	for _, repo := range []string{"Library/App", "app/", "/app", "a//b", ""} {
		_, err := e.images.Put(context.Background(), repo, "v1", oci.MediaTypeOCIManifest, body)
		ociErr(t, err, oci.CodeNameInvalid)
	}
}

func TestPutRootLayout(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("layout")
	l1, _ := e.layerBlob(100 << 10)
	l2, _ := e.layerBlob(100 << 10)
	// l1 is listed twice: blobs/ holds it once and TotalBytes counts it once.
	body := manifestBody(t, imageManifest(cfg, l1, l2, l1))
	m := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	if want := cfg.Size + l1.Size + l2.Size + int64(len(body)); m.Stats.TotalBytes != want {
		t.Fatalf("TotalBytes = %d, want %d (duplicate layer counted once)", m.Stats.TotalBytes, want)
	}
	root := e.resolve(ManifestRef("library/app", m.Digest))

	man, err := e.st.Lookup(root, ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if man.Mode != store.ModeFile {
		t.Fatalf("%s mode = %o, want %o", ManifestFile, man.Mode, store.ModeFile)
	}
	mk, err := key.Parse(man.ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	if mk.Length() != uint64(len(body)) {
		t.Fatalf("manifest key length = %d, want %d", mk.Length(), len(body))
	}
	got, err := e.st.ReadFile(mk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("stored manifest bytes differ from the pushed body")
	}

	metaEntry, err := e.st.Lookup(root, MetaFile)
	if err != nil {
		t.Fatal(err)
	}
	if metaEntry.Mode != store.ModeFile {
		t.Fatalf("%s mode = %o, want %o", MetaFile, metaEntry.Mode, store.ModeFile)
	}
	metaKey, err := key.Parse(metaEntry.ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	metaBytes, err := e.st.ReadFile(metaKey)
	if err != nil {
		t.Fatal(err)
	}
	var stored Meta
	if err := json.Unmarshal(metaBytes, &stored); err != nil {
		t.Fatalf("meta.json is not valid JSON: %v\n%s", err, metaBytes)
	}
	if stored.Version != 1 || stored.Digest != m.Digest || stored.Size != m.Size || stored.Kind != m.Kind ||
		stored.MediaType != m.MediaType || stored.ArtifactType != m.ArtifactType || stored.Stats != m.Stats || !stored.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("stored meta %+v differs from returned %+v", stored, *m)
	}
	if bytes.Contains(metaBytes, []byte(`"subject"`)) || bytes.Contains(metaBytes, []byte(`"annotations"`)) {
		t.Fatalf("meta.json carries absent fields:\n%s", metaBytes)
	}

	blobsEntry, err := e.st.Lookup(root, BlobsDir)
	if err != nil {
		t.Fatal(err)
	}
	if blobsEntry.Mode != store.ModeDir {
		t.Fatalf("%s mode = %o, want %o", BlobsDir, blobsEntry.Mode, store.ModeDir)
	}
	blobsKey, err := key.Parse(blobsEntry.ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	entries, more, err := e.st.ListDir(blobsKey, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(entries) != 3 {
		t.Fatalf("blobs/ has %d entries (more=%v), want 3", len(entries), more)
	}
	for i, ent := range entries {
		name := string(ent.Name)
		if i > 0 && !(string(entries[i-1].Name) < name) {
			t.Fatalf("blobs/ entries not sorted: %q then %q", entries[i-1].Name, name)
		}
		if ent.Mode != store.ModeDir {
			t.Fatalf("blobs/%s mode = %o, want %o", name, ent.Mode, store.ModeDir)
		}
		d, err := oci.ParseDigest(name)
		if err != nil {
			t.Fatalf("blobs/ entry %q is not a digest: %v", name, err)
		}
		ck, err := key.Parse(ent.ContentKey)
		if err != nil {
			t.Fatal(err)
		}
		if want := e.resolve(blob.RefName(d)); ck != want {
			t.Fatalf("blobs/%s -> %s, want the blob root %s", name, ck, want)
		}
	}
	for _, d := range []oci.Digest{cfg.Digest, l1.Digest, l2.Digest} {
		if _, err := e.st.Lookup(blobsKey, string(d)); err != nil {
			t.Fatalf("blobs/%s: %v", d, err)
		}
	}
	if _, err := e.st.Lookup(root, ManifestsDir); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Lookup(%s) on a manifest root = %v, want ErrNotFound", ManifestsDir, err)
	}
}

func TestPutIdempotent(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("idempotent")
	l1, _ := e.layerBlob(128 << 10)
	body := manifestBody(t, imageManifest(cfg, l1))

	first := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	second := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	if second.Digest != first.Digest || second.Size != first.Size || second.Stats.TotalBytes != first.Stats.TotalBytes {
		t.Fatalf("second push differs: %+v vs %+v", *second, *first)
	}
	if second.Stats.DiskBytes != 0 || second.Stats.DedupedBytes != second.Stats.LogicalBytes {
		t.Fatalf("second push of an identical manifest wrote %d disk bytes, deduped %d of %d",
			second.Stats.DiskBytes, second.Stats.DedupedBytes, second.Stats.LogicalBytes)
	}
	if e.resolve(TagRef("library/app", "v1")) != e.resolve(ManifestRef("library/app", first.Digest)) {
		t.Fatal("tag and manifest refs diverged after re-push")
	}
	im, err := e.images.Open("library/app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if im.Meta.Stats != second.Stats {
		t.Fatalf("stored stats %+v, want the latest push %+v", im.Meta.Stats, second.Stats)
	}
}

func TestPutCancelledContext(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("cancelled")
	l1, _ := e.layerBlob(64 << 10)
	body := manifestBody(t, imageManifest(cfg, l1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.images.Put(ctx, "app", "v1", oci.MediaTypeOCIManifest, body)
	if err == nil {
		t.Fatal("Put with a cancelled context succeeded")
	}
	var oe *oci.Error
	if errors.As(err, &oe) {
		t.Fatalf("cancelled context reported as a client error: %v", err)
	}
	e.absent(ManifestRef("app", oci.DigestOfBytes(body)))
	e.absent(TagRef("app", "v1"))
}
