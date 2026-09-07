# Import Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `oci-amber import <archive>` stores a `docker image save` archive into an amber store through the push pipeline, with a Bubble Tea progress UI and an end-of-run report.

**Architecture:** `dockerarchive` reads the tar in place and turns it into a plan (blobs, manifests with a pruned index, names). `importer` drives `blob.Store.Put` and `image.Store.Put` from the plan, fed by a new `blob.Observer` hook, and keeps a `Tracker` whose `Snapshot` the `tui` package renders. `cmd/oci-amber` gains the `import` subcommand.

**Tech Stack:** Go 1.26, urfave/cli v2, charmbracelet/bubbletea v1.3.10, bubbles v1.0.0, lipgloss v1.1.0, charmbracelet/x/term (transitive).

**Spec:** `docs/superpowers/specs/2026-09-05-import-command-design.md`

## Global Constraints

- Flat top-level packages; never `internal/`. Test-support code that several packages share lives in `dockerarchive/archivetest`.
- No new dependencies beyond bubbletea, bubbles, lipgloss (x/term is already transitive).
- Every `go test ./...` must pass with `-race`; run `gofmt -l .` and `go vet ./...` before each commit.
- Commits end with the trailer block:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
  ```
- Delete any binary you build (`go build -o ...`) before finishing a task.
- Doc comments in the style of the existing packages: full sentences, say why.

---

### Task 1: Section spools in `upload`

**Files:**
- Modify: `upload/spool.go`
- Test: `upload/spool_test.go`

**Interfaces:**
- Produces: `func NewSectionSpool(r io.ReaderAt, off, size int64, d oci.Digest) *Spool`. `Open` returns an `*io.SectionReader` over `[off, off+size)`; `Remove` only marks the spool removed.

- [ ] **Step 1: Write the failing tests**

Append to `upload/spool_test.go`:

```go
func TestSectionSpoolReadsOnlyItsWindow(t *testing.T) {
	data := []byte("0123456789abcdef")
	window := data[4:12]
	sp := NewSectionSpool(bytes.NewReader(data), 4, int64(len(window)), oci.DigestOfBytes(window))
	if sp.Size() != 8 {
		t.Fatalf("Size = %d, want 8", sp.Size())
	}
	if sp.Digest() != oci.DigestOfBytes(window) {
		t.Fatalf("Digest = %s", sp.Digest())
	}
	r, err := sp.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789ab" {
		t.Fatalf("read %q, want %q", got, "456789ab")
	}
	buf := make([]byte, 3)
	if _, err := r.ReadAt(buf, 5); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "9ab" {
		t.Fatalf("ReadAt(5) = %q, want %q", buf, "9ab")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(r)
	if string(again) != "456789ab" {
		t.Fatalf("after Seek read %q", again)
	}
}

func TestSectionSpoolRemoveLeavesSourceAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sp := NewSectionSpool(f, 6, 5, oci.DigestOfBytes([]byte("world")))
	if err := sp.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Remove must not touch the source file: %v", err)
	}
	if _, err := sp.Open(); err == nil {
		t.Fatal("Open after Remove must fail")
	}
}
```

Add `"bytes"`, `"io"`, `"os"`, `"path/filepath"` and `"github.com/amber-store/oci-amber/oci"` to the test file's imports if missing.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./upload -run 'TestSectionSpool' -v`
Expected: compile error `undefined: NewSectionSpool`.

- [ ] **Step 3: Implement**

In `upload/spool.go`, add fields to `Spool`:

```go
type Spool struct {
	size    int64
	digest  oci.Digest
	mem     []byte
	path    string
	ra      io.ReaderAt // section spool: the source, read at off
	off     int64
	touch   func()
	removed bool
}
```

Add the constructor after `NewMemorySpool`:

```go
// NewSectionSpool returns a Spool over the size bytes of r that start at
// off, vouched for by d: the caller has verified, or will verify before
// the blob is stored, that they hash to d. Nothing is copied; Open returns
// an io.SectionReader over that window, so r must stay usable for as long
// as the spool is, and may be shared by several spools (io.ReaderAt is
// safe for concurrent use). Remove marks the spool removed and leaves r
// alone, since the source is the caller's, an archive typically.
func NewSectionSpool(r io.ReaderAt, off, size int64, d oci.Digest) *Spool {
	return &Spool{size: size, digest: d, ra: r, off: off}
}
```

In `Open`, before the `sp.path == ""` branch:

```go
	if sp.ra != nil {
		return io.NewSectionReader(sp.ra, sp.off, sp.size), nil
	}
```

In `Remove`, after `sp.mem = nil`:

```go
	sp.ra = nil
```

Update the `Spool` doc comment's first sentence to "backed by a byte slice, by a file under the manager's directory, or by a window of a caller-owned io.ReaderAt".

- [ ] **Step 4: Run the tests**

Run: `go test ./upload -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add upload/spool.go upload/spool_test.go
git commit -m "upload: section spools over a caller-owned reader"
```

---

### Task 2: Observer hook in `blob`

**Files:**
- Create: `blob/observer.go`
- Modify: `blob/store.go` (`Options`, `Put`, `finalizeRaw`), `blob/analyze.go` (`analyze`), `blob/prism.go` (`ingestPrism`, `finalizePrism`, `roundTripCheck`), `blob/raw.go` (`ingestRaw`)
- Test: `blob/observer_test.go`

**Interfaces:**
- Produces:
  ```go
  type Stage string
  const (StageAnalyze Stage = "analyze"; StageDecompose Stage = "decompose"; StageVerify Stage = "verify"; StageRaw Stage = "raw")
  type Observer interface {
      BlobStage(d oci.Digest, s Stage)
      BlobProgress(d oci.Digest, n int64)
  }
  // Options gains: Observer Observer
  ```

- [ ] **Step 1: Write the failing tests**

Create `blob/observer_test.go`:

```go
package blob

import (
	"compress/gzip"
	"context"
	"sync"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

// recorder is an Observer that keeps every call in order.
type recorder struct {
	mu     sync.Mutex
	stages []Stage
	// progress holds, per stage in the order entered, every n reported.
	progress [][]int64
}

func (r *recorder) BlobStage(_ oci.Digest, s Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, s)
	r.progress = append(r.progress, nil)
}

func (r *recorder) BlobProgress(_ oci.Digest, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.progress) == 0 {
		panic("progress before any stage")
	}
	r.progress[len(r.progress)-1] = append(r.progress[len(r.progress)-1], n)
}

// assertStages checks the stage sequence and that, within each stage, the
// counts never decrease and the stages named in reachSize end at size.
func (r *recorder) assertStages(t *testing.T, size int64, want []Stage, reachSize ...Stage) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stages) != len(want) {
		t.Fatalf("stages = %v, want %v", r.stages, want)
	}
	for i := range want {
		if r.stages[i] != want[i] {
			t.Fatalf("stages = %v, want %v", r.stages, want)
		}
		var last int64 = -1
		for _, n := range r.progress[i] {
			if n < last {
				t.Fatalf("stage %s: progress went backwards: %v", want[i], r.progress[i])
			}
			if n > size {
				t.Fatalf("stage %s: progress %d exceeds size %d", want[i], n, size)
			}
			last = n
		}
		for _, s := range reachSize {
			if s == want[i] && last != size {
				t.Fatalf("stage %s ended at %d, want %d", s, last, size)
			}
		}
	}
}

func TestObserverPrismStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(64<<10, 1)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %s (%s), want prism", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageDecompose, StageVerify}, StageAnalyze, StageDecompose, StageVerify)
}

func TestObserverPrismWithoutVerify(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: false, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(64<<10, 1)), gzip.DefaultCompression)
	putPrism(t, b, data)
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageDecompose}, StageAnalyze, StageDecompose)
}

func TestObserverRawStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := randomBytes(t, 40<<10) // not a tar: analyze decides raw, then the bytes are stored verbatim
	meta, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotTar {
		t.Fatalf("kind/reason = %s/%s, want raw/not-tar", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageRaw}, StageRaw)
}

func TestObserverDecomposeDowngrade(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	full := tarBytes(t, "usr/lib/app", textBytes(8<<10, 3))
	data := gzipBytes(t, full[:tarHeaderSize+1024], gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonDecomposeFailed {
		t.Fatalf("kind/reason = %s/%s, want raw/decompose-failed", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageDecompose, StageRaw}, StageAnalyze, StageRaw)
}

func TestObserverDedupHitReportsNothing(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(8<<10, 1)), gzip.DefaultCompression)
	putPrism(t, b, data)
	before := len(rec.stages)
	if _, err := b.Put(context.Background(), spoolOf(data)); err != nil {
		t.Fatal(err)
	}
	if len(rec.stages) != before {
		t.Fatalf("a dedup hit reported stages: %v", rec.stages[before:])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./blob -run TestObserver -v`
Expected: compile errors (`undefined: Stage`, `unknown field Observer`).

- [ ] **Step 3: Create `blob/observer.go`**

```go
package blob

import (
	"io"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// Stage is one phase of a blob's finalization, in the order Put runs them.
// Analyze always comes first; a prism continues with decompose and, when
// VerifyRoundTrip is set, verify; a raw decision or a downgrade ends with
// raw.
type Stage string

const (
	StageAnalyze   Stage = "analyze"   // zrecipe pass one and the engine search
	StageDecompose Stage = "decompose" // pass two: decompress and take the tar apart
	StageVerify    Stage = "verify"    // round-trip check
	StageRaw       Stage = "raw"       // storing the bytes verbatim
)

// Observer receives finalization progress from Put. BlobStage says d
// entered s; BlobProgress says n bytes of d, counted against its
// compressed size, have been handled in the current stage. n never
// decreases within a stage. In analyze it is the spool's sequential read
// position, which reaches the size when pass one is done and then holds
// while the engine search reads through ReadAt; in decompose it is the
// pass-two read position; in verify the bytes recompressed so far; in raw
// the bytes stored so far. A dedup hit reports nothing. Methods are called
// from the goroutines running Put, concurrently for different digests.
type Observer interface {
	BlobStage(d oci.Digest, s Stage)
	BlobProgress(d oci.Digest, n int64)
}

// observeStage reports a stage transition when an observer is configured.
func (b *Store) observeStage(d oci.Digest, s Stage) {
	if b.opts.Observer != nil {
		b.opts.Observer.BlobStage(d, s)
	}
}

// observeReader wraps r so that its highest sequential read position is
// reported for d; ReadAt does not count. Without an observer r is
// returned as is. The wrapper does not own r: the caller keeps closing
// the original.
func (b *Store) observeReader(d oci.Digest, r upload.ReaderAtSeeker) upload.ReaderAtSeeker {
	if b.opts.Observer == nil {
		return r
	}
	obs := b.opts.Observer
	return &progressReader{ReaderAtSeeker: r, report: func(n int64) { obs.BlobProgress(d, n) }}
}

// observeWriter wraps w so that the bytes written through it are reported
// for d. Without an observer w is returned as is.
func (b *Store) observeWriter(d oci.Digest, w io.Writer) io.Writer {
	if b.opts.Observer == nil {
		return w
	}
	obs := b.opts.Observer
	return &progressWriter{w: w, report: func(n int64) { obs.BlobProgress(d, n) }}
}

// progressReader tracks the position of sequential reads over a seekable
// reader and reports every new high-water mark.
type progressReader struct {
	upload.ReaderAtSeeker
	pos, high int64
	report    func(int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.ReaderAtSeeker.Read(buf)
	p.pos += int64(n)
	if p.pos > p.high {
		p.high = p.pos
		p.report(p.high)
	}
	return n, err
}

func (p *progressReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := p.ReaderAtSeeker.Seek(offset, whence)
	if err == nil {
		p.pos = pos
	}
	return pos, err
}

// progressWriter counts the bytes written through it.
type progressWriter struct {
	w      io.Writer
	n      int64
	report func(int64)
}

func (p *progressWriter) Write(buf []byte) (int, error) {
	n, err := p.w.Write(buf)
	p.n += int64(n)
	p.report(p.n)
	return n, err
}
```

- [ ] **Step 4: Wire the hook into the pipeline**

`blob/store.go`, `Options`: add after `RecentTTL`:

```go
	// Observer, when set, receives stage transitions and byte counts from
	// Put. The registry leaves it nil; `oci-amber import` drives its
	// progress display from it.
	Observer Observer
```

`blob/store.go`, `Put`: immediately before `dec, err := b.analyze(ctx, sp)` add `b.observeStage(d, StageAnalyze)`.

`blob/store.go`, `finalizeRaw`: first line of the function body, before `w := b.st.NewWriter(ctx)`: `b.observeStage(meta.Digest, StageRaw)`.

`blob/analyze.go`, `analyze`: after the `defer c.Close()` block that follows `r, err := sp.Open()`, add:

```go
	r = b.observeReader(sp.Digest(), r)
```

(`r` is declared as `upload.ReaderAtSeeker` by `sp.Open()`, so the assignment type-checks; the deferred Close stays on the original.)

`blob/raw.go`, `ingestRaw`: change `k, err := w.PutStream(r)` to `k, err := w.PutStream(b.observeReader(sp.Digest(), r))`.

`blob/prism.go`, `ingestPrism`: after the `src.Seek(0, io.SeekStart)` check, add `rd := b.observeReader(sp.Digest(), src)` and pass `&spoolReader{r: rd}` to `newDecompressor` instead of `&spoolReader{r: src}`.

`blob/prism.go`, `finalizePrism`: before `res, err := b.ingestPrism(...)` add `b.observeStage(d, StageDecompose)`; inside `if b.opts.VerifyRoundTrip {`, before `storedParams, err := ...`, add `b.observeStage(d, StageVerify)`.

`blob/prism.go`, `roundTripCheck`: replace `if err := composeRecompress(ctx, h, src, params); err != nil {` with

```go
	if err := composeRecompress(ctx, b.observeWriter(want, h), src, params); err != nil {
```

- [ ] **Step 5: Run the tests**

Run: `go test ./blob -race`
Expected: PASS, including every pre-existing test (nil observer path).

- [ ] **Step 6: Commit**

```bash
git add blob/observer.go blob/observer_test.go blob/store.go blob/analyze.go blob/prism.go blob/raw.go
git commit -m "blob: observer hook for finalization stages and progress"
```

---

### Task 3: Archive builder for tests (`dockerarchive/archivetest`)

**Files:**
- Create: `dockerarchive/archivetest/builder.go`
- Test: `dockerarchive/archivetest/builder_test.go`

**Interfaces:**
- Produces:
  ```go
  type Builder struct{ ... }
  func New() *Builder
  func (b *Builder) AddBlob(data []byte) oci.Digest                 // blobs/sha256/<hex>
  func (b *Builder) AddBlobAs(d oci.Digest, data []byte)            // wrong-digest fixtures
  type Layer struct{ MediaType string; Data []byte }
  func (b *Builder) AddImage(config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string) oci.Descriptor
  func (b *Builder) AddIndex(children []oci.Descriptor, annotations map[string]string) oci.Descriptor
  func AbsentManifest(p oci.Platform) oci.Descriptor                // a child that is not in the archive
  func Attestation(target oci.Descriptor) (config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string)
  func (b *Builder) Top(entries ...oci.Descriptor)                  // index.json manifests
  type LegacyEntry struct{ Config oci.Digest; RepoTags []string; Layers []oci.Digest }
  func (b *Builder) Legacy(entries ...LegacyEntry)                  // manifest.json
  func (b *Builder) NoIndex()                                        // omit index.json
  func (b *Builder) Bytes() []byte
  func (b *Builder) WriteFile(dir, name string) (string, error)
  ```

The builder writes the shape observed from Docker 29: `blobs/`, `blobs/sha256/`, every blob as a read-only regular file, then `index.json`, `manifest.json`, `oci-layout`.

- [ ] **Step 1: Write the failing test**

Create `dockerarchive/archivetest/builder_test.go`:

```go
package archivetest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

func TestBuilderWritesDockerSaveShape(t *testing.T) {
	b := New()
	img := b.AddImage([]byte(`{"os":"linux"}`), []Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: []byte("layer")}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	idx := b.AddIndex([]oci.Descriptor{img, AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
	b.Top(idx)
	b.Legacy(LegacyEntry{Config: oci.DigestOfBytes([]byte(`{"os":"linux"}`)), RepoTags: []string{"app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes([]byte("layer"))}})

	var names []string
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b.Bytes()))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			files[h.Name] = data
		}
	}
	// blobs: config, layer, manifest, index = 4; plus the two dirs and three files.
	if len(names) != 2+4+3 {
		t.Fatalf("entries: %v", names)
	}
	if names[0] != "blobs/" || names[1] != "blobs/sha256/" {
		t.Fatalf("directories first: %v", names[:2])
	}
	last := names[len(names)-3:]
	if last[0] != "index.json" || last[1] != "manifest.json" || last[2] != "oci-layout" {
		t.Fatalf("trailing files: %v", last)
	}
	var index oci.Manifest
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != idx.Digest {
		t.Fatalf("index.json = %s", files["index.json"])
	}
	var legacy []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0].Config != "blobs/sha256/"+oci.DigestOfBytes([]byte(`{"os":"linux"}`)).Hex() || legacy[0].RepoTags[0] != "app:v1" {
		t.Fatalf("manifest.json = %s", files["manifest.json"])
	}
	if string(files["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Fatalf("oci-layout = %s", files["oci-layout"])
	}
	if _, ok := files["blobs/sha256/"+img.Digest.Hex()]; !ok {
		t.Fatal("image manifest blob missing")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./dockerarchive/archivetest`
