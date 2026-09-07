package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/core/cborx"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// lookupEntry finds name in the directory object dir through the fstree reader
// (Task 5 wraps this as Store.Lookup).
func lookupEntry(t *testing.T, s *Store, dir key.Key, name string) fstree.Entry {
	t.Helper()
	e, err := fstree.LookupEntry(dir, []byte(name), s.Get)
	if err != nil {
		t.Fatalf("LookupEntry(%q): %v", name, err)
	}
	return e
}

func TestDirAddInOrderAndLookup(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())

	fileA, err := w.PutBytes([]byte("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	fileB, err := w.PutBytes(pseudoRandomBytes(t, 2<<20, 7))
	if err != nil {
		t.Fatal(err)
	}
	sub := w.NewDir()
	if err := sub.AddFile("inner", fileA); err != nil {
		t.Fatalf("sub AddFile: %v", err)
	}
	subKey, err := sub.Finish()
	if err != nil {
		t.Fatalf("sub Finish: %v", err)
	}

	d := w.NewDir()
	for _, step := range []struct {
		name string
		add  func() error
	}{
		{"a.txt", func() error { return d.AddFile("a.txt", fileA) }},
		{"big", func() error { return d.AddFile("big", fileB) }},
		{"meta.json", func() error { return d.AddFile("meta.json", fileA) }},
		{"sub", func() error { return d.AddDir("sub", subKey) }},
	} {
		if err := step.add(); err != nil {
			t.Fatalf("add %q: %v", step.name, err)
		}
	}
	root, err := d.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if root.Type() != key.DirLeaf {
		t.Errorf("root type = %s, want DirLeaf for a 4-entry directory", root.Type())
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tc := range []struct {
		name    string
		mode    uint64
		content key.Key
	}{
		{"a.txt", ModeFile, fileA},
		{"big", ModeFile, fileB},
		{"meta.json", ModeFile, fileA},
		{"sub", ModeDir, subKey},
	} {
		e := lookupEntry(t, s, root, tc.name)
		if string(e.Name) != tc.name {
			t.Errorf("%s: Name = %q", tc.name, e.Name)
		}
		if e.Mode != tc.mode {
			t.Errorf("%s: Mode = %#o, want %#o", tc.name, e.Mode, tc.mode)
		}
		if e.UID != 0 || e.GID != 0 || e.Mtime != 0 {
			t.Errorf("%s: UID/GID/Mtime = %d/%d/%d, want zeros", tc.name, e.UID, e.GID, e.Mtime)
		}
		ck, err := key.Parse(e.ContentKey)
		if err != nil {
			t.Fatalf("%s: ContentKey: %v", tc.name, err)
		}
		if ck != tc.content {
			t.Errorf("%s: ContentKey = %s, want %s", tc.name, ck, tc.content)
		}
		if e.LinkTarget != nil || e.Rdev != nil || e.XattrsIn != nil || e.XattrsKey != nil {
			t.Errorf("%s: unexpected payload fields set", tc.name)
		}
	}
	if _, err := fstree.LookupEntry(root, []byte("missing"), s.Get); !errors.Is(err, fstree.ErrNotFound) {
		t.Errorf("Lookup missing: err = %v, want fstree.ErrNotFound", err)
	}
	// The subdirectory is reachable through the entry.
	inner := lookupEntry(t, s, subKey, "inner")
	if ck, _ := key.Parse(inner.ContentKey); ck != fileA {
		t.Errorf("sub/inner ContentKey = %s, want %s", ck, fileA)
	}
	if got := readFileContent(t, s, fileB); len(got) != 2<<20 {
		t.Errorf("big content = %d bytes, want %d", len(got), 2<<20)
	}
}

func TestDirRejectsOutOfOrderAndBadNames(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	blob, err := w.PutBytes([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	d := w.NewDir()
	if err := d.AddFile("b", blob); err != nil {
		t.Fatalf("AddFile b: %v", err)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"a", "not sorted"},   // before the previous entry
		{"b", "not sorted"},   // equal to the previous entry
		{"", "invalid"},       // empty
		{".", "invalid"},      // dot
		{"..", "invalid"},     // dot-dot
		{"c/d", "invalid"},    // slash
		{"c\x00d", "invalid"}, // NUL
	} {
		err := d.AddFile(tc.name, blob)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("AddFile(%q): err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	// A rejected entry does not disturb the order: "c" is still accepted.
	if err := d.AddFile("c", blob); err != nil {
		t.Errorf("AddFile c after rejects: %v", err)
	}
	// Names are compared bytewise: "B" < "b" but "c" < "d".
	if err := d.AddFile("B", blob); err == nil {
		t.Error("AddFile(\"B\") after \"c\" succeeded, want not sorted")
	}
	if err := d.AddFile("d", blob); err != nil {
		t.Errorf("AddFile d: %v", err)
	}
}

func TestDirRejectsWrongKeyTypes(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	blob, err := w.PutBytes([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := w.NewDir().Finish()
	if err != nil {
		t.Fatal(err)
	}
	d := w.NewDir()
	if err := d.AddFile("a", empty); err == nil {
		t.Error("AddFile with a directory key succeeded")
	}
	if err := d.AddDir("a", blob); err == nil {
		t.Error("AddDir with a blob key succeeded")
	}
	if err := d.AddFile("a", blob); err != nil {
		t.Errorf("AddFile with a blob key: %v", err)
	}
	if err := d.AddDir("b", empty); err != nil {
		t.Errorf("AddDir with a dir key: %v", err)
	}
}

func TestDirFinishOnceAndEmpty(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	blob, err := w.PutBytes([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	d := w.NewDir()
	root, err := d.Finish()
	if err != nil {
		t.Fatalf("Finish empty: %v", err)
	}
	if root.Type() != key.DirLeaf {
		t.Errorf("empty dir root type = %s, want DirLeaf", root.Type())
	}
	if _, err := d.Finish(); !errors.Is(err, errDirFinished) {
		t.Errorf("second Finish: err = %v, want errDirFinished", err)
	}
	if err := d.AddFile("late", blob); !errors.Is(err, errDirFinished) {
		t.Errorf("AddFile after Finish: err = %v, want errDirFinished", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries, more, err := fstree.ListEntries(root, nil, 10, s.Get)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 0 || more {
		t.Errorf("empty dir lists %d entries, more=%v", len(entries), more)
	}
}

func TestDirLargeBuildsIndexAndIsDeterministic(t *testing.T) {
	s := openWriterStore(t)
	const n = 3000
	blobs := make([]key.Key, 3)

	build := func(w *Writer) key.Key {
		t.Helper()
		for i := range blobs {
			k, err := w.PutBytes([]byte(fmt.Sprintf("content %d", i)))
			if err != nil {
				t.Fatal(err)
			}
			blobs[i] = k
		}
		d := w.NewDir()
		for i := range n {
			if err := d.AddFile(fmt.Sprintf("%08d", i), blobs[i%len(blobs)]); err != nil {
				t.Fatalf("AddFile %d: %v", i, err)
			}
		}
		root, err := d.Finish()
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		return root
	}

	w1 := s.NewWriter(context.Background())
	root1 := build(w1)
	st1, err := w1.Close()
	if err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if root1.Type() != key.DirNode {
		t.Errorf("root type = %s, want DirNode for %d entries", root1.Type(), n)
	}
	if st1.ObjectsNew < 3 {
		t.Errorf("ObjectsNew = %d, want several leaves and nodes", st1.ObjectsNew)
	}
	for _, i := range []int{0, 1, 127, 128, 1500, n - 1} {
		name := fmt.Sprintf("%08d", i)
		e := lookupEntry(t, s, root1, name)
		if ck, _ := key.Parse(e.ContentKey); ck != blobs[i%len(blobs)] {
			t.Errorf("%s: ContentKey = %s, want %s", name, ck, blobs[i%len(blobs)])
		}
		if e.Mode != ModeFile {
			t.Errorf("%s: Mode = %#o", name, e.Mode)
		}
	}
	if _, err := fstree.LookupEntry(root1, []byte(fmt.Sprintf("%08d", n)), s.Get); !errors.Is(err, fstree.ErrNotFound) {
		t.Errorf("Lookup past end: err = %v, want ErrNotFound", err)
	}
	entries, more, err := fstree.ListEntries(root1, nil, n+1, s.Get)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != n || more {
		t.Errorf("ListEntries: %d entries, more=%v; want %d, false", len(entries), more, n)
	}

	// Same entries in a second Writer: identical root, nothing new on disk.
	w2 := s.NewWriter(context.Background())
	root2 := build(w2)
	st2, err := w2.Close()
	if err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	if root2 != root1 {
		t.Errorf("rebuilt root = %s, want %s", root2, root1)
	}
	if st2.ObjectsNew != 0 || st2.DiskBytes != 0 || st2.NewLogicalBytes != 0 {
		t.Errorf("rebuild stats = %+v, want nothing new", st2)
	}
	if st2.DedupedBytes != st2.LogicalBytes || st2.LogicalBytes != st1.LogicalBytes {
		t.Errorf("rebuild LogicalBytes/DedupedBytes = %d/%d, want %d/%d", st2.LogicalBytes, st2.DedupedBytes, st1.LogicalBytes, st1.LogicalBytes)
	}
}

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
		{"xattrs key of the wrong type", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: file[:], XattrsKey: file[:]}},
		{"malformed xattrs key", fstree.Entry{Name: []byte("a"), Mode: TypeReg | 0o644, ContentKey: file[:], XattrsKey: []byte{1, 2, 3}}},
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
