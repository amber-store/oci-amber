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
		tarEntry{name: "var/cache/x", data: "xx"},
		tarEntry{name: "var/cache/y", data: "yy"},
	)
	upper := buildTar(t, tar.FormatPAX,
		tarEntry{name: "bin/app", data: "app v2", mode: 0o755},
		tarEntry{name: "bin/.wh.old"},
		tarEntry{name: "etc/", typ: tar.TypeDir, mode: 0o700, uid: 5},
		tarEntry{name: "etc/.wh.a.conf"},
		tarEntry{name: "lib/libnew.so", data: "new"}, // through the lib symlink
		tarEntry{name: "var/cache/.wh..wh..opq"},
		tarEntry{name: "var/cache/z", data: "zz"},
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
		"usr/lib/libnew.so": "new", "lib": "", "var": "", "var/cache": "", "var/cache/z": "zz", "tmp": "",
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
	if !bytes.Equal(libz.ContentKey, libz1.ContentKey) || !bytes.Equal(libz.ContentKey, layers[0].blobs[4][:]) {
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