Expected: `no Go files` / undefined errors.

- [ ] **Step 3: Implement `dockerarchive/archivetest/builder.go`**

```go
// Package archivetest builds `docker image save` archives in memory for
// tests: an OCI layout tar with the file order Docker 29 writes (blobs
// first, then index.json, manifest.json and oci-layout).
package archivetest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/amber-store/oci-amber/oci"
)

// Layer is one layer to add to an image.
type Layer struct {
	MediaType string
	Data      []byte
}

// LegacyEntry is one manifest.json entry.
type LegacyEntry struct {
	Config   oci.Digest
	RepoTags []string
	Layers   []oci.Digest
}

type blob struct {
	digest oci.Digest
	data   []byte
}

// Builder accumulates blobs and the three top-level files.
type Builder struct {
	blobs   []blob
	top     []oci.Descriptor
	legacy  []LegacyEntry
	noIndex bool
}

// New returns an empty builder.
func New() *Builder { return &Builder{} }

// AddBlob adds data under its sha256.
func (b *Builder) AddBlob(data []byte) oci.Digest {
	d := oci.DigestOfBytes(data)
	b.AddBlobAs(d, data)
	return d
}

// AddBlobAs adds data under d, which need not match; use it to build a
// corrupt archive.
func (b *Builder) AddBlobAs(d oci.Digest, data []byte) {
	for _, existing := range b.blobs {
		if existing.digest == d {
			return
		}
	}
	b.blobs = append(b.blobs, blob{digest: d, data: data})
}

// AddImage adds config, every layer and an OCI image manifest over them,
// and returns the manifest's descriptor carrying platform and annotations.
func (b *Builder) AddImage(config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string) oci.Descriptor {
	m := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
	}
	for _, l := range layers {
		m.Layers = append(m.Layers, oci.Descriptor{MediaType: l.MediaType, Digest: b.AddBlob(l.Data), Size: int64(len(l.Data))})
	}
	body := mustJSON(m)
	return oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: b.AddBlob(body), Size: int64(len(body)), Platform: platform, Annotations: annotations}
}

// AddIndex adds an OCI index over children (present or absent) and returns
// its descriptor.
func (b *Builder) AddIndex(children []oci.Descriptor, annotations map[string]string) oci.Descriptor {
	m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: children, Annotations: annotations}
	body := mustJSON(m)
	return oci.Descriptor{MediaType: oci.MediaTypeOCIIndex, Digest: b.AddBlob(body), Size: int64(len(body))}
}

// AbsentManifest is a descriptor for a platform whose manifest was not
// saved: a plausible digest that is in no archive.
func AbsentManifest(p oci.Platform) oci.Descriptor {
	body := []byte("absent " + p.String())
	return oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(body), Size: 610, Platform: &p}
}

// Attestation returns the parts of a BuildKit attestation manifest for
// target: an empty config, one in-toto layer, the unknown/unknown platform
// and the vnd.docker.reference annotations. Pass them to AddImage.
func Attestation(target oci.Descriptor) (config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string) {
	config = []byte(`{"architecture":"unknown","os":"unknown","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	layers = []Layer{{MediaType: "application/vnd.in-toto+json", Data: []byte(`{"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://spdx.dev/Document","subject":[]}`)}}
	platform = &oci.Platform{OS: "unknown", Architecture: "unknown"}
	annotations = map[string]string{
		"vnd.docker.reference.digest": target.Digest.String(),
		"vnd.docker.reference.type":   "attestation-manifest",
	}
	return
}

// Top sets index.json's manifests.
func (b *Builder) Top(entries ...oci.Descriptor) { b.top = entries }

// Legacy sets manifest.json's entries.
func (b *Builder) Legacy(entries ...LegacyEntry) { b.legacy = entries }

// NoIndex omits index.json, producing a legacy-only archive.
func (b *Builder) NoIndex() { b.noIndex = true }

// Bytes renders the archive.
func (b *Builder) Bytes() []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := func(name string) {
		must(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}))
	}
	file := func(name string, mode int64, data []byte) {
		must(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}))
		_, err := tw.Write(data)
		must(err)
	}
	dir("blobs/")
	dir("blobs/sha256/")
	for _, bl := range b.blobs {
		file("blobs/sha256/"+bl.digest.Hex(), 0o444, bl.data)
	}
	if !b.noIndex {
		file("index.json", 0o644, mustJSON(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: b.top}))
	}
	type legacyJSON struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	legacy := make([]legacyJSON, 0, len(b.legacy))
	for _, e := range b.legacy {
		l := legacyJSON{Config: "blobs/sha256/" + e.Config.Hex(), RepoTags: e.RepoTags}
		for _, d := range e.Layers {
			l.Layers = append(l.Layers, "blobs/sha256/"+d.Hex())
		}
		legacy = append(legacy, l)
	}
	file("manifest.json", 0o644, mustJSON(legacy))
	file("oci-layout", 0o444, []byte(`{"imageLayoutVersion":"1.0.0"}`))
	must(tw.Close())
	return buf.Bytes()
}

// WriteFile renders the archive to <dir>/<name> and returns the path.
func (b *Builder) WriteFile(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, b.Bytes(), 0o644)
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	must(err)
	return out
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

Note `oci.Manifest` marshals `manifests` with `omitempty`; an index with no children marshals without the field, which the reader treats as an index by media type. Fine for tests.

- [ ] **Step 4: Run the test**

Run: `go test ./dockerarchive/archivetest -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add dockerarchive/archivetest
git commit -m "dockerarchive/archivetest: build docker save archives for tests"
```

---

### Task 4: Reading the archive (`dockerarchive.Open`)

**Files:**
- Create: `dockerarchive/archive.go`
- Test: `dockerarchive/archive_test.go`

**Interfaces:**
- Produces:
  ```go
  var ErrNoIndex, ErrBlobMissing error
  type LegacyEntry struct{ Config string; RepoTags []string; Layers []string }
  type Archive struct{ Index *oci.Manifest; Legacy []LegacyEntry; ... }
  func Open(path string) (*Archive, error)
  func (a *Archive) Close() error
  func (a *Archive) Has(d oci.Digest) bool
  func (a *Archive) Size(d oci.Digest) (int64, bool)
  func (a *Archive) Section(d oci.Digest) (*io.SectionReader, error)
  func (a *Archive) ReadBlob(d oci.Digest) ([]byte, error)              // verifies sha256
  func (a *Archive) Verify(ctx context.Context, d oci.Digest, progress func(n int64)) error
  ```

- [ ] **Step 1: Write the failing tests**

Create `dockerarchive/archive_test.go`:

```go
package dockerarchive

import (
	"context"
	"errors"
	"io"
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

func TestOpenIgnoresUnrelatedEntriesAndDotSlash(t *testing.T) {
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
```

Add helpers at the bottom of the same test file:

```go
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
```

(imports: add `"archive/tar"`, `"bytes"`, `"os"`, `"path/filepath"`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./dockerarchive`
Expected: compile errors, `undefined: Open`.

- [ ] **Step 3: Implement `dockerarchive/archive.go`**

```go
// Package dockerarchive reads the archives `docker image save` writes:
// an OCI image layout (oci-layout, index.json, blobs/sha256/*) inside a
// tar, plus Docker's legacy manifest.json carrying the RepoTags. Blobs are
// read in place from the archive file, never extracted.
package dockerarchive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// Names inside the archive.
const (
	IndexFile    = "index.json"
	ManifestFile = "manifest.json"
	LayoutFile   = "oci-layout"
	blobPrefix   = "blobs/sha256/"
)

// maxTopFile bounds index.json and manifest.json; maxSmallBlob bounds what
// ReadBlob loads whole (manifests, indexes, configs).
const (
	maxTopFile   = 16 << 20
	maxSmallBlob = 64 << 20
)

var (
	// ErrNoIndex reports an archive without index.json: a legacy-only
	// archive, which this package does not read.
	ErrNoIndex = errors.New("dockerarchive: no index.json in the archive; only OCI layout archives are supported (docker 25 or later writes one)")
	// ErrBlobMissing reports a digest with no blobs/sha256 entry.
	ErrBlobMissing = errors.New("dockerarchive: blob not in archive")
)

// LegacyEntry is one element of manifest.json. Paths are archive paths
// ("blobs/sha256/<hex>").
type LegacyEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type section struct{ off, size int64 }

// Archive is an open docker save archive: the blob table plus the parsed
// top-level files.
type Archive struct {
	f      *os.File
	blobs  map[oci.Digest]section
	Index  *oci.Manifest // index.json
	Legacy []LegacyEntry // manifest.json; nil when absent
}

// Open opens the archive at path and scans its headers once: every
// blobs/sha256/<hex> regular file is recorded by offset and size,
// index.json and manifest.json are read whole. The file stays open until
// Close.
func Open(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a, err := scan(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return a, nil
}

// scan reads the tar headers from f, which must be positioned at 0 and is
// read without buffering so that its offset after Next is the start of
// the entry's content.
func scan(f *os.File) (*Archive, error) {
	a := &Archive{f: f, blobs: make(map[oci.Digest]section)}
	tr := tar.NewReader(f)
	var index, legacy []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dockerarchive: reading tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch {
		case name == IndexFile:
			if index, err = readTop(tr, name, h.Size); err != nil {
				return nil, err
			}
		case name == ManifestFile:
			if legacy, err = readTop(tr, name, h.Size); err != nil {
				return nil, err
			}
		case strings.HasPrefix(name, blobPrefix):
			hex := name[len(blobPrefix):]
			d, err := oci.ParseDigest("sha256:" + hex)
			if err != nil {
				continue // not a blob path, an unrelated file
			}
			off, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, fmt.Errorf("dockerarchive: locating %s: %w", name, err)
			}
			a.blobs[d] = section{off: off, size: h.Size}
		}
	}
	if index == nil {
		return nil, ErrNoIndex
	}
	m, err := oci.ParseManifest(index)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: %s: %w", IndexFile, err)
	}
	if !m.IsIndex() {
		return nil, fmt.Errorf("dockerarchive: %s is not an image index", IndexFile)
	}
	a.Index = m
	if legacy != nil {
		if err := json.Unmarshal(legacy, &a.Legacy); err != nil {
			return nil, fmt.Errorf("dockerarchive: %s: %w", ManifestFile, err)
		}
	}
	return a, nil
}

func readTop(r io.Reader, name string, size int64) ([]byte, error) {
	if size > maxTopFile {
		return nil, fmt.Errorf("dockerarchive: %s is %d bytes, more than %d", name, size, maxTopFile)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: reading %s: %w", name, err)
	}
	return b, nil
}

// Close closes the archive file. Sections obtained before Close fail
// afterwards.
func (a *Archive) Close() error { return a.f.Close() }

// Has reports whether d has a blob in the archive.
func (a *Archive) Has(d oci.Digest) bool {
	_, ok := a.blobs[d]
	return ok
}

// Size returns d's size in the archive.
func (a *Archive) Size(d oci.Digest) (int64, bool) {
	s, ok := a.blobs[d]
	return s.size, ok
}

// Section returns a reader over d's bytes. The reader is an io.ReaderAt
// and safe for concurrent use with other sections.
func (a *Archive) Section(d oci.Digest) (*io.SectionReader, error) {
	s, ok := a.blobs[d]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBlobMissing, d)
	}
	return io.NewSectionReader(a.f, s.off, s.size), nil
}

// ReadBlob reads d whole and verifies the bytes hash to d. It is for the
// small blobs (manifests, indexes, configs); layers go through Section.
func (a *Archive) ReadBlob(d oci.Digest) ([]byte, error) {
	sec, err := a.Section(d)
	if err != nil {
		return nil, err
	}
	if sec.Size() > maxSmallBlob {
		return nil, fmt.Errorf("dockerarchive: blob %s is %d bytes, too large to read whole", d, sec.Size())
	}
	b, err := io.ReadAll(sec)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: reading %s: %w", d, err)
	}
	if got := oci.DigestOfBytes(b); got != d {
		return nil, fmt.Errorf("dockerarchive: blob %s does not match its name: content is %s", d, got)
	}
	return b, nil
}

// Verify streams d and checks its sha256, calling progress (when not nil)
// with the running byte count. It stops early when ctx is done.
func (a *Archive) Verify(ctx context.Context, d oci.Digest, progress func(n int64)) error {
	sec, err := a.Section(d)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		k, err := sec.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if progress != nil && k > 0 {
			progress(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("dockerarchive: reading %s: %w", d, err)
		}
	}
	if got := oci.DigestFromSum(h.Sum(nil)); got != d {
		return fmt.Errorf("dockerarchive: blob %s does not match its name: content is %s", d, got)
	}
	return nil
}
```

`archive/tar` seeks past entry content when its reader is an `io.Seeker`, and reads headers with `io.ReadFull` straight from `f`, so after `Next` the file offset is the first content byte. Do not wrap `f` in a `bufio.Reader`.

- [ ] **Step 4: Run the tests**

Run: `go test ./dockerarchive/... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add dockerarchive/archive.go dockerarchive/archive_test.go
git commit -m "dockerarchive: open a docker save archive and index its blobs"
```

---

### Task 5: Planning (`dockerarchive.Plan`)

**Files:**
- Create: `dockerarchive/plan.go`, `dockerarchive/names.go`
- Test: `dockerarchive/plan_test.go`, `dockerarchive/names_test.go`

**Interfaces:**
- Produces:
  ```go
  type Name struct{ Repo, Tag string }
  func (n Name) String() string
  func ParseName(s string) (Name, error)          // --name values, verbatim
  type PlanBlob struct{ Digest oci.Digest; Size int64; MediaType string; Present bool }
  type PlanManifest struct{ Digest oci.Digest; MediaType string; Body []byte; IsIndex bool; Synthesized bool }
  type PlanEntry struct{ Digest oci.Digest; Names []Name; IsIndex bool; Platforms, Attestations int; Manifests []oci.Digest }
  type Plan struct{ Blobs []PlanBlob; Manifests []PlanManifest; Entries []PlanEntry }
  type PlanOptions struct{ Names []string; Present func(oci.Digest) (bool, error) }
  func (a *Archive) Plan(opts PlanOptions) (*Plan, error)
  ```
  `PlanEntry.Manifests` is the entry's manifests in publish order: children before parents, the entry's own digest last. `Plan.Manifests` is unique across entries in first-use order.

- [ ] **Step 1: Write the failing name tests**

Create `dockerarchive/names_test.go`:

```go
package dockerarchive

import "testing"

