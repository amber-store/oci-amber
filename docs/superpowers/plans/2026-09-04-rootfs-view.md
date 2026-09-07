# Root filesystem view Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every pushed image manifest gets a `rootfs/` directory in its image root: the container's root filesystem, built from the stored layers by replaying tar headers from each prism's recipe and pointing regular files at the content keys the prism already holds.

**Architecture:** A new `rootfs` package turns a prism's `recipe.bin` + `recipe.json` into a virtual tar that `archive/tar` reads header by header (content regions are skipped with `Seek`), merges the layers into an in-memory tree with OCI whiteout semantics and kernel-like symlink resolution, and emits the tree as an amber fstree with full metadata through `store.Dir.AddEntry`. `image.Put` calls it for image manifests and records a `rootfs` status in `meta.json`; failures to parse never fail a push.

**Tech Stack:** Go 1.26.6 via the Nix flake (`nix develop --command go ...`), `archive/tar`, amber-store-core `fstree`/`cborx`/`tarexport`, tar-prism `Index`/`Entry`, log/slog.

**Spec:** docs/superpowers/specs/2026-09-04-rootfs-view-design.md

## Global Constraints

- Module `github.com/amber-store/oci-amber`; run every go command as `nix develop --command go ...` from the repository root.
- Top-level packages only, no `internal/`. New package: `rootfs`. Import direction: `image -> rootfs -> store -> oci`, `image -> blob -> store`; `rootfs` must not import `blob`.
- Existing directory entries keep Mode `0o100644` / `0o040755` and zero uid, gid, mtime. Only entries under `rootfs/` carry real metadata.
- Whiteout semantics per the OCI image spec: `.wh.<name>` removes `<name>` from lower layers, `.wh..wh..opq` removes all lower children, whiteouts never apply to their own layer and never appear in the tree.
- Symlink resolution: parent components follow symlinks inside the rootfs, absolute targets start at the root, `..` at the root stays at the root, more than 40 hops skips the entry. The final component is never resolved through a symlink.
- Rootfs applicability: `kind = manifest` with config media type `application/vnd.oci.image.config.v1+json` or `application/vnd.docker.container.image.v1+json`. Indexes: no field. Other manifests: `not-applicable`.
- Skips are capped at `MaxSkipped = 100` stored records; `skippedCount` is the total.
- Xattr inline threshold: 256 bytes of canonical encoding (`store.XattrInlineMax`).
- Tests use `t.TempDir()`, never the network, and never leave binaries behind.
- Commit after every task with message style `<area>: <what>` and the trailer lines:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
  ```
- Run `nix develop --command go vet ./...` before every commit.

---

## File structure

```
store/dir.go            + type-bit constants, Dir.AddEntry, payload validation
store/write.go          + XattrInlineMax, Writer.PutXattrs
store/dir_test.go       + AddEntry tests
store/write_test.go     + PutXattrs tests
oci/manifest.go         + MediaTypeDockerConfig
blob/prism.go           amberSource -> Prism (exported), BlobKey, (*Blob).Prism, ErrNotPrism
blob/analyze.go         probeTar accepts an empty archive (all-zero blocks)
blob/analyze_test.go    + empty archive cases
blob/store_test.go      + TestPutEmptyArchive
rootfs/layer.go         Layer, LayerError, storeError, splice reader, header parsing, entry
rootfs/tree.go          node, tree, path resolution, put/link/whiteout/opaque
rootfs/builder.go       Builder, Apply, Write, Result, Skip, MaxSkipped
rootfs/layer_test.go    splice + parse tests
rootfs/tree_test.go     merge rule tests on the tree directly
rootfs/builder_test.go  end-to-end layer tests, export check, determinism
image/meta.go           + RootfsDir, RootfsStatus, Rootfs, Meta.Rootfs
image/rootfs.go         rootfsApplies, buildRootfs, reuseRootfs, logRootfs
image/store.go          Put integration, Image.Rootfs, Open lookup
image/stats.go          log keys
image/rootfs_test.go    status, reuse, determinism, log tests
registry/e2e_test.go    rootfs walk of image 1
README.md, docs/followups.md, spec deviations
```

---

### Task 1: store: typed directory entries and xattrs

**Files:**
- Modify: `store/dir.go`
- Modify: `store/write.go`
- Test: `store/dir_test.go`, `store/write_test.go`

**Interfaces:**
- Produces: `store.TypeMask, TypeFIFO, TypeChar, TypeDir, TypeBlock, TypeReg, TypeLink, TypeSocket uint64`; `func (d *Dir) AddEntry(e fstree.Entry) error`; `const XattrInlineMax = 256`; `func (w *Writer) PutXattrs(m map[string][]byte) (inline []byte, spilled key.Key, err error)`.

- [ ] **Step 1: Write the failing tests**

Append to `store/dir_test.go`:

```go
func TestDirAddEntryTypes(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	file, err := w.PutBytes([]byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	sub := w.NewDir()
	subKey, err := sub.Finish()
	if err != nil {
		t.Fatal(err)
	}
	d := w.NewDir()
	entries := []fstree.Entry{
		{Name: []byte("blk"), Mode: TypeBlock | 0o660, Rdev: []uint64{8, 1}},
		{Name: []byte("chr"), Mode: TypeChar | 0o666, UID: 5, GID: 7, Mtime: 12345, Rdev: []uint64{1, 3}},
		{Name: []byte("dir"), Mode: TypeDir | 0o700, ContentKey: subKey[:]},
		{Name: []byte("fifo"), Mode: TypeFIFO | 0o600},
		{Name: []byte("file"), Mode: TypeReg | 0o4755, UID: 1000, GID: 1000, Mtime: -5, ContentKey: file[:], XattrsIn: cborx.EncodeXattrs(map[string][]byte{"user.k": []byte("v")})},
		{Name: []byte("link"), Mode: TypeLink | 0o777, LinkTarget: []byte("../file")},
		{Name: []byte("sock"), Mode: TypeSocket | 0o755},
	}
	for _, e := range entries {
		if err := d.AddEntry(e); err != nil {
			t.Fatalf("AddEntry(%s): %v", e.Name, err)
		}
	}
	root, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range entries {
		got := lookupEntry(t, s, root, string(want.Name))
		if got.Mode != want.Mode || got.UID != want.UID || got.GID != want.GID || got.Mtime != want.Mtime ||
			!bytes.Equal(got.ContentKey, want.ContentKey) || !bytes.Equal(got.LinkTarget, want.LinkTarget) ||
			!slices.Equal(got.Rdev, want.Rdev) || !bytes.Equal(got.XattrsIn, want.XattrsIn) {
			t.Fatalf("%s: stored %+v, want %+v", want.Name, got, want)
		}
	}
}

func TestDirAddEntryRejects(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	file, err := w.PutBytes([]byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	sub := w.NewDir()
	subKey, err := sub.Finish()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		e    fstree.Entry
	}{
		{"file without content", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644}},
		{"file with dir key", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: subKey[:]}},
		{"dir with file key", fstree.Entry{Name: []byte("a"), Mode: TypeDir | 0o755, ContentKey: file[:]}},
		{"file with link target", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: file[:], LinkTarget: []byte("x")}},
		{"symlink without target", fstree.Entry{Name: []byte("a"), Mode: TypeLink | 0o777}},
		{"symlink with content", fstree.Entry{Name: []byte("a"), Mode: TypeLink | 0o777, LinkTarget: []byte("x"), ContentKey: file[:]}},
		{"char without rdev", fstree.Entry{Name: []byte("a"), Mode: TypeChar | 0o600}},
		{"block with one rdev", fstree.Entry{Name: []byte("a"), Mode: TypeBlock | 0o600, Rdev: []uint64{1}}},
		{"fifo with content", fstree.Entry{Name: []byte("a"), Mode: TypeFIFO | 0o600, ContentKey: file[:]}},
		{"no type bits", fstree.Entry{Name: []byte("a"), Mode: 0o644, ContentKey: file[:]}},
		{"bad name", fstree.Entry{Name: []byte("a/b"), Mode: TypeReg | 0o644, ContentKey: file[:]}},
		{"both xattr forms", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: file[:], XattrsIn: []byte{0xa0}, XattrsKey: file[:]}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := w.NewDir()
			if err := d.AddEntry(c.e); err == nil {
				t.Fatalf("AddEntry accepted %+v", c.e)
			}
		})
	}
	d := w.NewDir()
	if err := d.AddEntry(fstree.Entry{Name: []byte("b"), Mode: TypeReg | 0o644, ContentKey: file[:]}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddEntry(fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: file[:]}); err == nil {
		t.Fatal("AddEntry accepted an out-of-order name")
	}
}
```

Add `"bytes"`, `"slices"` and `"github.com/jobs-build/amber-store-core/cborx"` to the imports of `store/dir_test.go`.

Append to `store/write_test.go`:

```go
func TestPutXattrsInlineAndSpilled(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	inline, spilled, err := w.PutXattrs(nil)
	if err != nil || inline != nil || spilled != (key.Key{}) {
		t.Fatalf("PutXattrs(nil) = %v, %s, %v; want nothing", inline, spilled, err)
	}
	small := map[string][]byte{"security.capability": []byte("\x01\x00\x00\x02")}
	inline, spilled, err = w.PutXattrs(small)
	if err != nil {
		t.Fatal(err)
	}
	if spilled != (key.Key{}) || !bytes.Equal(inline, cborx.EncodeXattrs(small)) {
		t.Fatalf("small set: inline %x, spilled %s", inline, spilled)
	}
	large := map[string][]byte{"user.big": bytes.Repeat([]byte("x"), XattrInlineMax)}
	inline, spilled, err = w.PutXattrs(large)
	if err != nil {
		t.Fatal(err)
	}
	if inline != nil || spilled == (key.Key{}) || spilled.Type() != key.XattrSet {
		t.Fatalf("large set: inline %x, spilled %s", inline, spilled)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Get(spilled)
	if err != nil {
		t.Fatalf("spilled XattrSet not stored: %v", err)
	}
	got, err := cborx.DecodeXattrs(data)
	if err != nil || !bytes.Equal(got["user.big"], large["user.big"]) {
		t.Fatalf("decoded %v, %v", got, err)
	}
}
```

Add `"bytes"` and `"github.com/jobs-build/amber-store-core/cborx"` to the imports of `store/write_test.go` if absent.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./store/ -run 'TestDirAddEntry|TestPutXattrs' 2>&1 | head -20`
Expected: compile errors, `undefined: TypeBlock`, `d.AddEntry undefined`, `w.PutXattrs undefined`.

- [ ] **Step 3: Implement**

In `store/dir.go`, add after the `errDirFinished` var:

```go
// POSIX file type bits of fstree.Entry.Mode. They are the standard values,
// defined here so that store does not depend on x/sys.
const (
	TypeMask   uint64 = 0o170000
	TypeFIFO   uint64 = 0o010000
	TypeChar   uint64 = 0o020000
	TypeDir    uint64 = 0o040000
	TypeBlock  uint64 = 0o060000
	TypeReg    uint64 = 0o100000
	TypeLink   uint64 = 0o120000
	TypeSocket uint64 = 0o140000
)
```

Replace `add` and add `AddEntry` and `validatePayload`:

```go
func (d *Dir) add(name string, mode uint64, content key.Key) error {
	return d.AddEntry(fstree.Entry{Name: []byte(name), Mode: mode, ContentKey: content[:]})
}

// AddEntry adds e with the metadata it carries: mode, uid, gid, mtime and
// xattrs are stored as given. The name is validated and ordered like
// AddFile's. The payload must match the type bits of e.Mode: a file content
// key (Blob or FileNode) for TypeReg, a directory key (DirLeaf or DirNode)
// for TypeDir, a non-empty LinkTarget for TypeLink, exactly two Rdev numbers
// for TypeChar and TypeBlock, and none of those for TypeFIFO and TypeSocket.
// Any other type bits are an error, as are both xattr forms at once.
func (d *Dir) AddEntry(e fstree.Entry) error {
	if d.done {
		return errDirFinished
	}
	name := string(e.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("store: invalid dir entry name %q", name)
	}
	if d.n > 0 && name <= d.last {
		return fmt.Errorf("store: dir entry %q not sorted after %q", name, d.last)
	}
	if err := validatePayload(e); err != nil {
		return fmt.Errorf("store: dir entry %q: %w", name, err)
	}
	if err := d.db.AddEntry(d.w.Emit, e); err != nil {
		return err
	}
	d.last = name
	d.n++
	return nil
}

// validatePayload checks that e carries exactly the payload its type bits
// call for.
func validatePayload(e fstree.Entry) error {
	hasContent, hasLink, hasRdev := len(e.ContentKey) > 0, len(e.LinkTarget) > 0, len(e.Rdev) > 0
	switch typ := e.Mode & TypeMask; typ {
	case TypeReg, TypeDir:
		if hasLink || hasRdev {
			return errors.New("a file or directory carries only a content key")
		}
		k, err := key.Parse(e.ContentKey)
		if err != nil {
			return fmt.Errorf("content key: %w", err)
		}
		if t := k.Type(); typ == TypeReg && t != key.Blob && t != key.FileNode {
			return fmt.Errorf("content key %s is a %s, not file content", k, t)
		} else if typ == TypeDir && t != key.DirLeaf && t != key.DirNode {
			return fmt.Errorf("content key %s is a %s, not a directory", k, t)
		}
	case TypeLink:
		if hasContent || hasRdev || !hasLink {
			return errors.New("a symlink carries exactly a link target")
		}
	case TypeChar, TypeBlock:
		if hasContent || hasLink || len(e.Rdev) != 2 {
			return errors.New("a device carries exactly [major, minor]")
		}
	case TypeFIFO, TypeSocket:
		if hasContent || hasLink || hasRdev {
			return errors.New("a fifo or socket carries no payload")
		}
	default:
		return fmt.Errorf("unsupported type bits %#o", typ)
	}
	if len(e.XattrsIn) > 0 && len(e.XattrsKey) > 0 {
		return errors.New("inline and spilled xattrs are exclusive")
	}
	return nil
}
```

`AddFile` and `AddDir` keep their own type checks and messages and still call `add`.

In `store/write.go`, add the import `"github.com/jobs-build/amber-store-core/cborx"` and, after `PutBytes`:

```go
// XattrInlineMax is the largest canonical encoding of an extended-attribute
// set that is kept inline in a directory entry; a larger set is spilled to
// an XattrSet object. It is amber's own ingest default.
const XattrInlineMax = 256

// PutXattrs prepares m for a directory entry: the canonical encoding is
// returned as inline when it is at most XattrInlineMax bytes, otherwise an
// XattrSet object is emitted and its key returned. An empty m yields
// neither. Safe for concurrent use.
func (w *Writer) PutXattrs(m map[string][]byte) (inline []byte, spilled key.Key, err error) {
	if len(m) == 0 {
		return nil, key.Key{}, nil
	}
	enc := cborx.EncodeXattrs(m)
	if len(enc) <= XattrInlineMax {
		return enc, key.Key{}, nil
	}
	obj, err := fstree.EncodeXattrSet(m)
	if err != nil {
		return nil, key.Key{}, err
	}
	if err := w.Emit(obj); err != nil {
		return nil, key.Key{}, err
	}
	return nil, obj.Key, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./store/ 2>&1 | tail -5`
Expected: `ok  	github.com/amber-store/oci-amber/store`

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./store/
git add store/dir.go store/write.go store/dir_test.go store/write_test.go
git commit -m "store: typed directory entries with full metadata and xattrs" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 2: blob: expose a prism's parts

**Files:**
- Modify: `blob/prism.go` (`amberSource` and every use), `blob/pull.go`
- Modify: `blob/prism_test.go`, `blob/prism_fallback_test.go` if they name `amberSource`
- Test: `blob/prism_test.go`

**Interfaces:**
- Produces: `type Prism struct` with `Index() (*tarprism.Index, error)`, `Recipe() (io.ReadCloser, error)`, `Blob(index int, entry tarprism.Entry) (io.ReadCloser, error)`, `BlobKey(index int, entry tarprism.Entry) (key.Key, error)`; `var ErrNotPrism error`; `func (bl *Blob) Prism() (*Prism, error)`.

- [ ] **Step 1: Write the failing test**

Append to `blob/prism_test.go` (the file already has a store helper `newTestStore` and fixture helpers `tarBytes`, `gzipBytes`, `spoolOf` in `helpers_test.go`):

```go
func TestBlobPrismParts(t *testing.T) {
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	ctx := context.Background()
	content := textBytes(5000, 11)
	data := gzipBytes(t, tarBytes(t, "etc/motd", content), gzip.DefaultCompression)
	meta, err := b.Put(ctx, spoolOf(data))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %s, want prism", meta.Kind)
	}
	bl, err := b.Open(meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bl.Prism()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := p.Index()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Name != "etc/motd" {
		t.Fatalf("index entries = %+v", idx.Entries)
	}
	k, err := p.BlobKey(0, idx.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if int64(k.Length()) != idx.Entries[0].Size {
		t.Fatalf("blob key length %d, index size %d", k.Length(), idx.Entries[0].Size)
	}
	r, err := p.Blob(0, idx.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("blob content differs (%v)", err)
	}
	rc, err := p.Recipe()
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || len(recipe) == 0 || len(recipe)%512 != 0 {
		t.Fatalf("recipe is %d bytes (%v)", len(recipe), err)
	}
	if _, err := p.BlobKey(0, tarprism.Entry{Blob: "blobs/00000009", Size: 1}); err == nil {
		t.Fatal("BlobKey of an absent blob succeeded")
	}

	raw, err := b.Put(ctx, spoolOf([]byte(`{"architecture":"amd64"}`)))
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Open(raw.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rb.Prism(); !errors.Is(err, ErrNotPrism) {
		t.Fatalf("Prism() on a raw blob = %v, want ErrNotPrism", err)
	}
}
```

Ensure the imports of `blob/prism_test.go` include `"bytes"`, `"compress/gzip"`, `"context"`, `"errors"`, `"io"`, `tarprism "github.com/draganm/tar-prism"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop --command go test ./blob/ -run TestBlobPrismParts 2>&1 | head`
Expected: `bl.Prism undefined`.

- [ ] **Step 3: Implement**

In `blob/prism.go`, rename the type `amberSource` to `Prism` everywhere in the package (`grep -rn amberSource blob/`), including tests, and replace its doc and `Blob` method:

```go
// Prism is one prism blob's parts as tar-prism's Source: recipe.bin and
// every blob are streamed with store.Reader, recipe.json is read whole.
// BlobKey exposes a file's content key so that a tree can reference it
// without reading it.
type Prism struct {
	st     *store.Store
	recipe key.Key
	index  key.Key
	blobs  key.Key
}

// BlobKey returns the content key of entry's blob, checking that the stored
// length matches the index.
func (s *Prism) BlobKey(index int, entry tarprism.Entry) (key.Key, error) {
	name, ok := strings.CutPrefix(entry.Blob, tarprism.BlobsDir+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return key.Key{}, fmt.Errorf("entry %d (%s): blob path %q is not directly under %s/", index, entry.Name, entry.Blob, tarprism.BlobsDir)
	}
	k, err := s.st.LookupKey(s.blobs, name)
	if err != nil {
		return key.Key{}, fmt.Errorf("entry %d (%s): %w", index, entry.Name, err)
	}
	if int64(k.Length()) != entry.Size {
		return key.Key{}, fmt.Errorf("entry %d (%s): blob %s is %d bytes, index says %d", index, entry.Name, entry.Blob, k.Length(), entry.Size)
	}
	return k, nil
}

func (s *Prism) Blob(index int, entry tarprism.Entry) (io.ReadCloser, error) {
	k, err := s.BlobKey(index, entry)
	if err != nil {
		return nil, err
	}
	return s.st.NewReader(k), nil
}
```

In `blob/pull.go`, add:

```go
// ErrNotPrism reports a raw blob where a prism was needed.
var ErrNotPrism = errors.New("blob: not a prism")

// Prism returns the parts of a prism blob root. A raw blob returns an error
// wrapping ErrNotPrism that names the raw reason.
func (bl *Blob) Prism() (*Prism, error) {
	if bl.Meta.Kind != KindPrism {
		return nil, fmt.Errorf("%w: %s is stored %s (%s)", ErrNotPrism, bl.Meta.Digest, bl.Meta.Kind, bl.Meta.RawReason)
	}
	return bl.store.openSource(bl.root)
}
```

Add `"errors"` to `blob/pull.go`'s imports.

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./blob/ 2>&1 | tail -5`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./blob/
git add blob/
git commit -m "blob: export a prism's parts with per-file content keys" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 3: blob: an empty archive is a tar

**Files:**
- Modify: `blob/analyze.go:131-160` (`startsWithCompressedTarHeader`, `startsWithTarHeader`)
- Test: `blob/analyze_test.go`, `blob/store_test.go`

**Interfaces:**
- Produces: `func probeTar(r io.Reader) (bool, error)` (unexported), `const emptyArchiveMax = 10240`.

- [ ] **Step 1: Write the failing tests**

In `blob/analyze_test.go`, `TestAnalyzeClassifies`, add cases after `{"empty", ...}`:

```go
		{"empty tar", make([]byte, 1024), KindPrism, "", "none", false},
		{"gzipped empty tar", gzipBytes(t, make([]byte, 1024), gzip.DefaultCompression), KindPrism, "", "gzip", true},
		{"gnu padded empty tar", make([]byte, 10240), KindPrism, "", "none", false},
		{"zeros past a record", make([]byte, 10240+512), KindRaw, ReasonNotTar, "none", false},
		{"zero block then junk", append(make([]byte, 512), []byte("not a header")...), KindRaw, ReasonNotTar, "none", false},
```

Append to `blob/store_test.go`:

```go
func TestPutEmptyArchive(t *testing.T) {
	b, _, logs := newTestStore(t, Options{VerifyRoundTrip: true})
	ctx := context.Background()
	var buf bytes.Buffer
	if err := tar.NewWriter(&buf).Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, format string
		data         []byte
	}{
		{"plain", "none", buf.Bytes()},
		{"gzip", "gzip", gzipBytes(t, buf.Bytes(), gzip.DefaultCompression)},
	} {
		t.Run(c.name, func(t *testing.T) {
			meta, err := b.Put(ctx, spoolOf(c.data))
			if err != nil {
				t.Fatal(err)
			}
			if meta.Kind != KindPrism || meta.Entries != 0 || meta.Format != c.format {
				t.Fatalf("meta = kind %s entries %d format %s, want prism/0/%s", meta.Kind, meta.Entries, meta.Format, c.format)
			}
			bl, err := b.Open(meta.Digest)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := bl.WriteTo(ctx, &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), c.data) {
				t.Fatal("pulled bytes differ")
			}
			if !strings.Contains(logs.String(), "kind=prism") {
				t.Fatalf("log:\n%s", logs.String())
			}
		})
	}
}
```

Add `"archive/tar"`, `"bytes"`, `"compress/gzip"`, `"strings"` to `blob/store_test.go`'s imports as needed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./blob/ -run 'TestAnalyzeClassifies|TestPutEmptyArchive' 2>&1 | tail -20`
Expected: FAIL, the empty tar cases decide `raw not-tar`.

