package rootfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/store"
)

// fsFixture builds one tree and returns an FS over it and the big file's
// bytes.
func fsFixture(t *testing.T) (*FS, string) {
	t.Helper()
	st := openStore(t)
	big := strings.Repeat("0123456789abcdef", 160_000) // 2.5 MiB, several chunks
	entries := []tarEntry{
		{name: "usr/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/ls", data: big, mode: 0o755, uid: 7, gid: 8},
		{name: "bin", typ: tar.TypeSymlink, link: "usr/bin", mode: 0o777},
		{name: "sbin", typ: tar.TypeSymlink, link: "/usr/bin", mode: 0o777},
		{name: "up", typ: tar.TypeSymlink, link: "../../usr/bin", mode: 0o777},
		{name: "loop1", typ: tar.TypeSymlink, link: "loop2", mode: 0o777},
		{name: "loop2", typ: tar.TypeSymlink, link: "loop1", mode: 0o777},
		{name: "etc/passwd", data: "root:x:0:0\n", mode: 0o644},
		{name: "etc/rc.d/", typ: tar.TypeDir, mode: 0o750},
		{name: "etc/mtab", typ: tar.TypeSymlink, link: "/proc/mounts", mode: 0o777},
		{name: "dev/null", typ: tar.TypeChar, mode: 0o666, major: 1, minor: 3},
	}
	for i := 0; i < 300; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("many/f%03d", i), data: "xx"})
	}
	res, _ := build(t, st, buildTar(t, tar.FormatPAX, entries...))
	return NewFS(st, res.Root), big
}

// readTar reads a tar back into a map keyed by cleaned name.
func readTar(t *testing.T, r io.Reader) map[string]exported {
	t.Helper()
	out := map[string]exported{}
	tr := tar.NewReader(r)
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
		out[Clean(hdr.Name)] = exported{hdr: hdr, data: string(data)}
	}
	return out
}

func TestFSStat(t *testing.T) {
	fs, big := fsFixture(t)
	root, err := fs.Stat("")
	if err != nil || !root.IsDir() || root.Name != "" || root.Content != fs.Root() {
		t.Fatalf("root = %+v, %v", root, err)
	}
	for _, p := range []string{"usr/bin/ls", "bin/ls", "sbin/ls", "up/ls", "/bin/ls", "bin//ls", "./bin/./ls", "../../bin/ls", "usr/bin/../bin/ls"} {
		e, err := fs.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if !e.IsRegular() || e.Name != "ls" || e.Size != int64(len(big)) || e.Mode != store.TypeReg|0o755 || e.UID != 7 || e.GID != 8 || e.TypeName() != "file" {
			t.Fatalf("Stat(%q) = %+v", p, e)
		}
	}
	if e, err := fs.Stat("bin"); err != nil || !e.IsDir() || e.Name != "bin" || e.TypeName() != "dir" {
		t.Fatalf("Stat(bin) = %+v, %v (a symlink is followed at the last component too)", e, err)
	}
	if e, err := fs.Stat("etc/rc.d"); err != nil || !e.IsDir() || e.Mode != store.TypeDir|0o750 {
		t.Fatalf("Stat(etc/rc.d) = %+v, %v", e, err)
	}
	if e, err := fs.Stat("dev/null"); err != nil || e.TypeName() != "char" || e.Rdev != [2]uint64{1, 3} || e.Size != 0 {
		t.Fatalf("Stat(dev/null) = %+v, %v", e, err)
	}
	cases := []struct {
		p    string
		want error
	}{
		{"loop1", ErrLoop},
		{"loop1/x", ErrLoop},
		{"nope", ErrNotFound},
		{"etc/nope", ErrNotFound},
		{"etc/mtab", ErrNotFound}, // dangling: /proc/mounts does not exist
		{"etc/passwd/x", ErrNotDir},
		{"dev/null/x", ErrNotDir},
	}
	for _, c := range cases {
		if _, err := fs.Stat(c.p); !errors.Is(err, c.want) {
			t.Errorf("Stat(%q) = %v, want %v", c.p, err, c.want)
		}
	}
	// "etc/passwd/" cleans to "etc/passwd", which is a file: ErrNotDir
	// applies only when components remain, so that case is a file.
	if e, err := fs.Stat("etc/passwd/"); err != nil || !e.IsRegular() {
		t.Fatalf("Stat(etc/passwd/) = %+v, %v", e, err)
	}
}