func TestNameFromRepoTag(t *testing.T) {
	cases := []struct {
		in       string
		want     Name
		ok       bool
		wantErr  bool
	}{
		{"busybox:1.37", Name{"busybox", "1.37"}, true, false},
		{"library/busybox:1.37", Name{"library/busybox", "1.37"}, true, false},
		{"registry.example.ch/team/app:v1", Name{"team/app", "v1"}, true, false},
		{"localhost:5000/app:v1", Name{"app", "v1"}, true, false},
		{"localhost/app:v1", Name{"app", "v1"}, true, false},
		{"app", Name{"app", "latest"}, true, false},
		{"app@sha256:0000000000000000000000000000000000000000000000000000000000000000", Name{}, false, false},
		{"registry.example.ch/app@sha256:0000000000000000000000000000000000000000000000000000000000000000", Name{}, false, false},
		{"Bad Repo:v1", Name{}, false, true},
		{"app:bad tag", Name{}, false, true},
	}
	for _, c := range cases {
		got, ok, err := nameFromRepoTag(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v", c.in, err)
			continue
		}
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseName(t *testing.T) {
	good := map[string]Name{
		"app:v1":                          {"app", "v1"},
		"team/app:v1":                     {"team/app", "v1"},
		"registry.example.ch/team/app:v1": {"registry.example.ch/team/app", "v1"},
		"app":                             {"app", "latest"},
	}
	for in, want := range good {
		got, err := ParseName(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", ":v1", "app:", "app@sha256:abc", "UPPER:v1"} {
		if _, err := ParseName(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}
```

- [ ] **Step 2: Implement `dockerarchive/names.go`**

```go
package dockerarchive

import (
	"errors"
	"fmt"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// Name is a repository and tag to publish an image under.
type Name struct {
	Repo string
	Tag  string
}

func (n Name) String() string { return n.Repo + ":" + n.Tag }

// ParseName parses a --name value: repo[:tag], taken verbatim (a leading
// registry host is kept), with "latest" when the tag is missing. The tag
// is what follows the last ':' after the last '/'. Digest references are
// rejected: a name has to be a tag.
func ParseName(s string) (Name, error) {
	if s == "" {
		return Name{}, errors.New("empty name")
	}
	if strings.Contains(s, "@") {
		return Name{}, fmt.Errorf("%q: a name must be repo:tag, not a digest reference", s)
	}
	repo, tag := splitTag(s)
	if tag == "" {
		if strings.HasSuffix(s, ":") {
			return Name{}, fmt.Errorf("%q: empty tag", s)
		}
		tag = "latest"
	}
	if err := oci.ValidateRepository(repo); err != nil {
		return Name{}, fmt.Errorf("%q: %v", s, err)
	}
	if err := oci.ValidateTag(tag); err != nil {
		return Name{}, fmt.Errorf("%q: %v", s, err)
	}
	return Name{Repo: repo, Tag: tag}, nil
}

// splitTag splits repo[:tag] at the last ':' that follows the last '/'.
func splitTag(s string) (repo, tag string) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 || strings.IndexByte(s[i:], '/') >= 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// nameFromRepoTag turns a RepoTags entry into a Name: a leading registry
// host (a first path component holding '.' or ':', or "localhost") is
// dropped, a missing tag is "latest". ok is false for a digest reference,
// which names nothing to tag. Invalid repository or tag grammar is an
// error.
func nameFromRepoTag(s string) (Name, bool, error) {
	if strings.Contains(s, "@") {
		return Name{}, false, nil
	}
	if first, rest, found := strings.Cut(s, "/"); found && isHost(first) {
		s = rest
	}
	repo, tag := splitTag(s)
	if tag == "" {
		tag = "latest"
	}
	if err := oci.ValidateRepository(repo); err != nil {
		return Name{}, false, fmt.Errorf("RepoTags entry %q: %v", s, err)
	}
	if err := oci.ValidateTag(tag); err != nil {
		return Name{}, false, fmt.Errorf("RepoTags entry %q: %v", s, err)
	}
	return Name{Repo: repo, Tag: tag}, true, nil
}

// isHost is Docker's rule for the first component of a reference.
func isHost(c string) bool {
	return c == "localhost" || strings.ContainsAny(c, ".:")
}
```

Run: `go test ./dockerarchive -run 'TestNameFromRepoTag|TestParseName'` → PASS.

- [ ] **Step 3: Write the failing plan tests**

Create `dockerarchive/plan_test.go`:

```go
package dockerarchive

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/oci"
)

const gzipLayer = "application/vnd.oci.image.layer.v1.tar+gzip"

// busyboxLike builds the observed shape: a top index whose children are a
// present amd64 manifest, its attestation manifest, and two absent
// platforms; manifest.json names the amd64 image.
func busyboxLike(t *testing.T, tags ...string) (*archivetest.Builder, oci.Descriptor, oci.Descriptor, oci.Descriptor) {
	t.Helper()
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("gzip layer bytes")
	img := b.AddImage(config, []archivetest.Layer{{MediaType: gzipLayer, Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, map[string]string{"org.opencontainers.image.version": "1.37"})
	att := b.AddImage(archivetest.Attestation(img))
	idx := b.AddIndex([]oci.Descriptor{
		img,
		att,
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}),
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}),
	}, map[string]string{"keep": "me"})
	b.Top(idx)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: tags, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	return b, img, att, idx
}

func TestPlanPrunesIndexAndNamesEntry(t *testing.T) {
	b, img, att, idx := busyboxLike(t, "registry.example.ch/library/busybox:1.37", "busybox:latest")
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("entries = %+v", p.Entries)
	}
	e := p.Entries[0]
	if !e.IsIndex || e.Platforms != 1 || e.Attestations != 1 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Digest == idx.Digest {
		t.Fatal("pruned index must have a new digest")
	}
	wantNames := []Name{{"library/busybox", "1.37"}, {"busybox", "latest"}}
	if len(e.Names) != 2 || e.Names[0] != wantNames[0] || e.Names[1] != wantNames[1] {
		t.Fatalf("names = %v, want %v", e.Names, wantNames)
	}
	// Publish order: the two children, then the pruned index.
	if len(e.Manifests) != 3 || e.Manifests[2] != e.Digest {
		t.Fatalf("publish order = %v", e.Manifests)
	}
	if e.Manifests[0] != img.Digest || e.Manifests[1] != att.Digest {
		t.Fatalf("children order = %v, want %s, %s", e.Manifests[:2], img.Digest, att.Digest)
	}
	// The synthesized index keeps every other field and only the present children.
	var top *PlanManifest
	for i := range p.Manifests {
		if p.Manifests[i].Digest == e.Digest {
			top = &p.Manifests[i]
		}
	}
	if top == nil || !top.Synthesized || !top.IsIndex || top.MediaType != oci.MediaTypeOCIIndex {
		t.Fatalf("top manifest = %+v", top)
	}
	var pruned struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Manifests     []oci.Descriptor  `json:"manifests"`
		Annotations   map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(top.Body, &pruned); err != nil {
		t.Fatal(err)
	}
	if pruned.SchemaVersion != 2 || pruned.Annotations["keep"] != "me" || len(pruned.Manifests) != 2 {
		t.Fatalf("pruned index = %s", top.Body)
	}
	if pruned.Manifests[0].Digest != img.Digest || pruned.Manifests[0].Annotations["org.opencontainers.image.version"] != "1.37" || pruned.Manifests[1].Digest != att.Digest {
		t.Fatalf("pruned children = %+v", pruned.Manifests)
	}
	if oci.DigestOfBytes(top.Body) != e.Digest {
		t.Fatal("entry digest is not the digest of the synthesized body")
	}
	// Blobs: config, layer, attestation config, in-toto payload; unique, first use.
	if len(p.Blobs) != 4 {
		t.Fatalf("blobs = %+v", p.Blobs)
	}
	if p.Blobs[1].MediaType != gzipLayer || p.Blobs[1].Present {
		t.Fatalf("layer blob = %+v", p.Blobs[1])
	}
	if len(p.Manifests) != 3 {
		t.Fatalf("manifests = %d, want 3", len(p.Manifests))
	}
}

func TestPlanMarksPresentBlobs(t *testing.T) {
	b, _, _, _ := busyboxLike(t, "busybox:1.37")
	a := openBuilder(t, b)
	layer := oci.DigestOfBytes([]byte("gzip layer bytes"))
	p, err := a.Plan(PlanOptions{Present: func(d oci.Digest) (bool, error) { return d == layer, nil }})
	if err != nil {
		t.Fatal(err)
	}
	present := 0
	for _, bl := range p.Blobs {
		if bl.Present {
			present++
			if bl.Digest != layer {
				t.Fatalf("wrong blob marked present: %s", bl.Digest)
			}
		}
	}
	if present != 1 {
		t.Fatalf("present = %d, want 1", present)
	}
}

func TestPlanNameOverride(t *testing.T) {
	b, _, _, _ := busyboxLike(t, "busybox:1.37")
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{Names: []string{"mirror/busybox:1.37", "mirror/busybox:stable"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Entries[0].Names; len(got) != 2 || got[0] != (Name{"mirror/busybox", "1.37"}) || got[1] != (Name{"mirror/busybox", "stable"}) {
		t.Fatalf("names = %v", got)
	}
	if _, err := a.Plan(PlanOptions{Names: []string{"not valid"}}); err == nil {
		t.Fatal("invalid --name accepted")
	}
}

func TestPlanNoTagsIsAnError(t *testing.T) {
	b, _, _, _ := busyboxLike(t) // RepoTags empty: docker save by image id
	a := openBuilder(t, b)
	_, err := a.Plan(PlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want a hint about --name", err)
	}
}

func TestPlanMultiImageArchive(t *testing.T) {
	b := archivetest.New()
	cfgA, cfgB := []byte(`{"os":"linux","a":1}`), []byte(`{"os":"linux","b":1}`)
	shared := []byte("shared base layer")
	imgA := b.AddImage(cfgA, []archivetest.Layer{{MediaType: gzipLayer, Data: shared}, {MediaType: gzipLayer, Data: []byte("only a")}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	imgB := b.AddImage(cfgB, []archivetest.Layer{{MediaType: gzipLayer, Data: shared}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(imgA, imgB) // manifests directly at the top, as the graph driver writes them
	b.Legacy(
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(cfgA), RepoTags: []string{"a:1"}, Layers: []oci.Digest{oci.DigestOfBytes(shared), oci.DigestOfBytes([]byte("only a"))}},
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(cfgB), RepoTags: []string{"b:1"}, Layers: []oci.Digest{oci.DigestOfBytes(shared)}},
	)
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 || p.Entries[0].IsIndex || p.Entries[0].Names[0] != (Name{"a", "1"}) || p.Entries[1].Names[0] != (Name{"b", "1"}) {
		t.Fatalf("entries = %+v", p.Entries)
	}
	if len(p.Entries[0].Manifests) != 1 || p.Entries[0].Manifests[0] != imgA.Digest || p.Entries[1].Manifests[0] != imgB.Digest {
		t.Fatalf("manifest lists = %v / %v", p.Entries[0].Manifests, p.Entries[1].Manifests)
	}
	if len(p.Blobs) != 5 { // cfgA, shared, only a, cfgB; shared once
		t.Fatalf("blobs = %+v", p.Blobs)
	}
	if _, err := a.Plan(PlanOptions{Names: []string{"x:1"}}); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("--name on a two-image archive: %v", err)
	}
}

func TestPlanErrors(t *testing.T) {
	t.Run("all children absent", func(t *testing.T) {
		b := archivetest.New()
		idx := b.AddIndex([]oci.Descriptor{archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
		b.Top(idx)
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil || !strings.Contains(err.Error(), "no child") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("top-level entry missing", func(t *testing.T) {
		b := archivetest.New()
		b.Top(archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "amd64"}))
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("partial manifest", func(t *testing.T) {
		b := archivetest.New()
		config := []byte(`{"os":"linux"}`)
		missing := oci.DigestOfBytes([]byte("never added"))
		m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest,
			Config: &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
			Layers: []oci.Descriptor{{MediaType: gzipLayer, Digest: missing, Size: 11}}}
		body, _ := json.Marshal(m)
		d := b.AddBlob(body)
		b.Top(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: d, Size: int64(len(body))})
		b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"x:1"}})
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("legacy config matches nothing", func(t *testing.T) {
		b, _, _, _ := busyboxLike(t, "busybox:1.37")
		b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes([]byte("other")), RepoTags: []string{"x:1"}})
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("present callback error", func(t *testing.T) {
		b, _, _, _ := busyboxLike(t, "busybox:1.37")
		a := openBuilder(t, b)
		boom := func(oci.Digest) (bool, error) { return false, strings.NewReader("").UnreadRune() }
		if _, err := a.Plan(PlanOptions{Present: boom}); err == nil {
			t.Fatal("expected the callback's error")
		}
	})
}
```

- [ ] **Step 4: Run to verify failure**

Run: `go test ./dockerarchive -run TestPlan`
Expected: compile errors, `undefined: PlanOptions`.

- [ ] **Step 5: Implement `dockerarchive/plan.go`**

```go
package dockerarchive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// attestationAnnotation marks a BuildKit attestation manifest in an index.
const (
	attestationTypeAnnotation = "vnd.docker.reference.type"
	attestationType           = "attestation-manifest"
)

// PlanBlob is one config, layer or other blob to store.
type PlanBlob struct {
	Digest    oci.Digest
	Size      int64
	MediaType string
	Present   bool // already in the store; skip it
}

// PlanManifest is one manifest or index to publish. Synthesized marks a
// pruned index whose body this package produced.
type PlanManifest struct {
	Digest      oci.Digest
	MediaType   string
	Body        []byte
	IsIndex     bool
	Synthesized bool
}

// PlanEntry is one index.json entry: the image the archive was saved
// from, with the names to publish it under and its manifests in publish
// order (children first, itself last).
type PlanEntry struct {
	Digest       oci.Digest
	Names        []Name
	IsIndex      bool
	Platforms    int // children with a platform (or without any annotation), indexes only
	Attestations int // children annotated as attestation manifests
	Manifests    []oci.Digest
}

// Plan is what an import stores: unique blobs in first-use order, unique
// manifests in publish order, and the entries with their names.
type Plan struct {
	Blobs     []PlanBlob
	Manifests []PlanManifest
	Entries   []PlanEntry
}

// PlanOptions configure Plan. Names overrides the archive's RepoTags and is
// only allowed for a single-entry archive. Present, when set, is asked for
// every blob; a true answer marks the blob present.
type PlanOptions struct {
	Names   []string
	Present func(oci.Digest) (bool, error)
}

// node is a present manifest or index as resolved from the archive.
type node struct {
	desc        oci.Descriptor // as referenced, digest replaced for a pruned index
	body        []byte
	manifest    *oci.Manifest
	synthesized bool
	children    []*node // present children, indexes only
}

// Plan resolves every index.json entry, prunes absent children, collects
// blobs and manifests and assigns names.
func (a *Archive) Plan(opts PlanOptions) (*Plan, error) {
	var roots []*node
	for i, d := range a.Index.Manifests {
		n, present, err := a.resolve(d)
		if err != nil {
			return nil, fmt.Errorf("dockerarchive: %s entry %d: %w", IndexFile, i, err)
		}
		if !present {
			return nil, fmt.Errorf("dockerarchive: %s entry %d: manifest %s is not in the archive", IndexFile, i, d.Digest)
		}
		roots = append(roots, n)
	}
	p := &Plan{}
	seenBlob := map[oci.Digest]bool{}
	seenManifest := map[oci.Digest]bool{}
	for _, r := range roots {
		e := PlanEntry{Digest: r.desc.Digest, IsIndex: r.manifest.IsIndex()}
		var walk func(n *node)
		walk = func(n *node) {
			for _, c := range n.children {
				walk(c)
			}
			for _, bd := range n.manifest.BlobDescriptors() {
				if !seenBlob[bd.Digest] {
					seenBlob[bd.Digest] = true
					size, _ := a.Size(bd.Digest)
					p.Blobs = append(p.Blobs, PlanBlob{Digest: bd.Digest, Size: size, MediaType: bd.MediaType})
				}
			}
			if !seenManifest[n.desc.Digest] {
				seenManifest[n.desc.Digest] = true
				p.Manifests = append(p.Manifests, PlanManifest{Digest: n.desc.Digest, MediaType: n.desc.MediaType, Body: n.body, IsIndex: n.manifest.IsIndex(), Synthesized: n.synthesized})
			}
			e.Manifests = append(e.Manifests, n.desc.Digest)
		}
		walk(r)
		if e.IsIndex {
			for _, c := range r.children {
				if c.desc.Annotations[attestationTypeAnnotation] == attestationType {
					e.Attestations++
				} else {
					e.Platforms++
				}
			}
		}
		p.Entries = append(p.Entries, e)
	}
	if err := a.assignNames(p, roots, opts.Names); err != nil {
		return nil, err
	}
	if opts.Present != nil {
		for i := range p.Blobs {
			ok, err := opts.Present(p.Blobs[i].Digest)
			if err != nil {
				return nil, fmt.Errorf("dockerarchive: checking whether %s is stored: %w", p.Blobs[i].Digest, err)
			}
			p.Blobs[i].Present = ok
		}
	}
	return p, nil
}

// resolve reads the manifest d points at. present is false when its blob
// is not in the archive. An index with no present child, and an image
// manifest with some blob missing, are errors.
func (a *Archive) resolve(d oci.Descriptor) (*node, bool, error) {
	if !a.Has(d.Digest) {
		return nil, false, nil
	}
	body, err := a.ReadBlob(d.Digest)
	if err != nil {
		return nil, false, err
	}
	m, err := oci.ParseManifest(body)
	if err != nil {
		return nil, false, fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	if d.MediaType == "" {
		d.MediaType = m.EffectiveMediaType("")
	}
	n := &node{desc: d, body: body, manifest: m}
	if !m.IsIndex() {
		var missing []string
		for _, bd := range m.BlobDescriptors() {
			if !a.Has(bd.Digest) {
				missing = append(missing, bd.Digest.String())
			}
		}
		if len(missing) > 0 {
			return nil, false, fmt.Errorf("manifest %s: blobs missing from the archive: %s", d.Digest, strings.Join(missing, ", "))
		}
		return n, true, nil
	}
	var keep []int
	for i, cd := range m.Manifests {
		c, present, err := a.resolve(cd)
		if err != nil {
			return nil, false, err
		}
		if !present {
			continue
		}
		keep = append(keep, i)
		n.children = append(n.children, c)
	}
	if len(keep) == 0 {
		return nil, false, fmt.Errorf("index %s: no child manifest has its blobs in the archive", d.Digest)
	}
	if len(keep) < len(m.Manifests) {
		pruned, err := pruneIndex(body, keep, n.children)
		if err != nil {
			return nil, false, fmt.Errorf("index %s: %w", d.Digest, err)
		}
		n.body = pruned
		n.synthesized = true
		n.desc.Digest = oci.DigestOfBytes(pruned)
		n.desc.Size = int64(len(pruned))
		m, err = oci.ParseManifest(pruned)
		if err != nil {
			return nil, false, fmt.Errorf("index %s: re-parsing the pruned index: %w", d.Digest, err)
		}
		n.manifest = m
	}
	return n, true, nil
}

// pruneIndex rewrites body keeping only the manifests at the given
// positions, each replaced by its resolved child's descriptor so that a
// pruned nested index is referenced by its new digest. Every other field
// is carried over untouched through json.RawMessage.
func pruneIndex(body []byte, keep []int, children []*node) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	var manifests []json.RawMessage
	if err := json.Unmarshal(top["manifests"], &manifests); err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(keep))
	for j, i := range keep {
		if i >= len(manifests) {
			return nil, errors.New("manifests array shorter than parsed")
		}
		raw := manifests[i]
		if children[j].synthesized {
			var desc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &desc); err != nil {
				return nil, err
			}
			desc["digest"], _ = json.Marshal(children[j].desc.Digest)
			desc["size"], _ = json.Marshal(children[j].desc.Size)
			if raw, err := json.Marshal(desc); err == nil {
				kept = append(kept, raw)
				continue
			}
		}
		kept = append(kept, raw)
	}
	var err error
	if top["manifests"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	return json.Marshal(top)
}

// assignNames fills every entry's Names from manifest.json, or from the
// override, and rejects an entry left without a name.
func (a *Archive) assignNames(p *Plan, roots []*node, override []string) error {
	if len(override) > 0 {
		if len(p.Entries) != 1 {
			return fmt.Errorf("dockerarchive: --name applies to a single-image archive, this one holds %d images", len(p.Entries))
		}
		for _, s := range override {
			n, err := ParseName(s)
			if err != nil {
				return fmt.Errorf("dockerarchive: --name %v", err)
			}
			p.Entries[0].Names = appendName(p.Entries[0].Names, n)
		}
		return nil
	}
	// Map every present image manifest's config digest to its root.
	configRoot := map[oci.Digest]int{}
	for i, r := range roots {
		var walk func(n *node)
		walk = func(n *node) {
			if n.manifest.Config != nil {
				configRoot[n.manifest.Config.Digest] = i
			}
			for _, c := range n.children {
				walk(c)
			}
		}
		walk(r)
	}
	for _, le := range a.Legacy {
		hex := strings.TrimPrefix(le.Config, blobPrefix)
		d, err := oci.ParseDigest("sha256:" + hex)
		if err != nil {
			return fmt.Errorf("dockerarchive: %s: Config %q is not a blob path", ManifestFile, le.Config)
		}
		i, ok := configRoot[d]
		if !ok {
			return fmt.Errorf("dockerarchive: %s names config %s, which no manifest in %s uses", ManifestFile, d, IndexFile)
		}
		for _, tag := range le.RepoTags {
			n, ok, err := nameFromRepoTag(tag)
			if err != nil {
				return fmt.Errorf("dockerarchive: %s: %w", ManifestFile, err)
			}
			if ok {
				p.Entries[i].Names = appendName(p.Entries[i].Names, n)
			}
		}
	}
	for i, e := range p.Entries {
		if len(e.Names) == 0 {
			return fmt.Errorf("dockerarchive: image %s (entry %d) has no RepoTags; pass --name repo:tag", e.Digest, i)
		}
	}
	return nil
}

func appendName(names []Name, n Name) []Name {
	for _, have := range names {
		if have == n {
			return names
		}
	}
	return append(names, n)
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./dockerarchive/... -race`
Expected: PASS. If `TestPlanErrors/partial manifest` fails because `ParseManifest` rejects the body, check the JSON the test marshals; `oci.Manifest` marshals fine with `Config` and `Layers` set.

- [ ] **Step 7: Commit**

```bash
git add dockerarchive/plan.go dockerarchive/plan_test.go dockerarchive/names.go dockerarchive/names_test.go
git commit -m "dockerarchive: plan an import: pruned index, blobs, names"
```

---

### Task 6: Progress tracker and ETA (`importer.Tracker`)

**Files:**
- Create: `importer/tracker.go`
- Test: `importer/tracker_test.go`

**Interfaces:**
- Consumes: `blob.Stage`, `blob.Observer`, `blob.Meta`, `image.Meta`, `dockerarchive.PlanBlob/PlanManifest/PlanEntry/Name`.
- Produces:
  ```go
  type Phase int  // PhaseIdle, PhaseChecking, PhaseBlobs, PhaseManifests, PhaseDone
  type BlobState int // BlobPending, BlobPresent, BlobInFlight, BlobDone, BlobFailed
  type ManifestState int // ManifestPending, ManifestInFlight, ManifestDone
  type BlobRow struct{ Digest oci.Digest; Size int64; MediaType string; State BlobState; Stage blob.Stage; Progress int64; Fraction float64; Kind blob.Kind; RawReason blob.RawReason }
  type ManifestRow struct{ Digest oci.Digest; Names []dockerarchive.Name; IsIndex bool; State ManifestState; Rootfs *image.Rootfs }
  type Counts struct{ Pending, InFlight, Done, Present, Raw, Failed int }
  type Snapshot struct{ Phase Phase; Checked float64; Blobs []BlobRow; Counts Counts; Fraction float64; Elapsed time.Duration; ETA time.Duration; ETAKnown bool; Manifests []ManifestRow; Err error }
  type TrackerOptions struct{ Verify bool; Now func() time.Time }
  func NewTracker(opts TrackerOptions) *Tracker
  func (t *Tracker) Queue(p *dockerarchive.Plan)
  func (t *Tracker) Checked(n int64)
  func (t *Tracker) StartBlobs()
  func (t *Tracker) Start(d oci.Digest)
  func (t *Tracker) Done(d oci.Digest, m *blob.Meta)
  func (t *Tracker) Fail(d oci.Digest, err error)
  func (t *Tracker) StartManifests()
  func (t *Tracker) ManifestStart(d oci.Digest)
  func (t *Tracker) ManifestDone(d oci.Digest, m *image.Meta)
  func (t *Tracker) Finish(err error)
  func (t *Tracker) BlobStage(d oci.Digest, s blob.Stage)   // blob.Observer
  func (t *Tracker) BlobProgress(d oci.Digest, n int64)      // blob.Observer
  func (t *Tracker) Snapshot() Snapshot
  ```

- [ ] **Step 1: Write the failing tests**

Create `importer/tracker_test.go`:

```go
package importer

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/oci"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time             { return c.t }
func (c *clock) advance(d time.Duration)    { c.t = c.t.Add(d) }

func digest(s string) oci.Digest { return oci.DigestOfBytes([]byte(s)) }

func plan(sizes map[string]int64, present ...string) *dockerarchive.Plan {
	p := &dockerarchive.Plan{}
	for name, size := range sizes {
		pb := dockerarchive.PlanBlob{Digest: digest(name), Size: size, MediaType: "layer"}
		for _, pr := range present {
			if pr == name {
				pb.Present = true
			}
		}
		p.Blobs = append(p.Blobs, pb)
	}
	p.Manifests = []dockerarchive.PlanManifest{{Digest: digest("m1"), IsIndex: false}, {Digest: digest("idx"), IsIndex: true}}
	p.Entries = []dockerarchive.PlanEntry{{Digest: digest("idx"), Names: []dockerarchive.Name{{Repo: "app", Tag: "v1"}}, IsIndex: true, Manifests: []oci.Digest{digest("m1"), digest("idx")}}}
	return p
}

func approx(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestTrackerFractionsThroughPrismStages(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "b": 3000}))
	if s := tr.Snapshot(); s.Phase != PhaseChecking || s.Counts.Pending != 2 {
		t.Fatalf("after Queue: %+v", s)
	}
	tr.Checked(2000)
	approx(t, "Checked", tr.Snapshot().Checked, 0.5)
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobProgress(digest("a"), 500)
	s := tr.Snapshot()
	row := rowFor(t, s, "a")
	approx(t, "analyze half", row.Fraction, 0.25) // 0.5 share × 0.5
	approx(t, "overall", s.Fraction, 250.0/4000)
	if s.Counts.InFlight != 1 || s.Counts.Pending != 1 {
		t.Fatalf("counts = %+v", s.Counts)
	}
	tr.BlobProgress(digest("a"), 1000)
	tr.BlobStage(digest("a"), blob.StageDecompose)
	approx(t, "decompose start", rowFor(t, tr.Snapshot(), "a").Fraction, 0.5)
	tr.BlobProgress(digest("a"), 1000)
	tr.BlobStage(digest("a"), blob.StageVerify)
	approx(t, "verify start", rowFor(t, tr.Snapshot(), "a").Fraction, 0.75)
	tr.BlobProgress(digest("a"), 500)
	approx(t, "verify half", rowFor(t, tr.Snapshot(), "a").Fraction, 0.875)
	tr.Done(digest("a"), &blob.Meta{Digest: digest("a"), Size: 1000, Kind: blob.KindPrism})
	s = tr.Snapshot()
	if r := rowFor(t, s, "a"); r.State != BlobDone || r.Fraction != 1 || r.Kind != blob.KindPrism {
		t.Fatalf("done row = %+v", r)
	}
	approx(t, "overall after a", s.Fraction, 0.25)
}

func TestTrackerWithoutVerifyDecomposeTakesHalf(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: false, Now: time.Now})
	tr.Queue(plan(map[string]int64{"a": 100}))
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobStage(digest("a"), blob.StageDecompose)
	tr.BlobProgress(digest("a"), 50)
	approx(t, "decompose half, no verify", rowFor(t, tr.Snapshot(), "a").Fraction, 0.75)
}

func TestTrackerRawTakesTheRemainder(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: true, Now: time.Now})
	tr.Queue(plan(map[string]int64{"a": 100, "b": 100, "c": 100}))
	tr.StartBlobs()
	// raw right after analyze: 0.5 + 0.5 × n/size
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobStage(digest("a"), blob.StageRaw)
	tr.BlobProgress(digest("a"), 50)
	approx(t, "raw after analyze", rowFor(t, tr.Snapshot(), "a").Fraction, 0.75)
	// raw after decompose (downgrade): 0.75 + 0.25 × n/size
	tr.Start(digest("b"))
	tr.BlobStage(digest("b"), blob.StageAnalyze)
	tr.BlobStage(digest("b"), blob.StageDecompose)
	tr.BlobProgress(digest("b"), 100)
	tr.BlobStage(digest("b"), blob.StageRaw)
	tr.BlobProgress(digest("b"), 50)
	approx(t, "raw after decompose", rowFor(t, tr.Snapshot(), "b").Fraction, 0.875)
	// progress in a stage never pushes the fraction past the stage's end
	tr.BlobProgress(digest("b"), 100)
	approx(t, "raw complete", rowFor(t, tr.Snapshot(), "b").Fraction, 1)
	tr.Done(digest("b"), &blob.Meta{Digest: digest("b"), Size: 100, Kind: blob.KindRaw, RawReason: blob.ReasonNotTar})
	s := tr.Snapshot()
	if s.Counts.Raw != 1 || rowFor(t, s, "b").RawReason != blob.ReasonNotTar {
		t.Fatalf("raw accounting: %+v", s.Counts)
	}
}

