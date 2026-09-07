# Browse Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `oci-amber browse --store DIR [repo[:tag|@digest]]` is a terminal browser over a store: repositories, tags, how every image is stored (image roots, blob roots, prism parts) or its root filesystem, and a viewer that shows files as text, pretty-printed JSON or a hex dump.

**Architecture:** A new flat package `browse` holds a small node interface (`Lister`, `Opener`, `Resolver`) with concrete nodes over `store`, `image`, `blob` and `rootfs`; a Bubble Tea model that keeps a base stack (repositories, repository) plus, inside an image, two stacks (storage and filesystem) it switches between; pure rendering functions for listings, text and hex; and `Run`. Three small additions outside it make opening a store side-effect free and expose untagged manifests: `store.ErrInUse`, `blob.NewReadOnly`, `image.Store.Manifests`. `cmd/oci-amber` gains the `browse` subcommand.

**Tech Stack:** Go 1.26, urfave/cli v2, charmbracelet/bubbletea v1.3.10, bubbles v1.0.0 (`textinput`), lipgloss v1.1.0, charmbracelet/x/term (direct already), tar-prism (for the `blobs/`, `recipe.json`, `recipe.bin` names and `Index`).

**Spec:** `docs/superpowers/specs/2026-09-05-browse-command-design.md`

## Global Constraints

- Flat top-level packages; never `internal/`.
- No new module dependencies: everything used is already in `go.mod` (bubbletea, bubbles, lipgloss, x/term, tar-prism, amber-store-core).
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` (empty output) before every commit.
- Commits end with the trailer block:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
  ```
- Delete any binary you build (`go build -o ...`) before finishing a task.
- Doc comments in the style of the existing packages: full sentences, say why.
- The browser never writes: no `store.Writer`, no `blob.New`, no `MkdirAll` in the `browse` package or in `runBrowse`.
- Under `go test`, lipgloss renders no escape sequences (Ascii profile), so rendering tests compare exact plain strings.

---

### Task 1: `store.ErrInUse`

**Files:**
- Modify: `store/store.go`
- Test: `store/store_test.go` (package `store_test`)

**Interfaces:**
- Produces: `var store.ErrInUse error`; `store.Open` returns an error wrapping it when another process holds the directory.

- [ ] **Step 1: Write the failing test**

Append to `store/store_test.go`:

```go
func TestOpenWhileOpenReportsInUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	_, err = store.Open(dir, store.Options{})
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("second Open returned %v, want ErrInUse", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q does not name the directory", err)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./store -run TestOpenWhileOpenReportsInUse`
Expected: compile error `undefined: store.ErrInUse`.

- [ ] **Step 3: Implement**

In `store/store.go` add `"syscall"` to the imports, and next to `ErrNotFound`:

```go
// ErrInUse reports a store directory that another process has open. The
// packstore takes an exclusive flock on its directory, so a second Open
// (say `oci-amber browse` while `serve` runs) fails with EWOULDBLOCK.
var ErrInUse = errors.New("store: in use by another process")
```

In `Open`, replace the packstore error return:

```go
	objects, err := packstore.Open(filepath.Join(dir, "packstore"),
		packstore.WithSync(true), packstore.WithSegmentSize(cfg.SegmentSize))
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, dir)
		}
		return nil, fmt.Errorf("store: %w", err)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./store`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add store/store.go store/store_test.go