- [ ] **Step 3: Implement**

In `blob/analyze.go`, replace the bodies of `startsWithCompressedTarHeader` (after `defer dec.Close()`) and `startsWithTarHeader` (after the Seek) with `return probeTar(dec)` and `return probeTar(r)`, and add:

```go
// emptyArchiveMax is the longest all-zero stream accepted as an empty tar
// archive: two zero blocks end an archive and GNU tar pads to its 10 KiB
// record size.
const emptyArchiveMax = 10240

// probeTar reports whether r starts like a tar archive: a header block with
// a valid checksum, or an empty archive, which is nothing but zero blocks
// and at most emptyArchiveMax bytes long. A stream shorter than one block
// is not a tar; a read error decides nothing.
func probeTar(r io.Reader) (bool, error) {
	var hdr [tarHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	if isTarHeader(hdr[:]) {
		return true, nil
	}
	if !bytes.Equal(hdr[:], make([]byte, tarHeaderSize)) {
		return false, nil
	}
	rest, err := io.ReadAll(io.LimitReader(r, emptyArchiveMax-tarHeaderSize+1))
	if err != nil {
		return false, err
	}
	if len(rest) > emptyArchiveMax-tarHeaderSize {
		return false, nil
	}
	for _, c := range rest {
		if c != 0 {
			return false, nil
		}
	}
	return true, nil
}
```

Add `"bytes"` to the imports. Update the doc comments of both probes: "reports whether r begins with a tar header block carrying a valid checksum or is an empty archive (see probeTar)".

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./blob/ 2>&1 | tail -5`
Expected: `ok`. If `gzipped empty tar` is classified `not-reproducible` because zrecipe finds no engine for a 32-byte stream, keep the test but change its expectation to whatever `TestAnalyzeClassifies`'s `go gzip tar` case proves for Go's gzip, and note the result in the commit message.

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./blob/
git add blob/analyze.go blob/analyze_test.go blob/store_test.go
git commit -m "blob: classify an empty archive as a zero-entry prism" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 4: rootfs: replay tar headers from a prism

**Files:**
- Create: `rootfs/layer.go`
- Test: `rootfs/layer_test.go`

**Interfaces:**
- Consumes: `blob.Prism` shape (Task 2) only structurally; `store.Reader.Skip`.
- Produces: `type Layer interface { Index() (*tarprism.Index, error); Recipe() (io.ReadCloser, error); Blob(index int, entry tarprism.Entry) (io.ReadCloser, error); BlobKey(index int, entry tarprism.Entry) (key.Key, error) }`; `type LayerError struct { Layer oci.Digest; Err error }`; unexported `parseLayer(ctx, Layer) ([]entry, error)`, `entry`, `kind` constants, `storeError`, `cleanPath`.

- [ ] **Step 1: Write the failing tests**

Create `rootfs/layer_test.go`:

```go
package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tarprism "github.com/draganm/tar-prism"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

// openStore opens a temporary store.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// tarEntry describes one entry for buildTar. typ 0 is a regular file.
type tarEntry struct {
	name         string
	typ          byte
	data         string
	link         string
	mode         int64
	uid, gid     int
	mtime        time.Time
	xattrs       map[string]string
	major, minor int64
}