func TestTrackerPresentBlobsAreOutsideTheEstimate(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "big": 9000}, "big"))
	s := tr.Snapshot()
	if s.Counts.Present != 1 || s.Counts.Pending != 1 {
		t.Fatalf("counts = %+v", s.Counts)
	}
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobProgress(digest("a"), 1000)
	approx(t, "overall ignores present", tr.Snapshot().Fraction, 0.5)
	// A dedup hit inside Put reports no stage: Done without a stage counts as present.
	tr2 := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr2.Queue(plan(map[string]int64{"a": 1000}))
	tr2.StartBlobs()
	tr2.Start(digest("a"))
	tr2.Done(digest("a"), &blob.Meta{Digest: digest("a"), Size: 1000})
	if s := tr2.Snapshot(); s.Counts.Present != 1 || s.Counts.Done != 0 || rowFor(t, s, "a").State != BlobPresent {
		t.Fatalf("dedup hit: %+v", s.Counts)
	}
}

func TestTrackerETA(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "b": 1000}))
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	c.advance(time.Second)
	tr.BlobProgress(digest("a"), 1000) // fraction 0.5 of a → 500 of 2000 bytes in 1 s
	if s := tr.Snapshot(); s.ETAKnown {
		t.Fatalf("ETA known before the warm-up: %+v", s)
	}
	c.advance(time.Second) // 2 s elapsed, still 500 done → rate 250 B/s → 1500 left → 6 s
	s := tr.Snapshot()
	if !s.ETAKnown || s.ETA != 6*time.Second {
		t.Fatalf("ETA = %v (known %v), want 6s", s.ETA, s.ETAKnown)
	}
	if s.Elapsed != 2*time.Second {
		t.Fatalf("Elapsed = %v", s.Elapsed)
	}
}

func TestTrackerManifestsAndFinish(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: true, Now: time.Now})
	p := plan(map[string]int64{"a": 10})
	tr.Queue(p)
	tr.StartBlobs()
	tr.StartManifests()
	if s := tr.Snapshot(); s.Phase != PhaseManifests || len(s.Manifests) != 2 || s.Manifests[1].Names[0].Repo != "app" {
		t.Fatalf("manifest rows: %+v", s.Manifests)
	}
	tr.ManifestStart(digest("m1"))
	if s := tr.Snapshot(); s.Manifests[0].State != ManifestInFlight {
		t.Fatalf("m1 not in flight: %+v", s.Manifests[0])
	}
	tr.ManifestDone(digest("m1"), &image.Meta{Rootfs: &image.Rootfs{Status: image.RootfsOK, Entries: 42}})
	if s := tr.Snapshot(); s.Manifests[0].State != ManifestDone || s.Manifests[0].Rootfs.Entries != 42 {
		t.Fatalf("m1 not done: %+v", s.Manifests[0])
	}
	boom := errors.New("boom")
	tr.Finish(boom)
	if s := tr.Snapshot(); s.Phase != PhaseDone || s.Err != boom {
		t.Fatalf("after Finish: %+v", s)
	}
}

func rowFor(t *testing.T, s Snapshot, name string) BlobRow {
	t.Helper()
	for _, r := range s.Blobs {
		if r.Digest == digest(name) {
			return r
		}
	}
	t.Fatalf("no row for %s", name)
	return BlobRow{}
}
```

Add `"github.com/amber-store/oci-amber/image"` to the imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./importer`
Expected: `no Go files` / compile errors.

- [ ] **Step 3: Implement `importer/tracker.go`**