git commit -m "store: ErrInUse for a directory another process holds"
```

---

### Task 2: `blob.NewReadOnly`

**Files:**
- Modify: `blob/store.go`
- Test: `blob/store_test.go` (package `blob`)

**Interfaces:**
- Produces: `func blob.NewReadOnly(st *store.Store, log *slog.Logger) *blob.Store`; `var blob.ErrReadOnly error`. `Put` and `Delete` on such a store return `ErrReadOnly`; `Open`, `Exists`, `Blob.WriteTo`, `Blob.Prism` work.

- [ ] **Step 1: Write the failing test**

Append to `blob/store_test.go` (add `"compress/gzip"`, `"bytes"`, `"errors"` to its imports if missing):

```go
func TestReadOnlyStoreReadsButRefusesWrites(t *testing.T) {
	rw, st, _ := newTestStore(t, Options{})
	data := gzipBytes(t, tarBytes(t, "hello.txt", []byte("hello\n")), gzip.DefaultCompression)
	meta, err := rw.Put(t.Context(), spoolOf(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	ro := NewReadOnly(st, nil)
	if _, err := ro.Put(t.Context(), spoolOf([]byte("x"))); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Put returned %v, want ErrReadOnly", err)
	}
	if err := ro.Delete(meta.Digest); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Delete returned %v, want ErrReadOnly", err)
	}
	ok, err := ro.Exists(meta.Digest)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
	bl, err := ro.Open(meta.Digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if bl.Meta.Kind != KindPrism {
		t.Fatalf("kind %s, want prism", bl.Meta.Kind)
	}
	if _, err := bl.Prism(); err != nil {
		t.Fatalf("Prism: %v", err)
	}
	var out bytes.Buffer
	if err := bl.WriteTo(t.Context(), &out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("read-only pull differs from what was put")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./blob -run TestReadOnlyStoreReadsButRefusesWrites`
Expected: compile error `undefined: NewReadOnly`.

- [ ] **Step 3: Implement**

In `blob/store.go`, next to the other errors:

```go
	// ErrReadOnly reports Put or Delete on a Store made by NewReadOnly.
	ErrReadOnly = errors.New("blob: store is read-only")
```

Add the field `readOnly bool` to `Store` (after `recent`), then after `New`:

```go
// NewReadOnly returns a Store over st that only reads: Open, Exists and
// the pull path work, Put and Delete return ErrReadOnly. It needs no work
// directory and creates or deletes nothing, so a tool that only looks at
// a store (`oci-amber browse`) opens it without side effects. A nil log
// uses slog.Default.
func NewReadOnly(st *store.Store, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{st: st, log: log, readOnly: true, recent: make(map[oci.Digest]recentEntry)}
}
```

At the top of `Put`, after the nil-spool check:

```go
	if b.readOnly {
		return nil, ErrReadOnly
	}
```

At the top of `Delete`:

```go
	if b.readOnly {
		return ErrReadOnly
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./blob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add blob/store.go blob/store_test.go
git commit -m "blob: NewReadOnly opens a store without a work directory"
```

---

### Task 3: `image.Store.Manifests`

**Files:**
- Modify: `image/list.go`
- Test: `image/list_test.go` (package `image_test`)

**Interfaces:**
- Produces: `func (s *image.Store) Manifests(repo string) ([]oci.Digest, error)`: every manifest or index digest pushed to `repo`, tagged or not, sorted bytewise; `image.ErrNotFound` when there is none.

- [ ] **Step 1: Write the failing test**

Append to `image/list_test.go` (add `"slices"` to the imports if missing):

```go
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
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./image -run TestManifestsListsTaggedAndUntagged`
Expected: compile error `e.images.Manifests undefined`.

- [ ] **Step 3: Implement**

Append to `image/list.go`:

```go
// Manifests returns the digest of every manifest or index pushed to repo,
// tagged or not, sorted bytewise. The prefix "oci/manifest/<repo>/" is
// also a prefix of every nested repository's manifest refs, so each name
// is parsed and its repository compared exactly, as repoHasManifests
// does. A repository with no manifest refs yields ErrNotFound.
func (s *Store) Manifests(repo string) ([]oci.Digest, error) {
	refs, err := s.st.ListRefs(ManifestRef(repo, ""))
	if err != nil {
		return nil, fmt.Errorf("image: listing manifests of %s: %w", repo, err)
	}
	digests := make([]oci.Digest, 0, len(refs))
	for _, r := range refs {
		refRepo, d, ok := ParseManifestRef(r.Name)
		if ok && refRepo == repo {
			digests = append(digests, d)
		}
	}
	if len(digests) == 0 {
		return nil, ErrNotFound
	}
	slices.Sort(digests)
	return digests, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./image`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add image/list.go image/list_test.go
git commit -m "image: Manifests lists every manifest digest of a repository"
```

---

### Task 4: `browse` nodes over blobs and raw directories

The package skeleton, the test fixture every later task uses, and the nodes that need only `store` and `blob`: a regular file, a raw amber directory, a blob root and a prism's numbered blobs.

**Files:**
- Create: `browse/node.go`, `browse/browser.go`, `browse/storage.go`
- Test: `browse/fixture_test.go`, `browse/storage_test.go`

**Interfaces:**
- Produces:
  - `type Node interface { Crumb() string }`, `type Lister interface { Node; List() ([]Row, error) }`, `type Opener interface { Node; Open() (*File, error) }`, `type Resolver interface { Node; Resolve() (Node, error) }`
  - `type Row struct { Name, Detail string; Size int64; HasSize bool; Meta *RowMeta; Info []KV; Child Node; IsDir bool }`
  - `type RowMeta struct { Mode, UID, GID uint64; Mtime time.Time; Target string }`
  - `type KV struct { Key, Value string }`
  - `type File struct { Name string; Size int64; Key key.Key; Labels []KV; Open func() *store.Reader }`
  - `type Browser struct` with `func New(st *store.Store, blobs *blob.Store, images *image.Store) *Browser`
  - nodes: `*fileNode{st, name, key, labels}`, `*amberDirNode{st, name, dir, labels}`, `*blobRootNode{st, bl}`, `*prismBlobsNode{st, bl, dir}`
  - helpers: `shortRef(oci.Digest) string`, `plural(int, string) string`, `keyInfo(string, key.Key) []KV`, `entryRow(rootfs.Entry) Row`, `entryLabels(rootfs.Entry) []KV`, `blobKind(blob.Meta) string`, `blobLabels(blob.Meta) []KV`

- [ ] **Step 1: Write the fixture**

Create `browse/fixture_test.go`:

```go
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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
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
```

- [ ] **Step 2: Write the failing tests**

Create `browse/storage_test.go`:

```go
package browse

import (
	"bytes"
	"strings"
	"testing"

	tarprism "github.com/draganm/tar-prism"

	"github.com/amber-store/oci-amber/store"
)

func TestBlobRootListsPrismParts(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	rows := mustList(t, &blobRootNode{st: f.st, bl: bl})
	assertNames(t, rows, "blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json")

	blobs := rows[0]
	if !blobs.IsDir || blobs.Detail != "5 files" {
		t.Errorf("blobs row = %+v, want a directory with detail %q", blobs, "5 files")
	}
	if _, ok := blobs.Child.(*prismBlobsNode); !ok {
		t.Errorf("blobs Child is %T, want *prismBlobsNode", blobs.Child)
	}
	meta := rowNamed(t, rows, "meta.json")
	if !meta.HasSize || meta.Size == 0 || meta.Detail != "blob metadata" {
		t.Errorf("meta.json row = %+v", meta)
	}
	data := readAll(t, meta)
	if !bytes.Contains(data, []byte(`"kind": "prism"`)) {
		t.Errorf("meta.json content: %s", data)
	}
	if got := rowNamed(t, rows, "recipe.bin"); got.Detail != "tar recipe: every byte that is not file content" {
		t.Errorf("recipe.bin detail %q", got.Detail)
	}
}

func TestBlobRootListsRawParts(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.rawLayer)
	rows := mustList(t, &blobRootNode{st: f.st, bl: bl})
	assertNames(t, rows, "meta.json", "raw")
	raw := rows[1]
	if raw.Size != 4096 || raw.Detail != "the blob's bytes, verbatim" {
		t.Errorf("raw row = %+v", raw)
	}
	if len(readAll(t, raw)) != 4096 {
		t.Error("raw content length")
	}
}

func TestPrismBlobsAnnotatedWithTarEntries(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	dir := f.lookupKey(bl.Root(), tarprism.BlobsDir)
	rows := mustList(t, &prismBlobsNode{st: f.st, bl: bl, dir: dir})
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5: %v", len(rows), rowNames(rows))
	}
	byEntry := map[string]Row{}
	for _, r := range rows {
		if !strings.HasPrefix(r.Name, "0000") {
			t.Errorf("row name %q is not a blob number", r.Name)
		}
		byEntry[r.Detail] = r
	}
	for _, want := range []string{"bin/app", "etc/os-release", "etc/config.json", "usr/bin/tool.sh", "var/cache/x"} {
		if _, ok := byEntry[want]; !ok {
			t.Errorf("no row annotated %q", want)
		}
	}
	app := byEntry["bin/app"]
	if app.Size != int64(len(f.bigBinary)) {
		t.Errorf("bin/app size %d, want %d", app.Size, len(f.bigBinary))
	}
	if !bytes.Equal(readAll(t, app), f.bigBinary) {
		t.Error("bin/app content differs")
	}
	o := app.Child.(Opener)
	file, err := o.Open()
	if err != nil {
		t.Fatal(err)
	}
	if file.Labels[0] != (KV{"file", "bin/app"}) || file.Labels[1] != (KV{"blob", shortRef(f.layerA)}) {
		t.Errorf("labels %v", file.Labels)
	}
	if got := rowNamed(t, rows, app.Name).Info; len(got) < 5 || got[4] != (KV{"tar entry", "bin/app"}) {
		t.Errorf("info %v", got)
	}
}

func TestAmberDirShowsRawRootfs(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	root, ok := im.Rootfs()
	if !ok {
		t.Fatal("v1 has no rootfs")
	}
	rows := mustList(t, &amberDirNode{st: f.st, name: "rootfs", dir: root})
	assertNames(t, rows, "bin", "etc", "usr") // var/ was whited out by layer B

	etc := childList(t, rows, "etc")
	assertNames(t, etc, "abs", "config.json", "dangling", "hostname", "link-to-os", "os-release")
	link := rowNamed(t, etc, "link-to-os")
	if link.Meta == nil || link.Meta.Target != "os-release" || link.Child != nil {
		t.Errorf("symlink row = %+v; want target os-release and no Child", link)
	}
	if link.Meta.Mtime.Year() != 2026 {
		t.Errorf("mtime %v", link.Meta.Mtime)
	}
	os := rowNamed(t, etc, "os-release")
	if os.Meta.Mode&0o777 != 0o644 || os.Meta.Mode&store.TypeMask != store.TypeReg {
		t.Errorf("os-release mode %o", os.Meta.Mode)
	}
	if got := string(readAll(t, os)); !strings.HasPrefix(got, "PRETTY_NAME=") {
		t.Errorf("os-release content %q", got)
	}
	if rowNamed(t, etc, "abs").Meta.Target != "/etc/os-release" {
		t.Error("absolute symlink target")
	}
}

func TestFileNodeRefusesDirectoryKeys(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	n := &fileNode{st: f.st, name: "root", key: im.Root()}
	if _, err := n.Open(); err == nil || !strings.Contains(err.Error(), "not file content") {
		t.Fatalf("Open on a directory key: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := plural(1, "tag"); got != "1 tag" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(1234, "file"); got != "1,234 files" {
		t.Errorf("plural(1234) = %q", got)
	}
	if got := shortRef("sha256:4f7c9a1e0000000000000000000000000000000000000000000000000000abcd"); got != "sha256:4f7c9a1e" {
		t.Errorf("shortRef = %q", got)
	}
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./browse`
Expected: compile errors (`undefined: New`, `blobRootNode`, ...).

- [ ] **Step 4: Write `browse/node.go`**

```go
// Package browse is the terminal browser behind `oci-amber browse`: a node
// tree over the images, blobs and root filesystems a store holds, a
// Bubble Tea model that walks it one listing at a time, and a viewer that
// shows files as text, pretty-printed JSON or a hex dump. Nothing in the
// package writes to the store.
package browse

import (
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

// Node is one place the browser can be. Every Node is also a Lister, an
// Opener or a Resolver.
type Node interface {
	// Crumb is the node's breadcrumb segment; "" contributes nothing.
	Crumb() string
}

// Lister is a Node that lists rows.
type Lister interface {
	Node
	List() ([]Row, error)
}

// Opener is a Node that is a regular file.
type Opener interface {
	Node
	Open() (*File, error)
}

// Resolver is a Node whose kind is only known once it is followed: a
// symlink in the filesystem view. Resolve returns the Lister or Opener it
// leads to.
type Resolver interface {
	Node
	Resolve() (Node, error)
}

// Row is one line of a listing.
type Row struct {
	Name    string   // first column
	Detail  string   // annotation, already formatted; may be empty
	Size    int64    // bytes, shown when HasSize
	HasSize bool
	Meta    *RowMeta // ls -l columns for rootfs rows; nil elsewhere
	Info    []KV     // the info popup, in display order
	Child   Node     // what Enter opens; nil when nothing can be opened
	IsDir   bool     // rendered as a directory
}

// RowMeta are the ls -l columns of a rootfs entry.
type RowMeta struct {
	Mode     uint64 // type bits and permissions
	UID, GID uint64
	Mtime    time.Time
	Target   string // symlinks
}

// KV is one label/value pair of an info popup or a viewer status line.
type KV struct{ Key, Value string }

// File is an opened regular file.
type File struct {
	Name   string
	Size   int64
	Key    key.Key
	Labels []KV                 // facts for the viewer's status line
	Open   func() *store.Reader // a fresh reader positioned at the start
}
```

- [ ] **Step 5: Write `browse/browser.go`**

```go
package browse

import (
	"fmt"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// Browser builds nodes over an open store. It only reads.
type Browser struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
}

// New returns a Browser over st. blobs and images must be stores over the
// same st; blob.NewReadOnly is enough for blobs.
func New(st *store.Store, blobs *blob.Store, images *image.Store) *Browser {
	return &Browser{st: st, blobs: blobs, images: images}
}

// shortRef abbreviates a digest for rows and crumbs: "sha256:4f7c9a1e".
func shortRef(d oci.Digest) string { return d.Algorithm() + ":" + tui.ShortDigest(d) }

// plural renders n with word: "1 tag", "1,234 files".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%s %ss", tui.FormatCount(int64(n)), word)
}
```

- [ ] **Step 6: Write `browse/storage.go`**

```go
package browse

import (
	"fmt"
	"strings"
	"time"

	tarprism "github.com/draganm/tar-prism"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// fileNode is a regular file of the storage tree.
type fileNode struct {
	st     *store.Store
	name   string
	key    key.Key
	labels []KV
}

func (n *fileNode) Crumb() string { return n.name }

func (n *fileNode) Open() (*File, error) {
	if t := n.key.Type(); t != key.Blob && t != key.FileNode {
		return nil, fmt.Errorf("browse: %s is a %s, not file content", n.name, t)
	}
	st, k := n.st, n.key
	return &File{Name: n.name, Size: int64(k.Length()), Key: k, Labels: n.labels, Open: func() *store.Reader { return st.NewReader(k) }}, nil
}

// keyInfo are the info lines every storage row carries.
func keyInfo(name string, k key.Key) []KV {
	return []KV{
		{"name", name},
		{"key", k.String()},
		{"type", k.Type().String()},
		{"length", tui.FormatCount(int64(k.Length())) + " bytes"},
	}
}

// listDir decodes the entries of the directory object dir in name order.
func listDir(st *store.Store, dir key.Key) ([]rootfs.Entry, error) {
	entries, _, err := rootfs.NewFS(st, dir).List("", "", 0)
	return entries, err
}

// entryRow renders a rootfs entry's ls -l columns and info; the caller
// sets Child.
func entryRow(e rootfs.Entry) Row {
	mtime := time.Unix(0, e.Mtime).UTC()
	r := Row{Name: e.Name, IsDir: e.IsDir(), Meta: &RowMeta{Mode: e.Mode, UID: e.UID, GID: e.GID, Mtime: mtime, Target: e.Target}}
	info := []KV{
		{"name", e.Name},
		{"type", e.TypeName()},
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
		{"mtime", mtime.Format(time.RFC3339)},
	}
	switch e.Type() {
	case store.TypeReg:
		r.Size, r.HasSize = e.Size, true
		info = append(info, KV{"size", tui.FormatCount(e.Size) + " bytes"}, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeDir:
		info = append(info, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeLink:
		info = append(info, KV{"target", e.Target})
	case store.TypeChar, store.TypeBlock:
		info = append(info, KV{"device", fmt.Sprintf("%d, %d", e.Rdev[0], e.Rdev[1])})
	}
	r.Info = info
	return r
}

// entryLabels are the viewer status facts of a rootfs file.
func entryLabels(e rootfs.Entry) []KV {
	return []KV{
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
	}
}

// amberDirNode is a directory of the storage tree the browser has no
// special knowledge of: rootfs/ and everything under it, or an entry a
// future layout adds. Rows carry the entry's own metadata; symlinks are
// shown, not followed.
type amberDirNode struct {
	st     *store.Store
	name   string
	dir    key.Key
	labels []KV // inherited by files: the image or layer they belong to
}

func (n *amberDirNode) Crumb() string { return n.name }

func (n *amberDirNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s: %w", n.name, err)
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		r := entryRow(e)
		switch e.Type() {
		case store.TypeDir:
			r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: n.labels}
		case store.TypeReg:
			r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: append(entryLabels(e), n.labels...)}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// blobKind summarizes how a blob is stored: "prism gzip go-flate",
// "prism none", "raw not-tar".
func blobKind(m blob.Meta) string {
	if m.Kind == blob.KindRaw {
		return "raw " + string(m.RawReason)
	}
	s := "prism " + m.Format
	if m.Engine != "" {
		s += " " + m.Engine
	}
	return s
}

// blobLabels are the viewer status facts of a blob's parts.
func blobLabels(m blob.Meta) []KV {
	return []KV{{"blob", shortRef(m.Digest)}, {"kind", blobKind(m)}}
}

// partDetails describes the fixed entries of a blob root.
var partDetails = map[string]string{
	blob.MetaFile:       "blob metadata",
	blob.CompFile:       "zrecipe compression parameters",
	tarprism.IndexFile:  "tar-prism index: where each blob splices into the recipe",
	tarprism.RecipeFile: "tar recipe: every byte that is not file content",
	blob.RawFile:        "the blob's bytes, verbatim",
}

// blobRootNode is a blob root: the parts of a prism, or meta.json and the
// verbatim bytes of a raw blob.
type blobRootNode struct {
	st *store.Store
	bl *blob.Blob
}

func (n *blobRootNode) Crumb() string { return shortRef(n.bl.Meta.Digest) }

func (n *blobRootNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.bl.Root())
	if err != nil {
		return nil, fmt.Errorf("browse: listing blob root %s: %w", n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Name == tarprism.BlobsDir && e.IsDir():
			rows = append(rows, Row{
				Name: e.Name, Detail: plural(n.bl.Meta.Entries, "file"), IsDir: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &prismBlobsNode{st: n.st, bl: n.bl, dir: e.Content},
			})
		case e.IsRegular():
			rows = append(rows, Row{
				Name: e.Name, Detail: partDetails[e.Name], Size: e.Size, HasSize: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.st, name: e.Name, key: e.Content, labels: labels},
			})
		default:
			r := entryRow(e)
			if e.IsDir() {
				r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: labels}
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// prismBlobsNode is the blobs/ directory of a prism: one numbered file per
// regular file of the layer's tar, annotated with the tar entry's name
// from recipe.json.
type prismBlobsNode struct {
	st  *store.Store
	bl  *blob.Blob
	dir key.Key
}

func (n *prismBlobsNode) Crumb() string { return tarprism.BlobsDir }

func (n *prismBlobsNode) List() ([]Row, error) {
	prism, err := n.bl.Prism()
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	idx, err := prism.Index()
	if err != nil {
		return nil, fmt.Errorf("browse: reading %s of %s: %w", tarprism.IndexFile, n.bl.Meta.Digest, err)
	}
	byBlob := make(map[string]tarprism.Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		if name, ok := strings.CutPrefix(e.Blob, tarprism.BlobsDir+"/"); ok {
			byBlob[name] = e
		}
	}
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s of %s: %w", tarprism.BlobsDir, n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		if !e.IsRegular() {
			rows = append(rows, entryRow(e))
			continue
		}
		r := Row{Name: e.Name, Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content)}
		fileLabels := labels
		if te, ok := byBlob[e.Name]; ok {
			r.Detail = te.Name
			r.Info = append(r.Info, KV{"tar entry", te.Name}, KV{"recipe offset", tui.FormatCount(te.Offset)})
			fileLabels = append([]KV{{"file", te.Name}}, labels...)
		}
		r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: fileLabels}
		rows = append(rows, r)
	}
	return rows, nil
}
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./browse`
Expected: PASS. If `TestBlobRootListsPrismParts` reports a different file count than 5, tar-prism counted zero-length files differently than assumed; fix the expectation to what `Meta.Entries` says only after confirming the row detail comes from `Meta.Entries`.

- [ ] **Step 8: Commit**

```bash
git add browse/
git commit -m "browse: nodes over blob roots, prism blobs and raw directories"
```

---

### Task 5: Image roots, repositories and the Browser entry points

**Files:**
- Modify: `browse/storage.go`, `browse/browser.go`
- Create: `browse/repos.go`
- Test: `browse/image_test.go`

**Interfaces:**
- Consumes: Task 4's nodes and helpers; `image.Store.Manifests` (Task 3).
- Produces:
  - `*imageRootNode{b, repo, crumb, im}` with `manifest() (*oci.Manifest, error)`; `*imageBlobsNode{b, im, m, dir}`; `*imageManifestsNode{b, repo, m}`
  - `*reposNode{b}`, `*repoNode{b, repo, tags, manifests}`
  - `func (b *Browser) rootNode() *reposNode`, `func (b *Browser) repoNode(repo string) (*repoNode, error)`, `func (b *Browser) imageNode(repo, reference string) (*imageRootNode, error)`
  - `func childRow(b *Browser, repo string, d oci.Descriptor, child func(*image.Image) Node) Row` (the filesystem chooser in Task 6 reuses it)
  - `func rootfsDetail(*image.Rootfs) string`, `func isAttestation(oci.Descriptor) bool`, `func blobRow(*store.Store, *blob.Blob, string) Row`

- [ ] **Step 1: Write the failing tests**

Create `browse/image_test.go`:

```go
package browse

import (
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

func TestReposListsEveryRepository(t *testing.T) {
	f := newFixture(t)
	rows := mustList(t, f.b.rootNode())
	assertNames(t, rows, "library/app", "library/app/sub", "tools/rawimg")
	if rows[0].Detail != "2 tags · 4 manifests" {
		t.Errorf("library/app detail %q", rows[0].Detail)
	}
	if rows[1].Detail != "1 tag · 1 manifest" {
		t.Errorf("library/app/sub detail %q", rows[1].Detail)
	}
	if _, ok := rows[0].Child.(*repoNode); !ok || !rows[0].IsDir {
		t.Errorf("repository row = %+v", rows[0])
	}
}

func TestRepoListsTagsThenUntagged(t *testing.T) {
	f := newFixture(t)
	rows := childList(t, mustList(t, f.b.rootNode()), "library/app")
	untagged := []oci.Digest{f.m2, f.m3}
	slices.Sort(untagged)
	assertNames(t, rows, "latest", "v1", shortRef(untagged[0]), shortRef(untagged[1]))

	latest := rows[0]
	if latest.Detail != "index · "+shortRef(f.idx) || !latest.IsDir || !latest.HasSize {
		t.Errorf("latest row = %+v", latest)
	}
	if rows[1].Detail != "manifest · "+shortRef(f.m1) {
		t.Errorf("v1 detail %q", rows[1].Detail)
	}
	if rows[2].Detail != "untagged · manifest" {
		t.Errorf("untagged detail %q", rows[2].Detail)
	}
	root, ok := rows[1].Child.(*imageRootNode)
	if !ok || root.crumb != ":v1" || root.repo != "library/app" {
		t.Fatalf("v1 Child = %#v", rows[1].Child)
	}
	if u := rows[2].Child.(*imageRootNode); u.crumb != "@"+shortRef(untagged[0]) {
		t.Errorf("untagged crumb %q", u.crumb)
	}

	raw := childList(t, mustList(t, f.b.rootNode()), "tools/rawimg")
	if raw[0].Detail != "manifest · "+shortRef(f.rawM)+" · rootfs unavailable" {
		t.Errorf("rawimg detail %q", raw[0].Detail)
	}
}

func TestImageRootRows(t *testing.T) {
	f := newFixture(t)
	root, err := f.b.imageNode("library/app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	rows := mustList(t, root)
	assertNames(t, rows, "blobs", "manifest", "meta.json", "rootfs")
	if rows[0].Detail != "3 blobs" || !rows[0].IsDir {
		t.Errorf("blobs row = %+v", rows[0])
	}
	if rows[1].Detail != oci.MediaTypeOCIManifest || !rows[1].HasSize {
		t.Errorf("manifest row = %+v", rows[1])
	}
	if !strings.HasPrefix(rows[3].Detail, "ok · ") || !strings.HasSuffix(rows[3].Detail, " entries") {
		t.Errorf("rootfs detail %q", rows[3].Detail)
	}
	if _, ok := rows[3].Child.(*amberDirNode); !ok {
		t.Errorf("rootfs Child is %T", rows[3].Child)
	}
	if got := string(readAll(t, rows[1])); !strings.Contains(got, `"schemaVersion":2`) {
		t.Errorf("manifest bytes: %s", got)
	}

	idx, err := f.b.imageNode("library/app", "latest")
	if err != nil {
		t.Fatal(err)
	}
	rows = mustList(t, idx)
	assertNames(t, rows, "blobs", "manifest", "manifests", "meta.json")
	if rows[0].Detail != "0 blobs" || rows[2].Detail != "2 child manifests" {
		t.Errorf("index rows: %q, %q", rows[0].Detail, rows[2].Detail)
	}

	if _, err := f.b.imageNode("library/app", "nope"); err == nil {
		t.Error("unknown tag must fail")
	}
	byDigest, err := f.b.imageNode("library/app", f.m1.String())
	if err != nil || byDigest.crumb != "@"+shortRef(f.m1) {
		t.Errorf("by digest: %v, crumb %q", err, byDigest.crumb)
	}
}

func TestImageBlobsInManifestOrder(t *testing.T) {
	f := newFixture(t)
	root, _ := f.b.imageNode("library/app", "v1")
	rows := childList(t, mustList(t, root), "blobs")
	assertNames(t, rows, shortRef(f.conf), shortRef(f.layerA), shortRef(f.layerB))
	if rows[0].Detail != "config · raw not-tar" {
		t.Errorf("config detail %q", rows[0].Detail)
	}
	if !strings.HasPrefix(rows[1].Detail, "layer 1/2 · prism gzip go-flate · 5 files · ") || !strings.HasSuffix(rows[1].Detail, " uncompressed") {
		t.Errorf("layer A detail %q", rows[1].Detail)
	}
	if !strings.HasPrefix(rows[2].Detail, "layer 2/2 · prism none · ") {
		t.Errorf("layer B detail %q", rows[2].Detail)
	}
	if rows[1].Size != f.sizes[f.layerA] || !rows[1].IsDir {
		t.Errorf("layer A row = %+v", rows[1])
	}
	if _, ok := rows[1].Child.(*blobRootNode); !ok {
		t.Errorf("layer A Child is %T", rows[1].Child)
	}
	if rows[1].Info[0] != (KV{"digest", f.layerA.String()}) {
		t.Errorf("info %v", rows[1].Info)
	}
}

func TestImageManifestsShowPlatforms(t *testing.T) {
	f := newFixture(t)
	idx, _ := f.b.imageNode("library/app", "latest")
	rows := childList(t, mustList(t, idx), "manifests")
	assertNames(t, rows, shortRef(f.m1), shortRef(f.m2))
	if rows[0].Detail != "linux/amd64 · manifest" || rows[1].Detail != "linux/arm64 · manifest" {
		t.Errorf("details %q, %q", rows[0].Detail, rows[1].Detail)
	}
	child, ok := rows[1].Child.(*imageRootNode)
	if !ok || child.crumb != "@"+shortRef(f.m2) {
		t.Fatalf("child = %#v", rows[1].Child)
	}
	assertNames(t, mustList(t, child), "blobs", "manifest", "meta.json", "rootfs")
}

func TestRepoNodeFromNames(t *testing.T) {
	f := newFixture(t)
	rn, err := f.b.repoNode("library/app/sub")
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, mustList(t, rn), "x")
	if _, err := f.b.repoNode("nobody/here"); err == nil {
		t.Error("unknown repository must fail")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./browse`
Expected: compile errors (`f.b.rootNode undefined`, ...).

- [ ] **Step 3: Add the image-level nodes to `browse/storage.go`**

Add `"bytes"`, `"context"`, `"sort"` and `"github.com/amber-store/oci-amber/image"`, `"github.com/amber-store/oci-amber/oci"` to the imports, then append:

```go
// imageRootNode is an image root: the manifest, its meta.json, the blobs
// and child manifests it references, and the rootfs tree when it has one.
// crumb is ":tag" or "@sha256:…" as the image was reached.
type imageRootNode struct {
	b     *Browser
	repo  string
	crumb string
	im    *image.Image
}

func (n *imageRootNode) Crumb() string { return n.crumb }

// manifest parses the stored manifest bytes.
func (n *imageRootNode) manifest() (*oci.Manifest, error) {
	var buf bytes.Buffer
	if err := n.im.WriteTo(context.Background(), &buf); err != nil {
		return nil, fmt.Errorf("browse: reading manifest %s: %w", n.im.Meta.Digest, err)
	}
	m, err := oci.ParseManifest(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("browse: manifest %s: %w", n.im.Meta.Digest, err)
	}
	return m, nil
}

// rootfsDetail summarizes a meta.json rootfs field: "ok · 1,204 entries",
// "partial · 1,200 entries · 4 skipped", "unavailable".
func rootfsDetail(r *image.Rootfs) string {
	if r == nil {
		return ""
	}
	s := string(r.Status)
	if r.Status == image.RootfsOK || r.Status == image.RootfsPartial {
		s += " · " + tui.FormatCount(int64(r.Entries)) + " entries"
	}
	if r.SkippedCount > 0 {
		s += fmt.Sprintf(" · %d skipped", r.SkippedCount)
	}
	return s
}

func (n *imageRootNode) List() ([]Row, error) {
	m, err := n.manifest()
	if err != nil {
		return nil, err
	}
	entries, err := listDir(n.b.st, n.im.Root())
	if err != nil {
		return nil, fmt.Errorf("browse: listing image root %s: %w", n.im.Meta.Digest, err)
	}
	meta := n.im.Meta
	labels := []KV{{"image", n.repo + " " + shortRef(meta.Digest)}}
	uniqueBlobs := map[oci.Digest]bool{}
	for _, d := range m.BlobDescriptors() {
		uniqueBlobs[d.Digest] = true
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Name == image.ManifestFile && e.IsRegular():
			rows = append(rows, Row{Name: e.Name, Detail: meta.MediaType, Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}})
		case e.Name == image.MetaFile && e.IsRegular():
			rows = append(rows, Row{Name: e.Name, Detail: "kind, digest, stats and rootfs status", Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}})
		case e.Name == image.BlobsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: plural(len(uniqueBlobs), "blob"), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &imageBlobsNode{b: n.b, im: n.im, m: m, dir: e.Content}})
		case e.Name == image.ManifestsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: plural(len(m.Manifests), "child manifest"), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &imageManifestsNode{b: n.b, repo: n.repo, m: m}})
		case e.Name == image.RootfsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: rootfsDetail(meta.Rootfs), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &amberDirNode{st: n.b.st, name: e.Name, dir: e.Content, labels: labels}})
		default:
			r := entryRow(e)
			switch e.Type() {
			case store.TypeDir:
				r.Child = &amberDirNode{st: n.b.st, name: e.Name, dir: e.Content, labels: labels}
			case store.TypeReg:
				r.Child = &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// blobRow is a blob's row under an image: its role in the manifest, how it
// is stored and its sizes. It opens the blob root.
func blobRow(st *store.Store, bl *blob.Blob, role string) Row {
	m := bl.Meta
	var parts []string
	if role != "" {
		parts = append(parts, role)
	}
	parts = append(parts, blobKind(m))
	if m.Kind == blob.KindPrism {
		parts = append(parts, plural(m.Entries, "file"))
		if m.UncompressedSize > 0 {
			parts = append(parts, tui.FormatBytes(m.UncompressedSize)+" uncompressed")
		}
	}
	info := []KV{{"digest", m.Digest.String()}, {"kind", string(m.Kind)}, {"format", m.Format}}
	if m.Engine != "" {
		info = append(info, KV{"engine", strings.TrimSpace(m.Engine + " " + m.EngineVersion)})
	}
	if m.RawReason != "" {
		info = append(info, KV{"raw reason", string(m.RawReason)})
	}
	info = append(info, KV{"size", tui.FormatCount(m.Size) + " bytes"})
	if m.UncompressedSize > 0 {
		info = append(info, KV{"uncompressed", tui.FormatCount(m.UncompressedSize) + " bytes"})
	}
	if m.DiffID != "" {
		info = append(info, KV{"diff id", m.DiffID.String()})
	}
	if m.Kind == blob.KindPrism {
		info = append(info, KV{"files", tui.FormatCount(int64(m.Entries))})
	}
	info = append(info, KV{"uploaded", m.UploadedAt.UTC().Format(time.RFC3339)}, KV{"root key", bl.Root().String()})
	return Row{Name: shortRef(m.Digest), Detail: strings.Join(parts, " · "), Size: m.Size, HasSize: true, IsDir: true, Info: info,
		Child: &blobRootNode{st: st, bl: bl}}
}

// imageBlobsNode is the blobs/ directory of an image root: the config and
// layers in manifest order, each annotated with how it is stored, then any
// entry the manifest does not name.
type imageBlobsNode struct {
	b   *Browser
	im  *image.Image
	m   *oci.Manifest
	dir key.Key
}

func (n *imageBlobsNode) Crumb() string { return image.BlobsDir }

func (n *imageBlobsNode) List() ([]Row, error) {
	entries, err := listDir(n.b.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s of %s: %w", image.BlobsDir, n.im.Meta.Digest, err)
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Name] = true
	}
	type slot struct {
		digest oci.Digest
		roles  []string
	}
	var order []*slot
	byDigest := map[oci.Digest]*slot{}
	add := func(d oci.Digest, role string) {
		s := byDigest[d]
		if s == nil {
			s = &slot{digest: d}
			byDigest[d] = s
			order = append(order, s)
		}
		s.roles = append(s.roles, role)
	}
	if n.m.Config != nil {
		add(n.m.Config.Digest, "config")
	}
	for i, l := range n.m.Layers {
		add(l.Digest, fmt.Sprintf("layer %d/%d", i+1, len(n.m.Layers)))
	}
	rows := make([]Row, 0, len(entries))
	for _, s := range order {
		role := strings.Join(s.roles, ", ")
		if !present[s.digest.String()] {
			rows = append(rows, Row{Name: shortRef(s.digest), Detail: role + " · missing from " + image.BlobsDir + "/", Info: []KV{{"digest", s.digest.String()}}})
			continue
		}
		delete(present, s.digest.String())
		rows = append(rows, n.blobRow(s.digest, role))
	}
	for _, e := range entries { // whatever the manifest did not name, in name order
		if !present[e.Name] {
			continue
		}
		d, err := oci.ParseDigest(e.Name)
		if err != nil {
			rows = append(rows, entryRow(e))
			continue
		}
		rows = append(rows, n.blobRow(d, ""))
	}
	return rows, nil
}

// blobRow opens d and renders it; a blob that cannot be opened gets a row
// that says so and opens nothing.
func (n *imageBlobsNode) blobRow(d oci.Digest, role string) Row {
	bl, err := n.b.blobs.Open(d)
	if err != nil {
		detail := "unreadable: " + err.Error()
		if role != "" {
			detail = role + " · " + detail
		}
		return Row{Name: shortRef(d), Detail: detail, Info: []KV{{"digest", d.String()}}}
	}
	return blobRow(n.b.st, bl, role)
}

// isAttestation reports a BuildKit attestation manifest, marked by the
// annotation on the descriptor that references it.
func isAttestation(d oci.Descriptor) bool {
	return d.Annotations["vnd.docker.reference.type"] == "attestation-manifest"
}

// childRow is an index child's row: platform, attestation mark, kind and
// rootfs status. child builds what Enter opens from the opened image, so
// the storage view and the filesystem chooser share this.
func childRow(b *Browser, repo string, d oci.Descriptor, child func(*image.Image) Node) Row {
	var parts []string
	if d.Platform != nil {
		parts = append(parts, d.Platform.String())
	}
	if isAttestation(d) {
		parts = append(parts, "attestation")
	}
	info := []KV{{"digest", d.Digest.String()}, {"media type", d.MediaType}, {"size", tui.FormatCount(d.Size) + " bytes"}}
	if d.Platform != nil {
		info = append(info, KV{"platform", d.Platform.String()})
	}
	keys := make([]string, 0, len(d.Annotations))
	for k := range d.Annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		info = append(info, KV{k, d.Annotations[k]})
	}
	im, err := b.images.Open(repo, d.Digest.String())
	if err != nil {
		parts = append(parts, "missing: "+err.Error())
		return Row{Name: shortRef(d.Digest), Detail: strings.Join(parts, " · "), Size: d.Size, HasSize: true, Info: info}
	}
	parts = append(parts, string(im.Meta.Kind))
	if rf := im.Meta.Rootfs; rf != nil && rf.Status != image.RootfsOK && rf.Status != image.RootfsNotApplicable {
		parts = append(parts, "rootfs "+string(rf.Status))
	}
	return Row{Name: shortRef(d.Digest), Detail: strings.Join(parts, " · "), Size: d.Size, HasSize: true, IsDir: true, Info: info, Child: child(im)}
}

// imageManifestsNode is the manifests/ directory of an index root: its
// children in index order.
type imageManifestsNode struct {
	b    *Browser
	repo string
	m    *oci.Manifest
}

func (n *imageManifestsNode) Crumb() string { return image.ManifestsDir }

func (n *imageManifestsNode) List() ([]Row, error) {
	rows := make([]Row, 0, len(n.m.Manifests))
	for _, d := range n.m.Manifests {
		crumb := "@" + shortRef(d.Digest)
		rows = append(rows, childRow(n.b, n.repo, d, func(im *image.Image) Node {
			return &imageRootNode{b: n.b, repo: n.repo, crumb: crumb, im: im}
		}))
	}
	return rows, nil
}
```

- [ ] **Step 4: Write `browse/repos.go`**

```go
package browse

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/tui"
)

// reposNode is the entry screen: every repository with a tag or a manifest.
type reposNode struct{ b *Browser }

func (n *reposNode) Crumb() string { return "oci-amber" }

// repoRefs are one repository's tags and manifest digests, sorted.
type repoRefs struct {
	tags      []string
	manifests []oci.Digest
}

// catalog scans the tag and manifest namespaces once and groups them by
// repository; names come back in bytewise order.
func (b *Browser) catalog() ([]string, map[string]*repoRefs, error) {
	repos := map[string]*repoRefs{}
	get := func(repo string) *repoRefs {
		r := repos[repo]
		if r == nil {
			r = &repoRefs{}
			repos[repo] = r
		}
		return r
	}
	tagRefs, err := b.st.ListRefs(image.TagPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("browse: %w", err)
	}
	for _, r := range tagRefs {
		if repo, tag, ok := image.ParseTagRef(r.Name); ok {
			rr := get(repo)
			rr.tags = append(rr.tags, tag)
		}
	}
	manifestRefs, err := b.st.ListRefs(image.ManifestPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("browse: %w", err)
	}
	for _, r := range manifestRefs {
		if repo, d, ok := image.ParseManifestRef(r.Name); ok {
			rr := get(repo)
			rr.manifests = append(rr.manifests, d)
		}
	}
	for _, rr := range repos {
		slices.Sort(rr.tags)
		slices.Sort(rr.manifests)
	}
	return slices.Sorted(maps.Keys(repos)), repos, nil
}

func (n *reposNode) List() ([]Row, error) {
	names, repos, err := n.b.catalog()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(names))
	for _, name := range names {
		rr := repos[name]
		rows = append(rows, Row{
			Name:   name,
			Detail: plural(len(rr.tags), "tag") + " · " + plural(len(rr.manifests), "manifest"),
			IsDir:  true,
			Info: []KV{
				{"repository", name},
				{"tags", tui.FormatCount(int64(len(rr.tags)))},
				{"manifests", tui.FormatCount(int64(len(rr.manifests)))},
			},
			Child: &repoNode{b: n.b, repo: name, tags: rr.tags, manifests: rr.manifests},
		})
	}
	return rows, nil
}

// repoNode lists one repository: its tags, then the manifests no tag
// points at, both opened through image.Store.Open for their meta.json.
type repoNode struct {
	b         *Browser
	repo      string
	tags      []string
	manifests []oci.Digest
}

func (n *repoNode) Crumb() string { return n.repo }

func (n *repoNode) List() ([]Row, error) {
	tagged := make(map[oci.Digest]bool, len(n.tags))
	rows := make([]Row, 0, len(n.tags)+len(n.manifests))
	for _, tag := range n.tags {
		im, err := n.b.images.Open(n.repo, tag)
		if err != nil {
			return nil, fmt.Errorf("browse: opening %s:%s: %w", n.repo, tag, err)
		}
		tagged[im.Meta.Digest] = true
		rows = append(rows, imageRow(n.b, n.repo, tag, im))
	}
	for _, d := range n.manifests {
		if tagged[d] {
			continue
		}
		im, err := n.b.images.Open(n.repo, d.String())
		if err != nil {
			return nil, fmt.Errorf("browse: opening %s@%s: %w", n.repo, d, err)
		}
		rows = append(rows, imageRow(n.b, n.repo, d.String(), im))
	}
	return rows, nil
}

// imageRow is an image's row in a repository listing; reference is a tag
// or a digest string.
func imageRow(b *Browser, repo, reference string, im *image.Image) Row {
	m := im.Meta
	name, crumb := reference, ":"+reference
	var parts []string
	if oci.IsDigest(reference) {
		name, crumb = shortRef(m.Digest), "@"+shortRef(m.Digest)
		parts = append(parts, "untagged", string(m.Kind))
	} else {
		parts = append(parts, string(m.Kind), shortRef(m.Digest))
	}
	if rf := m.Rootfs; rf != nil && rf.Status != image.RootfsOK && rf.Status != image.RootfsNotApplicable {
		parts = append(parts, "rootfs "+string(rf.Status))
	}
	info := []KV{
		{"repository", repo},
		{"reference", reference},
		{"digest", m.Digest.String()},
		{"kind", string(m.Kind)},
		{"media type", m.MediaType},
		{"size", tui.FormatCount(m.Stats.TotalBytes) + " bytes"},
		{"created", m.CreatedAt.UTC().Format(time.RFC3339)},
	}
	if m.Rootfs != nil {
		info = append(info, KV{"rootfs", rootfsDetail(m.Rootfs)})
		if m.Rootfs.Reason != "" {
			info = append(info, KV{"rootfs reason", m.Rootfs.Reason})
		}
	}
	info = append(info, KV{"root key", im.Root().String()})
	return Row{Name: name, Detail: strings.Join(parts, " · "), Size: m.Stats.TotalBytes, HasSize: true, IsDir: true, Info: info,
		Child: &imageRootNode{b: b, repo: repo, crumb: crumb, im: im}}
}
```

- [ ] **Step 5: Add the entry points to `browse/browser.go`**

Add `"errors"` and `"github.com/amber-store/oci-amber/image"` (already imported) as needed, then append:

```go
// rootNode is the repository listing.
func (b *Browser) rootNode() *reposNode { return &reposNode{b: b} }

// repoNode is one repository's listing, or image.ErrNotFound.
func (b *Browser) repoNode(repo string) (*repoNode, error) {
	tags, err := b.images.Tags(repo)
	if err != nil {
		return nil, err
	}
	manifests, err := b.images.Manifests(repo)
	if err != nil && !errors.Is(err, image.ErrNotFound) {
		return nil, err
	}
	return &repoNode{b: b, repo: repo, tags: tags, manifests: manifests}, nil
}

// imageNode is the storage root of repo's reference, a tag or a digest,
// or image.ErrNotFound.
func (b *Browser) imageNode(repo, reference string) (*imageRootNode, error) {
	im, err := b.images.Open(repo, reference)
	if err != nil {
		return nil, err
	}
	crumb := ":" + reference
	if oci.IsDigest(reference) {
		crumb = "@" + shortRef(im.Meta.Digest)
	}
	return &imageRootNode{b: b, repo: repo, crumb: crumb, im: im}, nil
}
```

- [ ] **Step 6: Run the tests**

Run: `go test -race ./browse`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add browse/
git commit -m "browse: repositories, image roots, blob and child manifest listings"
```

---

### Task 6: The filesystem view

**Files:**
- Create: `browse/fs.go`
- Test: `browse/fs_test.go`

**Interfaces:**
- Consumes: `entryRow`, `entryLabels`, `childRow`, `shortRef` (Tasks 4–5); `rootfs.FS`.
- Produces: `func fsRootFor(b *Browser, repo string, im *image.Image) Lister`; nodes `*fsDirNode{st, fs, path, crumb, labels}`, `*fsFileNode`, `*fsLinkNode` (a `Resolver`), `*fsChooserNode{b, repo, im}`, `*fsUnavailableNode{crumb, rf}`.

- [ ] **Step 1: Write the failing tests**

Create `browse/fs_test.go`:

```go
package browse

import (
	"strings"
	"testing"
)

func TestFSRootFollowsSymlinks(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	root := fsRootFor(f.b, "library/app", im)
	if _, ok := root.(*fsDirNode); !ok {
		t.Fatalf("root is %T, want *fsDirNode", root)
	}
	rows := mustList(t, root)
	assertNames(t, rows, "bin", "etc", "usr")

	bin := childList(t, rows, "bin")
	assertNames(t, bin, "app", "sbin")
	sbin := rowNamed(t, bin, "sbin")
	if sbin.Meta == nil || sbin.Meta.Target != "../usr/bin" {
		t.Fatalf("sbin row = %+v", sbin)
	}
	r, ok := sbin.Child.(Resolver)
	if !ok {
		t.Fatalf("sbin Child is %T, want a Resolver", sbin.Child)
	}
	resolved, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := resolved.(*fsDirNode)
	if !ok || dir.Crumb() != "sbin" {
		t.Fatalf("resolved = %#v", resolved)
	}
	assertNames(t, mustList(t, dir), "tool.sh")

	etc := childList(t, rows, "etc")
	link := rowNamed(t, etc, "link-to-os")
	target, err := link.Child.(Resolver).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.(Opener); !ok {
		t.Fatalf("link-to-os resolves to %T, want an Opener", target)
	}
	if got := string(readAll(t, Row{Name: "link-to-os", Child: target})); !strings.HasPrefix(got, "PRETTY_NAME=") {
		t.Errorf("content through symlink: %q", got)
	}
	abs, err := rowNamed(t, etc, "abs").Child.(Resolver).Resolve()
	if err != nil {
		t.Fatalf("absolute symlink: %v", err)
	}
	if _, ok := abs.(Opener); !ok {
		t.Errorf("abs resolves to %T", abs)
	}
	if _, err := rowNamed(t, etc, "dangling").Child.(Resolver).Resolve(); err == nil || !strings.Contains(err.Error(), "no such path") {
		t.Errorf("dangling symlink: %v", err)
	}

	os := rowNamed(t, etc, "os-release")
	file, err := os.Child.(Opener).Open()
	if err != nil {
		t.Fatal(err)
	}
	if file.Labels[0] != (KV{"mode", "0644"}) || file.Labels[1] != (KV{"owner", "0:0"}) || file.Labels[2].Key != "image" {
		t.Errorf("labels %v", file.Labels)
	}
}

func TestFSRootForIndexIsChooser(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "latest")
	root := fsRootFor(f.b, "library/app", im)
	if _, ok := root.(*fsChooserNode); !ok {
		t.Fatalf("root is %T, want *fsChooserNode", root)
	}
	rows := mustList(t, root)
	assertNames(t, rows, shortRef(f.m1), shortRef(f.m2))
	if rows[1].Detail != "linux/arm64 · manifest" {
		t.Errorf("detail %q", rows[1].Detail)
	}
	arm, ok := rows[1].Child.(*fsDirNode)
	if !ok || arm.Crumb() != "linux/arm64" {
		t.Fatalf("arm64 Child = %#v", rows[1].Child)
	}
	etc := childList(t, mustList(t, arm), "etc")
	rowNamed(t, etc, "arch")
}

func TestFSRootForRawImageExplains(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("tools/rawimg", "r1")
	root := fsRootFor(f.b, "tools/rawimg", im)
	if _, ok := root.(*fsUnavailableNode); !ok {
		t.Fatalf("root is %T, want *fsUnavailableNode", root)
	}
	rows := mustList(t, root)
	if len(rows) != 1 || rows[0].Name != "rootfs unavailable" || !strings.Contains(rows[0].Detail, "stored raw") || rows[0].Child != nil {
		t.Errorf("rows = %+v", rows)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./browse`
Expected: compile error `undefined: fsRootFor`.

- [ ] **Step 3: Write `browse/fs.go`**

```go
package browse

import (
	"fmt"
	"path"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
)

// fsRootFor returns the filesystem view's root for an image: the rootfs
// root when it has a tree, a platform chooser for an index, otherwise a
// node that explains why there is none.
func fsRootFor(b *Browser, repo string, im *image.Image) Lister {
	if fs, ok := im.FS(); ok {
		return &fsDirNode{st: b.st, fs: fs, labels: []KV{{"image", repo + " " + shortRef(im.Meta.Digest)}}}
	}
	if im.Meta.Kind == image.KindIndex {
		return &fsChooserNode{b: b, repo: repo, im: im}
	}
	return &fsUnavailableNode{rf: im.Meta.Rootfs}
}

// fsDirNode is a directory of the filesystem view. Paths resolve through
// rootfs.FS, so symlinks are followed the way the /fs/ API follows them.
// path is cleaned and "" is the root.
type fsDirNode struct {
	st     *store.Store
	fs     *rootfs.FS
	path   string
	crumb  string
	labels []KV
}

func (n *fsDirNode) Crumb() string { return n.crumb }

func (n *fsDirNode) List() ([]Row, error) {
	entries, _, err := n.fs.List(n.path, "", 0)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		r := entryRow(e)
		p := path.Join(n.path, e.Name)
		switch e.Type() {
		case store.TypeDir:
			r.Child = &fsDirNode{st: n.st, fs: n.fs, path: p, crumb: e.Name, labels: n.labels}
		case store.TypeReg:
			r.Child = &fsFileNode{st: n.st, fs: n.fs, path: p, name: e.Name, labels: n.labels}
		case store.TypeLink:
			r.Child = &fsLinkNode{st: n.st, fs: n.fs, path: p, name: e.Name, target: e.Target, labels: n.labels}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// fsFileNode is a regular file of the filesystem view, opened by path so a
// file reached through a symlink resolves the same way a listing did.
type fsFileNode struct {
	st         *store.Store
	fs         *rootfs.FS
	path, name string
	labels     []KV
}

func (n *fsFileNode) Crumb() string { return n.name }

func (n *fsFileNode) Open() (*File, error) {
	e, err := n.fs.Stat(n.path)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	if !e.IsRegular() {
		return nil, fmt.Errorf("browse: %s is a %s, not a regular file", n.path, e.TypeName())
	}
	st, k := n.st, e.Content
	return &File{Name: n.name, Size: e.Size, Key: k, Labels: append(entryLabels(e), n.labels...),
		Open: func() *store.Reader { return st.NewReader(k) }}, nil
}

// fsLinkNode is a symlink of the filesystem view; Resolve follows it to a
// directory (listed under the link's own name, like `cd link`) or a file.
type fsLinkNode struct {
	st                 *store.Store
	fs                 *rootfs.FS
	path, name, target string
	labels             []KV
}

func (n *fsLinkNode) Crumb() string { return n.name }

func (n *fsLinkNode) Resolve() (Node, error) {
	e, err := n.fs.Stat(n.path)
	if err != nil {
		return nil, fmt.Errorf("browse: %s -> %s: %w", n.name, n.target, err)
	}
	switch e.Type() {
	case store.TypeDir:
		return &fsDirNode{st: n.st, fs: n.fs, path: n.path, crumb: n.name, labels: n.labels}, nil
	case store.TypeReg:
		return &fsFileNode{st: n.st, fs: n.fs, path: n.path, name: n.name, labels: n.labels}, nil
	}
	return nil, fmt.Errorf("browse: %s -> %s is a %s", n.name, n.target, e.TypeName())
}

// fsChooserNode lists an index's children so one platform's filesystem
// can be picked; the child's crumb is its platform.
type fsChooserNode struct {
	b    *Browser
	repo string
	im   *image.Image
}

func (n *fsChooserNode) Crumb() string { return "" }

func (n *fsChooserNode) List() ([]Row, error) {
	m, err := (&imageRootNode{b: n.b, repo: n.repo, im: n.im}).manifest()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(m.Manifests))
	for _, d := range m.Manifests {
		crumb := shortRef(d.Digest)
		if d.Platform != nil {
			crumb = d.Platform.String()
		}
		rows = append(rows, childRow(n.b, n.repo, d, func(im *image.Image) Node {
			switch root := fsRootFor(n.b, n.repo, im).(type) {
			case *fsDirNode:
				root.crumb = crumb
				return root
			case *fsUnavailableNode:
				root.crumb = crumb
				return root
			default:
				return root
			}
		}))
	}
	return rows, nil
}

// fsUnavailableNode stands in for a filesystem the image does not have:
// one row with the status and reason from meta.json.
type fsUnavailableNode struct {
	crumb string
	rf    *image.Rootfs
}

func (n *fsUnavailableNode) Crumb() string { return n.crumb }

func (n *fsUnavailableNode) List() ([]Row, error) {
	status, reason := "no rootfs", "this image root was stored before rootfs views existed"
	if n.rf != nil {
		status = "rootfs " + string(n.rf.Status)
		reason = n.rf.Reason
		if n.rf.Status == image.RootfsNotApplicable {
			reason = "the manifest does not describe a container image"
		}
	}
	return []Row{{Name: status, Detail: reason, Info: []KV{{"status", status}, {"reason", reason}}}}, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./browse`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add browse/fs.go browse/fs_test.go
git commit -m "browse: filesystem view over rootfs.FS with symlinks followed"
```

---

### Task 7: Content classification, text lines and the hex dump

Pure functions the viewer is built on: what a file is, how its text
splits into display lines, how a byte window renders as hex, and how a
window is read.

**Files:**
- Create: `browse/content.go`, `browse/text.go`, `browse/hex.go`
- Test: `browse/content_test.go`, `browse/hex_test.go`

**Interfaces:**
- Consumes: `File` (Task 4).
- Produces:
  - `const MaxTextSize = 8 << 20`, `const probeSize = 8 << 10`, `const hexRowBytes = 16`
  - `type Kind int` with `KindText`, `KindBinary`, `KindTooLarge`; `type Classification struct { Kind Kind; Label string }`
  - `func Classify(name string, size int64, probe []byte) Classification`
  - `type Text struct { Label string; Raw, Pretty []byte }`; `func LoadText(label string, data []byte) *Text`
  - `func Lines(data []byte) []string`; `func lineOffset(data []byte, n int) int64`; `func lineAt(data []byte, off int64) int`
  - `func RenderText(lines []string, top, left, height, width int, hits map[int]bool) string`
  - `func RenderHex(offset int64, data []byte, width int) string`; `func readWindow(f *File, start, length int64) ([]byte, error)`
  - string helpers `truncate(s string, width int) string`, `padRight(s string, width int) string`, `cutLeft(s string, n int) string`

- [ ] **Step 1: Write the failing tests**

Create `browse/content_test.go`:

```go
package browse

import (
	"bytes"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		size  int64
		probe []byte
		kind  Kind
		label string
	}{
		{"os-release", 40, []byte("ID=fixture\n"), KindText, "text"},
		{"config.json", 20, []byte(`{"a":1}`), KindText, "json"},
		{"app.yaml", 10, []byte("a: 1\n"), KindText, "yaml"},
		{"run", 20, []byte("#!/bin/sh\necho\n"), KindText, "shell"},
		{"tool.sh", 5, []byte("echo\n"), KindText, "shell"},
		{"README.md", 5, []byte("# hi\n"), KindText, "markdown"},
		{"c.toml", 5, []byte("a=1\n"), KindText, "toml"},
		{"app", 300, []byte("\x7fELF\x02\x01\x01\x00"), KindBinary, "binary"},
		{"latin1.txt", 5, []byte("caf\xe9\n"), KindBinary, "binary"},
		{"big.txt", MaxTextSize + 1, []byte("text"), KindTooLarge, "binary"},
		{"exactly.txt", MaxTextSize, []byte("text"), KindText, "text"},
		{"empty", 0, nil, KindText, "text"},
	}
	for _, tc := range tests {
		got := Classify(tc.name, tc.size, tc.probe)
		if got.Kind != tc.kind || got.Label != tc.label {
			t.Errorf("Classify(%s) = %+v, want kind %d label %q", tc.name, got, tc.kind, tc.label)
		}
	}
}

func TestClassifyToleratesRuneCutByProbe(t *testing.T) {
	// "é" is c3 a9; the probe ends after c3 while the file goes on.
	probe := append(bytes.Repeat([]byte("a"), 10), 0xc3)
	if got := Classify("x.txt", int64(len(probe))+1, probe); got.Kind != KindText {
		t.Errorf("cut rune at a truncated probe's end: %+v, want text", got)
	}
	if got := Classify("x.txt", int64(len(probe)), probe); got.Kind != KindBinary {
		t.Errorf("cut rune at the file's end: %+v, want binary", got)
	}
}

func TestLoadTextPrettyPrintsJSON(t *testing.T) {
	raw := []byte(`{"listen":":8080","workers":4,"tags":["a","b"]}`)
	tx := LoadText("text", raw)
	if tx.Label != "json" || tx.Pretty == nil {
		t.Fatalf("LoadText = %+v", tx)
	}
	want := "{\n  \"listen\": \":8080\",\n  \"workers\": 4,\n  \"tags\": [\n    \"a\",\n    \"b\"\n  ]\n}"
	if string(tx.Pretty) != want {
		t.Errorf("pretty:\n%s\nwant:\n%s", tx.Pretty, want)
	}
	if !bytes.Equal(tx.Raw, raw) {
		t.Error("Raw must be the stored bytes")
	}
	plain := LoadText("text", []byte("not json\n"))
	if plain.Label != "text" || plain.Pretty != nil {
		t.Errorf("plain = %+v", plain)
	}
	if LoadText("text", nil).Pretty != nil {
		t.Error("empty file is not JSON")
	}
}

func TestLines(t *testing.T) {
	got := Lines([]byte("one\ttab\r\nctrl\x01\nbad\xffbyte\nlast"))
	want := []string{"one    tab", "ctrl^A", `bad\xffbyte`, "last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Lines = %q, want %q", got, want)
	}
	if Lines(nil) != nil || len(Lines([]byte("a\n"))) != 1 {
		t.Error("empty file has no lines; a trailing newline adds none")
	}
}

func TestLineOffsets(t *testing.T) {
	data := []byte("ab\ncd\n\nef")
	for n, want := range []int64{0, 3, 6, 7} {
		if got := lineOffset(data, n); got != want {
			t.Errorf("lineOffset(%d) = %d, want %d", n, got, want)
		}
	}
	if got := lineOffset(data, 99); got != int64(len(data)) {
		t.Errorf("lineOffset past the end = %d", got)
	}
	for off, want := range map[int64]int{0: 0, 2: 0, 3: 1, 6: 2, 7: 3, 8: 3, 99: 3} {
		if got := lineAt(data, off); got != want {
			t.Errorf("lineAt(%d) = %d, want %d", off, got, want)
		}
	}
}

func TestRenderText(t *testing.T) {
	lines := []string{"alpha", "beta gamma delta", "third"}
	got := RenderText(lines, 0, 0, 4, 12, map[int]bool{1: true})
	want := "1 alpha\n2 beta gamma\n3 third\n"
	if got != want {
		t.Errorf("RenderText:\n%q\nwant:\n%q", got, want)
	}
	if got := RenderText(lines, 1, 5, 2, 40, nil); got != "2 gamma delta\n3 " {
		t.Errorf("scrolled: %q", got)
	}
	if got := RenderText(nil, 0, 0, 2, 20, nil); got != "\n" {
		t.Errorf("no lines: %q", got)
	}
}

func TestStringHelpers(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := padRight("ab", 4); got != "ab  " {
		t.Errorf("padRight = %q", got)
	}
	if got := cutLeft("héllo", 2); got != "llo" {
		t.Errorf("cutLeft = %q", got)
	}
	if got := cutLeft("ab", 5); got != "" {
		t.Errorf("cutLeft past the end = %q", got)
	}
}
```

Create `browse/hex_test.go`:

```go
package browse

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderHex(t *testing.T) {
	data := append([]byte("\x7fELF\x02\x01\x01\x00"), make([]byte, 8)...)
	data = append(data, '@')
	got := RenderHex(0x20, data, 100)
	want := "00000020  7f 45 4c 46 02 01 01 00  00 00 00 00 00 00 00 00  .ELF............\n" +
		"00000030  40" + strings.Repeat(" ", 48) + "@" // 47 cells of hex padding plus the column gap
	if got != want {
		t.Errorf("RenderHex:\n%q\nwant:\n%q", got, want)
	}
	if RenderHex(0, nil, 80) != "" {
		t.Error("no data renders nothing")
	}
	if got := RenderHex(0, data[:16], 20); len([]rune(strings.Split(got, "\n")[0])) != 20 {
		t.Errorf("rows are cut to the width: %q", got)
	}
}

func TestReadWindow(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	dir := f.lookupKey(bl.Root(), "blobs")
	rows := mustList(t, &prismBlobsNode{st: f.st, bl: bl, dir: dir})
	var app Row
	for _, r := range rows {
		if r.Detail == "bin/app" {
			app = r
		}
	}
	file, err := app.Child.(Opener).Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readWindow(file, 100<<10, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.bigBinary[100<<10:100<<10+4096]) {
		t.Error("window bytes differ")
	}
	tail, err := readWindow(file, file.Size-10, 4096)
	if err != nil || !bytes.Equal(tail, f.bigBinary[len(f.bigBinary)-10:]) {
		t.Errorf("tail window: %v, %d bytes", err, len(tail))
	}
	if past, err := readWindow(file, file.Size, 16); err != nil || len(past) != 0 {
		t.Errorf("window past the end: %v, %d bytes", err, len(past))
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./browse`
Expected: compile errors (`undefined: Classify`, ...).

- [ ] **Step 3: Write `browse/content.go`**

```go
package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// MaxTextSize is the largest file the text mode reads whole; bigger files
// open in hex only.
const MaxTextSize = 8 << 20

// probeSize is how much of a file's head decides between text and hex.
const probeSize = 8 << 10

// Kind is what the viewer shows a file as.
type Kind int

const (
	KindText     Kind = iota // UTF-8 text, shown with line numbers
	KindBinary               // anything else, shown as a hex dump; h switches to text
	KindTooLarge             // over MaxTextSize: hex only
)

// Classification is the viewer's verdict on a file.
type Classification struct {
	Kind  Kind
	Label string // json, yaml, shell, toml, markdown, text or binary
}

// Classify decides how to show a file of size bytes from its name and a
// probe of its first bytes: too large is hex only; a NUL byte or invalid
// UTF-8 is binary; the rest is text, labelled by extension or shebang.
// The JSON label is decided later, by LoadText, once the whole file is
// read. A probe shorter than the file may end inside a multi-byte rune,
// which is not a fault.
func Classify(name string, size int64, probe []byte) Classification {
	if size > MaxTextSize {
		return Classification{Kind: KindTooLarge, Label: "binary"}
	}
	if bytes.IndexByte(probe, 0) >= 0 || !validUTF8Prefix(probe, int64(len(probe)) < size) {
		return Classification{Kind: KindBinary, Label: "binary"}
	}
	return Classification{Kind: KindText, Label: labelFor(name, probe)}
}

// validUTF8Prefix reports whether p is valid UTF-8, ignoring an incomplete
// rune at its end when truncated says more bytes follow it.
func validUTF8Prefix(p []byte, truncated bool) bool {
	if truncated {
		for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
			if utf8.RuneStart(p[i]) {
				if !utf8.FullRune(p[i:]) {
					p = p[:i]
				}
				break
			}
		}
	}
	return utf8.Valid(p)
}

// labelFor names a text file's type from its extension, else its shebang.
func labelFor(name string, head []byte) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".sh", ".bash":
		return "shell"
	case ".toml":
		return "toml"
	case ".md":
		return "markdown"
	}
	if bytes.HasPrefix(head, []byte("#!")) {
		return "shell"
	}
	return "text"
}

// Text is a file loaded for the text mode.
type Text struct {
	Label  string
	Raw    []byte // the stored bytes
	Pretty []byte // indented JSON; nil unless the file is JSON
}

// LoadText wraps data; when json.Valid accepts it the label becomes json
// and Pretty holds it indented by two spaces with key order preserved.
func LoadText(label string, data []byte) *Text {
	t := &Text{Label: label, Raw: data}
	if len(bytes.TrimSpace(data)) > 0 && json.Valid(data) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err == nil {
			t.Label = "json"
			t.Pretty = buf.Bytes()
		}
	}
	return t
}

// Lines splits data into display lines: "\n" separates, a trailing "\r"
// is dropped, a tab becomes four spaces, other control characters render
// as ^X and bytes that are not UTF-8 as \xNN. An empty file has no lines
// and a trailing newline adds none.
func Lines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = sanitize(bytes.TrimSuffix(p, []byte("\r")))
	}
	return lines
}

// sanitize renders one line's bytes as printable text.
func sanitize(p []byte) string {
	var b strings.Builder
	for len(p) > 0 {
		r, n := utf8.DecodeRune(p)
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, p[0])
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
			b.WriteByte('^')
			b.WriteByte(byte(r) ^ 0x40)
		default:
			b.WriteRune(r)
		}
		p = p[n:]
	}
	return b.String()
}

// lineOffset is the byte offset in data where line n (0-based) starts;
// past the last line it is len(data).
func lineOffset(data []byte, n int) int64 {
	off := 0
	for ; n > 0; n-- {
		i := bytes.IndexByte(data[off:], '\n')
		if i < 0 {
			return int64(len(data))
		}
		off += i + 1
	}
	return int64(off)
}

// lineAt is the 0-based line that holds byte offset off; past the end it
// is the last line.
func lineAt(data []byte, off int64) int {
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	return bytes.Count(data[:off], []byte("\n"))
}
```

- [ ] **Step 4: Write `browse/text.go`**

```go
package browse

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleDim = lipgloss.NewStyle().Faint(true)
	styleHit = lipgloss.NewStyle().Reverse(true)
)

// RenderText renders lines[top:top+height], each shifted left by left
// columns and cut to width, behind right-aligned line numbers. Lines whose
// index is in hits are highlighted. Missing lines at the end are blank, so
// the result always has height lines.
func RenderText(lines []string, top, left, height, width int, hits map[int]bool) string {
	numWidth := len(strconv.Itoa(max(len(lines), 1)))
	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		n := top + i
		if n < 0 || n >= len(lines) {
			out = append(out, "")
			continue
		}
		num := styleDim.Render(fmt.Sprintf("%*d", numWidth, n+1))
		body := truncate(cutLeft(lines[n], left), max(width-numWidth-1, 0))
		if hits[n] {
			body = styleHit.Render(body)
		}
		out = append(out, num+" "+body)
	}
	return strings.Join(out, "\n")
}

// truncate cuts s to width cells; ANSI sequences are kept intact.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// padRight pads s with spaces to width cells.
func padRight(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// cutLeft drops the first n runes of s.
func cutLeft(s string, n int) string {
	if n <= 0 {
		return s
	}
	rs := []rune(s)
	if n >= len(rs) {
		return ""
	}
	return string(rs[n:])
}
```

- [ ] **Step 5: Write `browse/hex.go`**

```go
package browse

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// hexRowBytes is how many bytes one hex row shows.
const hexRowBytes = 16

// RenderHex renders data, which starts at offset in the file, as rows of
// 16 bytes: an eight-digit hex offset, two groups of eight hex bytes and
// the printable ASCII with '.' for the rest. A short last row is padded
// so the ASCII column stays aligned. Rows are cut to width.
func RenderHex(offset int64, data []byte, width int) string {
	var rows []string
	for start := 0; start < len(data); start += hexRowBytes {
		row := data[start:min(start+hexRowBytes, len(data))]
		var hex, asc strings.Builder
		for i := 0; i < hexRowBytes; i++ {
			if i == 8 {
				hex.WriteByte(' ')
			}
			if i >= len(row) {
				hex.WriteString("   ")
				continue
			}
			fmt.Fprintf(&hex, "%02x ", row[i])
			if c := row[i]; c >= 0x20 && c < 0x7f {
				asc.WriteByte(c)
			} else {
				asc.WriteByte('.')
			}
		}
		rows = append(rows, truncate(fmt.Sprintf("%08x  %s %s", offset+int64(start), hex.String(), asc.String()), width))
	}
	return strings.Join(rows, "\n")
}

// readWindow reads up to length bytes of f starting at start, fewer at
// the end of the file and none past it. Whole chunks before start are
// skipped by their key lengths alone, so a window deep inside a large
// file fetches about one chunk plus the window.
func readWindow(f *File, start, length int64) ([]byte, error) {
	if start < 0 || start >= f.Size || length <= 0 {
		return nil, nil
	}
	r := f.Open()
	defer r.Close()
	if err := r.Skip(start); err != nil {
		return nil, fmt.Errorf("browse: seeking %s to %d: %w", f.Name, start, err)
	}
	buf := make([]byte, min(length, f.Size-start))
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("browse: reading %s at %d: %w", f.Name, start, err)
	}
	return buf[:n], nil
}
```

- [ ] **Step 6: Run the tests**

Run: `go test -race ./browse`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add browse/content.go browse/text.go browse/hex.go browse/content_test.go browse/hex_test.go
git commit -m "browse: file classification, text lines and the hex dump"
```

---

### Task 8: Rendering listings, the status line and the popup

**Files:**
- Create: `browse/view.go`
- Test: `browse/view_test.go`

**Interfaces:**
- Consumes: `Row`, `RowMeta`, `KV`, `truncate`, `padRight`, `styleDim` (Tasks 4, 7), `tui.FormatBytes`.
- Produces:
  - `type listView struct { Crumbs []string; Rows []Row; Cursor, Count, Total int; Loading bool; Filter, Status, Input, Hints string; Popup []KV }` where `Rows` are only the rows on screen and `Cursor` indexes them (-1 for none)
  - `func RenderList(v listView, width, height int) string`
  - `type viewerView struct { Crumbs []string; Body, Status, Input string; Loading bool }`; `func RenderViewer(v viewerView, width, height int) string`
  - `func modeString(mode uint64) string`, `func breadcrumb(crumbs []string, width int) string`
  - `const chromeLines = 4`

- [ ] **Step 1: Write the failing tests**

Create `browse/view_test.go`:

```go
package browse

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/store"
)

func TestModeString(t *testing.T) {
	for mode, want := range map[uint64]string{
		0o100644: "-rw-r--r--",
		0o040755: "drwxr-xr-x",
		0o120777: "lrwxrwxrwx",
		0o104755: "-rwsr-xr-x",
		0o102755: "-rwxr-sr-x",
		0o041777: "drwxrwxrwt",
		0o100000: "----------",
		0o020660: "crw-rw----",
		0o060660: "brw-rw----",
		0o010600: "prw-------",
		0o140755: "srwxr-xr-x",
	} {
		if got := modeString(mode); got != want {
			t.Errorf("modeString(%o) = %q, want %q", mode, got, want)
		}
	}
}

func TestBreadcrumb(t *testing.T) {
	crumbs := []string{"library/app", ":v1", "storage", "blobs", "sha256:4f7c9a1e"}
	if got := breadcrumb(crumbs, 80); got != "library/app › :v1 › storage › blobs › sha256:4f7c9a1e" {
		t.Errorf("wide: %q", got)
	}
	got := breadcrumb(crumbs, 30)
	if !strings.HasPrefix(got, "… › ") || !strings.HasSuffix(got, "sha256:4f7c9a1e") || len([]rune(got)) > 30 {
		t.Errorf("narrow: %q", got)
	}
	if got := breadcrumb([]string{"oci-amber"}, 5); got != "oci-a" {
		t.Errorf("single crumb wider than the terminal: %q", got)
	}
}

func TestRenderListStorageRows(t *testing.T) {
	v := listView{
		Crumbs: []string{"library/app", ":v1", "storage"},
		Rows: []Row{
			{Name: "blobs", Detail: "3 blobs", IsDir: true},
			{Name: "manifest", Detail: "application/vnd.oci.image.manifest.v1+json", Size: 1234, HasSize: true},
			{Name: "meta.json", Detail: "kind, digest, stats and rootfs status", Size: 512, HasSize: true},
		},
		Cursor: 1, Count: 3, Total: 3,
		Hints: "enter open · q quit",
	}
	out := RenderList(v, 80, 9)
	lines := strings.Split(out, "\n")
	if len(lines) != 9 {
		t.Fatalf("%d lines, want 9:\n%s", len(lines), out)
	}
	if lines[0] != "library/app › :v1 › storage" {
		t.Errorf("breadcrumb %q", lines[0])
	}
	if lines[1] != strings.Repeat("─", 80) || lines[7] != lines[1] {
		t.Errorf("rules: %q / %q", lines[1], lines[7])
	}
	if !strings.HasPrefix(lines[2], "  blobs/") || !strings.Contains(lines[2], "3 blobs") {
		t.Errorf("row 0 %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "▸ manifest ") || !strings.HasSuffix(lines[3], "  1.2 KiB") {
		t.Errorf("cursor row %q", lines[3])
	}
	if !strings.HasSuffix(lines[4], "512 B") {
		t.Errorf("row 2 %q", lines[4])
	}
	if lines[5] != "" || lines[6] != "" {
		t.Errorf("padding lines %q %q", lines[5], lines[6])
	}
	if lines[8] != "3 rows  ·  enter open · q quit" {
		t.Errorf("status %q", lines[8])
	}
	for _, l := range lines {
		if len([]rune(l)) > 80 {
			t.Errorf("line wider than 80: %q", l)
		}
	}
}

func TestRenderListMetaRows(t *testing.T) {
	mtime := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	v := listView{
		Crumbs: []string{"library/app", ":v1", "filesystem", "etc"},
		Rows: []Row{
			{Name: "os-release", Size: 258, HasSize: true, Meta: &RowMeta{Mode: store.TypeReg | 0o644, Mtime: mtime}},
			{Name: "link-to-os", Meta: &RowMeta{Mode: store.TypeLink | 0o777, UID: 1000, GID: 1000, Mtime: mtime, Target: "os-release"}},
			{Name: "rc.d", IsDir: true, Meta: &RowMeta{Mode: store.TypeDir | 0o755, Mtime: mtime}},
		},
		Cursor: 0, Count: 3, Total: 6, Filter: "o",
	}
	lines := strings.Split(RenderList(v, 100, 8), "\n")
	// Columns: mode, owner padded to the widest ("1000:1000", 9 cells), a
	// 10-cell right-aligned size, mtime, name; two spaces between columns.
	// The cursor row is padded to the full width, hence TrimRight.
	if want := "▸ -rw-r--r--  0:0" + strings.Repeat(" ", 6+2+5) + "258 B  2026-09-03 18:00  os-release"; strings.TrimRight(lines[2], " ") != want {
		t.Errorf("file row:\n%q\nwant\n%q", lines[2], want)
	}
	if want := "  lrwxrwxrwx  1000:1000" + strings.Repeat(" ", 2+10+2) + "2026-09-03 18:00  link-to-os -> os-release"; lines[3] != want {
		t.Errorf("symlink row:\n%q\nwant\n%q", lines[3], want)
	}
	if !strings.HasSuffix(lines[4], "  rc.d/") {
		t.Errorf("dir row %q", lines[4])
	}
	if lines[7] != `3 of 6 rows · filter "o"` {
		t.Errorf("status %q", lines[7])
	}
}

func TestRenderListStates(t *testing.T) {
	base := listView{Crumbs: []string{"oci-amber"}}
	if out := RenderList(base, 40, 6); !strings.Contains(out, "(empty)") || !strings.Contains(out, "0 rows") {
		t.Errorf("empty:\n%s", out)
	}
	loading := base
	loading.Loading = true
	if out := RenderList(loading, 40, 6); !strings.Contains(out, "loading…") {
		t.Errorf("loading:\n%s", out)
	}
	filtered := base
	filtered.Filter = "zzz"
	filtered.Total = 4
	if out := RenderList(filtered, 40, 6); !strings.Contains(out, `no rows match "zzz"`) {
		t.Errorf("filtered:\n%s", out)
	}
	status := base
	status.Status = "rootfs: no such path: etc/dangling"
	if out := RenderList(status, 60, 6); !strings.Contains(out, "no such path") {
		t.Errorf("status:\n%s", out)
	}
	input := base
	input.Input = "filter: os"
	if lines := strings.Split(RenderList(input, 40, 6), "\n"); lines[5] != "filter: os" {
		t.Errorf("input replaces the status line: %q", lines[5])
	}
	popup := base
	popup.Rows = []Row{{Name: "x"}}
	popup.Count, popup.Total = 1, 1
	popup.Popup = []KV{{"digest", "sha256:abc"}, {"kind", "manifest"}}
	out := RenderList(popup, 60, 10)
	if !strings.Contains(out, "digest  sha256:abc") || !strings.Contains(out, "kind    manifest") || !strings.Contains(out, "╭") {
		t.Errorf("popup:\n%s", out)
	}
	if strings.Contains(out, "▸ x") {
		t.Error("the popup replaces the rows")
	}
}

func TestRenderViewer(t *testing.T) {
	v := viewerView{
		Crumbs: []string{"library/app", ":v1", "filesystem", "etc", "os-release"},
		Body:   "1 ID=fixture\n2 VERSION_ID=1",
		Status: "text · 24 B · 2 lines · h hex · esc back",
	}
	lines := strings.Split(RenderViewer(v, 60, 7), "\n")
	if len(lines) != 7 || lines[2] != "1 ID=fixture" || lines[3] != "2 VERSION_ID=1" || lines[4] != "" {
		t.Errorf("viewer:\n%s", strings.Join(lines, "\n"))
	}
	if lines[6] != v.Status {
		t.Errorf("status %q", lines[6])
	}
	v.Input = "offset: 0x"
	if lines := strings.Split(RenderViewer(v, 60, 7), "\n"); lines[6] != "offset: 0x" {
		t.Errorf("input %q", lines[6])
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./browse`
Expected: compile errors (`undefined: modeString`, ...).

- [ ] **Step 3: Write `browse/view.go`**

```go
package browse

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleSel   = lipgloss.NewStyle().Reverse(true)
	styleDir   = lipgloss.NewStyle().Bold(true)
	styleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// chromeLines is what a screen spends outside its body: the breadcrumb,
// two rules and the status line.
const chromeLines = 4

// sizeWidth fits FormatBytes' widest output, "1023.9 MiB".
const sizeWidth = 10

// listView is what RenderList needs from a frame: the rows on screen and
// the chrome around them.
type listView struct {
	Crumbs  []string
	Rows    []Row // the rows on screen, already filtered and scrolled
	Cursor  int   // index into Rows; -1 when no row is selected
	Count   int   // rows that pass the filter
	Total   int   // rows before filtering
	Loading bool
	Filter  string
	Status  string // transient message, an error mostly
	Input   string // when not "", the text input shown instead of the status
	Hints   string // key hints
	Popup   []KV   // when not nil, drawn instead of the rows
}

// RenderList renders one listing screen for a width×height terminal.
func RenderList(v listView, width, height int) string {
	body := max(height-chromeLines, 1)
	var lines []string
	switch {
	case v.Popup != nil:
		lines = popupLines(v.Popup, width)
	case v.Loading:
		lines = []string{styleDim.Render("  loading…")}
	case len(v.Rows) == 0:
		msg := "  (empty)"
		if v.Filter != "" {
			msg = fmt.Sprintf("  no rows match %q", v.Filter)
		}
		lines = []string{styleDim.Render(msg)}
	default:
		lines = renderRows(v.Rows, v.Cursor, body, width)
	}
	return screen(breadcrumb(v.Crumbs, width), lines, statusLine(v, width), width, height)
}

// viewerView is what RenderViewer needs: the rendered body and the chrome.
type viewerView struct {
	Crumbs  []string
	Body    string // RenderText or RenderHex output
	Status  string
	Input   string // when not "", shown instead of Status
	Loading bool
}

// RenderViewer renders one viewer screen.
func RenderViewer(v viewerView, width, height int) string {
	body := strings.Split(v.Body, "\n")
	if v.Loading {
		body = []string{styleDim.Render("  loading…")}
	}
	status := v.Status
	if v.Input != "" {
		status = v.Input
	}
	return screen(breadcrumb(v.Crumbs, width), body, status, width, height)
}

// screen stacks the breadcrumb, a rule, the body padded or cut to fit, a
// rule and the status line, every line cut to width so nothing wraps.
func screen(crumb string, body []string, status string, width, height int) string {
	n := max(height-chromeLines, 1)
	for len(body) < n {
		body = append(body, "")
	}
	body = body[:n]
	lines := make([]string, 0, n+chromeLines)
	lines = append(lines, crumb, rule(width))
	lines = append(lines, body...)
	lines = append(lines, rule(width), status)
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

func rule(width int) string { return styleDim.Render(strings.Repeat("─", max(width, 0))) }

// breadcrumb joins crumbs with " › "; when that is wider than the
// terminal, leading segments are dropped behind a "…".
func breadcrumb(crumbs []string, width int) string {
	dropped := false
	for {
		s := strings.Join(crumbs, " › ")
		if dropped {
			s = "… › " + s
		}
		if lipgloss.Width(s) <= width || len(crumbs) <= 1 {
			return styleTitle.Render(truncate(s, width))
		}
		crumbs = crumbs[1:]
		dropped = true
	}
}

// renderRows lays the rows out in columns: ls -l columns when the rows
// carry Meta, otherwise name, detail and size. The cursor row is drawn
// reversed without inner styling so the highlight is one span.
func renderRows(rows []Row, cursor, height, width int) []string {
	rows = rows[:min(len(rows), height)]
	lsl := false
	nameW, ownerW := 0, 0
	for _, r := range rows {
		w := lipgloss.Width(r.Name)
		if r.IsDir {
			w++
		}
		nameW = max(nameW, w)
		if r.Meta != nil {
			lsl = true
			ownerW = max(ownerW, len(fmt.Sprintf("%d:%d", r.Meta.UID, r.Meta.GID)))
		}
	}
	nameW = min(nameW, max(8, width/3))
	out := make([]string, 0, len(rows))
	for i, r := range rows {
		selected := i == cursor
		var line string
		if lsl {
			line = lslLine(r, ownerW, width-2, selected)
		} else {
			line = storageLine(r, nameW, width-2, selected)
		}
		if selected {
			out = append(out, "▸ "+styleSel.Render(padRight(line, width-2)))
		} else {
			out = append(out, "  "+line)
		}
	}
	return out
}

// storageLine is "name  detail  size" in width cells.
func storageLine(r Row, nameW, width int, plain bool) string {
	name := r.Name
	if r.IsDir {
		name += "/"
	}
	name = padRight(truncate(name, nameW), nameW)
	size := strings.Repeat(" ", sizeWidth)
	if r.HasSize {
		size = fmt.Sprintf("%*s", sizeWidth, tui.FormatBytes(r.Size))
	}
	detailW := max(width-nameW-2-sizeWidth-2, 0)
	detail := padRight(truncate(r.Detail, detailW), detailW)
	if !plain {
		if r.IsDir {
			name = styleDir.Render(name)
		}
		detail = styleDim.Render(detail)
	}
	return name + "  " + detail + "  " + size
}

// lslLine is "mode owner size mtime name [-> target]" in width cells.
func lslLine(r Row, ownerW, width int, plain bool) string {
	m := r.Meta
	size := strings.Repeat(" ", sizeWidth)
	if r.HasSize {
		size = fmt.Sprintf("%*s", sizeWidth, tui.FormatBytes(r.Size))
	}
	prefix := fmt.Sprintf("%s  %-*s  %s  %s  ", modeString(m.Mode), ownerW, fmt.Sprintf("%d:%d", m.UID, m.GID), size, m.Mtime.Format("2006-01-02 15:04"))
	name := r.Name
	if r.IsDir {
		name += "/"
	}
	if m.Target != "" {
		name += " -> " + m.Target
	}
	name = truncate(name, max(width-lipgloss.Width(prefix), 0))
	if !plain {
		prefix = styleDim.Render(prefix)
		if r.IsDir {
			name = styleDir.Render(name)
		}
	}
	return prefix + name
}

// modeString renders mode like ls -l: a type letter and nine permission
// bits with setuid, setgid and sticky folded in.
func modeString(mode uint64) string {
	var b [10]byte
	switch mode & store.TypeMask {
	case store.TypeDir:
		b[0] = 'd'
	case store.TypeLink:
		b[0] = 'l'
	case store.TypeChar:
		b[0] = 'c'
	case store.TypeBlock:
		b[0] = 'b'
	case store.TypeFIFO:
		b[0] = 'p'
	case store.TypeSocket:
		b[0] = 's'
	default:
		b[0] = '-'
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if mode&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		} else {
			b[i+1] = '-'
		}
	}
	fold := func(i int, bit uint64, x, X byte) {
		if mode&bit == 0 {
			return
		}
		if b[i] == 'x' {
			b[i] = x
		} else {
			b[i] = X
		}
	}
	fold(3, 0o4000, 's', 'S')
	fold(6, 0o2000, 's', 'S')
	fold(9, 0o1000, 't', 'T')
	return string(b[:])
}

// statusLine is the input when one is open, else the status message, the
// row count and the key hints.
func statusLine(v listView, width int) string {
	if v.Input != "" {
		return truncate(v.Input, width)
	}
	var parts []string
	if v.Status != "" {
		parts = append(parts, styleErr.Render(v.Status))
	}
	count := plural(v.Count, "row")
	if v.Filter != "" {
		count = fmt.Sprintf("%d of %d rows · filter %q", v.Count, v.Total, v.Filter)
	}
	parts = append(parts, count)
	if v.Hints != "" {
		parts = append(parts, styleDim.Render(v.Hints))
	}
	return truncate(strings.Join(parts, "  ·  "), width)
}

// popupLines draws the info pairs in a rounded box centred in width.
func popupLines(info []KV, width int) []string {
	keyW := 0
	for _, kv := range info {
		keyW = max(keyW, lipgloss.Width(kv.Key))
	}
	inner := make([]string, 0, len(info))
	innerW := 0
	for _, kv := range info {
		l := fmt.Sprintf("%-*s  %s", keyW, kv.Key, kv.Value)
		inner = append(inner, l)
		innerW = max(innerW, lipgloss.Width(l))
	}
	innerW = min(innerW, max(width-6, 10))
	for i, l := range inner {
		inner[i] = padRight(truncate(l, innerW), innerW)
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Render(strings.Join(inner, "\n"))
	lines := strings.Split(box, "\n")
	pad := strings.Repeat(" ", max((width-lipgloss.Width(lines[0]))/2, 0))
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./browse`
Expected: PASS. If `TestRenderListMetaRows` differs only in spacing, fix the expectation in the test to the exact output *only after* confirming the columns are mode, owner (padded to the widest), a 10-cell size, mtime and name, separated by two spaces each.

- [ ] **Step 5: Commit**

```bash
git add browse/view.go browse/view_test.go
git commit -m "browse: render listings, the status line and the info popup"
```

---

### Task 9: The Bubble Tea model and `Run`

**Files:**
- Create: `browse/model.go`, `browse/run.go`
- Test: `browse/model_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `type Options struct { Store *store.Store; Blobs *blob.Store; Images *image.Store; Start string }`; `func Run(ctx context.Context, opts Options) error`; internally `newModel(b *Browser, start string) (*model, error)`, `splitReference(s string) (repo, reference string)`.

- [ ] **Step 1: Write the failing tests**

Create `browse/model_test.go`:

```go
package browse

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive runs cmd and feeds every message it yields back into m until
// nothing is left, so a test sees the state after every load finished.
func drive(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

var keyTypes = map[string]tea.KeyType{
	"enter": tea.KeyEnter, "backspace": tea.KeyBackspace, "esc": tea.KeyEsc,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
}

// press sends keys one after another; a name in keyTypes is that key,
// anything else is typed as runes.
func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		if typ, ok := keyTypes[k]; ok {
			msg = tea.KeyMsg{Type: typ}
		}
		_, cmd := m.Update(msg)
		drive(t, m, cmd)
	}
}

func newTestModel(t *testing.T, f *fixture, start string) *model {
	t.Helper()
	m, err := newModel(f.b, start)
	if err != nil {
		t.Fatalf("newModel(%q): %v", start, err)
	}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drive(t, m, cmd)
	drive(t, m, m.Init())
	return m
}

func visibleNames(f *frame) []string {
	names := make([]string, len(f.visible))
	for i, idx := range f.visible {
		names[i] = f.rows[idx].Name
	}
	return names
}

func assertTop(t *testing.T, m *model, want ...string) {
	t.Helper()
	if got := visibleNames(m.top()); !slices.Equal(got, want) {
		t.Fatalf("top rows %v, want %v", got, want)
	}
}

func assertCrumbs(t *testing.T, m *model, want ...string) {
	t.Helper()
	if got := m.crumbs(); !slices.Equal(got, want) {
		t.Fatalf("crumbs %v, want %v", got, want)
	}
}

func TestModelStartsAtRepositories(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	assertTop(t, m, "library/app", "library/app/sub", "tools/rawimg")
	assertCrumbs(t, m, "oci-amber")
	out := m.View()
	if !strings.Contains(out, "oci-amber") || !strings.Contains(out, "3 rows") || !strings.Contains(out, "▸ library/app/") {
		t.Errorf("view:\n%s", out)
	}
	if lines := strings.Split(out, "\n"); len(lines) != 30 {
		t.Errorf("%d lines, want 30", len(lines))
	}
}

func TestEnterAndBackspaceWalkTheTree(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app")
	if names := visibleNames(m.top()); names[0] != "latest" || names[1] != "v1" {
		t.Fatalf("repo rows %v", names)
	}
	press(t, m, "down", "enter")
	if m.img == nil || len(m.img.storage) != 1 {
		t.Fatal("opening a tag must start the image group")
	}
	assertCrumbs(t, m, "library/app", ":v1", "storage")
	assertTop(t, m, "blobs", "manifest", "meta.json", "rootfs")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	assertTop(t, m, shortRef(f.conf), shortRef(f.layerA), shortRef(f.layerB))
	press(t, m, "down", "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs", shortRef(f.layerA))
	assertTop(t, m, "blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json")
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs", shortRef(f.layerA), "blobs")
	if rows := m.top().rows; len(rows) != 5 || rows[0].Detail == "" {
		t.Errorf("prism blobs rows %+v", rows)
	}
	press(t, m, "backspace", "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	if m.top().cursor != 1 {
		t.Errorf("cursor %d after returning, want 1", m.top().cursor)
	}
	press(t, m, "backspace", "backspace")
	if m.img != nil {
		t.Fatal("leaving the storage root must drop the image group")
	}
	assertCrumbs(t, m, "library/app")
	press(t, m, "backspace")
	assertCrumbs(t, m, "oci-amber")
	press(t, m, "backspace")
	assertCrumbs(t, m, "oci-amber")
}

func TestToggleFilesystemKeepsPosition(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	assertCrumbs(t, m, "library/app", ":v1", "storage")
	press(t, m, "enter") // blobs
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem")
	assertTop(t, m, "bin", "etc", "usr")
	press(t, m, "down", "enter")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "filesystem")
	press(t, m, "backspace")
	assertCrumbs(t, m, "library/app", ":v1", "storage", "blobs")
	if !strings.Contains(m.View(), "f filesystem") {
		t.Error("hints must offer the filesystem")
	}
}

func TestFilterHidesRows(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter") // etc
	assertTop(t, m, "abs", "config.json", "dangling", "hostname", "link-to-os", "os-release")
	press(t, m, "/", "os", "enter")
	assertTop(t, m, "hostname", "link-to-os", "os-release")
	if m.top().filter != "os" || !strings.Contains(m.View(), "3 of 6 rows") {
		t.Errorf("filter %q view:\n%s", m.top().filter, m.View())
	}
	press(t, m, "G")
	if m.top().currentRow().Name != "os-release" {
		t.Errorf("G under a filter lands on %q", m.top().currentRow().Name)
	}
	press(t, m, "esc")
	if m.top().filter != "" || len(m.top().visible) != 6 {
		t.Error("esc must clear the filter")
	}
	if m.top().currentRow().Name != "os-release" {
		t.Errorf("clearing the filter keeps the cursor on %q", m.top().currentRow().Name)
	}
}

func TestViewerTextAndHex(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "G", "enter") // etc/os-release
	fr := m.top()
	if fr.view == nil || fr.view.mode != modeText {
		t.Fatalf("viewer not open in text mode: %+v", fr.view)
	}
	if fr.view.lines[0] != `PRETTY_NAME="Fixture Linux"` {
		t.Errorf("first line %q", fr.view.lines[0])
	}
	out := m.View()
	if !strings.Contains(out, "1 PRETTY_NAME") || !strings.Contains(out, "text · ") || !strings.Contains(out, "mode 0644") || !strings.Contains(out, "3 lines") {
		t.Errorf("text view:\n%s", out)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc", "os-release")
	press(t, m, "h")
	if fr.view.mode != modeHex || !strings.Contains(m.View(), "50 52 45 54") {
		t.Errorf("hex view:\n%s", m.View())
	}
	press(t, m, "h")
	if fr.view.mode != modeText {
		t.Error("h must switch back to text")
	}
	press(t, m, "/", "VERSION", "enter")
	if len(fr.view.hits) != 1 || fr.view.hits[0] != 2 {
		t.Errorf("hits %v", fr.view.hits)
	}
	press(t, m, "backspace")
	if m.top().view != nil {
		t.Error("backspace must leave the viewer")
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
}

func TestViewerJSONPretty(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "down", "enter") // etc/config.json
	v := m.top().view
	if v == nil || v.text.Label != "json" || !v.pretty || v.lines[0] != "{" {
		t.Fatalf("json viewer %+v", v)
	}
	if !strings.Contains(m.View(), `"listen": ":8080"`) || !strings.Contains(m.View(), "json · ") {
		t.Errorf("view:\n%s", m.View())
	}
	press(t, m, "p")
	if v.pretty || !strings.HasPrefix(v.lines[0], `{"listen":`) {
		t.Errorf("raw lines %v", v.lines)
	}
	press(t, m, "p")
	if !v.pretty {
		t.Error("p toggles back")
	}
}

func TestViewerBinaryOpensInHex(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "enter", "enter") // bin/app
	fr := m.top()
	v := fr.view
	if v == nil || v.mode != modeHex || string(v.win[:4]) != "\x7fELF" {
		t.Fatalf("binary viewer %+v", v)
	}
	if out := m.View(); !strings.Contains(out, ".ELF") || !strings.Contains(out, "hex · 300.0 KiB") {
		t.Errorf("hex view:\n%s", out)
	}
	body := int64(m.bodyHeight())
	rows := (fr.file.Size + hexRowBytes - 1) / hexRowBytes
	last := (rows - body) * hexRowBytes
	press(t, m, "G")
	if v.hexOff != last || !strings.Contains(m.View(), fmt.Sprintf("%08x", last)) {
		t.Errorf("G: offset %#x, want %#x", v.hexOff, last)
	}
	press(t, m, "pgup")
	if v.hexOff != last-body*hexRowBytes {
		t.Errorf("pgup: offset %#x", v.hexOff)
	}
	press(t, m, ":", "0x100", "enter")
	if v.hexOff != 0x100 || !strings.Contains(m.View(), "00000100") {
		t.Errorf("goto: offset %#x\n%s", v.hexOff, m.View())
	}
	press(t, m, "down", "down")
	if v.hexOff != 0x120 {
		t.Errorf("down: offset %#x", v.hexOff)
	}
	press(t, m, "h")
	if v.mode != modeText || v.text == nil || len(v.lines) == 0 {
		t.Errorf("h on a binary loads it as text: %+v", v)
	}
	press(t, m, "h")
	if v.mode != modeHex {
		t.Error("h again returns to hex")
	}
}

func TestSymlinksResolveOnEnter(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:v1")
	press(t, m, "f", "down", "enter", "G", "up", "enter") // etc/link-to-os
	if v := m.top().view; v == nil || v.lines[0] != `PRETTY_NAME="Fixture Linux"` {
		t.Fatalf("symlink to a file opens the file: %+v", v)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc", "link-to-os")
	press(t, m, "backspace", "g", "down", "down", "enter") // etc/dangling
	if m.top().view != nil || !strings.Contains(m.status, "no such path") {
		t.Errorf("dangling symlink: status %q", m.status)
	}
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "etc")
	press(t, m, "backspace", "g", "enter", "down", "enter") // bin/sbin -> ../usr/bin
	assertCrumbs(t, m, "library/app", ":v1", "filesystem", "bin", "sbin")
	assertTop(t, m, "tool.sh")
}

func TestStartReferences(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app")
	assertCrumbs(t, m, "library/app")
	m = newTestModel(t, f, "library/app@"+f.m1.String())
	assertCrumbs(t, m, "library/app", "@"+shortRef(f.m1), "storage")
	for _, bad := range []string{"nobody/here", "nobody/here:x", "library/app:nope", "library/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"} {
		if _, err := newModel(f.b, bad); err == nil {
			t.Errorf("newModel(%q) must fail", bad)
		}
	}
	for s, want := range map[string][2]string{
		"library/app":       {"library/app", ""},
		"library/app:v1":    {"library/app", "v1"},
		"library/app@sha256:ab": {"library/app", "sha256:ab"},
		"a/b:c/d":           {"a/b:c/d", ""},
	} {
		if repo, ref := splitReference(s); repo != want[0] || ref != want[1] {
			t.Errorf("splitReference(%q) = %q, %q", s, repo, ref)
		}
	}
}

func TestIndexFilesystemChooser(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "library/app:latest")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":latest", "filesystem")
	assertTop(t, m, shortRef(f.m1), shortRef(f.m2))
	press(t, m, "enter")
	assertCrumbs(t, m, "library/app", ":latest", "filesystem", "linux/amd64")
	assertTop(t, m, "bin", "etc", "usr")
	press(t, m, "f")
	assertCrumbs(t, m, "library/app", ":latest", "storage")
}

func TestRawImageFilesystemUnavailable(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "tools/rawimg:r1")
	press(t, m, "f")
	assertTop(t, m, "rootfs unavailable")
	press(t, m, "enter")
	if m.status != "nothing to open here" {
		t.Errorf("status %q", m.status)
	}
}

func TestStaleLoadIsDropped(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	before := visibleNames(m.top())
	m.Update(listLoadedMsg{id: 9999, rows: []Row{{Name: "ghost"}}})
	if got := visibleNames(m.top()); !slices.Equal(got, before) {
		t.Errorf("a stale load changed the rows: %v", got)
	}
}

func TestInfoPopupAndQuit(t *testing.T) {
	f := newFixture(t)
	m := newTestModel(t, f, "")
	press(t, m, "i")
	if m.popup == nil || !strings.Contains(m.View(), "repository  library/app") {
		t.Errorf("popup:\n%s", m.View())
	}
	press(t, m, "down")
	if m.popup != nil || m.top().cursor != 0 {
		t.Error("the key that closes the popup does nothing else")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q must quit")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./browse`
Expected: compile errors (`undefined: newModel`, ...).

- [ ] **Step 3: Write `browse/model.go`**

```go
package browse

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/tui"
)

// stackKind names the two stacks an image has.
type stackKind int

const (
	stackStorage stackKind = iota
	stackFS
)

// viewMode is the viewer's representation of a file.
type viewMode int

const (
	modeText viewMode = iota
	modeHex
)

// inputKind says what the text input at the bottom collects.
type inputKind int

const (
	inputNone inputKind = iota
	inputFilter
	inputSearch
	inputGoto
)

// frame is one screen: a listing or a viewer with its state, so returning
// to it restores cursor, scroll and filter.
type frame struct {
	id        int
	node      Node
	loaded    bool // rows or the file are in
	loading   bool // a load command is in flight
	resolving bool // the symlink under the cursor is being followed
	// listing
	rows    []Row
	visible []int // indexes into rows that pass the filter
	cursor  int   // index into visible
	top     int   // first visible index on screen
	filter  string
	// viewer
	file *File
	view *viewer
}

// viewer is the state of an open file.
type viewer struct {
	class      Classification
	mode       viewMode
	text       *Text
	pretty     bool     // JSON shown indented
	lines      []string // lines of the current text representation
	top, left  int
	search     string
	hits       []int // line indexes matching search, ascending
	hexOff     int64 // offset of the first hex row on screen, a multiple of 16
	win        []byte
	winStart   int64
	winLoading bool
}

// imageGroup is the position inside one image: the storage stack, the
// filesystem stack and which one is active.
type imageGroup struct {
	crumb   string
	active  stackKind
	storage []*frame
	fs      []*frame
	fsRoot  key.Key // the image root the fs stack was built for
}

// model is the Bubble Tea model. base holds the repository listing and,
// below it, one repository; img is set while inside an image.
type model struct {
	b         *Browser
	base      []*frame
	img       *imageGroup
	width     int
	height    int
	nextID    int
	input     textinput.Model
	inputKind inputKind
	popup     []KV
	status    string
}

type listLoadedMsg struct {
	id   int
	rows []Row
	err  error
}

type fileLoadedMsg struct {
	id    int
	file  *File
	class Classification
	text  *Text  // text mode content, when read
	win   []byte // the first hex window, when opened in hex
	err   error
}

type windowLoadedMsg struct {
	id    int
	start int64
	data  []byte
	err   error
}

type resolvedMsg struct {
	id   int // the frame whose row was followed
	node Node
	err  error
}

// newModel builds the model at start: "" is the repository listing,
// "repo" a repository, "repo:tag" or "repo@digest" an image's storage
// root. A reference that does not exist is an error.
func newModel(b *Browser, start string) (*model, error) {
	m := &model{b: b, width: 80, height: 24, input: textinput.New()}
	m.base = []*frame{m.newFrame(b.rootNode())}
	if start == "" {
		return m, nil
	}
	repo, reference := splitReference(start)
	rn, err := b.repoNode(repo)
	if err != nil {
		return nil, fmt.Errorf("browse: repository %s: %w", repo, err)
	}
	m.base = append(m.base, m.newFrame(rn))
	if reference == "" {
		return m, nil
	}
	in, err := b.imageNode(repo, reference)
	if err != nil {
		return nil, fmt.Errorf("browse: %s: %w", start, err)
	}
	m.img = &imageGroup{crumb: in.crumb, storage: []*frame{m.newFrame(in)}}
	return m, nil
}

// splitReference splits "repo", "repo:tag" or "repo@digest": '@' starts a
// digest, a ':' after the last '/' starts a tag.
func splitReference(s string) (repo, reference string) {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i], s[i+1:]
	}
	slash := strings.LastIndexByte(s, '/')
	if i := strings.IndexByte(s[slash+1:], ':'); i >= 0 {
		return s[:slash+1+i], s[slash+1+i+1:]
	}
	return s, ""
}

func (m *model) newFrame(n Node) *frame {
	m.nextID++
	return &frame{id: m.nextID, node: n}
}

func (m *model) Init() tea.Cmd { return m.ensureLoaded() }

// bodyHeight is how many rows fit between the chrome lines.
func (m *model) bodyHeight() int { return max(m.height-chromeLines, 1) }

func (m *model) activeStack() []*frame {
	if m.img.active == stackFS {
		return m.img.fs
	}
	return m.img.storage
}

func (m *model) setActiveStack(s []*frame) {
	if m.img.active == stackFS {
		m.img.fs = s
	} else {
		m.img.storage = s
	}
}

// top is the frame on screen.
func (m *model) top() *frame {
	if m.img != nil {
		s := m.activeStack()
		return s[len(s)-1]
	}
	return m.base[len(m.base)-1]
}

// findFrame returns the frame with id, or nil once it was popped.
func (m *model) findFrame(id int) *frame {
	for _, f := range m.base {
		if f.id == id {
			return f
		}
	}
	if m.img != nil {
		for _, f := range m.img.storage {
			if f.id == id {
				return f
			}
		}
		for _, f := range m.img.fs {
			if f.id == id {
				return f
			}
		}
	}
	return nil
}

// popFrame removes f wherever it is. Emptying the fs stack returns to the
// storage stack; emptying the storage stack leaves the image; the
// repository listing is never removed.
func (m *model) popFrame(f *frame) {
	remove := func(s []*frame) []*frame {
		for i, g := range s {
			if g == f {
				return append(s[:i:i], s[i+1:]...)
			}
		}
		return s
	}
	if m.img != nil {
		m.img.storage = remove(m.img.storage)
		m.img.fs = remove(m.img.fs)
		if len(m.img.fs) == 0 && m.img.active == stackFS {
			m.img.active = stackStorage
			m.img.fsRoot = key.Key{}
		}
		if len(m.img.storage) == 0 {
			m.img = nil
		}
		return
	}
	if len(m.base) > 1 {
		m.base = remove(m.base)
	}
}

// hexWindowLen is how many bytes a hex window holds: the screen plus one
// screen before and after.
func (m *model) hexWindowLen() int64 { return 3 * int64(m.bodyHeight()) * hexRowBytes }

// ensureLoaded starts loading the top frame when it has nothing yet.
func (m *model) ensureLoaded() tea.Cmd {
	f := m.top()
	if f.loaded || f.loading {
		return nil
	}
	switch n := f.node.(type) {
	case Lister:
		f.loading = true
		return loadList(f.id, n)
	case Opener:
		f.loading = true
		return loadFile(f.id, n, m.hexWindowLen())
	}
	return nil
}

func loadList(id int, n Lister) tea.Cmd {
	return func() tea.Msg {
		rows, err := n.List()
		return listLoadedMsg{id: id, rows: rows, err: err}
	}
}

// loadFile opens n, classifies it from a probe, and reads either the
// whole text or the first hex window.
func loadFile(id int, n Opener, winLen int64) tea.Cmd {
	return func() tea.Msg {
		f, err := n.Open()
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		probe, err := readWindow(f, 0, probeSize)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		class := Classify(f.Name, f.Size, probe)
		msg := fileLoadedMsg{id: id, file: f, class: class}
		if class.Kind == KindText {
			data, err := readWindow(f, 0, f.Size)
			if err != nil {
				return fileLoadedMsg{id: id, err: err}
			}
			msg.text = LoadText(class.Label, data)
			return msg
		}
		win, err := readWindow(f, 0, winLen)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		msg.win = win
		return msg
	}
}

// loadText reads a file already open in hex so it can be shown as text.
func loadText(id int, f *File) tea.Cmd {
	return func() tea.Msg {
		data, err := readWindow(f, 0, f.Size)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		return fileLoadedMsg{id: id, text: LoadText("binary", data)}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(m.width-20, 10)
		if f := m.top(); f.view != nil {
			return m, m.ensureWindow(f)
		}
	case listLoadedMsg:
		return m, m.onListLoaded(msg)
	case fileLoadedMsg:
		return m, m.onFileLoaded(msg)
	case windowLoadedMsg:
		return m, m.onWindowLoaded(msg)
	case resolvedMsg:
		return m, m.onResolved(msg)
	case tea.KeyMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *model) onListLoaded(msg listLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.loading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		m.popFrame(f)
		return m.ensureLoaded()
	}
	f.loaded = true
	f.rows = msg.rows
	f.applyFilter()
	return nil
}

func (m *model) onFileLoaded(msg fileLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.loading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		if f.view == nil {
			m.popFrame(f)
		}
		return m.ensureLoaded()
	}
	if f.view != nil { // text requested from hex mode
		v := f.view
		v.text = msg.text
		v.pretty = msg.text.Pretty != nil
		v.lines = Lines(v.currentBytes())
		v.mode = modeText
		v.top = min(lineAt(v.text.Raw, v.hexOff), max(len(v.lines)-1, 0))
		return nil
	}
	f.loaded = true
	f.file = msg.file
	v := &viewer{class: msg.class}
	if msg.text != nil {
		v.text = msg.text
		v.pretty = msg.text.Pretty != nil
		v.lines = Lines(v.currentBytes())
	} else {
		v.mode = modeHex
		v.win = msg.win
		if v.win == nil {
			v.win = []byte{}
		}
	}
	f.view = v
	return nil
}

func (m *model) onWindowLoaded(msg windowLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil || f.view == nil {
		return nil
	}
	v := f.view
	v.winLoading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		return nil
	}
	v.winStart = msg.start
	v.win = msg.data
	if v.win == nil {
		v.win = []byte{}
	}
	return m.ensureWindow(f) // the position may have moved during the load
}

func (m *model) onResolved(msg resolvedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.resolving = false
	if msg.err != nil {
		m.status = msg.err.Error()
		return nil
	}
	return m.push(msg.node, f)
}

// push enters n from frame from: a Lister or Opener becomes a new frame
// on the active stack, an image root opened outside an image starts the
// image group, a Resolver is followed first.
func (m *model) push(n Node, from *frame) tea.Cmd {
	m.status = ""
	switch n := n.(type) {
	case Resolver:
		from.resolving = true
		return func() tea.Msg {
			r, err := n.Resolve()
			return resolvedMsg{id: from.id, node: r, err: err}
		}
	case *imageRootNode:
		if m.img == nil {
			m.img = &imageGroup{crumb: n.crumb, storage: []*frame{m.newFrame(n)}}
			return m.ensureLoaded()
		}
	}
	fr := m.newFrame(n)
	if m.img != nil {
		m.setActiveStack(append(m.activeStack(), fr))
	} else {
		m.base = append(m.base, fr)
	}
	return m.ensureLoaded()
}

// open follows the row under the cursor.
func (m *model) open() tea.Cmd {
	f := m.top()
	if f.loading || f.resolving || f.view != nil {
		return nil
	}
	row := f.currentRow()
	if row == nil {
		return nil
	}
	if row.Child == nil {
		m.status = "nothing to open here"
		return nil
	}
	return m.push(row.Child, f)
}

// back pops the top frame; on the repository listing it does nothing.
func (m *model) back() tea.Cmd {
	if m.img == nil && len(m.base) == 1 {
		return nil
	}
	m.status = ""
	m.popFrame(m.top())
	return m.ensureLoaded()
}

// toggleView switches between the storage and filesystem stacks. The
// filesystem shown is the one of the innermost image root on the storage
// stack; its stack is kept while that root does not change.
func (m *model) toggleView() tea.Cmd {
	if m.img == nil {
		m.status = "open an image first"
		return nil
	}
	m.status = ""
	if m.img.active == stackFS {
		m.img.active = stackStorage
		return m.ensureLoaded()
	}
	var root *imageRootNode
	for i := len(m.img.storage) - 1; i >= 0 && root == nil; i-- {
		root, _ = m.img.storage[i].node.(*imageRootNode)
	}
	if root == nil {
		return nil
	}
	if m.img.fs == nil || m.img.fsRoot != root.im.Root() {
		m.img.fs = []*frame{m.newFrame(fsRootFor(m.b, root.repo, root.im))}
		m.img.fsRoot = root.im.Root()
	}
	m.img.active = stackFS
	return m.ensureLoaded()
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	if m.popup != nil {
		m.popup = nil
		return nil
	}
	if m.inputKind != inputNone {
		return m.onInputKey(k)
	}
	s := k.String()
	switch s {
	case "q", "ctrl+c":
		return tea.Quit
	case "f":
		return m.toggleView()
	}
	f := m.top()
	if _, isFile := f.node.(Opener); isFile {
		return m.onViewerKey(f, s)
	}
	h := m.bodyHeight()
	switch s {
	case "up", "k":
		f.move(-1, h)
	case "down", "j":
		f.move(1, h)
	case "pgup":
		f.move(-h, h)
	case "pgdown":
		f.move(h, h)
	case "g", "home":
		f.moveTo(0, h)
	case "G", "end":
		f.moveTo(len(f.visible)-1, h)
	case "enter", "right", "l":
		return m.open()
	case "esc":
		if f.filter != "" {
			f.filter = ""
			f.applyFilter()
			return nil
		}
		return m.back()
	case "backspace", "left", "h":
		return m.back()
	case "/":
		m.startInput(inputFilter, f.filter)
	case "i":
		if r := f.currentRow(); r != nil {
			m.popup = r.Info
			if len(m.popup) == 0 {
				m.popup = []KV{{"name", r.Name}}
			}
		}
	}
	return nil
}

func (m *model) onViewerKey(f *frame, s string) tea.Cmd {
	v := f.view
	if v == nil {
		if s == "backspace" || s == "esc" {
			return m.back()
		}
		return nil
	}
	h := m.bodyHeight()
	switch s {
	case "backspace", "esc":
		return m.back()
	case "up", "k":
		return m.scrollViewer(f, -1)
	case "down", "j":
		return m.scrollViewer(f, 1)
	case "pgup":
		return m.scrollViewer(f, -h)
	case "pgdown":
		return m.scrollViewer(f, h)
	case "g", "home":
		return m.scrollViewerEnd(f, false)
	case "G", "end":
		return m.scrollViewerEnd(f, true)
	case "left":
		if v.mode == modeText {
			v.left = max(v.left-8, 0)
		}
	case "right":
		if v.mode == modeText {
			v.left += 8
		}
	case "h":
		return m.toggleHex(f)
	case "p":
		if v.text != nil && v.text.Pretty != nil {
			v.pretty = !v.pretty
			v.lines = Lines(v.currentBytes())
			v.top = min(v.top, max(len(v.lines)-1, 0))
			v.setSearch(v.search)
		}
	case "/":
		if v.mode == modeText {
			m.startInput(inputSearch, v.search)
		}
	case "n":
		v.nextHit(1)
	case "N":
		v.nextHit(-1)
	case ":":
		if v.mode == modeHex {
			m.startInput(inputGoto, "")
		}
	}
	return nil
}

// startInput opens the bottom-line text input for kind with initial text.
func (m *model) startInput(kind inputKind, initial string) {
	m.inputKind = kind
	switch kind {
	case inputFilter:
		m.input.Prompt = "filter: "
	case inputSearch:
		m.input.Prompt = "search: "
	case inputGoto:
		m.input.Prompt = "offset (decimal or 0x…): "
	}
	m.input.SetValue(initial)
	m.input.CursorEnd()
	m.input.Focus()
}

// onInputKey feeds the text input; filter and search apply as they are
// typed, a goto offset applies on enter, esc clears what was typed.
func (m *model) onInputKey(k tea.KeyMsg) tea.Cmd {
	f := m.top()
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc":
		switch m.inputKind {
		case inputFilter:
			f.filter = ""
			f.applyFilter()
		case inputSearch:
			if f.view != nil {
				f.view.setSearch("")
			}
		}
		m.inputKind = inputNone
		m.input.Blur()
		return nil
	case "enter":
		kind := m.inputKind
		m.inputKind = inputNone
		m.input.Blur()
		if f.view == nil {
			return nil
		}
		switch kind {
		case inputGoto:
			return m.gotoOffset(f, m.input.Value())
		case inputSearch:
			f.view.nextHit(0)
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	switch m.inputKind {
	case inputFilter:
		f.filter = m.input.Value()
		f.applyFilter()
	case inputSearch:
		if f.view != nil {
			f.view.setSearch(m.input.Value())
		}
	}
	return cmd
}

// scrollViewer moves the viewer by delta rows.
func (m *model) scrollViewer(f *frame, delta int) tea.Cmd {
	v := f.view
	if v.mode == modeHex {
		return m.scrollHex(f, int64(delta))
	}
	v.top = min(max(v.top+delta, 0), max(len(v.lines)-m.bodyHeight(), 0))
	return nil
}

// scrollViewerEnd jumps to the start or the end.
func (m *model) scrollViewerEnd(f *frame, toEnd bool) tea.Cmd {
	v := f.view
	if v.mode == modeHex {
		if toEnd {
			return m.scrollHex(f, 1<<40)
		}
		return m.scrollHex(f, -(1 << 40))
	}
	v.top = 0
	if toEnd {
		v.top = max(len(v.lines)-m.bodyHeight(), 0)
	}
	return nil
}

// scrollHex moves the hex view by rows, keeping the last page full, and
// loads a new window when the screen leaves the loaded one.
func (m *model) scrollHex(f *frame, deltaRows int64) tea.Cmd {
	v := f.view
	h := int64(m.bodyHeight())
	rows := (f.file.Size + hexRowBytes - 1) / hexRowBytes
	maxTop := max(rows-h, 0) * hexRowBytes
	row := v.hexOff/hexRowBytes + deltaRows
	v.hexOff = min(max(row, 0)*hexRowBytes, maxTop)
	return m.ensureWindow(f)
}

// ensureWindow loads the bytes around the hex position when the loaded
// window does not cover the screen.
func (m *model) ensureWindow(f *frame) tea.Cmd {
	v := f.view
	if v == nil || v.mode != modeHex || v.winLoading {
		return nil
	}
	h := int64(m.bodyHeight())
	need := min(h*hexRowBytes, f.file.Size-v.hexOff)
	if v.win != nil && v.hexOff >= v.winStart && v.hexOff+need <= v.winStart+int64(len(v.win)) {
		return nil
	}
	start := max(v.hexOff-h*hexRowBytes, 0)
	length := m.hexWindowLen()
	v.winLoading = true
	file, id := f.file, f.id
	return func() tea.Msg {
		data, err := readWindow(file, start, length)
		return windowLoadedMsg{id: id, start: start, data: data, err: err}
	}
}

// gotoOffset jumps the hex view to a typed offset.
func (m *model) gotoOffset(f *frame, s string) tea.Cmd {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	off, err := strconv.ParseInt(s, 0, 64)
	if err != nil || off < 0 {
		m.status = fmt.Sprintf("not an offset: %q", s)
		return nil
	}
	f.view.hexOff = off / hexRowBytes * hexRowBytes
	return m.scrollHex(f, 0)
}

// toggleHex switches the viewer between text and hex at the same place.
func (m *model) toggleHex(f *frame) tea.Cmd {
	v := f.view
	switch v.mode {
	case modeText:
		v.mode = modeHex
		v.hexOff = lineOffset(v.text.Raw, v.top) / hexRowBytes * hexRowBytes
		return m.scrollHex(f, 0)
	default:
		if v.class.Kind == KindTooLarge {
			m.status = fmt.Sprintf("too large for text (over %s)", tui.FormatBytes(MaxTextSize))
			return nil
		}
		if v.text == nil {
			f.loading = true
			return loadText(f.id, f.file)
		}
		v.mode = modeText
		v.top = min(lineAt(v.text.Raw, v.hexOff), max(len(v.lines)-1, 0))
	}
	return nil
}

// currentBytes is what the text mode shows: pretty JSON or the raw bytes.
func (v *viewer) currentBytes() []byte {
	if v.pretty && v.text.Pretty != nil {
		return v.text.Pretty
	}
	return v.text.Raw
}

// setSearch recomputes the matching lines, case-insensitively.
func (v *viewer) setSearch(s string) {
	v.search = s
	v.hits = v.hits[:0]
	if s == "" {
		return
	}
	needle := strings.ToLower(s)
	for i, l := range v.lines {
		if strings.Contains(strings.ToLower(l), needle) {
			v.hits = append(v.hits, i)
		}
	}
}

// nextHit scrolls to the next hit after the top line (dir > 0), the
// previous one before it (dir < 0) or the first at or after it (dir 0),
// wrapping around.
func (v *viewer) nextHit(dir int) {
	if len(v.hits) == 0 {
		return
	}
	switch {
	case dir > 0:
		for _, h := range v.hits {
			if h > v.top {
				v.top = h
				return
			}
		}
		v.top = v.hits[0]
	case dir < 0:
		for i := len(v.hits) - 1; i >= 0; i-- {
			if v.hits[i] < v.top {
				v.top = v.hits[i]
				return
			}
		}
		v.top = v.hits[len(v.hits)-1]
	default:
		for _, h := range v.hits {
			if h >= v.top {
				v.top = h
				return
			}
		}
		v.top = v.hits[0]
	}
}

// move shifts the cursor by delta rows and keeps it on screen.
func (f *frame) move(delta, height int) { f.moveTo(f.cursor+delta, height) }

// moveTo puts the cursor on visible row i, clamped, and scrolls so it is
// on screen.
func (f *frame) moveTo(i, height int) {
	if len(f.visible) == 0 {
		f.cursor, f.top = 0, 0
		return
	}
	f.cursor = min(max(i, 0), len(f.visible)-1)
	height = max(height, 1)
	if f.cursor < f.top {
		f.top = f.cursor
	}
	if f.cursor >= f.top+height {
		f.top = f.cursor - height + 1
	}
}

// currentRow is the row under the cursor, nil when there is none.
func (f *frame) currentRow() *Row {
	if f.cursor < 0 || f.cursor >= len(f.visible) {
		return nil
	}
	return &f.rows[f.visible[f.cursor]]
}

// applyFilter recomputes visible from filter, keeping the cursor on its
// row when that row survives and moving it to the first row otherwise.
func (f *frame) applyFilter() {
	prev := -1
	if r := f.currentRow(); r != nil {
		prev = f.visible[f.cursor]
	}
	f.visible = f.visible[:0]
	needle := strings.ToLower(f.filter)
	for i, r := range f.rows {
		if needle == "" || strings.Contains(strings.ToLower(r.Name), needle) || strings.Contains(strings.ToLower(r.Detail), needle) {
			f.visible = append(f.visible, i)
		}
	}
	f.cursor, f.top = 0, 0
	for j, i := range f.visible {
		if i == prev {
			f.cursor = j
			break
		}
	}
}

// crumbs is the breadcrumb: the repository, then inside an image its
// crumb, the active view's name and the crumbs of the frames above the
// view's root.
func (m *model) crumbs() []string {
	if m.img == nil {
		if len(m.base) == 1 {
			return []string{m.base[0].node.Crumb()}
		}
		c := make([]string, 0, len(m.base)-1)
		for _, f := range m.base[1:] {
			c = append(c, f.node.Crumb())
		}
		return c
	}
	c := []string{m.base[len(m.base)-1].node.Crumb(), m.img.crumb, "storage"}
	if m.img.active == stackFS {
		c[2] = "filesystem"
	}
	for _, f := range m.activeStack()[1:] {
		if s := f.node.Crumb(); s != "" {
			c = append(c, s)
		}
	}
	return c
}

func (m *model) View() string {
	f := m.top()
	crumbs := m.crumbs()
	if _, isFile := f.node.(Opener); isFile {
		return RenderViewer(m.viewerView(f, crumbs), m.width, m.height)
	}
	return RenderList(m.listView(f, crumbs), m.width, m.height)
}

func (m *model) listHints() string {
	hints := "enter open · backspace back · / filter · i info"
	if m.img != nil {
		if m.img.active == stackFS {
			hints += " · f storage"
		} else {
			hints += " · f filesystem"
		}
	}
	return hints + " · q quit"
}

// listView gathers the rows on screen and the chrome of a listing frame.
func (m *model) listView(f *frame, crumbs []string) listView {
	h := m.bodyHeight()
	f.moveTo(f.cursor, h)
	end := min(f.top+h, len(f.visible))
	rows := make([]Row, 0, max(end-f.top, 0))
	for _, idx := range f.visible[f.top:end] {
		rows = append(rows, f.rows[idx])
	}
	v := listView{
		Crumbs:  crumbs,
		Rows:    rows,
		Cursor:  f.cursor - f.top,
		Count:   len(f.visible),
		Total:   len(f.rows),
		Loading: !f.loaded,
		Filter:  f.filter,
		Status:  m.status,
		Hints:   m.listHints(),
		Popup:   m.popup,
	}
	if len(rows) == 0 {
		v.Cursor = -1
	}
	if m.inputKind != inputNone {
		v.Input = m.input.View()
	}
	return v
}

// viewerView renders a viewer frame's body and status.
func (m *model) viewerView(f *frame, crumbs []string) viewerView {
	v := viewerView{Crumbs: crumbs, Loading: f.view == nil}
	if m.inputKind != inputNone {
		v.Input = m.input.View()
	}
	if f.view == nil {
		return v
	}
	vw := f.view
	var parts []string
	if m.status != "" {
		parts = append(parts, styleErr.Render(m.status))
	}
	switch vw.mode {
	case modeText:
		vw.top = min(max(vw.top, 0), max(len(vw.lines)-1, 0))
		hits := make(map[int]bool, len(vw.hits))
		for _, i := range vw.hits {
			hits[i] = true
		}
		v.Body = RenderText(vw.lines, vw.top, vw.left, m.bodyHeight(), m.width, hits)
		parts = append(parts, vw.text.Label, tui.FormatBytes(f.file.Size), plural(len(vw.lines), "line"))
		if vw.text.Pretty != nil {
			if vw.pretty {
				parts = append(parts, "pretty")
			} else {
				parts = append(parts, "raw")
			}
		}
		if vw.search != "" {
			parts = append(parts, fmt.Sprintf("%s for %q", plural(len(vw.hits), "hit"), vw.search))
		}
	default:
		v.Body = m.hexBody(f)
		pct := 0.0
		if f.file.Size > 0 {
			pct = 100 * float64(vw.hexOff) / float64(f.file.Size)
		}
		parts = append(parts, "hex", tui.FormatBytes(f.file.Size), fmt.Sprintf("offset %#x · %.1f%%", vw.hexOff, pct))
	}
	for _, kv := range f.file.Labels {
		parts = append(parts, kv.Key+" "+kv.Value)
	}
	hints := ": goto · h text · g/G ends · esc back"
	if vw.mode == modeText {
		hints = "h hex · / search · n/N hits · ←/→ scroll · esc back"
		if vw.text.Pretty != nil {
			hints = "p raw/pretty · " + hints
		}
	}
	parts = append(parts, styleDim.Render(hints))
	v.Status = strings.Join(parts, " · ")
	return v
}

// hexBody renders the hex rows on screen from the loaded window.
func (m *model) hexBody(f *frame) string {
	v := f.view
	if f.file.Size == 0 {
		return styleDim.Render("  (empty file)")
	}
	off := v.hexOff - v.winStart
	if v.win == nil || off < 0 || off > int64(len(v.win)) {
		return styleDim.Render("  loading…")
	}
	end := min(off+int64(m.bodyHeight())*hexRowBytes, int64(len(v.win)))
	return RenderHex(v.hexOff, v.win[off:end], m.width)
}
```

- [ ] **Step 4: Write `browse/run.go`**

```go
package browse

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// Options configure Run.
type Options struct {
	Store  *store.Store
	Blobs  *blob.Store // blob.NewReadOnly over Store is enough
	Images *image.Store
	Start  string // "", "repo", "repo:tag" or "repo@digest"
}

// Run resolves Start, then runs the browser in the alternate screen until
// q, ctrl-c or ctx is done. A start reference that does not exist is
// returned before anything is drawn. The caller owns signals: a SIGINT
// that cancels ctx ends the program and returns nil. A terminal failure
// is returned as a *tui.TerminalError.
func Run(ctx context.Context, opts Options) error {
	m, err := newModel(New(opts.Store, opts.Blobs, opts.Images), opts.Start)
	if err != nil {
		return err
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithoutSignalHandler())
	if _, err := p.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return &tui.TerminalError{Err: err}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./browse`
Expected: PASS. Then `go vet ./browse` and `gofmt -l browse` (no output).

- [ ] **Step 6: Commit**

```bash
git add browse/model.go browse/run.go browse/model_test.go
git commit -m "browse: Bubble Tea model with two stacks per image, viewer and Run"
```

---

### Task 10: The `browse` subcommand

**Files:**
- Create: `cmd/oci-amber/browse.go`
- Modify: `cmd/oci-amber/main.go` (`newApp` signature, the command table, `main`)
- Modify: `cmd/oci-amber/app_test.go:33-47`, `cmd/oci-amber/import_test.go:22-37` (the `newApp` calls gain a third argument)
- Test: `cmd/oci-amber/browse_test.go`

**Interfaces:**
- Consumes: `browse.Run`, `browse.Options` (Task 9), `blob.NewReadOnly` (Task 2), `store.ErrInUse` (Task 1).
- Produces: `type browseConfig struct { Store, LogFile, Start string; Stdout, Stderr io.Writer }`, `func browseFlags() []cli.Flag`, `func browseConfigFromCLI(c *cli.Context) (browseConfig, error)`, `func runBrowse(ctx context.Context, cfg browseConfig) error`; `newApp(serve, imp, brw)`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/oci-amber/browse_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/store"
)

// runBrowseApp runs the browse command with args and returns the config
// the action received, or the error the app returned.
func runBrowseApp(t *testing.T, args ...string) (browseConfig, error) {
	t.Helper()
	var got browseConfig
	called := false
	app := newApp(
		func(context.Context, config) error { return nil },
		func(context.Context, importConfig) error { return nil },
		func(_ context.Context, cfg browseConfig) error {
			called = true
			got = cfg
			return nil
		})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "browse"}, args...))
	if err == nil && !called {
		t.Fatal("browse action was not called")
	}
	return got, err
}

func TestBrowseFlags(t *testing.T) {
	clearEnv(t)
	cfg, err := runBrowseApp(t, "--store", "/srv/amber")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if want := (browseConfig{Store: "/srv/amber"}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	cfg, err = runBrowseApp(t, "--store", "/srv/amber", "--log-file", "/tmp/browse.log", "library/app:v1")
	if err != nil {
		t.Fatalf("browse with args: %v", err)
	}
	if cfg.Start != "library/app:v1" || cfg.LogFile != "/tmp/browse.log" {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := runBrowseApp(t, "--store", "/srv/amber", "a", "b"); err == nil || !strings.Contains(err.Error(), "at most one reference") {
		t.Errorf("two references: %v", err)
	}
	if _, err := runBrowseApp(t); err == nil {
		t.Error("--store is required")
	}
	t.Setenv("OCI_AMBER_STORE", "/env/store")
	if cfg, err := runBrowseApp(t); err != nil || cfg.Store != "/env/store" {
		t.Errorf("store from the environment: %+v, %v", cfg, err)
	}
}

func TestRunBrowseRefusesMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := runBrowse(context.Background(), browseConfig{Store: dir, Stdout: &bytes.Buffer{}, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "no oci-amber store at "+dir) {
		t.Fatalf("missing store: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("browse must not create a store directory")
	}
}

func TestRunBrowseNeedsTerminal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	err = runBrowse(context.Background(), browseConfig{Store: dir, Stdout: &bytes.Buffer{}, Stderr: io.Discard})
	if err == nil || err.Error() != "browse needs a terminal" {
		t.Fatalf("not a terminal: %v", err)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./cmd/oci-amber -run 'Browse'`
Expected: compile errors (`undefined: browseConfig`, wrong argument count to `newApp`).

- [ ] **Step 3: Write `cmd/oci-amber/browse.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v2"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/browse"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/store"
)

// browseConfig is everything `browse` needs. browseConfigFromCLI fills it
// from flags; tests construct it directly and call runBrowse.
type browseConfig struct {
	Store   string
	LogFile string
	Start   string    // "", repo, repo:tag or repo@digest
	Stdout  io.Writer // nil means os.Stdout; must be a terminal
	Stderr  io.Writer // nil means os.Stderr
}

func browseFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "store", Usage: "store `directory` (required)", EnvVars: envVar("store"), Required: true},
		&cli.StringFlag{Name: "log-file", Usage: "write the log to `path`; without it warnings are printed after the screen closes", EnvVars: envVar("log-file")},
	}
}

func browseConfigFromCLI(c *cli.Context) (browseConfig, error) {
	if c.NArg() > 1 {
		return browseConfig{}, errors.New("browse takes at most one reference (repo, repo:tag or repo@digest)")
	}
	cfg := browseConfig{Store: c.String("store"), LogFile: c.String("log-file"), Start: c.Args().First()}
	if cfg.Store == "" {
		return browseConfig{}, errors.New("--store must not be empty")
	}
	return cfg, nil
}

// runBrowse checks that the store exists and stdout is a terminal, opens
// the store without touching the work directory and runs the browser
// until it quits. Nothing is written to the store: a missing store is an
// error rather than created, and the blob store is read-only.
func runBrowse(ctx context.Context, cfg browseConfig) error {
	stdout, stderr := cfg.Stdout, cfg.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if _, err := os.Stat(filepath.Join(cfg.Store, store.ConfigFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no oci-amber store at %s", cfg.Store)
		}
		return fmt.Errorf("checking store %s: %w", cfg.Store, err)
	}
	if f, ok := stdout.(*os.File); !ok || !term.IsTerminal(f.Fd()) {
		return errors.New("browse needs a terminal")
	}

	// Logging: a file gets everything; otherwise warnings and errors are
	// kept for after the screen is gone, as the import TUI does.
	var deferred bytes.Buffer
	var logOut io.Writer = &deferred
	level := slog.LevelWarn
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		logOut, level = f, slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	defer func() {
		if deferred.Len() > 0 {
			io.Copy(stderr, &deferred)
		}
	}()

	st, err := store.Open(cfg.Store, store.Options{Logger: log})
	if errors.Is(err, store.ErrInUse) {
		return fmt.Errorf("store %s is in use by another process", cfg.Store)
	}
	if err != nil {
		return fmt.Errorf("opening store %s: %w", cfg.Store, err)
	}
	blobs := blob.NewReadOnly(st, log)
	images := image.New(st, blobs, log)
	runErr := browse.Run(ctx, browse.Options{Store: st, Blobs: blobs, Images: images, Start: cfg.Start})
	if err := st.Close(); err != nil {
		return errors.Join(runErr, fmt.Errorf("closing store: %w", err))
	}
	return runErr
}
```

- [ ] **Step 4: Wire the command in `cmd/oci-amber/main.go`**

Change the package comment's first sentence to mention the third command:

```go
// Command oci-amber runs an OCI distribution registry whose storage is an
// embedded amber store. The `serve` subcommand runs the registry, `import`
// stores a `docker image save` archive without running it, and `browse`
// walks a store in the terminal.
```

Change `main` to `newApp(serve, runImport, runBrowse)`. Change `newApp`'s signature and doc:

```go
// newApp builds the command line application. serve, imp and brw are what
// the `serve`, `import` and `browse` subcommands run once their flags have
// been validated; main passes run, runImport and runBrowse, tests pass
// functions that capture the config.
func newApp(serve func(ctx context.Context, cfg config) error, imp func(ctx context.Context, cfg importConfig) error, brw func(ctx context.Context, cfg browseConfig) error) *cli.App {
```

and append to `Commands`, after the `import` entry:

```go
		}, {
			Name:      "browse",
			Usage:     "browse a store in the terminal: images, how they are stored, their root filesystems",
			ArgsUsage: "[repo[:tag|@digest]]",
			Flags:     browseFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := browseConfigFromCLI(c)
				if err != nil {
					return err
				}
				return brw(c.Context, cfg)
			},
		}},
```

In `cmd/oci-amber/app_test.go` (`runApp`) and `cmd/oci-amber/import_test.go` (`runImportApp`) add the third argument to the `newApp` calls:

```go
		func(context.Context, browseConfig) error { return nil })
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./cmd/oci-amber`
Expected: PASS (the crane smoke test runs too when `crane` is on PATH; it is inside `nix develop`).

- [ ] **Step 6: Commit**

```bash
git add cmd/oci-amber/browse.go cmd/oci-amber/browse_test.go cmd/oci-amber/main.go cmd/oci-amber/app_test.go cmd/oci-amber/import_test.go
git commit -m "cmd/oci-amber: browse subcommand"
```

---

### Task 11: Documentation, a real-terminal smoke test, pull request

**Files:**
- Modify: `README.md`, `docs/superpowers/specs/2026-09-05-browse-command-design.md`
- Modify: `browse/fixture_test.go` (an env-gated fixture writer for the smoke test)

- [ ] **Step 1: Let the fixture be written to a chosen directory**

In `browse/fixture_test.go` rename `newFixture` to `newFixtureIn(t *testing.T, dir string) *fixture` (replace `dir := t.TempDir()` by the parameter) and add:

```go
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
```

(add `"os"` to the imports). Run `go test ./browse` to see everything still passes and `TestWriteFixture` is skipped.

- [ ] **Step 2: README**

In the opening paragraph, after "and imports `docker image save` archives directly", add ", and has a terminal browser over the store". Then insert this section after "## Importing a docker save archive" and before "## Configuration":

````markdown
## Browsing a store

`oci-amber browse` opens a store in a terminal browser: the repositories
and their tags, how every image is stored, the root filesystem its layers
produce, and the content of any file it reaches.

```sh
./oci-amber browse --store /var/lib/oci-amber
./oci-amber browse --store /var/lib/oci-amber library/app:v1
```

It opens the store directly, so it cannot run while `serve` has it open
(`store … is in use by another process`), and it never writes: the work
directory is not touched and a missing store is an error, not created.
Stdout must be a terminal. `--log-file path` keeps the log; without it,
warnings are printed to stderr after the screen closes. The optional
argument starts inside a repository (`repo`) or an image (`repo:tag`,
`repo@sha256:…`).

The screen is one listing at a time under a breadcrumb. The repositories
come first; a repository lists its tags (kind, short digest, size, rootfs
status when not `ok`) and then the manifests no tag points at. An image
opens in the **storage** view: the amber tree under its root, annotated
with what oci-amber knows about it.

```
library/app › :v1 › storage
  blobs/          3 blobs
  manifest        application/vnd.oci.image.manifest.v1+json    1.2 KiB
  meta.json       kind, digest, stats and rootfs status          812 B
  rootfs/         ok · 1,204 entries
```

`blobs/` lists the config and layers in manifest order with how each is
stored (`layer 1/2 · prism gzip go-flate · 1,834 files · 402.1 MiB
uncompressed`, `config · raw not-tar`); a blob opens its root
(`meta.json`, `comp.json`, `recipe.json`, `recipe.bin`, `blobs/`, or
`meta.json` and `raw`), and a prism's `blobs/` shows every numbered file
next to the tar entry it holds. An index's `manifests/` lists its
children by platform. `rootfs/` is the stored tree as it is, in `ls -l`
columns, symlinks shown but not followed.

`f` switches to the **filesystem** view of the image the breadcrumb is
under: the same tree through the resolver the `/fs/` API uses, so
symlinks are followed (Enter on a symlink to a directory descends under
the link's own name). An index offers its platforms first; an image
without a view says why. `f` again returns to the storage view where it
was; both views keep their position.

Enter on a file opens the viewer. Text (valid UTF-8 without NUL bytes) is
shown with line numbers; JSON is pretty-printed and `p` shows the stored
bytes; anything else is a hex dump that reads only the window on screen,
so a 2 GiB blob opens at once, and `:` jumps to an offset. `h` switches
between text and hex; files over 8 MiB are hex only.

| Key | Action |
|---|---|
| `↑` `↓` `pgup` `pgdn` `g` `G` | move |
| `enter` `→` | open the row |
| `backspace` `←` `esc` | back |
| `f` | storage ↔ filesystem |
| `/` | filter the listing; in the viewer, search (`n`/`N` between hits) |
| `i` | full digest, key, mode, owner, mtime and more for the row |
| `h` | text ↔ hex in the viewer; back in a listing |
| `p` | JSON pretty ↔ stored bytes |
| `:` | go to an offset in hex |
| `q` `ctrl-c` | quit |
````

- [ ] **Step 3: Record the implementation decisions in the spec**

Append to the "Decisions taken during the design review" list in `docs/superpowers/specs/2026-09-05-browse-command-design.md` a second list:

```markdown
Decisions taken during implementation (2026-09-05):

- Backspace at the root of the filesystem stack returns to the storage
  stack where it was, instead of leaving the image; leaving happens from
  the storage root. Popping frames after a failed load follows the same
  rule.
- The repository listing's detail is `N tags · M manifests` rather than
  an untagged count: telling untagged manifests apart needs every tag's
  meta.json, which the repository's own listing reads anyway.
- In a listing `h` is back and `l` is open (vi keys); in the viewer `h`
  toggles hex and `←`/`→` scroll text horizontally, so back there is
  Backspace or Esc.
- A prism's `blobs/` rows read "N files" from meta.json's `entries`, the
  count of regular files.
- The "store in use" path is covered by `store.ErrInUse`'s test; at the
  command level the terminal check runs before the store is opened, so a
  test without a terminal cannot reach it.
```

- [ ] **Step 4: Smoke test with the real binary under a pseudo-terminal**

```bash
export OCI_AMBER_BROWSE_FIXTURE=/private/tmp/claude-502/-Users-dragan-draganm-oci-amber/6009937b-f5a9-4135-b065-1626b421c2fe/scratchpad/fixture-store
rm -rf "$OCI_AMBER_BROWSE_FIXTURE"
go test ./browse -run TestWriteFixture -count=1
go build -o /private/tmp/claude-502/-Users-dragan-draganm-oci-amber/6009937b-f5a9-4135-b065-1626b421c2fe/scratchpad/oci-amber ./cmd/oci-amber
```

Write `smoke.py` in the scratchpad and run it with `python3`:

```python
import fcntl, os, pty, select, struct, sys, termios, time

BIN = sys.argv[1]
STORE = os.path.join(sys.argv[2], "store")
pid, fd = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    os.execv(BIN, [BIN, "browse", "--store", STORE])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))

def read_for(sec):
    out, end = b"", time.time() + sec
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            try:
                out += os.read(fd, 65536)
            except OSError:
                break
    return out

out = read_for(2.0)
for key in [b"\r", b"j", b"\r", b"f", b"j", b"\r", b"G", b"\r", b"h", b"q"]:
    os.write(fd, key)
    out += read_for(0.6)
_, status = os.waitpid(pid, 0)
text = out.decode("utf-8", "replace")
for want in ["library/app", "storage", "filesystem", "os-release", "PRETTY_NAME", "50 52 45 54"]:
    print(("ok  " if want in text else "MISSING  ") + want)
print("exit status", status)
```

Run: `python3 smoke.py "$SCRATCH/oci-amber" "$OCI_AMBER_BROWSE_FIXTURE"`; every line must say `ok` and the exit status must be 0. Also run the binary without a store (`./oci-amber browse --store /nonexistent`) and expect `oci-amber: no oci-amber store at /nonexistent`. Then delete the binary and the fixture store.

- [ ] **Step 5: Full verification**

```bash
gofmt -l .            # no output
go vet ./...
go test -race ./...
```

- [ ] **Step 6: Commit and open the pull request**

```bash
git add README.md docs/superpowers/specs/2026-09-05-browse-command-design.md browse/fixture_test.go
git commit -m "docs: browse command"
git push -u origin browse-command
gh pr create --title "browse: terminal browser over a store" --body "..."
```

The PR body: what the command does (three paragraphs at most), the three
supporting changes (`store.ErrInUse`, `blob.NewReadOnly`,
`image.Store.Manifests`), the test plan (unit tests per package, the pty
smoke test), and the required trailer:

```
🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
```

---

## Summary

| Task | Delivers |
|---|---|
| 1 | `store.ErrInUse` |
| 2 | `blob.NewReadOnly`, `blob.ErrReadOnly` |
| 3 | `image.Store.Manifests` |
| 4 | `browse` skeleton, fixture, file/raw-dir/blob-root/prism-blobs nodes |
| 5 | image roots, `blobs/`, `manifests/`, repositories, Browser entry points |
| 6 | filesystem view with symlink following, index chooser, unavailable rootfs |
| 7 | classification, text lines, hex dump, window reads |
| 8 | list, viewer and popup rendering |
| 9 | Bubble Tea model, two stacks per image, viewer keys, `Run` |
| 10 | `oci-amber browse` subcommand |
| 11 | README, spec decisions, pty smoke test, PR |

## Test plan

- `go test -race ./...` green after every task.
- `browse` node tests over a real fixture store (prism and raw blobs, an index, an untagged manifest, symlinks, a whiteout).
- Pure rendering tests compare exact strings (lipgloss is plain under `go test`).
- Model tests drive `Update` with key messages and execute commands synchronously.
- Command tests cover flags, a missing store and a missing terminal.
- A pty smoke test walks repositories → image → filesystem → a file → hex with the real binary.