// buildTar writes entries with archive/tar in the given format.
func buildTar(t *testing.T, format tar.Format, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, en := range entries {
		hdr := &tar.Header{Name: en.name, Typeflag: en.typ, Linkname: en.link, Mode: en.mode, Uid: en.uid, Gid: en.gid,
			ModTime: en.mtime, Devmajor: en.major, Devminor: en.minor, Format: format}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.ModTime.IsZero() {
			hdr.ModTime = time.Unix(1_700_000_000, 0)
		}
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(en.data))
		}
		if len(en.xattrs) > 0 {
			hdr.PAXRecords = map[string]string{}
			for k, v := range en.xattrs {
				hdr.PAXRecords["SCHILY.xattr."+k] = v
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("header %s: %v", en.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(en.data)); err != nil {
				t.Fatalf("data %s: %v", en.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// patchHeader rewrites the header block at offset off with f and fixes its
// checksum, so tests can craft entries archive/tar's writer refuses.
func patchHeader(archive []byte, off int, f func(blk []byte)) {
	blk := archive[off : off+512]
	f(blk)
	copy(blk[148:156], "        ")
	var sum int64
	for _, c := range blk {
		sum += int64(c)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

// octal writes n as a NUL-terminated octal field of width w.
func octal(dst []byte, n int64, w int) {
	copy(dst, fmt.Sprintf("%0*o\x00", w-1, n))
}

// memLayer is a Layer over a real store: the prism parts of one archive,
// decomposed by tar-prism, with the recipe and every blob stored. It counts
// how many times content readers were opened.
type memLayer struct {
	st     *store.Store
	index  *tarprism.Index
	recipe key.Key
	blobs  []key.Key
	opened int
}

type memSink struct {
	w   *store.Writer
	l   *memLayer
	buf bytes.Buffer
}

func (s *memSink) Recipe() (io.WriteCloser, error) { return s, nil }
func (s *memSink) Write(p []byte) (int, error)    { return s.buf.Write(p) }
func (s *memSink) Close() error {
	if s.l.recipe != (key.Key{}) {
		return nil
	}
	k, err := s.w.PutBytes(s.buf.Bytes())
	s.l.recipe = k
	return err
}
func (s *memSink) Blob(index int, entry tarprism.Entry, r io.Reader) error {
	k, err := s.w.PutStream(io.LimitReader(r, entry.Size))
	if err != nil {
		return err
	}
	s.l.blobs = append(s.l.blobs, k)
	return nil
}
func (s *memSink) Index(idx *tarprism.Index) error { s.l.index = idx; return nil }

func newMemLayer(t *testing.T, st *store.Store, archive []byte) *memLayer {
	t.Helper()
	w := st.NewWriter(context.Background())
	defer w.Abort()
	l := &memLayer{st: st}
	if err := tarprism.DecomposeTo(bytes.NewReader(archive), &memSink{w: w, l: l}); err != nil {
		t.Fatalf("DecomposeTo: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *memLayer) Index() (*tarprism.Index, error) { return l.index, nil }
func (l *memLayer) Recipe() (io.ReadCloser, error)  { return l.st.NewReader(l.recipe), nil }
func (l *memLayer) BlobKey(i int, e tarprism.Entry) (key.Key, error) {
	if i < 0 || i >= len(l.blobs) {
		return key.Key{}, fmt.Errorf("no blob %d", i)
	}
	return l.blobs[i], nil
}
func (l *memLayer) Blob(i int, e tarprism.Entry) (io.ReadCloser, error) {
	l.opened++
	k, err := l.BlobKey(i, e)
	if err != nil {
		return nil, err
	}
	return l.st.NewReader(k), nil
}

// parse runs parseLayer and fails the test on error.
func parse(t *testing.T, l Layer) []entry {
	t.Helper()
	entries, err := parseLayer(context.Background(), l)
	if err != nil {
		t.Fatalf("parseLayer: %v", err)
	}
	return entries
}

func TestParseLayerReplaysHeaders(t *testing.T) {
	st := openStore(t)
	longName := "share/doc/" + strings.Repeat("a-rather-long-directory-name/", 5) + "NOTICE"
	big := strings.Repeat("0123456789abcdef", 100_000) // 1.6 MB, several chunks
	for _, format := range []tar.Format{tar.FormatPAX, tar.FormatGNU} {
		t.Run(format.String(), func(t *testing.T) {
			entries := []tarEntry{
				{name: "bin/", typ: tar.TypeDir, mode: 0o755},
				{name: "bin/app", data: big, mode: 0o4755, uid: 10, gid: 20},
				{name: "bin/empty"},
				{name: "bin/link", typ: tar.TypeSymlink, link: strings.Repeat("../", 60) + "app", mode: 0o777},
				{name: "bin/hard", typ: tar.TypeLink, link: "bin/app"},
				{name: "dev/null", typ: tar.TypeChar, mode: 0o666, major: 1, minor: 3},
				{name: "dev/sda", typ: tar.TypeBlock, mode: 0o660, major: 8, minor: 0},
				{name: "run/fifo", typ: tar.TypeFifo, mode: 0o600},
				{name: longName, data: "notice\n"},
				{name: "etc/motd", data: "hello\n", mtime: time.Unix(1_700_000_000, 123_456_789)},
			}
			if format == tar.FormatPAX {
				entries = append(entries, tarEntry{name: "etc/caps", data: "x", xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02\x00\x00\x00\x00"}})
			}
			archive := buildTar(t, format, entries...)
			l := newMemLayer(t, st, archive)

			// Expected: the same headers straight from the archive, with the
			// blob keys in archive order.
			tr := tar.NewReader(bytes.NewReader(archive))
			var want []entry
			blob := 0
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				var k key.Key
				if hdr.Typeflag == tar.TypeReg {
					k = l.blobs[blob]
					blob++
				}
				want = append(want, convert(hdr, k))
			}
			got := parse(t, l)
			if len(got) != len(want) {
				t.Fatalf("parsed %d entries, want %d", len(got), len(want))
			}
			for i := range want {
				if !entryEqual(got[i], want[i]) {
					t.Fatalf("entry %d:\n got %+v\nwant %+v", i, got[i], want[i])
				}
			}
			if blob != len(l.blobs) {
				t.Fatalf("archive has %d regular files, tar-prism stored %d", blob, len(l.blobs))
			}
			if l.opened != 0 {
				t.Fatalf("parseLayer opened %d content readers; headers alone must do", l.opened)
			}
		})
	}
}

// entryEqual compares two entries field by field, xattrs by value.
func entryEqual(a, b entry) bool {
	if a.kind != b.kind || a.path != b.path || a.target != b.target || a.mode != b.mode || a.uid != b.uid || a.gid != b.gid ||
		a.mtime != b.mtime || a.rdev != b.rdev || a.content != b.content || a.reason != b.reason || len(a.xattrs) != len(b.xattrs) {
		return false
	}
	for k, v := range a.xattrs {
		if !bytes.Equal(v, b.xattrs[k]) {
			return false
		}
	}
	return true
}

func TestParseLayerOneByteFileReadsContent(t *testing.T) {
	st := openStore(t)
	l := newMemLayer(t, st, buildTar(t, tar.FormatPAX, tarEntry{name: "one", data: "x"}, tarEntry{name: "two", data: "xy"}))
	got := parse(t, l)
	if len(got) != 2 || got[0].kind != kindFile || got[0].content != l.blobs[0] || got[1].content != l.blobs[1] {
		t.Fatalf("entries %+v", got)
	}
	// archive/tar reads the last byte of every file after seeking; for a
	// one-byte file that byte is at offset 0, which is served for real.
	if l.opened != 1 {
		t.Fatalf("opened %d content readers, want 1 (the one-byte file)", l.opened)
	}
}

func TestParseLayerSkipsGNUSparse(t *testing.T) {
	st := openStore(t)
	compact := strings.Repeat("d", 1024)
	archive := buildTar(t, tar.FormatGNU, tarEntry{name: "sparse", data: compact}, tarEntry{name: "after", data: "after\n"})
	// Turn the first entry into an old GNU sparse file: one data run of
	// 1024 bytes at offset 0, real size 4096.
	patchHeader(archive, 0, func(blk []byte) {
		blk[156] = tar.TypeGNUSparse
		octal(blk[386:398], 0, 12)
		octal(blk[398:410], 1024, 12)
		blk[482] = 0
		octal(blk[483:495], 4096, 12)
	})
	got := parse(t, newMemLayer(t, st, archive))
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(got))
	}
	if got[0].kind != kindSkip || got[0].reason != "sparse file" || got[0].path != "sparse" {
		t.Fatalf("sparse entry = %+v", got[0])
	}
	if got[1].kind != kindFile || got[1].path != "after" {
		t.Fatalf("entry after the sparse file = %+v", got[1])
	}
}

func TestParseLayerErrors(t *testing.T) {
	st := openStore(t)
	t.Run("hard link with payload", func(t *testing.T) {
		// tar-prism keeps the payload in the recipe; archive/tar expects
		// none and reads the payload as the next header.
		archive := buildTar(t, tar.FormatGNU, tarEntry{name: "hard", data: strings.Repeat("p", 512)}, tarEntry{name: "after", data: "after\n"})
		patchHeader(archive, 0, func(blk []byte) {
			blk[156] = tar.TypeLink
			copy(blk[157:], "target\x00")
		})
		_, err := parseLayer(context.Background(), newMemLayer(t, st, archive))
		var se *storeError
		if err == nil || errors.As(err, &se) {
			t.Fatalf("err = %v, want an archive error", err)
		}
	})
	t.Run("index disagrees", func(t *testing.T) {
		l := newMemLayer(t, st, buildTar(t, tar.FormatPAX, tarEntry{name: "a", data: "aaaa"}, tarEntry{name: "b", data: "bbbb"}))
		short := &tarprism.Index{Version: l.index.Version, BLAKE3: l.index.BLAKE3, Entries: l.index.Entries[:1]}
		_, err := parseLayer(context.Background(), &indexLayer{Layer: l, idx: short})
		var se *storeError
		if err == nil || errors.As(err, &se) {
			t.Fatalf("err = %v, want an archive error", err)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		l := newMemLayer(t, st, buildTar(t, tar.FormatPAX, tarEntry{name: "a", data: "aaaa"}))
		boom := errors.New("boom")
		_, err := parseLayer(context.Background(), &failingRecipe{Layer: l, err: boom})
		var se *storeError
		if !errors.As(err, &se) || !errors.Is(err, boom) {
			t.Fatalf("err = %v, want a *storeError wrapping boom", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := newMemLayer(t, st, buildTar(t, tar.FormatPAX, tarEntry{name: "a", data: "aaaa"}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := parseLayer(ctx, l); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// indexLayer overrides the index of a Layer.
type indexLayer struct {
	Layer
	idx *tarprism.Index
}

func (l *indexLayer) Index() (*tarprism.Index, error) { return l.idx, nil }

// failingRecipe serves a recipe whose reads fail.
type failingRecipe struct {
	Layer
	err error
}

func (l *failingRecipe) Recipe() (io.ReadCloser, error) { return io.NopCloser(&errReader{l.err}), nil }

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{".", "", true},
		{"./", "", true},
		{"/", "", true},
		{"a", "a", true},
		{"./a/b/", "a/b", true},
		{"/etc/passwd", "etc/passwd", true},
		{"a//b/./c", "a/b/c", true},
		{"a/../b", "b", true},
		{"/../a", "a", true},
		{"..", "..", false},
		{"../a", "../a", false},
		{"a/../../b", "../b", false},
	}
	for _, c := range cases {
		got, ok := cleanPath(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("cleanPath(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestConvert(t *testing.T) {
	k, err := key.New(key.Blob, 1, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(1_700_000_000, 5)
	cases := []struct {
		name string
		hdr  tar.Header
		want entry
	}{
		{"file", tar.Header{Name: "./a/b", Typeflag: tar.TypeReg, Mode: 0o100644, Uid: 1, Gid: 2, ModTime: ts},
			entry{kind: kindFile, path: "a/b", mode: 0o644, uid: 1, gid: 2, mtime: ts.UnixNano(), content: k}},
		{"dir", tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}, entry{kind: kindDir, path: "d", mode: 0o755}},
		{"root dir", tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}, entry{kind: kindDir, path: "", mode: 0o755}},
		{"symlink", tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/abs/../x", Mode: 0o777}, entry{kind: kindSymlink, path: "l", target: "/abs/../x", mode: 0o777}},
		{"hardlink", tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "./a/b", Mode: 0o644}, entry{kind: kindHardlink, path: "h", target: "a/b", mode: 0o644}},
		{"hardlink escaping", tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../x"}, entry{kind: kindSkip, path: "h", reason: "hard link target escapes the root"}},
		{"char", tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}, entry{kind: kindChar, path: "dev/null", mode: 0o666, rdev: [2]uint64{1, 3}}},
		{"block", tar.Header{Name: "dev/sda", Typeflag: tar.TypeBlock, Mode: 0o660, Devmajor: 8}, entry{kind: kindBlock, path: "dev/sda", mode: 0o660, rdev: [2]uint64{8, 0}}},
		{"fifo", tar.Header{Name: "f", Typeflag: tar.TypeFifo, Mode: 0o600}, entry{kind: kindFIFO, path: "f", mode: 0o600}},
		{"cont", tar.Header{Name: "c", Typeflag: tar.TypeCont, Mode: 0o600}, entry{kind: kindFile, path: "c", mode: 0o600, content: k}},
		{"whiteout", tar.Header{Name: "a/.wh.b", Typeflag: tar.TypeReg}, entry{kind: kindWhiteout, path: "a/b"}},
		{"whiteout at root", tar.Header{Name: ".wh.b", Typeflag: tar.TypeReg}, entry{kind: kindWhiteout, path: "b"}},
		{"opaque", tar.Header{Name: "a/.wh..wh..opq", Typeflag: tar.TypeReg}, entry{kind: kindOpaque, path: "a"}},
		{"opaque at root", tar.Header{Name: ".wh..wh..opq", Typeflag: tar.TypeReg}, entry{kind: kindOpaque, path: ""}},
		{"escapes", tar.Header{Name: "../x", Typeflag: tar.TypeReg}, entry{kind: kindSkip, path: "../x", reason: "path escapes the root"}},
		{"sparse pax", tar.Header{Name: "s", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"GNU.sparse.major": "1"}}, entry{kind: kindSkip, path: "s", reason: "sparse file"}},
		{"unknown type", tar.Header{Name: "u", Typeflag: 'X', Mode: 0o644}, entry{kind: kindSkip, path: "u", mode: 0o644, reason: `unsupported type 'X'`}},
		{"negative ids", tar.Header{Name: "n", Typeflag: tar.TypeReg, Uid: -1, Gid: -2}, entry{kind: kindFile, path: "n", content: k}},
		{"xattrs", tar.Header{Name: "x", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.user.a": "1", "other": "2"}},
			entry{kind: kindFile, path: "x", content: k, xattrs: map[string][]byte{"user.a": []byte("1")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := convert(&c.hdr, k)
			if !entryEqual(got, c.want) {
				t.Fatalf("\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./rootfs/ 2>&1 | head -5`
Expected: build failure, `undefined: parseLayer` and friends.

- [ ] **Step 3: Implement `rootfs/layer.go`**

```go
// Package rootfs builds the root filesystem of a container image from the
// prisms its layers are stored as. Tar headers are replayed from each
// layer's recipe without reading file contents, the layers are merged with
// OCI whiteout semantics, and the result is written as an amber directory
// tree whose regular files point at the content the prisms already hold.
package rootfs

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	tarprism "github.com/draganm/tar-prism"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/oci"
)

// Layer is what the builder needs from a stored prism: tar-prism's index and
// recipe, and each regular file's content as a stream or by key. *blob.Prism
// satisfies it.
type Layer interface {
	Index() (*tarprism.Index, error)
	Recipe() (io.ReadCloser, error)
	Blob(index int, entry tarprism.Entry) (io.ReadCloser, error)
	BlobKey(index int, entry tarprism.Entry) (key.Key, error)
}

// LayerError reports a layer whose archive could not be parsed: archive/tar
// rejected a header, or the headers and the prism's index disagree about
// where the regular files are. The image gets no rootfs; the push succeeds.
type LayerError struct {
	Layer oci.Digest
	Err   error
}

func (e *LayerError) Error() string { return fmt.Sprintf("layer %s: %v", e.Layer, e.Err) }
func (e *LayerError) Unwrap() error { return e.Err }

// storeError marks a failure of the store behind a Layer (index, recipe or
// content reads). It fails the push, not just the layer.
type storeError struct{ err error }

func (e *storeError) Error() string { return "store: " + e.err.Error() }
func (e *storeError) Unwrap() error { return e.err }

// kind is what an archive entry does to the tree.
type kind int

const (
	kindFile     kind = iota // regular file; content is set
	kindDir                  // directory
	kindSymlink              // target is the link target, verbatim
	kindHardlink             // target is the cleaned path of the linked entry
	kindChar                 // character device; rdev is set
	kindBlock                // block device; rdev is set
	kindFIFO                 // named pipe
	kindWhiteout             // path is the entry to remove from lower layers
	kindOpaque               // path is the directory whose lower children go
	kindSkip                 // not represented; reason says why
)

// entry is one archive entry with its path cleaned.
type entry struct {
	kind     kind
	path     string // cleaned, slash separated; "" is the root
	target   string
	mode     uint64 // permission, setuid, setgid and sticky bits only
	uid, gid uint64
	mtime    int64
	rdev     [2]uint64
	xattrs   map[string][]byte
	content  key.Key
	reason   string
}

func (e entry) skip(reason string) entry {
	e.kind, e.reason = kindSkip, reason
	return e
}

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
	paxXattrPrefix = "SCHILY.xattr."
	paxSparse      = "GNU.sparse."
	// ctxCheckEvery is how many headers parseLayer reads between checks of
	// the context.
	ctxCheckEvery = 1024
)

// parseLayer reads every header of layer through a splice and returns the
// entries in archive order. File contents are never read except where
// archive/tar itself reads them (a PAX sparse 1.0 map). A malformed archive
// is a plain error, which Apply wraps as a LayerError; a store failure is a
// *storeError; a done context returns its error.
func parseLayer(ctx context.Context, layer Layer) ([]entry, error) {
	idx, err := layer.Index()
	if err != nil {
		return nil, &storeError{err}
	}
	recipe, err := layer.Recipe()
	if err != nil {
		return nil, &storeError{err}
	}
	s := newSplice(recipe, idx.Entries, func(i int) (io.ReadCloser, error) {
		return layer.Blob(i, idx.Entries[i])
	})
	defer s.Close()
	tr := tar.NewReader(s)
	var entries []entry
	consumed := 0
	for n := 0; ; n++ {
		if n%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		hdr, err := tr.Next()
		if errors.Is(err, tar.ErrInsecurePath) {
			err = nil // the header is complete; cleanPath deals with the name
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var se *storeError
			if errors.As(err, &se) {
				return nil, err
			}
			return nil, fmt.Errorf("offset %d: %w", s.pos, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		var content key.Key
		if hasContent(hdr.Typeflag) {
			i, ok := s.regionAt()
			if !ok {
				return nil, fmt.Errorf("offset %d: entry %q has content but the index has no blob there", s.pos, hdr.Name)
			}
			k, err := layer.BlobKey(i, idx.Entries[i])
			if err != nil {
				return nil, &storeError{err}
			}
			content = k
			consumed++
		}
		entries = append(entries, convert(hdr, content))
	}
	if consumed != len(idx.Entries) {
		return nil, fmt.Errorf("archive has %d regular files, the index has %d", consumed, len(idx.Entries))
	}
	return entries, nil
}

// hasContent reports whether archive/tar and tar-prism both treat a header
// of this type as carrying content that tar-prism cut into a blob. Old
// archives' TypeRegA has already become TypeReg or TypeDir here.
func hasContent(flag byte) bool {
	switch flag {
	case tar.TypeReg, tar.TypeCont, tar.TypeGNUSparse:
		return true
	}
	return false
}

// convert turns a header into an entry; content is the file's content key
// for types that carry content.
func convert(hdr *tar.Header, content key.Key) entry {
	e := entry{
		mode:   uint64(hdr.Mode) & 0o7777,
		uid:    clampUint(int64(hdr.Uid)),
		gid:    clampUint(int64(hdr.Gid)),
		mtime:  mtimeNanos(hdr.ModTime),
		xattrs: paxXattrs(hdr.PAXRecords),
	}
	p, ok := cleanPath(hdr.Name)
	e.path = p
	switch {
	case !ok:
		return e.skip("path escapes the root")
	case isSparse(hdr):
		return e.skip("sparse file")
	}
	if base := path.Base(p); p != "" && strings.HasPrefix(base, whiteoutPrefix) {
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		if base == opaqueWhiteout {
			e.kind, e.path = kindOpaque, dir
		} else {
			e.kind, e.path = kindWhiteout, joinPath(dir, strings.TrimPrefix(base, whiteoutPrefix))
		}
		return e
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeCont:
		e.kind, e.content = kindFile, content
	case tar.TypeDir:
		e.kind = kindDir
	case tar.TypeSymlink:
		e.kind, e.target = kindSymlink, hdr.Linkname
	case tar.TypeLink:
		target, ok := cleanPath(hdr.Linkname)
		if !ok {
			return e.skip("hard link target escapes the root")
		}
		e.kind, e.target = kindHardlink, target
	case tar.TypeChar:
		e.kind, e.rdev = kindChar, [2]uint64{clampUint(hdr.Devmajor), clampUint(hdr.Devminor)}
	case tar.TypeBlock:
		e.kind, e.rdev = kindBlock, [2]uint64{clampUint(hdr.Devmajor), clampUint(hdr.Devminor)}
	case tar.TypeFifo:
		e.kind = kindFIFO
	default:
		return e.skip(fmt.Sprintf("unsupported type %q", hdr.Typeflag))
	}
	return e
}

// cleanPath normalizes an archive name: leading "/" and "./", a trailing
// "/" and every ".", "//" and ".." that path.Clean resolves go. The root is
// "". ok is false when the name escapes the root ("..", "../x").
func cleanPath(name string) (string, bool) {
	p := strings.TrimPrefix(path.Clean(name), "/")
	if p == "." || p == "" {
		return "", true
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return p, false
	}
	return p, true
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// isSparse reports an old GNU sparse entry or a PAX sparse one. Their
// content in the store is the archive's compact form, not the file.
func isSparse(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, paxSparse) {
			return true
		}
	}
	return false
}

// paxXattrs collects the SCHILY.xattr.* records.
func paxXattrs(records map[string]string) map[string][]byte {
	var m map[string][]byte
	for k, v := range records {
		name, ok := strings.CutPrefix(k, paxXattrPrefix)
		if !ok || name == "" {
			continue
		}
		if m == nil {
			m = map[string][]byte{}
		}
		m[name] = []byte(v)
	}
	return m
}

func clampUint(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

func mtimeNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// splice is the virtual archive archive/tar reads: the recipe with every
// regular file's content region spliced back in at its index offset. A read
// at the start of a region serves the real content (archive/tar reads
// content there only for a PAX sparse 1.0 map); after a seek the rest of the
// region is served as zeros, which only archive/tar's one-byte read before
// the next header ever sees. Passing over a region costs nothing.
type splice struct {
	recipe  io.ReadCloser
	skip    func(int64) error // the recipe's own Skip, when it has one
	entries []tarprism.Entry
	starts  []int64 // virtual offset where each region starts
	open    func(i int) (io.ReadCloser, error)

	pos     int64
	next    int           // first region not yet passed
	content io.ReadCloser // real reader of region next, once opened
	zeros   bool          // the rest of region next is served as zeros
	matched int           // last region handed out by regionAt
}

func newSplice(recipe io.ReadCloser, entries []tarprism.Entry, open func(int) (io.ReadCloser, error)) *splice {
	starts := make([]int64, len(entries))
	var shift int64
	for i, e := range entries {
		starts[i] = e.Offset + shift
		shift += e.Size
	}
	s := &splice{recipe: recipe, entries: entries, starts: starts, open: open, matched: -1}
	if sk, ok := recipe.(interface{ Skip(int64) error }); ok {
		s.skip = sk.Skip
	}
	return s
}

func (s *splice) end(i int) int64 { return s.starts[i] + s.entries[i].Size }

// inRegion moves next past every region that ends at or before pos and
// reports whether pos is inside region next.
func (s *splice) inRegion() bool {
	for s.next < len(s.starts) && s.pos >= s.end(s.next) {
		s.closeContent()
		s.zeros = false
		s.next++
	}
	return s.next < len(s.starts) && s.pos >= s.starts[s.next]
}

func (s *splice) closeContent() {
	if s.content != nil {
		s.content.Close()
		s.content = nil
	}
}

// Read implements io.Reader over the virtual archive.
func (s *splice) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.inRegion() {
		i := s.next
		if rem := s.end(i) - s.pos; int64(len(p)) > rem {
			p = p[:rem]
		}
		if !s.zeros && s.content == nil && s.pos == s.starts[i] {
			c, err := s.open(i)
			if err != nil {
				return 0, &storeError{err}
			}
			s.content = c
		}
		if s.content != nil {
			n, err := s.content.Read(p)
			s.pos += int64(n)
			switch {
			case err != nil && !errors.Is(err, io.EOF):
				return n, &storeError{err}
			case n == 0 && err != nil:
				return 0, &storeError{fmt.Errorf("%s ends %d bytes short of the index", s.entries[i].Blob, s.end(i)-s.pos)}
			}
			return n, nil
		}
		clear(p)
		s.pos += int64(len(p))
		return len(p), nil
	}
	if s.next < len(s.starts) {
		if d := s.starts[s.next] - s.pos; d < int64(len(p)) {
			p = p[:d]
		}
	}
	n, err := s.recipe.Read(p)
	s.pos += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &storeError{err}
	}
	return n, err
}

// Seek implements io.Seeker for a forward io.SeekCurrent, which is how
// archive/tar skips file content. Passing over a region is free; recipe
// bytes are skipped with the recipe's Skip or read and discarded. Seeking
// past the end is not an error: the next Read reports io.EOF.
func (s *splice) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekCurrent || offset < 0 {
		return 0, errors.New("rootfs: the splice seeks forward from the current position only")
	}
	target := s.pos + offset
	for s.pos < target {
		if s.inRegion() {
			s.closeContent()
			s.zeros = true
			s.pos = min(target, s.end(s.next))
			continue
		}
		n := target - s.pos
		if s.next < len(s.starts) {
			n = min(n, s.starts[s.next]-s.pos)
		}
		if s.skip != nil {
			if err := s.skip(n); err != nil {
				return s.pos, &storeError{err}
			}
			s.pos += n
			continue
		}
		m, err := io.CopyN(io.Discard, s.recipe, n)
		s.pos += m
		if errors.Is(err, io.EOF) {
			return s.pos, nil
		}
		if err != nil {
			return s.pos, &storeError{err}
		}
	}
	return s.pos, nil
}

// regionAt returns the region of the entry archive/tar just returned: the
// first region not yet passed, which must contain the current position.
// Every region is handed out at most once.
func (s *splice) regionAt() (int, bool) {
	i := s.next
	if i >= len(s.starts) || i == s.matched || s.pos < s.starts[i] || s.pos > s.end(i) {
		return 0, false
	}
	s.matched = i
	return i, true
}

// Close releases the recipe and any open content reader.
func (s *splice) Close() error {
	s.closeContent()
	return s.recipe.Close()
}
```

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./rootfs/ 2>&1 | tail -20`
Expected: `ok`. If `TestParseLayerReplaysHeaders` reports `parseLayer opened N content readers`, print which entries opened them: archive/tar reads content only at region offset 0, so a non-zero count means `regionAt`/`inRegion` let a real read happen elsewhere; fix the splice, not the test.

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./rootfs/
git add rootfs/layer.go rootfs/layer_test.go
git commit -m "rootfs: replay a layer's tar headers from its prism recipe" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 5: rootfs: the merge tree

**Files:**
- Create: `rootfs/tree.go`
- Test: `rootfs/tree_test.go`

**Interfaces:**
- Consumes: `entry`, `kind*` (Task 4); `store.Type*` (Task 1).
- Produces: unexported `node`, `tree`, `newTree()`, `(*tree).put(entry) error`, `(*tree).link(entry) error`, `(*tree).whiteout(entry) error`, `(*tree).opaque(entry) error`, `(*tree).lookup(path) (*node, error)`, `skipError`, `errLoop`.

- [ ] **Step 1: Write the failing tests**

Create `rootfs/tree_test.go`:

```go
package rootfs

import (
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

func fakeKey(t *testing.T, s string) key.Key {
	t.Helper()
	k, err := key.New(key.Blob, uint64(len(s)), []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fileEntry(t *testing.T, p string) entry {
	return entry{kind: kindFile, path: p, mode: 0o644, uid: 1, mtime: 10, content: fakeKey(t, p)}
}
func dirEntry(p string) entry           { return entry{kind: kindDir, path: p, mode: 0o750, uid: 2, mtime: 20} }
func symlinkEntry(p, target string) entry { return entry{kind: kindSymlink, path: p, mode: 0o777, target: target} }
func hardlinkEntry(p, target string) entry {
	return entry{kind: kindHardlink, path: p, mode: 0o600, uid: 3, mtime: 30, target: target}
}
func whiteoutEntry(p string) entry { return entry{kind: kindWhiteout, path: p} }
func opaqueEntry(p string) entry   { return entry{kind: kindOpaque, path: p} }

// paths lists every path in the tree, sorted.
func paths(tr *tree) []string {
	var out []string
	var walk func(prefix string, n *node)
	walk = func(prefix string, n *node) {
		for name, c := range n.children {
			p := joinPath(prefix, name)
			out = append(out, p)
			if c.typ() == store.TypeDir {
				walk(p, c)
			}
		}
	}
	walk("", tr.root)
	sort.Strings(out)
	return out
}

func mustPut(t *testing.T, tr *tree, entries ...entry) {
	t.Helper()
	for _, e := range entries {
		var err error
		switch e.kind {
		case kindHardlink:
			err = tr.link(e)
		case kindWhiteout:
			err = tr.whiteout(e)
		case kindOpaque:
			err = tr.opaque(e)
		default:
			err = tr.put(e)
		}
		if err != nil {
			t.Fatalf("apply %+v: %v", e, err)
		}
	}
}

func get(t *testing.T, tr *tree, p string) *node {
	t.Helper()
	n, err := tr.lookup(p)
	if err != nil {
		t.Fatalf("lookup %s: %v", p, err)
	}
	if n == nil {
		t.Fatalf("lookup %s: missing", p)
	}
	return n
}

func TestTreePutAndReplace(t *testing.T) {
	tr := newTree()
	mustPut(t, tr, fileEntry(t, "a/b/c"), fileEntry(t, "a/f"))
	if got := paths(tr); !slices.Equal(got, []string{"a", "a/b", "a/b/c", "a/f"}) {
		t.Fatalf("paths = %v", got)
	}
	a := get(t, tr, "a")
	if !a.implicit || a.mode != store.TypeDir|0o755 || a.uid != 0 || a.mtime != 0 {
		t.Fatalf("implicit dir a = %+v", a)
	}
	// An explicit directory keeps the children and takes the metadata.
	mustPut(t, tr, dirEntry("a"))
	a = get(t, tr, "a")
	if a.implicit || a.mode != store.TypeDir|0o750 || a.uid != 2 || a.mtime != 20 || len(a.children) != 2 {
		t.Fatalf("explicit dir a = %+v", a)
	}
	// A file over a directory drops the subtree; a directory over a file
	// starts empty.
	mustPut(t, tr, fileEntry(t, "a/b"))
	if got := paths(tr); !slices.Equal(got, []string{"a", "a/b", "a/f"}) {
		t.Fatalf("after file over dir: %v", got)
	}
	if n := get(t, tr, "a/b"); n.typ() != store.TypeReg || n.content != fakeKey(t, "a/b") {
		t.Fatalf("a/b = %+v", n)
	}
	mustPut(t, tr, dirEntry("a/f"))
	if n := get(t, tr, "a/f"); n.typ() != store.TypeDir || len(n.children) != 0 {
		t.Fatalf("a/f = %+v", n)
	}
	// A file in the way of a parent component is replaced by an implicit
	// directory.
	mustPut(t, tr, fileEntry(t, "a/b/deeper"))
	if n := get(t, tr, "a/b"); n.typ() != store.TypeDir || !n.implicit {
		t.Fatalf("a/b = %+v", n)
	}
	// Root entries: a directory is ignored, anything else is a skip.
	mustPut(t, tr, dirEntry(""))
	err := tr.put(fileEntry(t, ""))
	var se *skipError
	if !errors.As(err, &se) || se.reason != "root is not a directory" {
		t.Fatalf("file at the root: %v", err)
	}
}

func TestTreeSymlinkParents(t *testing.T) {
	tr := newTree()
	mustPut(t, tr, dirEntry("usr/bin"), symlinkEntry("bin", "usr/bin"), fileEntry(t, "bin/foo"))
	if got := paths(tr); !slices.Equal(got, []string{"bin", "usr", "usr/bin", "usr/bin/foo"}) {
		t.Fatalf("relative symlink parent: %v", got)
	}
	mustPut(t, tr, symlinkEntry("sbin", "/usr/bin"), fileEntry(t, "sbin/bar"))
	if _, err := tr.lookup("usr/bin/bar"); err != nil {
		t.Fatal(err)
	}
	if get(t, tr, "usr/bin/bar") == nil {
		t.Fatal("absolute symlink parent not followed")
	}
	mustPut(t, tr, symlinkEntry("up", "../../usr/bin"), fileEntry(t, "up/baz"))
	get(t, tr, "usr/bin/baz")
	// A symlink at the last component is not followed: it is replaced.
	mustPut(t, tr, dirEntry("bin"))
	if n := get(t, tr, "bin"); n.typ() != store.TypeDir {
		t.Fatalf("bin = %+v", n)
	}
	if got := paths(tr); slices.Contains(got, "bin/foo") {
		t.Fatalf("bin became a directory but kept usr/bin's files: %v", got)
	}
	// Loops are reported.
	mustPut(t, tr, symlinkEntry("l1", "l2"), symlinkEntry("l2", "l1"))
	if err := tr.put(fileEntry(t, "l1/x")); !errors.Is(err, errLoop) {
		t.Fatalf("loop: %v", err)
	}
}

func TestTreeHardlinks(t *testing.T) {
	tr := newTree()
	mustPut(t, tr, fileEntry(t, "a"), symlinkEntry("s", "a"), dirEntry("d"), hardlinkEntry("h", "a"), hardlinkEntry("hs", "s"))
	h := get(t, tr, "h")
	if h.typ() != store.TypeReg || h.content != fakeKey(t, "a") || h.mode != store.TypeReg|0o600 || h.uid != 3 || h.mtime != 30 {
		t.Fatalf("h = %+v", h)
	}
	if hs := get(t, tr, "hs"); hs.typ() != store.TypeLink || hs.link != "a" {
		t.Fatalf("hs = %+v", hs)
	}
	var se *skipError
	if err := tr.link(hardlinkEntry("m", "missing")); !errors.As(err, &se) || se.reason != "hard link target not found" {
		t.Fatalf("missing target: %v", err)
	}
	if err := tr.link(hardlinkEntry("m", "d")); !errors.As(err, &se) || se.reason != "hard link to a directory" {
		t.Fatalf("directory target: %v", err)
	}
	if got := paths(tr); slices.Contains(got, "m") {
		t.Fatalf("skipped link was placed: %v", got)
	}
}

func TestTreeWhiteouts(t *testing.T) {
	tr := newTree()
	mustPut(t, tr, fileEntry(t, "etc/a"), fileEntry(t, "etc/b"), fileEntry(t, "var/log/x"), symlinkEntry("lnk", "etc"), fileEntry(t, "keep"))
	mustPut(t, tr, whiteoutEntry("etc/a"), whiteoutEntry("var"), whiteoutEntry("lnk"), whiteoutEntry("nothing/here"), whiteoutEntry("also-nothing"))
	if got := paths(tr); !slices.Equal(got, []string{"etc", "etc/b", "keep"}) {
		t.Fatalf("after whiteouts: %v", got)
	}
	mustPut(t, tr, symlinkEntry("cfg", "etc"), fileEntry(t, "etc/c"))
	mustPut(t, tr, whiteoutEntry("cfg/b")) // through the symlinked directory
	if got := paths(tr); !slices.Equal(got, []string{"cfg", "etc", "etc/c", "keep"}) {
		t.Fatalf("whiteout through a symlink: %v", got)
	}
	mustPut(t, tr, opaqueEntry("cfg"))
	if got := paths(tr); !slices.Equal(got, []string{"cfg", "etc", "keep"}) {
		t.Fatalf("opaque through a symlink: %v", got)
	}
	mustPut(t, tr, opaqueEntry("missing"), opaqueEntry("keep"))
	if got := paths(tr); !slices.Equal(got, []string{"cfg", "etc", "keep"}) {
		t.Fatalf("opaque on a missing or non-directory path changed the tree: %v", got)
	}
	mustPut(t, tr, opaqueEntry(""))
	if got := paths(tr); len(got) != 0 {
		t.Fatalf("opaque at the root left %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./rootfs/ -run TestTree 2>&1 | head -5`
Expected: `undefined: newTree`.

- [ ] **Step 3: Implement `rootfs/tree.go`**

```go
package rootfs

import (
	"fmt"
	"path"
	"strings"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

// maxSymlinkHops bounds one path resolution, like the kernel's 40.
const maxSymlinkHops = 40

// node is one entry of the merged tree.
type node struct {
	mode     uint64 // type bits and permissions
	uid, gid uint64
	mtime    int64
	content  key.Key          // TypeReg
	link     string           // TypeLink
	rdev     [2]uint64        // TypeChar, TypeBlock
	xattrs   map[string][]byte
	children map[string]*node // TypeDir
	implicit bool             // a directory created for a child, no header seen
}

func newDir() *node {
	return &node{mode: store.TypeDir | 0o755, children: map[string]*node{}, implicit: true}
}

func (n *node) typ() uint64 { return n.mode & store.TypeMask }

// skipError says why an entry was left out of the tree.
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

var errLoop = &skipError{"symlink loop"}

// tree is the merged filesystem being built.
type tree struct{ root *node }

func newTree() *tree { return &tree{root: newDir()} }

// splitPath returns the components of a cleaned path; the root has none.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// splitParent splits a non-root cleaned path into its parent and last
// component.
func splitParent(p string) (dir, name string) {
	dir, name = path.Dir(p), path.Base(p)
	if dir == "." {
		dir = ""
	}
	return dir, name
}

// resolveDir walks comps from the root and returns the stack of directories
// ending at the one reached. Symlinks are followed: an absolute target
// restarts at the root, ".." pops (never above the root), more than
// maxSymlinkHops links is errLoop. With create set, a missing component
// becomes an implicit directory and a component that is neither a
// directory nor a symlink is replaced by one (the later entry wins);
// without it, such a component ends the walk with a nil stack.
func (t *tree) resolveDir(comps []string, create bool) ([]*node, error) {
	stack := []*node{t.root}
	hops := 0
	for len(comps) > 0 {
		c := comps[0]
		comps = comps[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		dir := stack[len(stack)-1]
		child := dir.children[c]
		switch {
		case child == nil:
			if !create {
				return nil, nil
			}
			child = newDir()
			dir.children[c] = child
		case child.typ() == store.TypeLink:
			hops++
			if hops > maxSymlinkHops {
				return nil, errLoop
			}
			target := child.link
			if strings.HasPrefix(target, "/") {
				stack = []*node{t.root}
			}
			comps = append(strings.Split(strings.TrimPrefix(target, "/"), "/"), comps...)
			continue
		case child.typ() != store.TypeDir:
			if !create {
				return nil, nil
			}
			child = newDir()
			dir.children[c] = child
		}
		stack = append(stack, child)
	}
	return stack, nil
}

// parentOf resolves the directory that holds p's last component, creating
// implicit directories on the way, and returns it with that name.
func (t *tree) parentOf(p string) (*node, string, error) {
	dir, name := splitParent(p)
	stack, err := t.resolveDir(splitPath(dir), true)
	if err != nil {
		return nil, "", err
	}
	return stack[len(stack)-1], name, nil
}

// lookup returns the node at p, following symlinks in parent components but
// not at the last one, or nil when there is none.
func (t *tree) lookup(p string) (*node, error) {
	if p == "" {
		return t.root, nil
	}
	dir, name := splitParent(p)
	stack, err := t.resolveDir(splitPath(dir), false)
	if err != nil || stack == nil {
		return nil, err
	}
	return stack[len(stack)-1].children[name], nil
}

// put places a file, directory, symlink, device or fifo at its path. A
// directory over a directory keeps the children and takes the metadata;
// anything else replaces the old subtree. A directory entry for the root is
// ignored, any other root entry is a skip.
func (t *tree) put(e entry) error {
	if e.path == "" {
		if e.kind == kindDir {
			return nil
		}
		return &skipError{"root is not a directory"}
	}
	parent, name, err := t.parentOf(e.path)
	if err != nil {
		return err
	}
	n := &node{mode: e.mode, uid: e.uid, gid: e.gid, mtime: e.mtime, xattrs: e.xattrs}
	switch e.kind {
	case kindFile:
		n.mode |= store.TypeReg
		n.content = e.content
	case kindDir:
		n.mode |= store.TypeDir
		n.children = map[string]*node{}
		if old := parent.children[name]; old != nil && old.typ() == store.TypeDir {
			n.children = old.children
		}
	case kindSymlink:
		n.mode |= store.TypeLink
		n.link = e.target
	case kindChar:
		n.mode |= store.TypeChar
		n.rdev = e.rdev
	case kindBlock:
		n.mode |= store.TypeBlock
		n.rdev = e.rdev
	case kindFIFO:
		n.mode |= store.TypeFIFO
	default:
		return fmt.Errorf("rootfs: put of kind %d", e.kind)
	}
	parent.children[name] = n
	return nil
}

// link places a hard link: the target's payload and type bits with e's
// permission bits, ownership, mtime and xattrs. A missing target or a
// directory is a skip.
func (t *tree) link(e entry) error {
	target, err := t.lookup(e.target)
	if err != nil {
		return err
	}
	if target == nil {
		return &skipError{"hard link target not found"}
	}
	if target.typ() == store.TypeDir {
		return &skipError{"hard link to a directory"}
	}
	parent, name, err := t.parentOf(e.path)
	if err != nil {
		return err
	}
	n := *target
	n.mode = target.typ() | e.mode
	n.uid, n.gid, n.mtime, n.xattrs = e.uid, e.gid, e.mtime, e.xattrs
	parent.children[name] = &n
	return nil
}

// whiteout removes the entry at e.path, whatever it is; a missing one is a
// no-op.
func (t *tree) whiteout(e entry) error {
	if e.path == "" {
		return nil
	}
	dir, name := splitParent(e.path)
	stack, err := t.resolveDir(splitPath(dir), false)
	if err != nil || stack == nil {
		return err
	}
	delete(stack[len(stack)-1].children, name)
	return nil
}

// opaque removes every child of the directory at e.path; a missing or
// non-directory path is a no-op.
func (t *tree) opaque(e entry) error {
	stack, err := t.resolveDir(splitPath(e.path), false)
	if err != nil || stack == nil {
		return err
	}
	stack[len(stack)-1].children = map[string]*node{}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./rootfs/ 2>&1 | tail -20`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./rootfs/
git add rootfs/tree.go rootfs/tree_test.go
git commit -m "rootfs: merge tree with whiteouts and symlink-aware paths" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 6: rootfs: Builder, Apply and Write

**Files:**
- Create: `rootfs/builder.go`
- Test: `rootfs/builder_test.go`

**Interfaces:**
- Consumes: `parseLayer`, `entry`, `tree` methods (Tasks 4, 5); `store.Writer.NewDir`, `Dir.AddEntry`, `Writer.PutXattrs` (Task 1).
- Produces: `const MaxSkipped = 100`; `type Skip struct { Layer oci.Digest; Path string; Reason string }` (json tags `layer`, `path`, `reason`); `type Result struct { Root key.Key; Entries int; Skipped []Skip; SkippedCount int }`; `type Builder`; `func New() *Builder`; `func (b *Builder) Apply(ctx context.Context, digest oci.Digest, layer Layer) error`; `func (b *Builder) Write(w *store.Writer) (Result, error)`.

- [ ] **Step 1: Write the failing tests**

Create `rootfs/builder_test.go`:

```go
package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/key"
	"github.com/jobs-build/amber-store-core/tarexport"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// exported is one entry of a tree exported back to a tar.
type exported struct {
	hdr  *tar.Header
	data string
}

// export writes the tree at root to a tar with amber's tarexport and reads
// it back, keyed by cleaned path.
func export(t *testing.T, st *store.Store, root key.Key) map[string]exported {
	t.Helper()
	var buf bytes.Buffer
	if err := tarexport.Write(&buf, root, st.Get); err != nil {
		t.Fatalf("tarexport: %v", err)
	}
	out := map[string]exported{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := cleanPath(hdr.Name)
		out[p] = exported{hdr: hdr, data: string(data)}
	}
	return out
}

func digestOf(s string) oci.Digest { return oci.DigestOfBytes([]byte(s)) }

// build applies archives in order and writes the tree.
func build(t *testing.T, st *store.Store, archives ...[]byte) (Result, []*memLayer) {
	t.Helper()
	b := New()
	var layers []*memLayer
	for i, a := range archives {
		l := newMemLayer(t, st, a)
		layers = append(layers, l)
		if err := b.Apply(context.Background(), digestOf(fmt.Sprint("layer", i)), l); err != nil {
			t.Fatalf("Apply layer %d: %v", i, err)
		}
	}
	w := st.NewWriter(context.Background())
	defer w.Abort()
	res, err := b.Write(w)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return res, layers
}

func TestBuildMergesLayers(t *testing.T) {
	st := openStore(t)
	mtime := time.Unix(1_700_000_000, 42)
	lower := buildTar(t, tar.FormatPAX,
		tarEntry{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "bin/app", data: "app v1", mode: 0o755},
		tarEntry{name: "bin/old", data: "old"},
		tarEntry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "etc/a.conf", data: "a=1"},
		tarEntry{name: "etc/b.conf", data: "b=1"},
		tarEntry{name: "usr/lib/libz.so", data: strings.Repeat("z", 5000), mode: 0o644, uid: 7, gid: 8, mtime: mtime},
		tarEntry{name: "usr/lib/libz.so.1", typ: tar.TypeLink, link: "usr/lib/libz.so", mode: 0o644, uid: 7, gid: 8, mtime: mtime},
		tarEntry{name: "lib", typ: tar.TypeSymlink, link: "usr/lib", mode: 0o777},
		tarEntry{name: "var/cache/x", data: "x"},
		tarEntry{name: "var/cache/y", data: "y"},
	)
	upper := buildTar(t, tar.FormatPAX,
		tarEntry{name: "bin/app", data: "app v2", mode: 0o755},
		tarEntry{name: "bin/.wh.old"},
		tarEntry{name: "etc/", typ: tar.TypeDir, mode: 0o700, uid: 5},
		tarEntry{name: "etc/.wh.a.conf"},
		tarEntry{name: "lib/libnew.so", data: "new"}, // through the lib symlink
		tarEntry{name: "var/cache/.wh..wh..opq"},
		tarEntry{name: "var/cache/z", data: "z"},
		tarEntry{name: "tmp/", typ: tar.TypeDir, mode: 0o1777},
	)
	res, layers := build(t, st, lower, upper)
	if res.SkippedCount != 0 || len(res.Skipped) != 0 {
		t.Fatalf("skips: %+v", res.Skipped)
	}
	got := export(t, st, res.Root)
	want := map[string]string{
		"bin": "", "bin/app": "app v2", "etc": "", "etc/b.conf": "b=1", "usr": "", "usr/lib": "",
		"usr/lib/libz.so": strings.Repeat("z", 5000), "usr/lib/libz.so.1": strings.Repeat("z", 5000),
		"usr/lib/libnew.so": "new", "lib": "", "var": "", "var/cache": "", "var/cache/z": "z", "tmp": "",
	}
	if len(got) != len(want) {
		t.Fatalf("exported %d entries, want %d:\n%v", len(got), len(want), keys(got))
	}
	for p, data := range want {
		e, ok := got[p]
		if !ok {
			t.Fatalf("missing %s", p)
		}
		if e.hdr.Typeflag == tar.TypeReg && e.data != data {
			t.Fatalf("%s: content %q, want %q", p, e.data, data)
		}
	}
	if res.Entries != len(want) {
		t.Fatalf("Entries = %d, want %d", res.Entries, len(want))
	}
	if e := got["etc"]; e.hdr.Typeflag != tar.TypeDir || e.hdr.Mode != 0o700 || e.hdr.Uid != 5 {
		t.Fatalf("etc = %+v", e.hdr)
	}
	if e := got["tmp"]; e.hdr.Mode != 0o1777 {
		t.Fatalf("tmp mode = %o", e.hdr.Mode)
	}
	if e := got["lib"]; e.hdr.Typeflag != tar.TypeSymlink || e.hdr.Linkname != "usr/lib" {
		t.Fatalf("lib = %+v", e.hdr)
	}
	if e := got["usr/lib/libz.so.1"]; e.hdr.Uid != 7 || e.hdr.Gid != 8 || !e.hdr.ModTime.Equal(mtime) {
		t.Fatalf("hard link metadata = %+v", e.hdr)
	}
	// The hard link shares the target's content key, and every file's key
	// is the one tar-prism stored: nothing was chunked twice.
	libz, err := st.Lookup(dirKey(t, st, res.Root, "usr/lib"), "libz.so")
	if err != nil {
		t.Fatal(err)
	}
	libz1, err := st.Lookup(dirKey(t, st, res.Root, "usr/lib"), "libz.so.1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(libz.ContentKey, libz1.ContentKey) || !bytes.Equal(libz.ContentKey, layers[0].blobs[3][:]) {
		t.Fatal("content keys are not the prism's")
	}
	if layers[0].opened != 0 || layers[1].opened != 0 {
		t.Fatalf("content was read: %d, %d", layers[0].opened, layers[1].opened)
	}
}

// dirKey descends the directory path p from root.
func dirKey(t *testing.T, st *store.Store, root key.Key, p string) key.Key {
	t.Helper()
	k := root
	for _, c := range splitPath(p) {
		next, err := st.LookupKey(k, c)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		k = next
	}
	return k
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBuildWhiteoutsSpareOwnLayer(t *testing.T) {
	st := openStore(t)
	lower := buildTar(t, tar.FormatPAX, tarEntry{name: "foo", data: "lower"}, tarEntry{name: "d/x", data: "x"})
	upper := buildTar(t, tar.FormatPAX,
		tarEntry{name: "foo", data: "upper"}, // before its own whiteout
		tarEntry{name: ".wh.foo"},
		tarEntry{name: "d/y", data: "y"},
		tarEntry{name: "d/.wh..wh..opq"}, // after its own entry
	)
	res, _ := build(t, st, lower, upper)
	got := export(t, st, res.Root)
	if e, ok := got["foo"]; !ok || e.data != "upper" {
		t.Fatalf("foo = %+v, want the upper layer's file", e)
	}
	if _, ok := got["d/x"]; ok {
		t.Fatal("opaque whiteout kept the lower d/x")
	}
	if e, ok := got["d/y"]; !ok || e.data != "y" {
		t.Fatalf("opaque whiteout removed its own layer's d/y: %+v", e)
	}
	for p := range got {
		if strings.Contains(p, ".wh.") {
			t.Fatalf("whiteout %s appears in the tree", p)
		}
	}
}

func TestBuildXattrsAndDevices(t *testing.T) {
	st := openStore(t)
	bigAttr := strings.Repeat("v", 300)
	archive := buildTar(t, tar.FormatPAX,
		tarEntry{name: "bin/ping", data: "ping", mode: 0o755, xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00"}},
		tarEntry{name: "big", data: "b", xattrs: map[string]string{"user.big": bigAttr}},
		tarEntry{name: "dev/null", typ: tar.TypeChar, mode: 0o666, major: 1, minor: 3},
		tarEntry{name: "dev/sda", typ: tar.TypeBlock, mode: 0o660, major: 8, minor: 16},
		tarEntry{name: "run/fifo", typ: tar.TypeFifo, mode: 0o600},
	)
	res, _ := build(t, st, archive)
	got := export(t, st, res.Root)
	if v := got["bin/ping"].hdr.PAXRecords["SCHILY.xattr.security.capability"]; v != "\x01\x00\x00\x02\x00\x20\x00\x00" {
		t.Fatalf("capability xattr = %q", v)
	}
	if v := got["big"].hdr.PAXRecords["SCHILY.xattr.user.big"]; v != bigAttr {
		t.Fatalf("spilled xattr = %q", v)
	}
	big, err := st.Lookup(res.Root, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(big.XattrsKey) == 0 || len(big.XattrsIn) != 0 {
		t.Fatalf("a %d-byte xattr set was not spilled: %+v", len(bigAttr), big)
	}
	if e := got["dev/null"].hdr; e.Typeflag != tar.TypeChar || e.Devmajor != 1 || e.Devminor != 3 || e.Mode != 0o666 {
		t.Fatalf("dev/null = %+v", e)
	}
	if e := got["dev/sda"].hdr; e.Typeflag != tar.TypeBlock || e.Devmajor != 8 || e.Devminor != 16 {
		t.Fatalf("dev/sda = %+v", e)
	}
	if e := got["run/fifo"].hdr; e.Typeflag != tar.TypeFifo || e.Mode != 0o600 {
		t.Fatalf("run/fifo = %+v", e)
	}
}

func TestBuildSkips(t *testing.T) {
	st := openStore(t)
	archive := buildTar(t, tar.FormatGNU,
		tarEntry{name: "ok", data: "ok"},
		tarEntry{name: "../escape", data: "e"},
		tarEntry{name: "sparse", data: strings.Repeat("s", 1024)},
		tarEntry{name: "dangling", typ: tar.TypeLink, link: "nowhere"},
		tarEntry{name: "weird", typ: 'X', data: ""},
		tarEntry{name: "l1", typ: tar.TypeSymlink, link: "l2"},
		tarEntry{name: "l2", typ: tar.TypeSymlink, link: "l1"},
		tarEntry{name: "l1/inside", data: "i"},
	)
	// Second header block (the escaping entry comes first after "ok"'s
	// header+data): make "sparse" an old GNU sparse file.
	off := bytes.Index(archive, []byte("sparse\x00"))
	patchHeader(archive, off, func(blk []byte) {
		blk[156] = tar.TypeGNUSparse
		octal(blk[386:398], 0, 12)
		octal(blk[398:410], 1024, 12)
		octal(blk[483:495], 4096, 12)
	})
	res, _ := build(t, st, archive)
	want := []Skip{
		{Layer: digestOf("layer0"), Path: "../escape", Reason: "path escapes the root"},
		{Layer: digestOf("layer0"), Path: "sparse", Reason: "sparse file"},
		{Layer: digestOf("layer0"), Path: "dangling", Reason: "hard link target not found"},
		{Layer: digestOf("layer0"), Path: "weird", Reason: `unsupported type 'X'`},
		{Layer: digestOf("layer0"), Path: "l1/inside", Reason: "symlink loop"},
	}
	if res.SkippedCount != len(want) || len(res.Skipped) != len(want) {
		t.Fatalf("skipped %d/%d: %+v", len(res.Skipped), res.SkippedCount, res.Skipped)
	}
	for i := range want {
		if res.Skipped[i] != want[i] {
			t.Fatalf("skip %d = %+v, want %+v", i, res.Skipped[i], want[i])
		}
	}
	got := export(t, st, res.Root)
	if len(got) != 3 || got["ok"].data != "ok" || got["l1"].hdr.Typeflag != tar.TypeSymlink || got["l2"].hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("tree = %v", keys(got))
	}
	if res.Entries != 3 {
		t.Fatalf("Entries = %d, want 3", res.Entries)
	}
}

func TestBuildSkipCap(t *testing.T) {
	st := openStore(t)
	var entries []tarEntry
	for i := 0; i < MaxSkipped+50; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("../e%03d", i), data: "x"})
	}
	res, _ := build(t, st, buildTar(t, tar.FormatPAX, entries...))
	if len(res.Skipped) != MaxSkipped || res.SkippedCount != MaxSkipped+50 {
		t.Fatalf("skipped %d recorded, %d counted", len(res.Skipped), res.SkippedCount)
	}
	if res.Skipped[0].Path != "../e000" || res.Skipped[MaxSkipped-1].Path != fmt.Sprintf("../e%03d", MaxSkipped-1) {
		t.Fatalf("recorded skips are not the first %d", MaxSkipped)
	}
}

func TestBuildEmptyAndDeterministic(t *testing.T) {
	st := openStore(t)
	none, _ := build(t, st)
	var empty bytes.Buffer
	if err := tar.NewWriter(&empty).Close(); err != nil {
		t.Fatal(err)
	}
	emptyLayer, _ := build(t, st, empty.Bytes())
	if none.Root != emptyLayer.Root || none.Entries != 0 || emptyLayer.Entries != 0 {
		t.Fatalf("empty trees differ: %s (%d) vs %s (%d)", none.Root, none.Entries, emptyLayer.Root, emptyLayer.Entries)
	}
	a := buildTar(t, tar.FormatPAX, tarEntry{name: "x", data: "x"}, tarEntry{name: "d/y", data: "y"})
	b := buildTar(t, tar.FormatPAX, tarEntry{name: "d/z", data: "z"})
	first, _ := build(t, st, a, b)
	second, _ := build(t, st, a, b)
	if first.Root != second.Root {
		t.Fatalf("same layers gave %s and %s", first.Root, second.Root)
	}
	if first.Entries != 4 {
		t.Fatalf("Entries = %d, want 4", first.Entries)
	}
}

func TestBuildLayerErrorAppliesNothing(t *testing.T) {
	st := openStore(t)
	good := buildTar(t, tar.FormatPAX, tarEntry{name: "a", data: "a"})
	bad := buildTar(t, tar.FormatGNU, tarEntry{name: "hard", data: strings.Repeat("p", 512)}, tarEntry{name: "b", data: "b"})
	patchHeader(bad, 0, func(blk []byte) { blk[156] = tar.TypeLink })
	b := New()
	if err := b.Apply(context.Background(), digestOf("good"), newMemLayer(t, st, good)); err != nil {
		t.Fatal(err)
	}
	err := b.Apply(context.Background(), digestOf("bad"), newMemLayer(t, st, bad))
	var le *LayerError
	if !errors.As(err, &le) || le.Layer != digestOf("bad") || !strings.HasPrefix(err.Error(), "layer "+string(digestOf("bad"))+": ") {
		t.Fatalf("err = %v, want a *LayerError for the bad layer", err)
	}
	w := st.NewWriter(context.Background())
	defer w.Abort()
	res, err := b.Write(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := export(t, st, res.Root)
	if len(got) != 1 || got["a"].data != "a" {
		t.Fatalf("tree after a failed layer = %v", keys(got))
	}

	// A store failure and a cancelled context are not layer errors.
	l := newMemLayer(t, st, good)
	boom := errors.New("boom")
	err = New().Apply(context.Background(), digestOf("good"), &failingRecipe{Layer: l, err: boom})
	if errors.As(err, &le) || !errors.Is(err, boom) {
		t.Fatalf("store failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = New().Apply(ctx, digestOf("good"), l)
	if errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./rootfs/ -run TestBuild 2>&1 | head -5`
Expected: `undefined: New`, `undefined: Result`.

- [ ] **Step 3: Implement `rootfs/builder.go`**

```go
package rootfs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// MaxSkipped is how many skipped entries a Result lists; SkippedCount keeps
// counting past it.
const MaxSkipped = 100

// Skip is one archive entry that was left out of the tree.
type Skip struct {
	Layer  oci.Digest `json:"layer"`
	Path   string     `json:"path"`
	Reason string     `json:"reason"`
}

// Result is a written tree.
type Result struct {
	Root         key.Key
	Entries      int // entries in the tree, the root excluded
	Skipped      []Skip
	SkippedCount int
}

// Builder merges layers into a root filesystem.
type Builder struct {
	t            *tree
	skipped      []Skip
	skippedCount int
}

// New returns a Builder with an empty root.
func New() *Builder { return &Builder{t: newTree()} }

// Apply parses layer and merges it over the layers applied before: its
// whiteouts remove what lower layers put there, then its entries are placed
// in archive order, so a whiteout never touches its own layer. Entries
// that cannot be represented are recorded as skips. A *LayerError means the
// archive could not be parsed and nothing of it was applied; any other
// error is a store failure or the context's.
func (b *Builder) Apply(ctx context.Context, digest oci.Digest, layer Layer) error {
	entries, err := parseLayer(ctx, layer)
	if err != nil {
		var se *storeError
		switch {
		case errors.As(err, &se):
			return fmt.Errorf("rootfs: layer %s: %w", digest, se.err)
		case ctx.Err() != nil:
			return err
		}
		return &LayerError{Layer: digest, Err: err}
	}
	for _, e := range entries {
		switch e.kind {
		case kindWhiteout:
			b.record(digest, e, b.t.whiteout(e))
		case kindOpaque:
			b.record(digest, e, b.t.opaque(e))
		}
	}
	for _, e := range entries {
		switch e.kind {
		case kindWhiteout, kindOpaque:
		case kindSkip:
			b.record(digest, e, &skipError{e.reason})
		case kindHardlink:
			b.record(digest, e, b.t.link(e))
		default:
			b.record(digest, e, b.t.put(e))
		}
	}
	return nil
}

// record notes an entry the tree refused.
func (b *Builder) record(digest oci.Digest, e entry, err error) {
	if err == nil {
		return
	}
	b.skippedCount++
	if len(b.skipped) < MaxSkipped {
		b.skipped = append(b.skipped, Skip{Layer: digest, Path: e.path, Reason: err.Error()})
	}
}

// Write emits the tree through w, bottom-up with every directory's entries
// in bytewise name order, and returns the root key. Regular files reference
// the content keys the layers hold; only directory objects and spilled
// xattr sets are new.
func (b *Builder) Write(w *store.Writer) (Result, error) {
	root, n, err := emitDir(w, b.t.root)
	if err != nil {
		return Result{}, err
	}
	return Result{Root: root, Entries: n, Skipped: b.skipped, SkippedCount: b.skippedCount}, nil
}

func emitDir(w *store.Writer, dir *node) (key.Key, int, error) {
	d := w.NewDir()
	count := 0
	for _, name := range slices.Sorted(maps.Keys(dir.children)) {
		c := dir.children[name]
		e := fstree.Entry{Name: []byte(name), Mode: c.mode, UID: c.uid, GID: c.gid, Mtime: c.mtime}
		switch c.typ() {
		case store.TypeDir:
			k, n, err := emitDir(w, c)
			if err != nil {
				return key.Key{}, 0, err
			}
			e.ContentKey = k[:]
			count += n
		case store.TypeReg:
			e.ContentKey = c.content[:]
		case store.TypeLink:
			e.LinkTarget = []byte(c.link)
		case store.TypeChar, store.TypeBlock:
			e.Rdev = []uint64{c.rdev[0], c.rdev[1]}
		}
		if len(c.xattrs) > 0 {
			inline, spilled, err := w.PutXattrs(c.xattrs)
			if err != nil {
				return key.Key{}, 0, fmt.Errorf("rootfs: xattrs of %s: %w", name, err)
			}
			e.XattrsIn = inline
			if spilled != (key.Key{}) {
				e.XattrsKey = spilled[:]
			}
		}
		if err := d.AddEntry(e); err != nil {
			return key.Key{}, 0, fmt.Errorf("rootfs: %w", err)
		}
		count++
	}
	k, err := d.Finish()
	if err != nil {
		return key.Key{}, 0, fmt.Errorf("rootfs: finishing directory: %w", err)
	}
	return k, count, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./rootfs/ 2>&1 | tail -20`
Expected: `ok`. `tarexport` names directories with or without a trailing slash and may or may not emit the root; `export` cleans names, so only the entry set matters. If `TestBuildSkips` fails on the sparse patch offset, print `off` and check it lands on a header block (multiple of 512).

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./rootfs/
git add rootfs/builder.go rootfs/builder_test.go
git commit -m "rootfs: build and write the merged root filesystem" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 7: image: store the rootfs in the image root

**Files:**
- Modify: `oci/manifest.go` (media type constant)
- Modify: `image/meta.go`, `image/store.go`, `image/stats.go`
- Create: `image/rootfs.go`
- Test: `image/rootfs_test.go`

**Interfaces:**
- Consumes: `rootfs.New/Apply/Write/Result/Skip/LayerError` (Task 6), `blob.(*Blob).Prism`, `blob.ErrNotPrism` (Task 2).
- Produces: `oci.MediaTypeDockerConfig`; `image.RootfsDir = "rootfs"`; `type RootfsStatus string` with `RootfsOK, RootfsPartial, RootfsUnavailable, RootfsNotApplicable`; `type Rootfs struct { Status RootfsStatus; Entries int; Reason string; Skipped []rootfs.Skip; SkippedCount int }`; `Meta.Rootfs *Rootfs`; `func (im *Image) Rootfs() (key.Key, bool)`.

- [ ] **Step 1: Write the failing tests**

Create `image/rootfs_test.go`:

```go
package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

type fsEntry struct {
	name, data, link string
	typ              byte
}

// layerTar writes a small gzipped layer.
func layerTar(t *testing.T, entries ...fsEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, en := range entries {
		hdr := &tar.Header{Name: en.name, Typeflag: en.typ, Linkname: en.link, Mode: 0o644, Uid: 1000, ModTime: time.Unix(1_700_000_000, 0), Format: tar.FormatPAX}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(en.data))
		}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(en.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// tarLayer pushes a gzipped tar layer built from entries.
func (e *env) tarLayer(entries ...fsEntry) oci.Descriptor {
	e.t.Helper()
	d, m := e.putBlob(layerMediaType, layerTar(e.t, entries...))
	if m.Kind != "prism" {
		e.t.Fatalf("layer stored %s (%s), want prism", m.Kind, m.RawReason)
	}
	return d
}

// walk lists every path under dir, sorted, with its entry.
func (e *env) walk(dir key.Key) map[string]store.Type {
	e.t.Helper()
	out := map[string]store.Type{}
	var rec func(prefix string, k key.Key)
	rec = func(prefix string, k key.Key) {
		entries, _, err := e.st.ListDir(k, "", 0)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ent := range entries {
			p := prefix + string(ent.Name)
			out[p] = store.Type(ent.Mode & store.TypeMask)
			if ent.Mode&store.TypeMask == store.TypeDir {
				ck, err := key.Parse(ent.ContentKey)
				if err != nil {
					e.t.Fatal(err)
				}
				rec(p+"/", ck)
			}
		}
	}
	rec("", dir)
	return out
}

// storedMeta reads the image root's meta.json bytes.
func (e *env) storedMeta(root key.Key) []byte {
	e.t.Helper()
	k, err := e.st.LookupKey(root, MetaFile)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := e.st.ReadFile(k)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func TestPutRootfsOK(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("rootfs-ok")
	l1 := e.tarLayer(fsEntry{name: "bin/", typ: tar.TypeDir}, fsEntry{name: "bin/app", data: "v1"}, fsEntry{name: "etc/old", data: "old"}, fsEntry{name: "lnk", typ: tar.TypeSymlink, link: "bin/app"})
	l2 := e.tarLayer(fsEntry{name: "bin/app", data: "v2"}, fsEntry{name: "etc/.wh.old"}, fsEntry{name: "etc/new", data: "new"})
	m := e.put("library/app", "v1", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, l1, l2)))
	if m.Rootfs == nil || m.Rootfs.Status != RootfsOK || m.Rootfs.Entries != 6 || m.Rootfs.Reason != "" || m.Rootfs.SkippedCount != 0 {
		t.Fatalf("Rootfs = %+v", m.Rootfs)
	}
	im, err := e.images.Open("library/app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	rk, ok := im.Rootfs()
	if !ok {
		t.Fatal("Open lost the rootfs")
	}
	want := map[string]store.Type{"bin": store.TypeDir, "bin/app": store.TypeReg, "etc": store.TypeDir, "etc/new": store.TypeReg, "lnk": store.TypeLink}
	got := e.walk(rk)
	if len(got) != len(want) {
		t.Fatalf("rootfs = %v, want %v", got, want)
	}
	for p, typ := range want {
		if got[p] != typ {
			t.Fatalf("%s: type %o, want %o (all: %v)", p, got[p], typ, got)
		}
	}
	bin, err := e.st.LookupKey(rk, "bin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := e.st.Lookup(bin, "app")
	if err != nil {
		t.Fatal(err)
	}
	ak, err := key.Parse(app.ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := e.st.ReadFile(ak); err != nil || string(data) != "v2" {
		t.Fatalf("bin/app = %q, %v", data, err)
	}
	if app.UID != 1000 || app.Mode != store.TypeReg|0o644 || app.Mtime != 1_700_000_000*int64(time.Second) {
		t.Fatalf("bin/app metadata = %+v", app)
	}
	root := e.resolve(ManifestRef("library/app", m.Digest))
	if rootEntry, err := e.st.Lookup(root, RootfsDir); err != nil || rootEntry.Mode != store.ModeDir {
		t.Fatalf("rootfs/ entry = %+v, %v", rootEntry, err)
	}
	var stored Meta
	if err := json.Unmarshal(e.storedMeta(root), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Rootfs == nil || *stored.Rootfs != *m.Rootfs {
		t.Fatalf("stored rootfs %+v, returned %+v", stored.Rootfs, m.Rootfs)
	}
	line := lastLine(e.logs.String(), "image pushed")
	if !strings.Contains(line, " rootfs=ok ") || !strings.Contains(line, " rootfs_entries=6 ") {
		t.Fatalf("log line: %s", line)
	}
}

func TestPutRootfsUnavailable(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("rootfs-raw")
	good := e.tarLayer(fsEntry{name: "a", data: "a"})
	raw, _ := e.layerBlob(4096)
	m := e.put("library/app", "raw", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, good, raw)))
	if m.Rootfs == nil || m.Rootfs.Status != RootfsUnavailable || m.Rootfs.Entries != 0 {
		t.Fatalf("Rootfs = %+v", m.Rootfs)
	}
	if want := fmt.Sprintf("layer %s is stored raw (not-tar)", raw.Digest); m.Rootfs.Reason != want {
		t.Fatalf("reason %q, want %q", m.Rootfs.Reason, want)
	}
	im, err := e.images.Open("library/app", "raw")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := im.Rootfs(); ok {
		t.Fatal("an unavailable rootfs has a key")
	}
	root := e.resolve(ManifestRef("library/app", m.Digest))
	if _, err := e.st.Lookup(root, RootfsDir); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rootfs/ present: %v", err)
	}
	warn := lastLine(e.logs.String(), "rootfs unavailable")
	if !strings.Contains(warn, "level=WARN") || !strings.Contains(warn, "repo=library/app") || !strings.Contains(warn, "reason=") {
		t.Fatalf("warn line: %q", warn)
	}
	if line := lastLine(e.logs.String(), "image pushed"); !strings.Contains(line, " rootfs=unavailable ") || strings.Contains(line, "rootfs_entries") {
		t.Fatalf("log line: %s", line)
	}

	// An archive archive/tar cannot parse: a hard link carrying a payload.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "hard", Typeflag: tar.TypeReg, Size: 512, Mode: 0o644, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("p"), 512)); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "b", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("b"))
	tw.Close()
	archive := buf.Bytes()
	archive[156] = tar.TypeLink
	copy(archive[148:156], "        ")
	var sum int64
	for _, c := range archive[:512] {
		sum += int64(c)
	}
	copy(archive[148:156], fmt.Sprintf("%06o\x00 ", sum))
	bad, bm := e.putBlob(layerMediaType, archive)
	if bm.Kind != "prism" {
		t.Fatalf("crafted layer stored %s", bm.Kind)
	}
	m = e.put("library/app", "bad", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, good, bad)))
	if m.Rootfs.Status != RootfsUnavailable || !strings.HasPrefix(m.Rootfs.Reason, "layer "+string(bad.Digest)+": ") {
		t.Fatalf("Rootfs = %+v", m.Rootfs)
	}
}

func TestPutRootfsPartial(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("rootfs-partial")
	l := e.tarLayer(fsEntry{name: "ok", data: "ok"}, fsEntry{name: "dangling", typ: tar.TypeLink, link: "missing"})
	m := e.put("library/app", "partial", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, l)))
	if m.Rootfs.Status != RootfsPartial || m.Rootfs.Entries != 1 || m.Rootfs.SkippedCount != 1 || len(m.Rootfs.Skipped) != 1 {
		t.Fatalf("Rootfs = %+v", m.Rootfs)
	}
	if s := m.Rootfs.Skipped[0]; s.Layer != l.Digest || s.Path != "dangling" || s.Reason != "hard link target not found" {
		t.Fatalf("skip = %+v", s)
	}
	im, err := e.images.Open("library/app", "partial")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := im.Rootfs(); !ok {
		t.Fatal("partial rootfs has no key")
	}
	warn := lastLine(e.logs.String(), "rootfs partial")
	if !strings.Contains(warn, "level=WARN") || !strings.Contains(warn, "skipped=1") || !strings.Contains(warn, "path=dangling") {
		t.Fatalf("warn line: %q", warn)
	}
	if line := lastLine(e.logs.String(), "image pushed"); !strings.Contains(line, " rootfs=partial ") || !strings.Contains(line, " rootfs_entries=1 ") {
		t.Fatalf("log line: %s", line)
	}
	var stored Meta
	root := e.resolve(ManifestRef("library/app", m.Digest))
	if err := json.Unmarshal(e.storedMeta(root), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Rootfs.SkippedCount != 1 || len(stored.Rootfs.Skipped) != 1 || stored.Rootfs.Skipped[0] != m.Rootfs.Skipped[0] {
		t.Fatalf("stored rootfs %+v", stored.Rootfs)
	}
}

func TestPutRootfsNotApplicable(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.putBlob("application/vnd.example.config.v1+json", []byte(`{"example":true}`))
	l := e.tarLayer(fsEntry{name: "chart.yaml", data: "name: x"})
	m := e.put("library/chart", "v1", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, l)))
	if m.Rootfs == nil || m.Rootfs.Status != RootfsNotApplicable || m.Rootfs.Entries != 0 || m.Rootfs.Reason != "" {
		t.Fatalf("Rootfs = %+v", m.Rootfs)
	}
	root := e.resolve(ManifestRef("library/chart", m.Digest))
	if _, err := e.st.Lookup(root, RootfsDir); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rootfs/ present: %v", err)
	}
	if !bytes.Contains(e.storedMeta(root), []byte(`"status": "not-applicable"`)) {
		t.Fatalf("meta.json: %s", e.storedMeta(root))
	}
	if line := lastLine(e.logs.String(), "image pushed"); !strings.Contains(line, " rootfs=not-applicable ") {
		t.Fatalf("log line: %s", line)
	}
	if strings.Contains(e.logs.String(), "rootfs unavailable") {
		t.Fatal("not-applicable logged a warning")
	}

	// A Docker config counts as an image; an index carries no field.
	dcfg, _ := e.putBlob(oci.MediaTypeDockerConfig, []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`))
	dm := e.put("library/app", "docker", oci.MediaTypeDockerManifest, manifestBody(t, oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeDockerManifest, Config: &dcfg, Layers: []oci.Descriptor{l}}))
	if dm.Rootfs.Status != RootfsOK || dm.Rootfs.Entries != 1 {
		t.Fatalf("docker manifest rootfs = %+v", dm.Rootfs)
	}
	idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: []oci.Descriptor{{MediaType: oci.MediaTypeOCIManifest, Digest: dm.Digest, Size: dm.Size}}}
	im := e.put("library/app", "idx", oci.MediaTypeOCIIndex, manifestBody(t, idx))
	if im.Rootfs != nil {
		t.Fatalf("index rootfs = %+v", im.Rootfs)
	}
	if bytes.Contains(e.storedMeta(e.resolve(ManifestRef("library/app", im.Digest))), []byte(`"rootfs"`)) {
		t.Fatal("index meta.json carries a rootfs field")
	}
	if line := lastLine(e.logs.String(), "image pushed"); strings.Contains(line, "rootfs=") {
		t.Fatalf("index log line carries rootfs: %s", line)
	}
}

func TestPutRootfsReuseAndDeterminism(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("rootfs-reuse")
	l := e.tarLayer(fsEntry{name: "a", data: "a"}, fsEntry{name: "dangling", typ: tar.TypeLink, link: "missing"})
	body := manifestBody(t, imageManifest(cfg, l))
	first := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	firstKey, _ := mustOpen(t, e, "library/app", "v1").Rootfs()

	info, k, ok, err := e.images.reuseRootfs("library/app", first.Digest)
	if err != nil || !ok || k != firstKey || info.Status != RootfsPartial || info.SkippedCount != 1 {
		t.Fatalf("reuseRootfs = %+v, %s, %v, %v", info, k, ok, err)
	}
	if _, _, ok, err := e.images.reuseRootfs("library/other", first.Digest); err != nil || ok {
		t.Fatalf("reuseRootfs for an unknown repo = %v, %v", ok, err)
	}
	second := e.put("library/app", "v2", oci.MediaTypeOCIManifest, body)
	if *second.Rootfs != *first.Rootfs {
		t.Fatalf("re-push rootfs %+v, want %+v", second.Rootfs, first.Rootfs)
	}
	if k, _ := mustOpen(t, e, "library/app", "v2").Rootfs(); k != firstKey {
		t.Fatal("re-push changed the rootfs key")
	}

	// The same image in another repository is rebuilt to the same key.
	other := e.put("library/other", "v1", oci.MediaTypeOCIManifest, body)
	if k, _ := mustOpen(t, e, "library/other", "v1").Rootfs(); k != firstKey || *other.Rootfs != *first.Rootfs {
		t.Fatalf("other repo: key %s, rootfs %+v", k, other.Rootfs)
	}
}

func mustOpen(t *testing.T, e *env, repo, ref string) *Image {
	t.Helper()
	im, err := e.images.Open(repo, ref)
	if err != nil {
		t.Fatal(err)
	}
	return im
}
```

`store.Type` does not exist: in `walk` use `map[string]uint64` and `ent.Mode & store.TypeMask` instead (fix the three spots). `lastLine` is in `image/stats_test.go`.

Note `*stored.Rootfs != *m.Rootfs` compares a struct holding a slice: replace both such comparisons with `rootfsEqual(a, b *Rootfs) bool` that compares fields and `slices.Equal(a.Skipped, b.Skipped)`; add it to the test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop --command go test ./image/ -run TestPutRootfs 2>&1 | head -5`
Expected: `undefined: RootfsOK`, `m.Rootfs undefined`.

- [ ] **Step 3: Implement**

`oci/manifest.go`, in the media type const block:

```go
	MediaTypeDockerConfig       = "application/vnd.docker.container.image.v1+json"
```

`image/meta.go`: add `"github.com/amber-store/oci-amber/rootfs"` to the imports, `RootfsDir = "rootfs" // the merged root filesystem, manifests with status ok or partial` to the entry-name const block, and:

```go
// RootfsStatus says whether an image root holds a rootfs/ and why not.
type RootfsStatus string

const (
	RootfsOK            RootfsStatus = "ok"             // rootfs/ holds every entry of every layer
	RootfsPartial       RootfsStatus = "partial"        // rootfs/ is present; Skipped lists what was left out
	RootfsUnavailable   RootfsStatus = "unavailable"    // no rootfs/; Reason says which layer prevented it
	RootfsNotApplicable RootfsStatus = "not-applicable" // the manifest does not describe a container image
)

// Rootfs is the rootfs field of a manifest's meta.json. Entries is the
// number of entries under rootfs/, the root excluded; it is 0 unless Status
// is RootfsOK or RootfsPartial.
type Rootfs struct {
	Status       RootfsStatus  `json:"status"`
	Entries      int           `json:"entries"`
	Reason       string        `json:"reason,omitempty"`
	Skipped      []rootfs.Skip `json:"skipped,omitempty"`
	SkippedCount int           `json:"skippedCount,omitempty"`
}
```

and the field `Rootfs *Rootfs `json:"rootfs,omitempty"`` in `Meta` after `Stats`.

Create `image/rootfs.go`:

```go
package image

import (
	"context"
	"errors"
	"fmt"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
)

// rootfsApplies reports whether m describes a container image whose layers
// form a root filesystem: an image manifest with an OCI or Docker image
// config. Indexes and artifacts do not.
func rootfsApplies(m *oci.Manifest) bool {
	if m.IsIndex() || m.Config == nil {
		return false
	}
	switch m.Config.MediaType {
	case oci.MediaTypeOCIConfig, oci.MediaTypeDockerConfig:
		return true
	}
	return false
}

// buildRootfs produces the rootfs field of the manifest m being pushed to
// repo and, for ok and partial, the key of its rootfs/ directory, writing
// new objects through w. A root of the same digest already in repo lends
// its field and key. A raw layer or one whose archive cannot be parsed
// makes the field unavailable; nothing is written then. Any other failure
// is returned.
func (s *Store) buildRootfs(ctx context.Context, w *store.Writer, repo string, digest oci.Digest, m *oci.Manifest) (*Rootfs, key.Key, error) {
	if !rootfsApplies(m) {
		return &Rootfs{Status: RootfsNotApplicable}, key.Key{}, nil
	}
	if info, k, ok, err := s.reuseRootfs(repo, digest); err != nil {
		return nil, key.Key{}, err
	} else if ok {
		return info, k, nil
	}
	b := rootfs.New()
	for _, d := range m.Layers {
		bl, err := s.blobs.Open(d.Digest)
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: opening layer %s: %w", d.Digest, err)
		}
		prism, err := bl.Prism()
		if errors.Is(err, blob.ErrNotPrism) {
			return &Rootfs{Status: RootfsUnavailable, Reason: fmt.Sprintf("layer %s is stored raw (%s)", d.Digest, bl.Meta.RawReason)}, key.Key{}, nil
		}
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: layer %s: %w", d.Digest, err)
		}
		err = b.Apply(ctx, d.Digest, prism)
		var le *rootfs.LayerError
		if errors.As(err, &le) {
			return &Rootfs{Status: RootfsUnavailable, Reason: err.Error()}, key.Key{}, nil
		}
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: building rootfs: %w", err)
		}
	}
	res, err := b.Write(w)
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("image: writing rootfs: %w", err)
	}
	info := &Rootfs{Status: RootfsOK, Entries: res.Entries}
	if res.SkippedCount > 0 {
		info.Status, info.Skipped, info.SkippedCount = RootfsPartial, res.Skipped, res.SkippedCount
	}
	return info, res.Root, nil
}

// reuseRootfs returns the rootfs field and key of the root that
// oci/manifest/<repo>/<digest> already points at, when that root carries a
// rootfs field. The same digest has the same layers, so nothing needs to
// be rebuilt on a re-push.
func (s *Store) reuseRootfs(repo string, digest oci.Digest) (*Rootfs, key.Key, bool, error) {
	name := ManifestRef(repo, digest)
	root, err := s.st.Resolve(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, key.Key{}, false, nil
	}
	if err != nil {
		return nil, key.Key{}, false, fmt.Errorf("image: resolving %s: %w", name, err)
	}
	meta, err := s.readMeta(root)
	if err != nil {
		return nil, key.Key{}, false, err
	}
	if meta.Rootfs == nil {
		return nil, key.Key{}, false, nil
	}
	var k key.Key
	if meta.Rootfs.Status == RootfsOK || meta.Rootfs.Status == RootfsPartial {
		if k, err = s.st.LookupKey(root, RootfsDir); err != nil {
			return nil, key.Key{}, false, fmt.Errorf("image: %s in root %s: %w", RootfsDir, root, err)
		}
	}
	return meta.Rootfs, k, true, nil
}

// logRootfs warns about a rootfs that is missing or incomplete.
func (s *Store) logRootfs(repo string, digest oci.Digest, r *Rootfs) {
	switch {
	case r == nil:
	case r.Status == RootfsUnavailable:
		s.log.Warn("rootfs unavailable", "repo", repo, "digest", string(digest), "reason", r.Reason)
	case r.Status == RootfsPartial && len(r.Skipped) > 0:
		s.log.Warn("rootfs partial", "repo", repo, "digest", string(digest), "skipped", r.SkippedCount,
			"path", r.Skipped[0].Path, "reason", r.Skipped[0].Reason)
	}
}
```

`image/store.go`:

- In `Image`, add `rootfs key.Key` and `hasRootfs bool`, and:

```go
// Rootfs returns the key of the image's rootfs/ directory, when Meta.Rootfs
// says one is present.
func (im *Image) Rootfs() (key.Key, bool) { return im.rootfs, im.hasRootfs }
```

- In `Open`, after `manifestKey` is looked up:

```go
	im := &Image{Meta: meta, root: root, manifest: manifestKey, st: s.st}
	if meta.Rootfs != nil && (meta.Rootfs.Status == RootfsOK || meta.Rootfs.Status == RootfsPartial) {
		k, err := s.st.LookupKey(root, RootfsDir)
		if err != nil {
			return nil, fmt.Errorf("image: %s in root %s: %w", RootfsDir, root, err)
		}
		im.rootfs, im.hasRootfs = k, true
	}
	return im, nil
```

- In `Put`, replace the block from `// Pass one:` up to `w.PutBytes(body)` with:

```go
	// Pass one: the rootfs (image manifests only), then the manifest's own
	// objects (manifest bytes, blobs/ and manifests/), all through the
	// accounting writer.
	w := s.st.NewWriter(ctx)
	defer w.Abort()
	var rootfsKey key.Key
	if !m.IsIndex() {
		meta.Rootfs, rootfsKey, err = s.buildRootfs(ctx, w, repo, digest, m)
		if err != nil {
			return nil, err
		}
	}
	manifestKey, err := w.PutBytes(body)
```

  and in the root build, after `root.AddFile(MetaFile, metaKey)`:

```go
	if rootfsKey != (key.Key{}) {
		if err := root.AddDir(RootfsDir, rootfsKey); err != nil {
			return nil, fmt.Errorf("image: building root: %w", err)
		}
	}
```

  and after `s.logPushed(...)`: `s.logRootfs(repo, digest, meta.Rootfs)`.

`image/stats.go`, in `logPushed`, build the attribute list so the rootfs keys come after `manifests`:

```go
	attrs := []any{
		"repo", repo,
		"reference", reference,
		"digest", string(m.Digest),
		"kind", string(m.Kind),
		"blobs", blobs,
		"manifests", manifests,
	}
	if m.Rootfs != nil {
		attrs = append(attrs, "rootfs", string(m.Rootfs.Status))
		if m.Rootfs.Status == RootfsOK || m.Rootfs.Status == RootfsPartial {
			attrs = append(attrs, "rootfs_entries", m.Rootfs.Entries)
		}
	}
	attrs = append(attrs,
		"total_bytes", m.Stats.TotalBytes,
		"logical_bytes", m.Stats.LogicalBytes,
		"deduped_bytes", m.Stats.DedupedBytes,
		"deduped_percent", roundTo(m.Stats.DedupedPercent(), 1),
		"disk_bytes", m.Stats.DiskBytes,
		"compression_ratio", roundTo(m.Stats.CompressionRatio(), 2),
		"duration", d,
	)
	s.log.Info("image pushed", attrs...)
```

Update the doc comment's example line to include `rootfs=ok rootfs_entries=4213`.

- [ ] **Step 4: Run the tests**

Run: `nix develop --command go test ./image/ ./registry/ 2>&1 | tail -20`
Expected: `ok` for both. Existing image tests push raw random layers with an OCI config, so their meta now reports `unavailable`; `TestPutRootLayout` compares `stored` with `*m` field by field and needs `Rootfs` compared with `rootfsEqual` (or left out of the comparison). The e2e test's `image pushed` records gain keys; nothing there asserts an exact key set.

- [ ] **Step 5: Commit**

```bash
nix develop --command go vet ./...
git add oci/manifest.go image/
git commit -m "image: store the merged root filesystem in image roots" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 8: end-to-end check and documentation

**Files:**
- Modify: `registry/e2e_test.go`
- Modify: `README.md`, `docs/followups.md`, `docs/superpowers/specs/2026-09-04-rootfs-view-design.md`

- [ ] **Step 1: Add the rootfs walk to the end-to-end test**

In `registry/e2e_test.go`:

- Add `images *image.Store` to `e2eEnv` and set it in `newE2EEnv` (`images := image.New(...)` already exists there).
- Give layer B a whiteout so the merge is visible: add `{name: ".wh.var"}` to `fx.tarB`'s entries. Then `grep -n "entries" registry/e2e_test.go` and, if phase 2 asserts layer B's `entries`, raise it by one (the whiteout is a zero-length regular file, so tar-prism counts it).
- Add a phase, called from `TestE2EPushPull` after the push phase (find where `e.push()` is called and add `e.checkRootfs()` after it):

```go
// checkRootfs opens image 1 and walks its rootfs/: layer A with layer B's
// files merged in and var/ whited out.
func (e *e2eEnv) checkRootfs() {
	t := e.t
	im, err := e.images.Open(e2eApp, e.m1.String())
	if err != nil {
		t.Fatalf("Open image 1: %v", err)
	}
	if im.Meta.Rootfs == nil || im.Meta.Rootfs.Status != image.RootfsOK {
		t.Fatalf("image 1 rootfs = %+v, want ok", im.Meta.Rootfs)
	}
	root, ok := im.Rootfs()
	if !ok {
		t.Fatal("image 1 has no rootfs key")
	}
	got := map[string]fstree.Entry{}
	var walk func(prefix string, k key.Key)
	walk = func(prefix string, k key.Key) {
		entries, _, err := e.st.ListDir(k, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range entries {
			p := prefix + string(ent.Name)
			got[p] = ent
			if ent.Mode&store.TypeMask == store.TypeDir {
				ck, err := key.Parse(ent.ContentKey)
				if err != nil {
					t.Fatal(err)
				}
				walk(p+"/", ck)
			}
		}
	}
	walk("", root)
	longDir := "share/doc/" + strings.TrimSuffix(strings.Repeat("a-rather-long-directory-name/", 5), "/")
	want := []string{"bin", "bin/app", "bin/app-link", "etc", "etc/config.yaml", "etc/hostname", "etc/hosts", "etc/os-release",
		"lib", "lib/libfoo.so", "lib/libfoo.so.1", "share", "share/readme.txt", "share/doc"}
	parts := strings.Split(longDir, "/")
	for i := 3; i <= len(parts); i++ {
		want = append(want, strings.Join(parts[:i], "/"))
	}
	want = append(want, longDir+"/NOTICE")
	sort.Strings(want)
	names := make([]string, 0, len(got))
	for p := range got {
		names = append(names, p)
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Fatalf("rootfs paths:\n got %v\nwant %v", names, want)
	}
	if im.Meta.Rootfs.Entries != len(want) {
		t.Fatalf("rootfs entries = %d, want %d", im.Meta.Rootfs.Entries, len(want))
	}
	if e := got["bin/app-link"]; e.Mode&store.TypeMask != store.TypeLink || string(e.LinkTarget) != "app" {
		t.Fatalf("bin/app-link = %+v", e)
	}
	if !bytes.Equal(got["lib/libfoo.so"].ContentKey, got["lib/libfoo.so.1"].ContentKey) {
		t.Fatal("hard link does not share the target's content key")
	}
	app, err := key.Parse(got["bin/app"].ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	if app.Length() != uint64(len(e.fx.big)) {
		t.Fatalf("bin/app is %d bytes, want %d", app.Length(), len(e.fx.big))
	}
	for _, rec := range e.logs.records(t, "image pushed") {
		if rec["digest"] == e.m1.String() && rec["rootfs"] != "ok" {
			t.Fatalf("image 1 push line: %v", rec)
		}
	}
}
```

  Add `"sort"`, `"slices"`, `"github.com/jobs-build/amber-store-core/fstree"` to the imports if absent (`key`, `store`, `image` are already imported).

- [ ] **Step 2: Run the end-to-end and crane tests**

Run: `nix develop --command go test ./registry/ ./cmd/... 2>&1 | tail -10`
Expected: `ok`. If the walk finds `var` or `var/empty`, the whiteout did not land in layer B: check `fx.tarB`.

- [ ] **Step 3: Document**

README:

- Storage layout: the image root list gains `rootfs/` with the sentence: "An image root of a container image also holds `rootfs/`, the root filesystem the layers produce: an amber directory tree with the tar's modes, ownership, mtimes, symlinks, hard links, devices and xattrs, whose regular files point at the content the layers' prisms already store. `meta.json` carries a `rootfs` object with `status` (`ok`, `partial`, `unavailable`, `not-applicable`), `entries`, and for `partial` the first 100 skipped entries and their count, for `unavailable` the reason. A raw layer, a sparse file, a hard link carrying a payload, or a path escaping the root cannot be represented; the push succeeds and the status says so."
- Logging: `image pushed` gains `rootfs=<status>` and `rootfs_entries=<n>` on manifests; document the two Warn lines `rootfs unavailable` (`repo`, `digest`, `reason`) and `rootfs partial` (`repo`, `digest`, `skipped`, `path`, `reason`).
- The `blob stored` paragraph: an empty archive (only zero blocks) is a prism with `entries=0`.
- Limitations: add "The rootfs view skips sparse files and entries it cannot place, and has no view for images with a raw layer. Building it reads tar headers only." and "No HTTP surface serves `rootfs/` yet."

`docs/followups.md`: add a section "### rootfs view" with the deferred items from the spec's Follow-ups section.

Spec: under a new "## Deviations" heading at the end, record anything the implementation changed (for example the `entries` field is always present and 0 when there is no tree).

- [ ] **Step 4: Full verification**

Run: `nix develop --command go vet ./... && nix develop --command go test -race ./... 2>&1 | tail -15`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add registry/e2e_test.go README.md docs/followups.md docs/superpowers/specs/2026-09-04-rootfs-view-design.md
git commit -m "registry,docs: walk the stored rootfs end to end and document it" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>" -m "Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```