func TestFSList(t *testing.T) {
	fs, _ := fsFixture(t)
	var all []string
	after := ""
	pages := 0
	for {
		entries, more, err := fs.List("many", after, 100)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range entries {
			all = append(all, e.Name)
		}
		if !more {
			break
		}
		after = entries[len(entries)-1].Name
	}
	if pages != 3 || len(all) != 300 || !slices.IsSorted(all) || all[0] != "f000" || all[299] != "f299" {
		t.Fatalf("pages=%d entries=%d first=%q last=%q", pages, len(all), all[0], all[len(all)-1])
	}
	entries, more, err := fs.List("", "", 0)
	if err != nil || more {
		t.Fatalf("List root: %v, more=%v", err, more)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"bin", "dev", "etc", "loop1", "loop2", "many", "sbin", "up", "usr"}
	if !slices.Equal(names, want) {
		t.Fatalf("root entries = %v, want %v", names, want)
	}
	if entries[0].TypeName() != "symlink" || entries[0].Target != "usr/bin" {
		t.Fatalf("bin = %+v", entries[0])
	}
	if _, _, err := fs.List("etc/passwd", "", 0); !errors.Is(err, ErrNotDir) {
		t.Fatalf("List of a file = %v", err)
	}
	if _, _, err := fs.List("nope", "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List of a missing path = %v", err)
	}
	through, _, err := fs.List("bin", "", 0)
	if err != nil || len(through) != 1 || through[0].Name != "ls" {
		t.Fatalf("List through a symlink = %+v, %v", through, err)
	}
	page, more, err := fs.List("many", "f100", 5)
	if err != nil || !more || len(page) != 5 || page[0].Name != "f101" {
		t.Fatalf("List after f100 = %+v, more=%v, %v", page, more, err)
	}
}

func TestFSOpenAndSkip(t *testing.T) {
	fs, big := fsFixture(t)
	e, r, err := fs.Open("bin/ls")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if e.Size != int64(len(big)) {
		t.Fatalf("Size = %d, want %d", e.Size, len(big))
	}
	if err := r.Skip(1_500_000); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != big[1_500_000:] {
		t.Fatalf("tail is %d bytes (%v), want %d", len(got), err, len(big)-1_500_000)
	}
	for _, p := range []string{"etc", "dev/null", "bin"} {
		if _, _, err := fs.Open(p); !errors.Is(err, ErrNotFile) {
			t.Fatalf("Open(%q) = %v, want ErrNotFile", p, err)
		}
	}
	if _, _, err := fs.Open("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(nope) = %v", err)
	}
	passwd, err := fs.Stat("etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := fs.Content(passwd)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r2)
	if err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("Content = %q, %v", data, err)
	}
	if _, err := fs.Content(Entry{Name: "d", Mode: store.TypeDir}); !errors.Is(err, ErrNotFile) {
		t.Fatalf("Content of a directory = %v", err)
	}
}

func TestFSWriteTar(t *testing.T) {
	fs, big := fsFixture(t)
	var buf bytes.Buffer
	if err := fs.WriteTar(&buf, "etc"); err != nil {
		t.Fatal(err)
	}
	got := readTar(t, &buf)
	if len(got) != 3 || got["passwd"].data != "root:x:0:0\n" || got["mtab"].hdr.Linkname != "/proc/mounts" ||
		got["rc.d"].hdr.Typeflag != tar.TypeDir || got["rc.d"].hdr.Mode != 0o750 {
		t.Fatalf("etc tar = %v", got)
	}
	buf.Reset()
	if err := fs.WriteTar(&buf, ""); err != nil {
		t.Fatal(err)
	}
	all := readTar(t, &buf)
	if all["usr/bin/ls"].data != big || all["dev/null"].hdr.Typeflag != tar.TypeChar || all["bin"].hdr.Linkname != "usr/bin" || len(all) != 315 {
		t.Fatalf("root tar has %d entries", len(all))
	}
	buf.Reset()
	if err := fs.WriteTar(&buf, "sbin"); err != nil {
		t.Fatal(err)
	}
	if through := readTar(t, &buf); len(through) != 1 || through["ls"].data != big {
		t.Fatalf("tar through a symlink = %d entries", len(through))
	}
	if err := fs.WriteTar(&buf, "etc/passwd"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("tar of a file = %v", err)
	}
	if err := fs.WriteTar(&buf, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tar of a missing path = %v", err)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"": "", "/": "", ".": "", "//a//b/": "a/b", "../x": "x", "a/../../b": "b", "./a": "a", "a/./b": "a/b", "a/b/..": "a",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
