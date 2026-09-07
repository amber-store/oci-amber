package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
func (e *env) walk(dir key.Key) map[string]uint64 {
	e.t.Helper()
	out := map[string]uint64{}
	var rec func(prefix string, k key.Key)
	rec = func(prefix string, k key.Key) {
		entries, _, err := e.st.ListDir(k, "", 0)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ent := range entries {
			p := prefix + string(ent.Name)
			out[p] = ent.Mode & store.TypeMask
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
	if m.Rootfs == nil || m.Rootfs.Status != RootfsOK || m.Rootfs.Entries != 5 || m.Rootfs.Reason != "" || m.Rootfs.SkippedCount != 0 {
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
	fsys, ok := im.FS()
	if !ok {
		t.Fatal("FS() reports no rootfs")
	}
	listed, _, err := fsys.List("", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listed {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"bin", "etc", "lnk"}) {
		t.Fatalf("FS root listing = %v", names)
	}
	want := map[string]uint64{"bin": store.TypeDir, "bin/app": store.TypeReg, "etc": store.TypeDir, "etc/new": store.TypeReg, "lnk": store.TypeLink}
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
	if stored.Rootfs == nil || !rootfsEqual(stored.Rootfs, m.Rootfs) {
		t.Fatalf("stored rootfs %+v, returned %+v", stored.Rootfs, m.Rootfs)
	}
	line := lastLine(e.logs.String(), "image pushed")
	if !strings.Contains(line, " rootfs=ok ") || !strings.Contains(line, " rootfs_entries=5 ") {
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
	if _, ok := im.FS(); ok {
		t.Fatal("an unavailable rootfs has an FS")
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
	if !rootfsEqual(second.Rootfs, first.Rootfs) {
		t.Fatalf("re-push rootfs %+v, want %+v", second.Rootfs, first.Rootfs)
	}
	if k, _ := mustOpen(t, e, "library/app", "v2").Rootfs(); k != firstKey {
		t.Fatal("re-push changed the rootfs key")
	}
	if n := strings.Count(e.logs.String(), `msg="rootfs built"`); n != 1 {
		t.Fatalf("rootfs built %d times for two pushes of one digest, want 1:\n%s", n, e.logs.String())
	}

	// An unavailable rootfs is not reused: the raw layer may be a prism by
	// the next push.
	raw, _ := e.layerBlob(2048)
	unavailable := e.put("library/app", "raw", oci.MediaTypeOCIManifest, manifestBody(t, imageManifest(cfg, raw)))
	if unavailable.Rootfs.Status != RootfsUnavailable {
		t.Fatalf("Rootfs = %+v", unavailable.Rootfs)
	}
	if _, _, ok, err := e.images.reuseRootfs("library/app", unavailable.Digest); err != nil || ok {
		t.Fatalf("reuseRootfs of an unavailable rootfs = %v, %v; want false", ok, err)
	}

	// The same image in another repository is rebuilt to the same key.
	other := e.put("library/other", "v1", oci.MediaTypeOCIManifest, body)
	if k, _ := mustOpen(t, e, "library/other", "v1").Rootfs(); k != firstKey || !rootfsEqual(other.Rootfs, first.Rootfs) {
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

// rootfsEqual compares two rootfs fields, skips included.
func rootfsEqual(a, b *Rootfs) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Status == b.Status && a.Entries == b.Entries && a.Reason == b.Reason && a.SkippedCount == b.SkippedCount && slices.Equal(a.Skipped, b.Skipped)
}

// TestPutBuildsRootfsOutsideTheRepositoryLock pushes two platform
// manifests into one repository at once and holds each build in the
// rootfsHook seam until the other has arrived. Both arrive only when the
// build runs before the repository lock; a build under the lock would
// keep the second push waiting for the first to finish.
func TestPutBuildsRootfsOutsideTheRepositoryLock(t *testing.T) {
	e := newEnv(t)
	layer := e.tarLayer(fsEntry{name: "etc/os-release", data: "ID=test\n"})
	amd, _ := e.configBlob("amd64")
	arm, _ := e.configBlob("arm64")
	bodies := [][]byte{manifestBody(t, imageManifest(amd, layer)), manifestBody(t, imageManifest(arm, layer))}

	arrived := make(chan struct{}, len(bodies))
	release := make(chan struct{})
	rootfsHook = func(string, oci.Digest) {
		arrived <- struct{}{}
		<-release
	}
	t.Cleanup(func() { rootfsHook = nil })

	type result struct {
		meta *Meta
		err  error
	}
	results := make(chan result, len(bodies))
	for _, body := range bodies {
		go func() {
			m, err := e.images.Put(context.Background(), "library/app", oci.DigestOfBytes(body).String(), oci.MediaTypeOCIManifest, body)
			results <- result{m, err}
		}()
	}
	both := true
	for range bodies {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			both = false
		}
	}
	close(release)
	for range bodies {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.meta.Rootfs == nil || r.meta.Rootfs.Status != RootfsOK {
			t.Fatalf("Rootfs = %+v, want ok", r.meta.Rootfs)
		}
	}
	if !both {
		t.Fatal("the second rootfs build waited for the first push to finish: the build runs under the repository lock")
	}
}