```go
// Package importer stores a planned docker save archive through the blob
// and image stores and tracks progress for a display.
package importer

import (
	"sync"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// Phase is where an import is.
type Phase int

const (
	PhaseIdle      Phase = iota
	PhaseChecking        // verifying blob digests against the archive
	PhaseBlobs           // storing blobs
	PhaseManifests       // publishing manifests and tags
	PhaseDone
)

// BlobState is one blob's state.
type BlobState int

const (
	BlobPending  BlobState = iota
	BlobPresent            // already stored: skipped at planning, or a dedup hit
	BlobInFlight
	BlobDone
	BlobFailed
)

// ManifestState is one manifest's state.
type ManifestState int

const (
	ManifestPending ManifestState = iota
	ManifestInFlight
	ManifestDone
)

// BlobRow is a blob's progress. Fraction is 0..1 of the blob's own work.
type BlobRow struct {
	Digest    oci.Digest
	Size      int64
	MediaType string
	State     BlobState
	Stage     blob.Stage
	Progress  int64 // bytes reported in the current stage
	Fraction  float64
	Kind      blob.Kind      // set when done
	RawReason blob.RawReason // set when done raw
}

// ManifestRow is a manifest's progress.
type ManifestRow struct {
	Digest  oci.Digest
	Names   []dockerarchive.Name // set for entries
	IsIndex bool
	State   ManifestState
	Rootfs  *image.Rootfs // set when done, manifests only
}

// Counts summarize the blob rows.
type Counts struct {
	Pending, InFlight, Done, Present, Raw, Failed int
}

// Snapshot is a consistent copy of the tracker's state for rendering.
type Snapshot struct {
	Phase     Phase
	Checked   float64 // checking phase progress, 0..1
	Blobs     []BlobRow
	Counts    Counts
	Fraction  float64 // blob phase progress, 0..1, present blobs excluded
	Elapsed   time.Duration
	ETA       time.Duration
	ETAKnown  bool
	Manifests []ManifestRow
	Err       error
}

// TrackerOptions configure a Tracker. Verify says whether the blob store
// runs the round-trip check, which decides the stage shares. Now defaults
// to time.Now.
type TrackerOptions struct {
	Verify bool
	Now    func() time.Time
}

// etaWarmup is how long the blob phase must have run before an ETA is
// reported; a rate measured over less is noise.
const etaWarmup = 2 * time.Second

type blobRow struct {
	BlobRow
	stageBase, stageShare float64
	sawStage              bool
}

// Tracker is the shared progress state: the importer records state
// changes, the blob store (through blob.Observer) records stages and byte
// counts, renderers take snapshots. All methods are safe for concurrent
// use.
type Tracker struct {
	opts TrackerOptions
	now  func() time.Time

	mu        sync.Mutex
	phase     Phase
	started   time.Time // Queue
	blobStart time.Time // StartBlobs
	toCheck   int64
	checked   int64
	blobs     []*blobRow
	byDigest  map[oci.Digest]*blobRow
	manifests []ManifestRow
	err       error
}

// NewTracker returns an idle tracker.
func NewTracker(opts TrackerOptions) *Tracker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Tracker{opts: opts, now: opts.Now, byDigest: map[oci.Digest]*blobRow{}}
}

// Queue loads the plan: one row per blob and per manifest, present blobs
// already marked, and enters the checking phase.
func (t *Tracker) Queue(p *dockerarchive.Plan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseChecking
	t.started = t.now()
	for _, pb := range p.Blobs {
		r := &blobRow{BlobRow: BlobRow{Digest: pb.Digest, Size: pb.Size, MediaType: pb.MediaType}}
		if pb.Present {
			r.State = BlobPresent
			r.Fraction = 1
		} else {
			t.toCheck += pb.Size
		}
		t.blobs = append(t.blobs, r)
		t.byDigest[pb.Digest] = r
	}
	names := map[oci.Digest][]dockerarchive.Name{}
	for _, e := range p.Entries {
		names[e.Digest] = e.Names
	}
	for _, pm := range p.Manifests {
		t.manifests = append(t.manifests, ManifestRow{Digest: pm.Digest, Names: names[pm.Digest], IsIndex: pm.IsIndex})
	}
}

// Checked records the bytes verified so far in the checking phase.
func (t *Tracker) Checked(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checked = n
}

// StartBlobs enters the blob phase; the ETA clock starts here.
func (t *Tracker) StartBlobs() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseBlobs
	t.blobStart = t.now()
}

// Start marks d in flight.
func (t *Tracker) Start(d oci.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.byDigest[d]; r != nil {
		r.State = BlobInFlight
	}
}

// Done records Put's result for d. A blob for which no stage was ever
// reported was a dedup hit inside Put and counts as present.
func (t *Tracker) Done(d oci.Digest, m *blob.Meta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil {
		return
	}
	r.State = BlobDone
	if !r.sawStage {
		r.State = BlobPresent
	}
	r.Fraction = 1
	if m != nil {
		r.Kind = m.Kind
		r.RawReason = m.RawReason
	}
}

// Fail records a failed Put for d.
func (t *Tracker) Fail(d oci.Digest, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.byDigest[d]; r != nil {
		r.State = BlobFailed
	}
}

// StartManifests enters the manifest phase.
func (t *Tracker) StartManifests() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseManifests
}

// ManifestStart marks d in flight.
func (t *Tracker) ManifestStart(d oci.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.manifests {
		if t.manifests[i].Digest == d {
			t.manifests[i].State = ManifestInFlight
		}
	}
}

// ManifestDone records a published manifest and its rootfs outcome.
func (t *Tracker) ManifestDone(d oci.Digest, m *image.Meta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.manifests {
		if t.manifests[i].Digest == d {
			t.manifests[i].State = ManifestDone
			if m != nil {
				t.manifests[i].Rootfs = m.Rootfs
			}
		}
	}
}

// Finish ends the run with err (nil on success).
func (t *Tracker) Finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseDone
	t.err = err
}

// BlobStage implements blob.Observer.
func (t *Tracker) BlobStage(d oci.Digest, s blob.Stage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil {
		return
	}
	prevEnd := r.stageBase + r.stageShare
	if !r.sawStage {
		prevEnd = 0
	}
	r.sawStage = true
	r.Stage = s
	r.Progress = 0
	switch s {
	case blob.StageAnalyze:
		r.stageBase, r.stageShare = 0, 0.5
	case blob.StageDecompose:
		r.stageBase = 0.5
		if t.opts.Verify {
			r.stageShare = 0.25
		} else {
			r.stageShare = 0.5
		}
	case blob.StageVerify:
		r.stageBase, r.stageShare = 0.75, 0.25
	default: // raw: from wherever the previous stage ended to the end
		r.stageBase, r.stageShare = prevEnd, 1-prevEnd
	}
	r.Fraction = r.stageBase
}

// BlobProgress implements blob.Observer.
func (t *Tracker) BlobProgress(d oci.Digest, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil || !r.sawStage {
		return
	}
	r.Progress = n
	part := 1.0
	if r.Size > 0 {
		part = min(1, float64(n)/float64(r.Size))
	}
	r.Fraction = r.stageBase + r.stageShare*part
}

// Snapshot copies the state. Fraction and the ETA are derived here.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := Snapshot{Phase: t.phase, Err: t.err}
	if !t.started.IsZero() {
		s.Elapsed = now.Sub(t.started)
	}
	if t.toCheck > 0 {
		s.Checked = min(1, float64(t.checked)/float64(t.toCheck))
	} else if t.phase > PhaseChecking {
		s.Checked = 1
	}
	var total, progress float64
	s.Blobs = make([]BlobRow, len(t.blobs))
	for i, r := range t.blobs {
		s.Blobs[i] = r.BlobRow
		switch r.State {
		case BlobPending:
			s.Counts.Pending++
		case BlobInFlight:
			s.Counts.InFlight++
		case BlobDone:
			s.Counts.Done++
			if r.Kind == blob.KindRaw {
				s.Counts.Raw++
			}
		case BlobPresent:
			s.Counts.Present++
			continue // outside the estimate
		case BlobFailed:
			s.Counts.Failed++
		}
		total += float64(r.Size)
		progress += float64(r.Size) * r.Fraction
	}
	if total > 0 {
		s.Fraction = progress / total
	} else if t.phase > PhaseBlobs {
		s.Fraction = 1
	}
	if t.phase == PhaseBlobs && !t.blobStart.IsZero() {
		elapsed := now.Sub(t.blobStart)
		if elapsed >= etaWarmup && progress > 0 {
			rate := progress / elapsed.Seconds()
			s.ETA = time.Duration((total - progress) / rate * float64(time.Second))
			s.ETAKnown = true
		}
	}
	s.Manifests = append([]ManifestRow(nil), t.manifests...)
	return s
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./importer -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importer/tracker.go importer/tracker_test.go
git commit -m "importer: progress tracker with stage fractions and an ETA"
```

---

### Task 7: Running the plan (`importer.Importer`, `importer.Report`)

**Files:**
- Create: `importer/importer.go`, `importer/report.go`
- Test: `importer/importer_test.go`

**Interfaces:**
- Consumes: `blob.Store.Put/Open/Exists`, `image.Store.Put`, `dockerarchive.Archive.Section/Verify`, `upload.NewSectionSpool`, `Tracker`.
- Produces:
  ```go
  type Options struct{ Workers int }
  type Importer struct{ ... }
  func New(blobs *blob.Store, images *image.Store, arch *dockerarchive.Archive, plan *dockerarchive.Plan, tr *Tracker, opts Options) *Importer
  func (im *Importer) Run(ctx context.Context) (*Report, error)

  type EntryReport struct{ Names []dockerarchive.Name; Digest oci.Digest; IsIndex bool; Platforms, Attestations int; Rootfs []*image.Rootfs; Stats image.Stats }
  type BlobCounts struct{ Processed, Prism, Raw, Present int; RawReasons map[blob.RawReason]int }
  type Report struct{ Duration time.Duration; Entries []EntryReport; Blobs BlobCounts; Compressed, Uncompressed, Added, Logical, Deduped int64 }
  func (r *Report) DedupRatio() (float64, bool)     // compressed ÷ added; false when nothing was added
  func (r *Report) NotWrittenPercent() float64      // 100 × (1 − added/compressed)
  func (r *Report) ChunksReusedPercent() float64    // 100 × deduped/logical
  ```

- [ ] **Step 1: Write the failing tests**

Create `importer/importer_test.go`:

```go
package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

const gzipLayer = "application/vnd.oci.image.layer.v1.tar+gzip"

// env is a store with a blob store, an image store and a tracker wired as
// the blob observer, the way the command wires them.
type env struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
	tr     *Tracker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := NewTracker(TrackerOptions{Verify: true})
	blobs, err := blob.New(st, blob.Options{WorkDir: t.TempDir(), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute, MaxConcurrentFinalize: 2, VerifyRoundTrip: true, RecentTTL: 24 * time.Hour, Observer: tr}, log)
	if err != nil {
		t.Fatal(err)
	}
	return &env{st: st, blobs: blobs, images: image.New(st, blobs, log), tr: tr}
}

// tarGz builds a gzip tar with one file of compressible text.
func tarGz(t *testing.T, name string, size int, seed int64) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	words := []string{"alpha ", "beta ", "gamma ", "delta ", "epsilon\n"}
	var text bytes.Buffer
	for text.Len() < size {
		text.WriteString(words[r.Intn(len(words))])
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(text.Len()), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(text.Bytes())
	tw.Close()
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(tb.Bytes())
	gw.Close()
	return gz.Bytes()
}

// fixture is the busybox-like archive: an index over a present amd64
// image (two layers: a prism and a not-tar raw one), its attestation, and
// two absent platforms; two names in two repositories.
func fixture(t *testing.T) (*dockerarchive.Archive, *dockerarchive.Plan, *env) {
	t.Helper()
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layerA := tarGz(t, "usr/share/words", 200<<10, 1)
	layerB := []byte(strings.Repeat("not a tar, stored raw ", 100))
	img := b.AddImage(config, []archivetest.Layer{{MediaType: gzipLayer, Data: layerA}, {MediaType: gzipLayer, Data: layerB}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	att := b.AddImage(archivetest.Attestation(img))
	idx := b.AddIndex([]oci.Descriptor{img, att, archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
	b.Top(idx)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"registry.example.ch/team/app:v1", "app:latest"}, Layers: []oci.Digest{oci.DigestOfBytes(layerA), oci.DigestOfBytes(layerB)}})
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { arch.Close() })
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	return arch, plan, e
}

func TestRunStoresEverything(t *testing.T) {
	arch, plan, e := fixture(t)
	rep, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entry := plan.Entries[0]
	for _, n := range entry.Names {
		im, err := e.images.Open(n.Repo, n.Tag)
		if err != nil {
			t.Fatalf("open %s: %v", n, err)
		}
		if im.Meta.Kind != image.KindIndex || im.Meta.Digest != entry.Digest {
			t.Fatalf("%s: %+v", n, im.Meta)
		}
		for _, d := range entry.Manifests[:len(entry.Manifests)-1] {
			if _, err := e.images.Open(n.Repo, d.String()); err != nil {
				t.Fatalf("child %s in %s: %v", d, n.Repo, err)
			}
		}
	}
	// The platform image got its rootfs; the layers are one prism and one raw.
	child, _ := e.images.Open("app", entry.Manifests[0].String())
	if child.Meta.Rootfs == nil || child.Meta.Rootfs.Status != image.RootfsUnavailable {
		// layer B is raw, so the rootfs is unavailable; that is the expected, honest outcome
		t.Fatalf("rootfs = %+v", child.Meta.Rootfs)
	}
	kinds := map[blob.Kind]int{}
	for _, pb := range plan.Blobs {
		bl, err := e.blobs.Open(pb.Digest)
		if err != nil {
			t.Fatalf("blob %s: %v", pb.Digest, err)
		}
		kinds[bl.Meta.Kind]++
	}
	if kinds[blob.KindPrism] != 1 || kinds[blob.KindRaw] != 3 { // configs and the in-toto payload are raw too
		t.Fatalf("kinds = %v", kinds)
	}
	// Report.
	if rep.Blobs.Processed != 5 || rep.Blobs.Prism != 1 || rep.Blobs.Raw != 4 || rep.Blobs.Present != 0 {
		t.Fatalf("blob counts = %+v", rep.Blobs)
	}
	if rep.Blobs.RawReasons[blob.ReasonNotTar] != 4 {
		t.Fatalf("raw reasons = %v", rep.Blobs.RawReasons)
	}
	var wantCompressed int64
	for _, pb := range plan.Blobs {
		wantCompressed += pb.Size
	}
	for _, pm := range plan.Manifests {
		wantCompressed += int64(len(pm.Body))
	}
	if rep.Compressed != wantCompressed {
		t.Fatalf("Compressed = %d, want %d", rep.Compressed, wantCompressed)
	}
	if rep.Uncompressed <= rep.Compressed {
		t.Fatalf("Uncompressed = %d should exceed Compressed = %d (layer A inflates)", rep.Uncompressed, rep.Compressed)
	}
	if rep.Added <= 0 || rep.Added != rep.Entries[0].Stats.DiskBytes {
		t.Fatalf("Added = %d, entry disk bytes = %d", rep.Added, rep.Entries[0].Stats.DiskBytes)
	}
	if ratio, ok := rep.DedupRatio(); !ok || ratio <= 0 {
		t.Fatalf("ratio = %v,%v", ratio, ok)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Platforms != 1 || rep.Entries[0].Attestations != 1 || len(rep.Entries[0].Rootfs) != 1 {
		t.Fatalf("entry report = %+v", rep.Entries[0])
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Counts.Done != 5 || s.Err != nil {
		t.Fatalf("tracker: %+v", s)
	}
}

func TestRunAgainIsAllPresentAndAddsNothing(t *testing.T) {
	arch, plan, e := fixture(t)
	if _, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	plan2, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker(TrackerOptions{Verify: true})
	rep, err := New(e.blobs, e.images, arch, plan2, tr2, Options{Workers: 2}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blobs.Present != 5 || rep.Blobs.Processed != 0 {
		t.Fatalf("second run counts = %+v", rep.Blobs)
	}
	// Re-publishing a manifest rewrites its meta.json (a fresh createdAt)
	// and the root nodes above it, the same few hundred bytes a re-push
	// costs the registry; no blob content is written.
	if rep.Added <= 0 || rep.Added >= 4096 {
		t.Fatalf("second run added %d bytes, want only manifest metadata", rep.Added)
	}
	if rep.Uncompressed == 0 {
		t.Fatal("uncompressed size must come from the stored metas for present blobs")
	}
}

func TestRunCancelledPublishesNoTag(t *testing.T) {
	arch, plan, e := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.images.Open("app", "latest"); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("tag published after a cancelled run: %v", err)
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Err == nil {
		t.Fatalf("tracker after cancel: %+v", s)
	}
}

func TestRunCorruptBlobFailsBeforeWriting(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	good := tarGz(t, "a", 4<<10, 2)
	claimed := oci.DigestOfBytes([]byte("what the path claims"))
	b.AddBlobAs(claimed, good)
	m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
		Layers: []oci.Descriptor{{MediaType: gzipLayer, Digest: claimed, Size: int64(len(good))}}}
	body, _ := json.Marshal(m)
	b.Top(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: b.AddBlob(body), Size: int64(len(body))})
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"x:1"}})
	path, _ := b.WriteFile(t.TempDir(), "bad.tar")
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 1}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	for _, pb := range plan.Blobs {
		if ok, _ := e.blobs.Exists(pb.Digest); ok {
			t.Fatalf("blob %s was stored despite the corrupt archive", pb.Digest)
		}
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Err == nil {
		t.Fatalf("tracker: %+v", s)
	}
}
```

Add `"encoding/json"` to the imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./importer -run TestRun`
Expected: compile errors, `undefined: New`.

- [ ] **Step 3: Implement `importer/report.go`**

```go
package importer

