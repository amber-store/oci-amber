package rootfs

import (
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/amber-store/core/key"

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
func dirEntry(p string) entry { return entry{kind: kindDir, path: p, mode: 0o750, uid: 2, mtime: 20} }
func symlinkEntry(p, target string) entry {
	return entry{kind: kindSymlink, path: p, mode: 0o777, target: target}
}
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