import (
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// EntryReport describes one published image.
type EntryReport struct {
	Names        []dockerarchive.Name
	Digest       oci.Digest
	IsIndex      bool
	Platforms    int
	Attestations int
	// Rootfs holds the rootfs outcome of the entry's platform manifests
	// (the entry itself when it is a manifest), nil entries excluded.
	Rootfs []*image.Rootfs
	// Stats are the image stats of the entry's first Put: they fold in the
	// blobs stored by this run, the manifest objects and the rootfs trees.
	Stats image.Stats
}

// BlobCounts summarize what happened to the blobs.
type BlobCounts struct {
	Processed  int // not present at planning time
	Prism      int
	Raw        int
	Present    int // present at planning time plus dedup hits during the run
	RawReasons map[blob.RawReason]int
}

// Report is the outcome of a run.
type Report struct {
	Duration time.Duration
	Entries  []EntryReport
	Blobs    BlobCounts
	// Compressed is every unique blob's size plus every manifest body, as
	// they are in the archive. Uncompressed replaces prisms' sizes by their
	// decompressed size.
	Compressed   int64
	Uncompressed int64
	// Added is the sum of the entries' DiskBytes: what the run appended to
	// the pack segments. Logical and Deduped are the corresponding sums.
	Added   int64
	Logical int64
	Deduped int64
}

// DedupRatio is Compressed over Added; ok is false when nothing was added.
func (r *Report) DedupRatio() (float64, bool) {
	if r.Added == 0 {
		return 0, false
	}
	return float64(r.Compressed) / float64(r.Added), true
}

// NotWrittenPercent is the share of the compressed bytes that did not
// reach the pack segments.
func (r *Report) NotWrittenPercent() float64 {
	if r.Compressed == 0 {
		return 0
	}
	return 100 * (1 - float64(r.Added)/float64(r.Compressed))
}

// ChunksReusedPercent is Deduped over Logical, the registry's
// deduped_percent.
func (r *Report) ChunksReusedPercent() float64 {
	if r.Logical == 0 {
		return 0
	}
	return 100 * float64(r.Deduped) / float64(r.Logical)
}
```

- [ ] **Step 4: Implement `importer/importer.go`**

```go
package importer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// Options configure an Importer. Workers is how many blobs are finalized
// at once; the command passes --max-concurrent-finalize.
type Options struct {
	Workers int
}

// Importer stores one plan.
type Importer struct {
	blobs  *blob.Store
	images *image.Store
	arch   *dockerarchive.Archive
	plan   *dockerarchive.Plan
	tr     *Tracker
	opts   Options
}

// New returns an importer over the given stores and plan. tr must be the
// blob store's Observer for stage progress to show.
func New(blobs *blob.Store, images *image.Store, arch *dockerarchive.Archive, plan *dockerarchive.Plan, tr *Tracker, opts Options) *Importer {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	return &Importer{blobs: blobs, images: images, arch: arch, plan: plan, tr: tr, opts: opts}
}

// Run checks, stores and publishes the plan and returns the report. On
// error nothing else is published; blobs stored before the failure stay,
// so a re-run resumes through dedup hits.
func (im *Importer) Run(ctx context.Context) (*Report, error) {
	start := time.Now()
	im.tr.Queue(im.plan)
	rep, err := im.run(ctx)
	im.tr.Finish(err)
	if err != nil {
		return nil, err
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

func (im *Importer) run(ctx context.Context) (*Report, error) {
	if err := im.check(ctx); err != nil {
		return nil, err
	}
	metas, err := im.storeBlobs(ctx)
	if err != nil {
		return nil, err
	}
	return im.publish(ctx, metas)
}

// check verifies every non-present blob against its digest.
func (im *Importer) check(ctx context.Context) error {
	var done int64
	for _, pb := range im.plan.Blobs {
		if pb.Present {
			continue
		}
		err := im.arch.Verify(ctx, pb.Digest, func(n int64) { im.tr.Checked(done + n) })
		if err != nil {
			return err
		}
		done += pb.Size
		im.tr.Checked(done)
	}
	return nil
}

// storeBlobs runs Put over the non-present blobs with a worker pool and
// returns every blob's meta, present ones read from the store.
func (im *Importer) storeBlobs(ctx context.Context) (map[oci.Digest]*blob.Meta, error) {
	im.tr.StartBlobs()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		metas    = map[oci.Digest]*blob.Meta{}
		firstErr error
	)
	jobs := make(chan dockerarchive.PlanBlob)
	var wg sync.WaitGroup
	for i := 0; i < im.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pb := range jobs {
				meta, err := im.putBlob(ctx, pb)
				mu.Lock()
				if err != nil {
					im.tr.Fail(pb.Digest, err)
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					im.tr.Done(pb.Digest, meta)
					metas[pb.Digest] = meta
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, pb := range im.plan.Blobs {
		if pb.Present {
			continue
		}
		select {
		case jobs <- pb:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, pb := range im.plan.Blobs {
		if !pb.Present {
			continue
		}
		bl, err := im.blobs.Open(pb.Digest)
		if err != nil {
			return nil, fmt.Errorf("importer: reading stored blob %s: %w", pb.Digest, err)
		}
		m := bl.Meta
		metas[pb.Digest] = &m
	}
	return metas, nil
}

func (im *Importer) putBlob(ctx context.Context, pb dockerarchive.PlanBlob) (*blob.Meta, error) {
	im.tr.Start(pb.Digest)
	sec, err := im.arch.Section(pb.Digest)
	if err != nil {
		return nil, err
	}
	meta, err := im.blobs.Put(ctx, upload.NewSectionSpool(sec, 0, pb.Size, pb.Digest))
	if err != nil {
		return nil, fmt.Errorf("importer: storing %s: %w", pb.Digest, err)
	}
	return meta, nil
}

// publish puts every manifest in every repository an entry is named in,
// children first, then the entry under each of its tags, and builds the
// report.
func (im *Importer) publish(ctx context.Context, metas map[oci.Digest]*blob.Meta) (*Report, error) {
	im.tr.StartManifests()
	byDigest := map[oci.Digest]dockerarchive.PlanManifest{}
	for _, pm := range im.plan.Manifests {
		byDigest[pm.Digest] = pm
	}
	rep := &Report{Blobs: BlobCounts{RawReasons: map[blob.RawReason]int{}}}
	for _, e := range im.plan.Entries {
		er := EntryReport{Names: e.Names, Digest: e.Digest, IsIndex: e.IsIndex, Platforms: e.Platforms, Attestations: e.Attestations}
		var repos []string
		for _, n := range e.Names {
			if !contains(repos, n.Repo) {
				repos = append(repos, n.Repo)
			}
		}
		first := true
		for _, repo := range repos {
			for _, d := range e.Manifests {
				pm := byDigest[d]
				refs := []string{d.String()}
				if d == e.Digest {
					refs = nil
					for _, n := range e.Names {
						if n.Repo == repo {
							refs = append(refs, n.Tag)
						}
					}
				}
				for _, ref := range refs {
					im.tr.ManifestStart(d)
					meta, err := im.images.Put(ctx, repo, ref, pm.MediaType, pm.Body)
					if err != nil {
						return nil, fmt.Errorf("importer: publishing %s/%s: %w", repo, ref, err)
					}
					im.tr.ManifestDone(d, meta)
					if d == e.Digest && first {
						er.Stats = meta.Stats
						first = false
					}
					if first && !pm.IsIndex && meta.Rootfs != nil && meta.Rootfs.Status != image.RootfsNotApplicable {
						er.Rootfs = append(er.Rootfs, meta.Rootfs)
					}
					if d == e.Digest && !pm.IsIndex && meta.Rootfs != nil && meta.Rootfs.Status != image.RootfsNotApplicable && len(er.Rootfs) == 0 {
						er.Rootfs = append(er.Rootfs, meta.Rootfs)
					}
				}
			}
		}
		rep.Entries = append(rep.Entries, er)
		rep.Added += er.Stats.DiskBytes
		rep.Logical += er.Stats.LogicalBytes
		rep.Deduped += er.Stats.DedupedBytes
	}
	im.account(rep, metas)
	return rep, nil
}

// account fills the blob counts and byte totals from the plan and metas.
func (im *Importer) account(rep *Report, metas map[oci.Digest]*blob.Meta) {
	snap := im.tr.Snapshot()
	state := map[oci.Digest]BlobState{}
	for _, r := range snap.Blobs {
		state[r.Digest] = r.State
	}
	for _, pb := range im.plan.Blobs {
		rep.Compressed += pb.Size
		m := metas[pb.Digest]
		if m != nil && m.Kind == blob.KindPrism {
			rep.Uncompressed += m.UncompressedSize
		} else {
			rep.Uncompressed += pb.Size
		}
		if pb.Present || state[pb.Digest] == BlobPresent {
			rep.Blobs.Present++
			continue
		}
		rep.Blobs.Processed++
		if m == nil {
			continue
		}
		switch m.Kind {
		case blob.KindPrism:
			rep.Blobs.Prism++
		case blob.KindRaw:
			rep.Blobs.Raw++
			rep.Blobs.RawReasons[m.RawReason]++
		}
	}
	for _, pm := range im.plan.Manifests {
		rep.Compressed += int64(len(pm.Body))
		rep.Uncompressed += int64(len(pm.Body))
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var _ = errors.New // keep errors imported for wrapping helpers added later
```

Remove the trailing `var _ = errors.New` line and the `errors` import if nothing uses them once the file compiles.

The rootfs bookkeeping in `publish` is deliberately simple: the first repository's Put of each platform manifest contributes its `Rootfs` (attestation manifests have status `not-applicable` and are skipped; an entry that is itself a manifest contributes its own). Simplify the two `if` blocks into one helper `noteRootfs(er *EntryReport, pm, meta, firstRepo bool)` if the reviewer prefers, keeping the tests green.

- [ ] **Step 5: Run the tests**

Run: `go test ./importer -race`
Expected: PASS. `TestRunStoresEverything` asserts the rootfs is `unavailable` because layer B is raw; if `image` reports `ok` instead, check the fixture's layer B really is not a tar.

- [ ] **Step 6: Commit**

```bash
git add importer/importer.go importer/report.go importer/importer_test.go
git commit -m "importer: check, store and publish a plan; build the report"
```

---

### Task 8: Formatting and the report (`tui/format.go`, `tui/report.go`)

**Files:**
- Create: `tui/format.go`, `tui/report.go`
- Test: `tui/format_test.go`, `tui/report_test.go`

**Interfaces:**
- Produces:
  ```go
  func FormatBytes(n int64) string        // "1.9 MiB", "512 B", "0 B"
  func FormatCount(n int64) string        // "1,900,727"
  func FormatClock(d time.Duration) string // "0:42", "12:05", "1:02:03"
  func FormatShort(d time.Duration) string // "42s", "2m10s", "1h03m"
  func FormatETA(d time.Duration) string   // "~42s", "~2m10s", ">1h"
  func ShortDigest(d oci.Digest) string    // first 8 hex chars
  func RenderReport(r *importer.Report, archive string) string
  ```

- [ ] **Step 1: Write the failing tests**

Create `tui/format_test.go`:

```go
package tui

import (
	"testing"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1023: "1023 B", 1024: "1.0 KiB", 1900727: "1.8 MiB", 4388352: "4.2 MiB", 5 << 30: "5.0 GiB"}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1900727: "1,900,727", -1234567: "-1,234,567"}
	for in, want := range cases {
		if got := FormatCount(in); got != want {
			t.Errorf("FormatCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDurations(t *testing.T) {
	if got := FormatClock(42 * time.Second); got != "0:42" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatClock(12*time.Minute + 5*time.Second); got != "12:05" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatClock(time.Hour + 2*time.Minute + 3*time.Second); got != "1:02:03" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatShort(42*time.Second + 300*time.Millisecond); got != "42s" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatShort(2*time.Minute + 10*time.Second); got != "2m10s" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatShort(time.Hour + 3*time.Minute); got != "1h03m" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatETA(2*time.Minute + 10*time.Second); got != "~2m10s" {
		t.Errorf("FormatETA = %q", got)
	}
	if got := FormatETA(90 * time.Minute); got != ">1h" {
		t.Errorf("FormatETA = %q", got)
	}
	if got := FormatETA(0); got != "~0s" {
		t.Errorf("FormatETA(0) = %q", got)
	}
}

func TestShortDigest(t *testing.T) {
	d := oci.DigestOfBytes([]byte("x"))
	if got := ShortDigest(d); len(got) != 8 || got != d.Hex()[:8] {
		t.Errorf("ShortDigest = %q", got)
	}
}
```

Create `tui/report_test.go`:

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/oci"
)

func sampleReport() *importer.Report {
	return &importer.Report{
		Duration: 42 * time.Second,
		Entries: []importer.EntryReport{{
			Names:        []dockerarchive.Name{{Repo: "busybox", Tag: "1.37"}},
			Digest:       oci.DigestOfBytes([]byte("index")),
			IsIndex:      true,
			Platforms:    1,
			Attestations: 1,
			Rootfs:       []*image.Rootfs{{Status: image.RootfsOK, Entries: 1204}},
			Stats:        image.Stats{DiskBytes: 1153024, LogicalBytes: 3000000, DedupedBytes: 1146000},
		}},
		Blobs:        importer.BlobCounts{Processed: 6, Prism: 5, Raw: 1, Present: 2, RawReasons: map[blob.RawReason]int{blob.ReasonNotTar: 1}},
		Compressed:   1900727,
		Uncompressed: 4388352,
		Added:        1153024,
		Logical:      3000000,
		Deduped:      1146000,
	}
}

func TestRenderReport(t *testing.T) {
	out := RenderReport(sampleReport(), "busybox.tar")
	for _, want := range []string{
		"Imported busybox.tar in 42s",
		"busybox:1.37",
		"index, 1 platform + 1 attestation",
		"rootfs ok, 1,204 entries",
		"8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present",
		"Compressed      1.8 MiB     1,900,727 bytes",
		"Uncompressed    4.2 MiB     4,388,352 bytes",
		"Added to CAS    1.1 MiB     1,153,024 bytes",
		"Dedup ratio     1.65x",
		"39.3% not written",
		"Chunks reused   38.2%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderReportNothingAdded(t *testing.T) {
	r := sampleReport()
	r.Added = 0
	r.Blobs = importer.BlobCounts{Present: 8, RawReasons: map[blob.RawReason]int{}}
	out := RenderReport(r, "busybox.tar")
	if !strings.Contains(out, "everything already present") || !strings.Contains(out, "8 already present") {
		t.Errorf("report:\n%s", out)
	}
	r.Added = 286 // a re-import rewrites manifest metadata
	out = RenderReport(r, "busybox.tar")
	if !strings.Contains(out, "everything already present, 286 bytes of manifest metadata rewritten") {
		t.Errorf("report:\n%s", out)
	}
}

func TestRenderReportManifestEntryAndSeveralRootfs(t *testing.T) {
	r := sampleReport()
	r.Entries[0].IsIndex = false
	r.Entries[0].Names = append(r.Entries[0].Names, dockerarchive.Name{Repo: "busybox", Tag: "latest"})
	out := RenderReport(r, "b.tar")
	if !strings.Contains(out, "busybox:1.37, busybox:latest") || !strings.Contains(out, "   manifest   ") {
		t.Errorf("report:\n%s", out)
	}
	r.Entries[0].IsIndex = true
	r.Entries[0].Platforms = 2
	r.Entries[0].Rootfs = []*image.Rootfs{{Status: image.RootfsOK, Entries: 5}, {Status: image.RootfsUnavailable}}
	out = RenderReport(r, "b.tar")
	if !strings.Contains(out, "2 platforms") || !strings.Contains(out, "rootfs 1/2 ok") {
		t.Errorf("report:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./tui`
Expected: compile errors.

- [ ] **Step 3: Implement `tui/format.go`**

```go
// Package tui renders an import's progress: a Bubble Tea program for a
// terminal, periodic status lines otherwise, and the report both end with.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

// FormatBytes renders n in binary units with one decimal, bytes exact.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n) / unit
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, suffix)
		}
		f /= unit
	}
	return fmt.Sprintf("%.1f EiB", f)
}

// FormatCount renders n with thousands separators.
func FormatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// FormatClock renders elapsed time as m:ss or h:mm:ss.
func FormatClock(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// FormatShort renders a duration as 42s, 2m10s or 1h03m.
func FormatShort(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// FormatETA renders an estimate: ~ plus FormatShort, or >1h.
func FormatETA(d time.Duration) string {
	if d > time.Hour {
		return ">1h"
	}
	return "~" + FormatShort(d)
}

// ShortDigest is the first eight hex characters of d.
func ShortDigest(d oci.Digest) string {
	h := d.Hex()
	if len(h) > 8 {
		h = h[:8]
	}
	return h
}
```

Check `FormatShort(2m10s)`: the test wants `2m10s`, and `%dm%02ds` gives `2m10s`. Good.

- [ ] **Step 4: Implement `tui/report.go`**

```go
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
)

// RenderReport renders the end-of-run report as plain text.
func RenderReport(r *importer.Report, archive string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Imported %s in %s\n\n", archive, FormatShort(r.Duration))
	for _, e := range r.Entries {
		names := make([]string, len(e.Names))
		for i, n := range e.Names {
			names[i] = n.String()
		}
		fmt.Fprintf(&b, "  %s   %s   %s   %s\n", strings.Join(names, ", "), shortRef(e), kindOf(e), rootfsOf(e))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%-15s %s\n", "Blobs", blobsLine(r.Blobs))
	line := func(label string, n int64, note string) {
		fmt.Fprintf(&b, "%-15s %-11s %s bytes   %s\n", label, FormatBytes(n), FormatCount(n), note)
	}
	line("Compressed", r.Compressed, "blob and manifest bytes as they are in the archive")
	line("Uncompressed", r.Uncompressed, "after decompression; raw blobs counted as is")
	line("Added to CAS", r.Added, "appended to pack segments, manifests and rootfs tree included")
	switch ratio, ok := r.DedupRatio(); {
	case r.Blobs.Processed == 0:
		// A re-import stores no blob; what it adds is the rewritten manifest
		// metadata (meta.json and root nodes), a few hundred bytes.
		note := "everything already present"
		if r.Added > 0 {
			note += fmt.Sprintf(", %s bytes of manifest metadata rewritten", FormatCount(r.Added))
		}
		fmt.Fprintf(&b, "%-15s %s\n", "Dedup ratio", note)
	case ok:
		fmt.Fprintf(&b, "%-15s %-11s compressed bytes ÷ bytes added to CAS   (%.1f%% not written)\n", "Dedup ratio", fmt.Sprintf("%.2fx", ratio), r.NotWrittenPercent())
	default:
		fmt.Fprintf(&b, "%-15s %s\n", "Dedup ratio", "nothing added")
	}
	fmt.Fprintf(&b, "%-15s %-11s of offered bytes were already in the store\n", "Chunks reused", fmt.Sprintf("%.1f%%", r.ChunksReusedPercent()))
	return b.String()
}

func shortRef(e importer.EntryReport) string {
	h := e.Digest.Hex()
	if len(h) >= 8 {
		return "sha256:" + h[:4] + "…" + h[len(h)-4:]
	}
	return e.Digest.String()
}

func kindOf(e importer.EntryReport) string {
	if !e.IsIndex {
		return "manifest"
	}
	return fmt.Sprintf("index, %s + %s", plural(e.Platforms, "platform"), plural(e.Attestations, "attestation"))
}

func rootfsOf(e importer.EntryReport) string {
	switch len(e.Rootfs) {
	case 0:
		return ""
	case 1:
		rf := e.Rootfs[0]
		if rf.Status == image.RootfsOK || rf.Status == image.RootfsPartial {
			return fmt.Sprintf("rootfs %s, %s entries", rf.Status, FormatCount(int64(rf.Entries)))
		}
		return "rootfs " + string(rf.Status)
	}
	ok := 0
	for _, rf := range e.Rootfs {
		if rf.Status == image.RootfsOK {
			ok++
		}
	}
	return fmt.Sprintf("rootfs %d/%d ok", ok, len(e.Rootfs))
}

func blobsLine(c importer.BlobCounts) string {
	total := c.Processed + c.Present
	if c.Processed == 0 {
		return fmt.Sprintf("%d processed, %d already present", total, c.Present)
	}
	stored := fmt.Sprintf("%d stored (%d prism, %d raw%s)", c.Prism+c.Raw, c.Prism, c.Raw, rawReasons(c.RawReasons))
	return fmt.Sprintf("%d processed: %s, %d already present", total, stored, c.Present)
}

func rawReasons(m map[blob.RawReason]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(keys) == 1 {
			parts = append(parts, k)
		} else {
			parts = append(parts, fmt.Sprintf("%d %s", m[blob.RawReason(k)], k))
		}
	}
	return ": " + strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
```

Check the `Blobs` test string: Processed 6 + Present 2 = "8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present". Good.

- [ ] **Step 5: Run the tests**

Run: `go test ./tui -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tui/format.go tui/format_test.go tui/report.go tui/report_test.go
git commit -m "tui: size and duration formatting; the import report"
```

---

### Task 9: Bubble Tea model and plain mode (`tui/model.go`, `tui/plain.go`)

**Files:**
- Create: `tui/model.go`, `tui/view.go`, `tui/plain.go`
- Test: `tui/view_test.go`, `tui/plain_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces:
  ```go
  func Run(tr *importer.Tracker, title string, cancel func(), run func() (*importer.Report, error)) (*importer.Report, error)
  func RunPlain(w io.Writer, tr *importer.Tracker, interval time.Duration, run func() (*importer.Report, error)) (*importer.Report, error)
  func RenderView(s importer.Snapshot, title string, width int, bar func(float64) string, spinner string) string
  func StatusLine(s importer.Snapshot) string
  ```

- [ ] **Step 1: Add the dependencies**

```bash
go get github.com/charmbracelet/bubbletea@v1.3.10 github.com/charmbracelet/bubbles@v1.0.0 github.com/charmbracelet/lipgloss@v1.1.0
go mod tidy
```

`go.mod` must still list `golang.org/x/term` (used directly in Task 10; `go mod tidy` will move it to a direct requirement then).

- [ ] **Step 2: Write the failing view tests**

Create `tui/view_test.go`:

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/oci"
)

func plainBar(f float64) string { return "[" + strings.Repeat("#", int(f*10)) + strings.Repeat(".", 10-int(f*10)) + "]" }

func blobSnapshot() importer.Snapshot {
	return importer.Snapshot{
		Phase: importer.PhaseBlobs,
		Blobs: []importer.BlobRow{
			{Digest: oci.DigestOfBytes([]byte("a")), Size: 1900727, State: importer.BlobInFlight, Stage: blob.StageDecompose, Fraction: 0.52, Progress: 950000},
			{Digest: oci.DigestOfBytes([]byte("b")), Size: 38 << 20, State: importer.BlobInFlight, Stage: blob.StageAnalyze, Fraction: 0.5, Progress: 38 << 20},
			{Digest: oci.DigestOfBytes([]byte("c")), Size: 10, State: importer.BlobDone, Fraction: 1},
			{Digest: oci.DigestOfBytes([]byte("d")), Size: 10, State: importer.BlobPresent, Fraction: 1},
			{Digest: oci.DigestOfBytes([]byte("e")), Size: 10, State: importer.BlobPending},
		},
		Counts:   importer.Counts{Pending: 1, InFlight: 2, Done: 1, Present: 1},
		Fraction: 0.41,
		Elapsed:  42 * time.Second,
		ETA:      2*time.Minute + 10*time.Second,
		ETAKnown: true,
	}
}

func TestRenderViewBlobPhase(t *testing.T) {
	out := RenderView(blobSnapshot(), "Importing busybox.tar → busybox:1.37", 100, plainBar, "⠋")
	for _, want := range []string{
		"Importing busybox.tar → busybox:1.37",
		"elapsed 0:42",
		"1 done · 1 already present · 2 in flight · 1 pending",
		ShortDigest(oci.DigestOfBytes([]byte("a"))) + "  1.8 MiB",
		"decompose",
		"52%",
		"analyze",
		"searching…",
		"41%",
		"ETA ~2m10s",
		"q or ctrl-c to cancel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ShortDigest(oci.DigestOfBytes([]byte("e")))) {
		t.Errorf("pending blobs must not get a row:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("line wider than the terminal: %q", line)
		}
	}
}

func TestRenderViewCheckingAndManifests(t *testing.T) {
	s := importer.Snapshot{Phase: importer.PhaseChecking, Checked: 0.41, Elapsed: time.Second}
	out := RenderView(s, "t", 80, plainBar, "")
	if !strings.Contains(out, "checking archive") || !strings.Contains(out, "41%") || strings.Contains(out, "ETA") {
		t.Errorf("checking view:\n%s", out)
	}
	s = importer.Snapshot{Phase: importer.PhaseManifests, Manifests: []importer.ManifestRow{
		{Digest: oci.DigestOfBytes([]byte("m")), State: importer.ManifestDone, Rootfs: &image.Rootfs{Status: image.RootfsOK, Entries: 1204}},
		{Digest: oci.DigestOfBytes([]byte("i")), IsIndex: true, State: importer.ManifestInFlight, Names: []dockerarchive.Name{{Repo: "busybox", Tag: "1.37"}}},
	}}
	out = RenderView(s, "t", 80, plainBar, "⠋")
	for _, want := range []string{"rootfs ok, 1,204 entries", "⠋", "busybox:1.37", "publishing"} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest view lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(RenderView(importer.Snapshot{Phase: importer.PhaseBlobs, Fraction: 0.1}, "t", 80, plainBar, ""), "ETA estimating") {
		t.Error("unknown ETA must render as estimating")
	}
}

func TestStatusLine(t *testing.T) {
	got := StatusLine(blobSnapshot())
	if got != "blobs 1/4 · 41% · ETA ~2m10s" {
		t.Errorf("StatusLine = %q", got)
	}
	s := importer.Snapshot{Phase: importer.PhaseChecking, Checked: 0.5}
	if got := StatusLine(s); got != "checking archive · 50%" {
		t.Errorf("StatusLine(checking) = %q", got)
	}
	s = importer.Snapshot{Phase: importer.PhaseManifests, Manifests: []importer.ManifestRow{{State: importer.ManifestDone}, {State: importer.ManifestInFlight}}}
	if got := StatusLine(s); got != "publishing manifests 1/2" {
		t.Errorf("StatusLine(manifests) = %q", got)
	}
}
```

Create `tui/plain_test.go`:

```go
package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/importer"
)

func TestRunPlainPrintsStatusAndReturnsResult(t *testing.T) {
	tr := importer.NewTracker(importer.TrackerOptions{Verify: true})
	var out bytes.Buffer
	want := &importer.Report{}
	rep, err := RunPlain(&out, tr, 10*time.Millisecond, func() (*importer.Report, error) {
		tr.Queue(&dockerarchivePlan)
		tr.StartBlobs()
		time.Sleep(50 * time.Millisecond)
		return want, nil
	})
	if err != nil || rep != want {
		t.Fatalf("rep, err = %v, %v", rep, err)
	}
	if !strings.Contains(out.String(), "blobs 0/1") {
		t.Fatalf("no status line printed:\n%s", out.String())
	}
	boom := errors.New("boom")
	if _, err := RunPlain(&out, tr, time.Hour, func() (*importer.Report, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
```

Add at the top of `tui/plain_test.go` a one-blob plan value:

```go
var dockerarchivePlan = dockerarchive.Plan{Blobs: []dockerarchive.PlanBlob{{Digest: oci.DigestOfBytes([]byte("x")), Size: 10}}}
```

with imports `"github.com/amber-store/oci-amber/dockerarchive"` and `"github.com/amber-store/oci-amber/oci"`.

- [ ] **Step 3: Implement `tui/view.go`**

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// RenderView renders one frame for width columns. bar draws a progress bar
// for a fraction; spinner is the current spinner frame. It is a pure
// function so tests can render snapshots without a terminal.
func RenderView(s importer.Snapshot, title string, width int, bar func(float64) string, spinner string) string {
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	elapsed := "elapsed " + FormatClock(s.Elapsed)
	gap := width - lipgloss.Width(title) - lipgloss.Width(elapsed)
	if gap < 2 {
		gap = 2
	}
	b.WriteString(styleTitle.Render(title) + strings.Repeat(" ", gap) + styleDim.Render(elapsed) + "\n\n")
	switch s.Phase {
	case importer.PhaseChecking, importer.PhaseIdle:
		fmt.Fprintf(&b, "  checking archive  %s  %3.0f%%\n", bar(s.Checked), s.Checked*100)
	case importer.PhaseBlobs:
		c := s.Counts
		fmt.Fprintf(&b, "  blobs  %s\n", styleDim.Render(fmt.Sprintf("%d done · %d already present · %d in flight · %d pending", c.Done, c.Present, c.InFlight, c.Pending)))
		for _, r := range s.Blobs {
			if r.State != importer.BlobInFlight {
				continue
			}
			b.WriteString(blobLine(r, bar))
		}
		eta := "ETA estimating"
		if s.ETAKnown {
			eta = "ETA " + FormatETA(s.ETA)
		}
		fmt.Fprintf(&b, "\n  %s  %3.0f%%   %s\n", bar(s.Fraction), s.Fraction*100, eta)
	case importer.PhaseManifests, importer.PhaseDone:
		b.WriteString("  publishing manifests\n")
		for _, m := range s.Manifests {
			b.WriteString(manifestLine(m, spinner))
		}
	}
	b.WriteString("\n  " + styleDim.Render("q or ctrl-c to cancel") + "\n")
	return clampWidth(b.String(), width)
}

func blobLine(r importer.BlobRow, bar func(float64) string) string {
	stage := string(r.Stage)
	if stage == "" {
		stage = "queued"
	}
	detail := fmt.Sprintf("%3.0f%%", r.Fraction*100)
	if r.Stage == blob.StageAnalyze && r.Size > 0 && r.Progress >= r.Size {
		detail = "searching…"
	}
	return fmt.Sprintf("  ▸ %s  %-9s  %-9s  %s  %s\n", ShortDigest(r.Digest), FormatBytes(r.Size), stage, bar(r.Fraction), detail)
}

func manifestLine(m importer.ManifestRow, spinner string) string {
	label := ShortDigest(m.Digest)
	if len(m.Names) > 0 {
		names := make([]string, len(m.Names))
		for i, n := range m.Names {
			names[i] = n.String()
		}
		label = strings.Join(names, ", ")
	}
	kind := "manifest"
	if m.IsIndex {
		kind = "index"
	}
	switch m.State {
	case importer.ManifestInFlight:
		what := "building rootfs"
		if m.IsIndex {
			what = "publishing"
		}
		return fmt.Sprintf("  %s %s  %s  %s\n", spinner, label, kind, styleDim.Render(what))
	case importer.ManifestDone:
		out := styleOK.Render("✓")
		rf := ""
		if m.Rootfs != nil && m.Rootfs.Status != image.RootfsNotApplicable {
			switch m.Rootfs.Status {
			case image.RootfsOK, image.RootfsPartial:
				rf = fmt.Sprintf("rootfs %s, %s entries", m.Rootfs.Status, FormatCount(int64(m.Rootfs.Entries)))
			default:
				rf = styleWarn.Render("rootfs " + string(m.Rootfs.Status))
			}
		}
		return fmt.Sprintf("  %s %s  %s  %s\n", out, label, kind, rf)
	default:
		return fmt.Sprintf("  %s %s  %s\n", styleDim.Render("·"), label, kind)
	}
}

// clampWidth truncates every line to width cells so the block never wraps,
// which would break the in-place redraw.
func clampWidth(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = lipgloss.NewStyle().MaxWidth(width).Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// StatusLine is the one-line summary plain mode prints.
func StatusLine(s importer.Snapshot) string {
	switch s.Phase {
	case importer.PhaseChecking, importer.PhaseIdle:
		return fmt.Sprintf("checking archive · %.0f%%", s.Checked*100)
	case importer.PhaseBlobs:
		c := s.Counts
		total := c.Pending + c.InFlight + c.Done + c.Failed
		eta := "ETA estimating"
		if s.ETAKnown {
			eta = "ETA " + FormatETA(s.ETA)
		}
		return fmt.Sprintf("blobs %d/%d · %.0f%% · %s", c.Done, total, s.Fraction*100, eta)
	default:
		done := 0
		for _, m := range s.Manifests {
			if m.State == importer.ManifestDone {
				done++
			}
		}
		return fmt.Sprintf("publishing manifests %d/%d", done, len(s.Manifests))
	}
}
```

- [ ] **Step 4: Implement `tui/model.go`**

```go
package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/amber-store/oci-amber/importer"
)

// tickInterval is how often the view is refreshed from the tracker.
const tickInterval = 250 * time.Millisecond

type tickMsg time.Time

// doneMsg says the import returned; the program quits with an empty view.
type doneMsg struct{}

// model is the Bubble Tea model: it owns no import state, it renders the
// tracker's snapshots.
type model struct {
	tr     *importer.Tracker
	title  string
	cancel func()
	width  int
	bar    progress.Model
	spin   spinner.Model
	done   bool
}

func newModel(tr *importer.Tracker, title string, cancel func()) model {
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	bar.Width = 30
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	return model{tr: tr, title: title, cancel: cancel, width: 80, bar: bar, spin: sp}
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(tick(), m.spin.Tick) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.bar.Width = max(10, min(40, msg.Width/3))
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cancel()
		}
	case tickMsg:
		return m, tick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case doneMsg:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() string {
	if m.done {
		return ""
	}
	return RenderView(m.tr.Snapshot(), m.title, m.width, m.bar.ViewAs, m.spin.View())
}

// Run drives run under the TUI: run starts on a goroutine, the program
// renders the tracker until run returns, and run's result is returned. cancel
// is called on q or ctrl-c; it should cancel run's context. Signals are left
// to the caller (the program does not install a handler), so a SIGINT from
// outside cancels through the caller's context the same way.
func Run(tr *importer.Tracker, title string, cancel func(), run func() (*importer.Report, error)) (*importer.Report, error) {
	p := tea.NewProgram(newModel(tr, title, cancel), tea.WithoutSignalHandler())
	type result struct {
		rep *importer.Report
		err error
	}
	results := make(chan result, 1)
	go func() {
		rep, err := run()
		results <- result{rep, err}
		p.Send(doneMsg{})
	}()
	if _, err := p.Run(); err != nil {
		// The terminal failed under us; the import keeps going and its
		// result still decides the exit status.
		cancel()
	}
	r := <-results
	return r.rep, r.err
}
```

- [ ] **Step 5: Implement `tui/plain.go`**

```go
package tui

import (
	"fmt"
	"io"
	"time"

	"github.com/amber-store/oci-amber/importer"
)

// RunPlain drives run without a screen: a status line is written to w every
// interval until run returns. It is what a non-terminal stdout gets.
func RunPlain(w io.Writer, tr *importer.Tracker, interval time.Duration, run func() (*importer.Report, error)) (*importer.Report, error) {
	type result struct {
		rep *importer.Report
		err error
	}
	results := make(chan result, 1)
	go func() {
		rep, err := run()
		results <- result{rep, err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case r := <-results:
			return r.rep, r.err
		case <-ticker.C:
			fmt.Fprintln(w, StatusLine(tr.Snapshot()))
		}
	}
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./tui -race`
Expected: PASS. The Bubble Tea program itself is not run in tests (it needs a terminal); `Run` is exercised by the manual smoke test in Task 11.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum tui/model.go tui/view.go tui/plain.go tui/view_test.go tui/plain_test.go
git commit -m "tui: Bubble Tea progress view and plain status lines"
```

---

### Task 10: The `import` subcommand (`cmd/oci-amber`)

**Files:**
- Create: `cmd/oci-amber/import.go`
- Modify: `cmd/oci-amber/main.go` (`newApp`, `serveFlags` → shared `storeFlags`, `main`), `cmd/oci-amber/app_test.go` (`runApp` passes both actions)
- Test: `cmd/oci-amber/import_test.go`

**Interfaces:**
- Consumes: everything above; `golang.org/x/term.IsTerminal`.
- Produces:
  ```go
  type importConfig struct {
      Store, WorkDir string
      MaxInMemory int64; AnalyzeParallelism int; AnalyzeTimeout time.Duration; MaxConcurrentFinalize int; VerifyRoundTrip bool
      LogLevel slog.Level; LogFile string
      Archive string; Names []string; Progress string // "tui" | "plain" | "auto"
      Stdin io.Reader; Stdout, Stderr io.Writer       // nil → os.*
  }
  func importFlags() []cli.Flag
  func importConfigFromCLI(c *cli.Context) (importConfig, error)
  func runImport(ctx context.Context, cfg importConfig) error
  func newApp(serve func(context.Context, config) error, imp func(context.Context, importConfig) error) *cli.App
  ```

- [ ] **Step 1: Write the failing flag tests**

Create `cmd/oci-amber/import_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// runImportApp runs the import command with args and returns the config
// the action received, or the error the app returned.
func runImportApp(t *testing.T, args ...string) (importConfig, error) {
	t.Helper()
	var got importConfig
	called := false
	app := newApp(func(context.Context, config) error { return nil }, func(_ context.Context, cfg importConfig) error {
		called = true
		got = cfg
		return nil
	})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "import"}, args...))
	if err == nil && !called {
		t.Fatal("import action was not called")
	}
	return got, err
}

func TestImportFlagDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := runImportApp(t, "--store", "/srv/amber", "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	want := importConfig{
		Store:                 "/srv/amber",
		WorkDir:               filepath.Join("/srv/amber", "work"),
		MaxInMemory:           64 << 20,
		AnalyzeParallelism:    2,
		AnalyzeTimeout:        15 * time.Minute,
		MaxConcurrentFinalize: max(1, runtime.NumCPU()/2),
		VerifyRoundTrip:       true,
		LogLevel:              slog.LevelInfo,
		Archive:               "image.tar",
		Progress:              "auto",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestImportFlagsExplicit(t *testing.T) {
	clearEnv(t)
	cfg, err := runImportApp(t, "--store", "/s", "--name", "a:1", "--name", "b:2", "--progress", "plain", "--log-file", "/tmp/x.log", "--verify-roundtrip=false", "-")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Archive != "-" || !reflect.DeepEqual(cfg.Names, []string{"a:1", "b:2"}) || cfg.Progress != "plain" || cfg.LogFile != "/tmp/x.log" || cfg.VerifyRoundTrip {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestImportRejectsBadValues(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{
		{"--store", "/s"},                                  // no archive
		{"--store", "/s", "a.tar", "b.tar"},                // two archives
		{"--store", "/s", "--progress", "fancy", "a.tar"},  // bad progress mode
		{"--store", "/s", "--name", "not valid", "a.tar"},  // bad name
		{"a.tar"},                                          // no store
	} {
		if _, err := runImportApp(t, args...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

// TestRunImportPlain imports a synthesized archive in plain mode and checks
// the report and the store.
func TestRunImportPlain(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte(strings.Repeat("plain layer bytes ", 64))
	img := b.AddImage(config, []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"demo/app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	tmp := t.TempDir()
	path, err := b.WriteFile(tmp, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelInfo,
		Archive: path, Progress: "plain", Stdout: &stdout, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("runImport: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Imported image.tar in", "demo/app:v1", "Compressed", "Added to CAS", "Dedup ratio"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "blob stored") {
		t.Errorf("plain mode must log to stderr:\n%s", stderr.String())
	}
	// The tag is there: open the store again the way serve would.
	cfg2 := cfg
	cfg2.Stdout, cfg2.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := runImport(context.Background(), cfg2); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !strings.Contains(cfg2.Stdout.(*bytes.Buffer).String(), "everything already present") {
		t.Errorf("second run report:\n%s", cfg2.Stdout.(*bytes.Buffer).String())
	}
	_ = image.KindManifest
}

func TestRunImportFromStdin(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	img := b.AddImage(config, nil, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"empty:1"}})
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelWarn,
		Archive: "-", Progress: "plain", Stdin: bytes.NewReader(b.Bytes()), Stdout: &stdout, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("runImport: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "empty:1") {
		t.Errorf("report:\n%s", stdout.String())
	}
	entries, _ := filepath.Glob(filepath.Join(tmp, "store", "work", "oci-amber", "import-*.tar"))
	if len(entries) != 0 {
		t.Errorf("stdin copy left behind: %v", entries)
	}
}
```

Drop the `_ = image.KindManifest` line and the `image` import if unused.

- [ ] **Step 2: Update `app_test.go`'s `runApp`** to call `newApp(func(...) error {...}, func(context.Context, importConfig) error { return nil })`.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./cmd/oci-amber -run 'TestImport|TestRunImport'`
Expected: compile errors.

- [ ] **Step 4: Refactor `main.go`**

Change `newApp` to:

```go
// newApp builds the command line application. serve and imp are what the
// `serve` and `import` subcommands run once their flags have been
// validated; main passes run and runImport, tests pass functions that
// capture the config.
func newApp(serve func(ctx context.Context, cfg config) error, imp func(ctx context.Context, cfg importConfig) error) *cli.App {
	return &cli.App{
		Name:            "oci-amber",
		Usage:           "OCI distribution registry backed by an embedded amber store",
		HideHelpCommand: true,
		Commands: []*cli.Command{{
			Name:  "serve",
			Usage: "run the registry",
			Flags: serveFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := configFromCLI(c)
				if err != nil {
					return err
				}
				return serve(c.Context, cfg)
			},
		}, {
			Name:      "import",
			Usage:     "store a `docker image save` archive without running the registry",
			ArgsUsage: "<archive.tar | ->",
			Flags:     importFlags(),
			Action: func(c *cli.Context) error {
				cfg, err := importConfigFromCLI(c)
				if err != nil {
					return err
				}
				return imp(c.Context, cfg)
			},
		}},
	}
}
```

Split the flag table: `storeFlags()` returns the eight flags both commands share (`store`, `work-dir`, `max-in-memory`, `analyze-parallelism`, `analyze-timeout`, `max-concurrent-finalize`, `verify-roundtrip`, `log-level`) with the same definitions as today; `serveFlags()` returns `append(storeFlags(), listen, upload-timeout, gc-interval)`. Keep `--work-dir`'s usage text as it is for serve; the import command reuses the same flag object so the text mentions uploads/ too, which is acceptable. Update `main` to `newApp(serve, runImport)` and the package doc comment to name both subcommands.

- [ ] **Step 5: Create `cmd/oci-amber/import.go`**

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// importRecentTTL keeps every blob's accounting in the recent-uploads table
// for the whole run: the image store reads it when the manifest is
// published, which can be hours after a layer was stored.
const importRecentTTL = 365 * 24 * time.Hour

// plainStatusInterval is how often plain mode prints a status line.
const plainStatusInterval = 5 * time.Second

// importConfig is everything `import` needs. importConfigFromCLI fills it
// from flags; tests construct it directly and call runImport.
type importConfig struct {
	Store                 string
	WorkDir               string // "" means <Store>/work
	MaxInMemory           int64
	AnalyzeParallelism    int
	AnalyzeTimeout        time.Duration
	MaxConcurrentFinalize int
	VerifyRoundTrip       bool
	LogLevel              slog.Level
	LogFile               string
	Archive               string   // path, or "-" for stdin
	Names                 []string // --name overrides
	Progress              string   // "auto" | "tui" | "plain"
	Stdin                 io.Reader
	Stdout, Stderr        io.Writer
}

func importFlags() []cli.Flag {
	return append(storeFlags(),
		&cli.StringSliceFlag{Name: "name", Usage: "publish under `repo:tag` instead of the archive's RepoTags (repeatable; single-image archives only)"},
		&cli.StringFlag{Name: "progress", Value: "auto", Usage: "progress display: auto, tui or plain", EnvVars: envVar("progress")},
		&cli.StringFlag{Name: "log-file", Usage: "write the full log to `path`", EnvVars: envVar("log-file")},
	)
}

func importConfigFromCLI(c *cli.Context) (importConfig, error) {
	if c.NArg() != 1 {
		return importConfig{}, errors.New("import takes exactly one archive path (or - for stdin)")
	}
	cfg := importConfig{
		Store:                 c.String("store"),
		WorkDir:               c.String("work-dir"),
		AnalyzeParallelism:    c.Int("analyze-parallelism"),
		AnalyzeTimeout:        c.Duration("analyze-timeout"),
		MaxConcurrentFinalize: c.Int("max-concurrent-finalize"),
		VerifyRoundTrip:       c.Bool("verify-roundtrip"),
		LogFile:               c.String("log-file"),
		Archive:               c.Args().First(),
		Names:                 c.StringSlice("name"),
		Progress:              c.String("progress"),
	}
	if cfg.Store == "" {
		return importConfig{}, errors.New("--store must not be empty")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(cfg.Store, "work")
	}
	size, err := parseSize(c.String("max-in-memory"))
	if err != nil {
		return importConfig{}, fmt.Errorf("--max-in-memory: %w", err)
	}
	cfg.MaxInMemory = size
	if cfg.AnalyzeParallelism < 1 {
		return importConfig{}, fmt.Errorf("--analyze-parallelism must be at least 1, got %d", cfg.AnalyzeParallelism)
	}
	if cfg.AnalyzeTimeout <= 0 {
		return importConfig{}, fmt.Errorf("--analyze-timeout must be positive, got %s", cfg.AnalyzeTimeout)
	}
	if cfg.MaxConcurrentFinalize < 1 {
		return importConfig{}, fmt.Errorf("--max-concurrent-finalize must be at least 1, got %d", cfg.MaxConcurrentFinalize)
	}
	switch cfg.Progress {
	case "auto", "tui", "plain":
	default:
		return importConfig{}, fmt.Errorf("--progress must be auto, tui or plain, got %q", cfg.Progress)
	}
	for _, n := range cfg.Names {
		if _, err := dockerarchive.ParseName(n); err != nil {
			return importConfig{}, fmt.Errorf("--name %v", err)
		}
	}
	level, err := parseLogLevel(c.String("log-level"))
	if err != nil {
		return importConfig{}, fmt.Errorf("--log-level: %w", err)
	}
	cfg.LogLevel = level
	if len(cfg.Names) == 0 {
		cfg.Names = nil
	}
	return cfg, nil
}

// runImport opens the archive and the store, plans, runs the import under
// the chosen progress display and prints the report.
func runImport(ctx context.Context, cfg importConfig) error {
	stdin, stdout, stderr := cfg.Stdin, cfg.Stdout, cfg.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	mode := cfg.Progress
	if mode == "auto" {
		mode = "plain"
		if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			mode = "tui"
		}
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Join(cfg.Store, "work")
	}
	ownDir := filepath.Join(workDir, workSubdir)
	if err := os.MkdirAll(ownDir, 0o755); err != nil {
		return fmt.Errorf("creating work directory %s: %w", ownDir, err)
	}

	// Logging: a file gets everything; plain mode logs to stderr; the TUI
	// keeps warnings and errors for after the screen is gone.
	var deferred bytes.Buffer
	var logOut io.Writer = stderr
	level := cfg.LogLevel
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		logOut = f
	} else if mode == "tui" {
		logOut = &deferred
		level = max(level, slog.LevelWarn)
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	defer func() {
		if deferred.Len() > 0 {
			io.Copy(stderr, &deferred)
		}
	}()

	path := cfg.Archive
	if path == "-" {
		tmp := filepath.Join(ownDir, fmt.Sprintf("import-%d.tar", os.Getpid()))
		if err := spoolStdin(stdin, tmp); err != nil {
			return err
		}
		defer os.Remove(tmp)
		path = tmp
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer arch.Close()

	st, err := store.Open(cfg.Store, store.Options{Logger: log})
	if err != nil {
		return fmt.Errorf("opening store %s: %w", cfg.Store, err)
	}
	defer st.Close()

	tr := importer.NewTracker(importer.TrackerOptions{Verify: cfg.VerifyRoundTrip})
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               ownDir,
		MaxInMemory:           cfg.MaxInMemory,
		AnalyzeParallelism:    cfg.AnalyzeParallelism,
		AnalyzeTimeout:        cfg.AnalyzeTimeout,
		MaxConcurrentFinalize: cfg.MaxConcurrentFinalize,
		VerifyRoundTrip:       cfg.VerifyRoundTrip,
		RecentTTL:             importRecentTTL,
		Observer:              tr,
	}, log)
	if err != nil {
		return fmt.Errorf("creating blob store: %w", err)
	}
	images := image.New(st, blobs, log)

	plan, err := arch.Plan(dockerarchive.PlanOptions{Names: cfg.Names, Present: blobs.Exists})
	if err != nil {
		return err
	}
	im := importer.New(blobs, images, arch, plan, tr, importer.Options{Workers: cfg.MaxConcurrentFinalize})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := func() (*importer.Report, error) { return im.Run(ctx) }
	var rep *importer.Report
	if mode == "tui" {
		rep, err = tui.Run(tr, importTitle(cfg.Archive, plan), cancel, run)
	} else {
		rep, err = tui.RunPlain(stderr, tr, plainStatusInterval, run)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errors.New("import cancelled")
		}
		return err
	}
	fmt.Fprint(stdout, tui.RenderReport(rep, filepath.Base(cfg.Archive)))
	return nil
}

// importTitle is the TUI's first line: the archive and the names.
func importTitle(archive string, plan *dockerarchive.Plan) string {
	var names []string
	for _, e := range plan.Entries {
		for _, n := range e.Names {
			names = append(names, n.String())
		}
	}
	label := archive
	if archive == "-" {
		label = "stdin"
	} else {
		label = filepath.Base(archive)
	}
	return fmt.Sprintf("Importing %s → %s", label, strings.Join(names, ", "))
}

// spoolStdin copies r to path so the archive can be read in place.
func spoolStdin(r io.Reader, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("spooling stdin: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("spooling stdin: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("spooling stdin: %w", err)
	}
	return nil
}
```

`go mod tidy` after adding the `golang.org/x/term` import promotes it to a direct dependency.

- [ ] **Step 6: Run all the tests**

Run: `go test ./... -race` and `go vet ./...` and `gofmt -l .`
Expected: PASS, no vet findings, no unformatted files. The existing `TestServeFlag*` tests must still pass after the `newApp` signature change.

- [ ] **Step 7: Commit**

```bash
git add cmd/oci-amber go.mod go.sum
git commit -m "cmd/oci-amber: import subcommand"
```

---

### Task 11: Documentation, real-archive smoke test, pull request

**Files:**
- Modify: `README.md`, `docs/superpowers/specs/2026-09-05-import-command-design.md` (only if the implementation deviated), `cmd/oci-amber/main.go` package comment

- [ ] **Step 1: README section**

After the "Running" section's push/pull examples, add:

````markdown
## Importing a docker save archive

`oci-amber import` stores an image saved with `docker image save` straight
into the store, without running the registry, through the same pipeline a
push goes through. On a terminal it shows per-layer progress and an ETA;
when it is done it prints a report of what was ingested.

```sh
docker image save busybox:1.37 -o busybox.tar
./oci-amber import --store /var/lib/oci-amber busybox.tar
docker image save app:v1 | ./oci-amber import --store /var/lib/oci-amber -
```

The image is published under the archive's `RepoTags` with a leading
registry host removed (`registry.example.ch/team/app:v1` becomes
`team/app:v1`); `--name repo:tag` overrides that for a single-image
archive. Only the platforms whose blobs the archive holds are published:
the index is pruned to them, as `docker push` does. Archives without an
OCI layout (`index.json`; Docker before 25, podman's docker-archive) are
rejected.

`import` shares `--store`, `--work-dir`, `--max-in-memory`,
`--analyze-parallelism`, `--analyze-timeout`, `--max-concurrent-finalize`,
`--verify-roundtrip` and `--log-level` with `serve`, and adds
`--progress auto|tui|plain` (`auto` picks the TUI on a terminal),
`--log-file path` and `--name`. It cannot run while `serve` has the store
open. An interrupted import leaves the blobs it stored in place; running
it again skips them.

The report:

```
Imported busybox.tar in 42s

  busybox:1.37   sha256:3f0a…9c2e   index, 1 platform + 1 attestation   rootfs ok, 1,204 entries

Blobs           8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present
Compressed      1.8 MiB     1,900,727 bytes   blob and manifest bytes as they are in the archive
Uncompressed    4.2 MiB     4,388,352 bytes   after decompression; raw blobs counted as is
Added to CAS    1.1 MiB     1,153,024 bytes   appended to pack segments, manifests and rootfs tree included
Dedup ratio     1.65x       compressed bytes ÷ bytes added to CAS   (39.3% not written)
Chunks reused   38.2%       of offered bytes were already in the store
```
````

Also mention `import` in the README's first paragraph ("It speaks the standard `/v2/` API ... and imports `docker image save` archives directly.").

- [ ] **Step 2: Smoke test against a real archive**

```bash
cd /Users/dragan/draganm/oci-amber
S=/private/tmp/claude-502/-Users-dragan-draganm-oci-amber/e221da09-c864-43d4-a1bb-a5b986d1fee4/scratchpad
docker image save busybox:1.37 -o $S/busybox.tar
go build -o $S/oci-amber ./cmd/oci-amber
# plain mode first (captures the report), then the TUI in a pty via script(1)
$S/oci-amber import --store $S/store --progress plain $S/busybox.tar
$S/oci-amber import --store $S/store2 $S/busybox.tar   # in the user's terminal, or: script -q /dev/null $S/oci-amber import --store $S/store2 --progress tui $S/busybox.tar
# serve and pull back byte-identical
$S/oci-amber serve --store $S/store --listen 127.0.0.1:5055 & sleep 1
nix develop --command crane pull 127.0.0.1:5055/busybox:1.37 $S/pulled.tar --format=oci
kill %1
rm -rf $S/oci-amber $S/store $S/store2 $S/pulled.tar $S/busybox.tar
```

Verify: the report's blob counts match the archive (7 blobs: index is a manifest, so 6 blobs plus the pruned index as a manifest); the layer `e001ca9b…` is a prism; the pulled manifest's layer digest equals the archive's. Fix anything the real archive reveals (a media type, an annotation shape) before the PR, and note deviations in the spec's "Decisions" list.

- [ ] **Step 3: Final checks and PR**

```bash
gofmt -l . && go vet ./... && go test ./... -race
git add README.md docs
git commit -m "docs: import command"
git push -u origin import-command
gh pr create --title "import: store a docker save archive with a progress TUI" --body-file <(cat <<'EOF'
## Summary

- `oci-amber import <archive|->` stores a `docker image save` archive through the push pipeline (zrecipe, tar-prism, rootfs view) without running the registry.
- The archive is read in place; a pruned index is published for the platforms the archive holds, under the archive's `RepoTags` (host stripped) or `--name`.
- `blob` gains an `Observer` hook; a Bubble Tea TUI shows in-flight layers, stage progress and an ETA, plain status lines off a terminal.
- After the run a report prints compressed and uncompressed bytes ingested, bytes added to the CAS, the dedup ratio and the chunk reuse figure.

Spec: `docs/superpowers/specs/2026-09-05-import-command-design.md`
Plan: `docs/superpowers/plans/2026-09-05-import-command.md`

## Test plan

- [ ] `go test ./... -race`
- [ ] `docker image save busybox:1.37` imported in plain and TUI modes; `crane pull` from `serve` returns the same layer digests
- [ ] second import is all dedup hits and adds nothing

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
EOF
)
```
