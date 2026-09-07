package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/amber-store/core/chunkers"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// Helpers are prefixed rt (read test) so they cannot collide with helpers the
// Task 3 and Task 4 test files declare in the same package.

func rtStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func rtRandom(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// rtPut stores data as one file through the Task 4 writer and returns its
// key once the objects are durable.
func rtPut(t *testing.T, st *Store, data []byte) key.Key {
	t.Helper()
	w := st.NewWriter(context.Background())
	k, err := w.PutStream(bytes.NewReader(data))
	if err != nil {
		w.Abort()
		t.Fatalf("PutStream: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return k
}

// rtLeaves returns the Blob keys under the file object k in content order.
func rtLeaves(t *testing.T, st *Store, k key.Key) []key.Key {
	t.Helper()
	switch k.Type() {
	case key.Blob:
		return []key.Key{k}
	case key.FileNode:
		data, err := st.Get(k)
		if err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
		children, err := fstree.DecodeFileNode(data)
		if err != nil {
			t.Fatalf("DecodeFileNode %s: %v", k, err)
		}
		var out []key.Key
		for _, c := range children {
			out = append(out, rtLeaves(t, st, c)...)
		}
		return out
	default:
		t.Fatalf("%s is not a file object (type %v)", k, k.Type())
		return nil
	}
}

// rtFetches records the Blob objects a counting Reader fetched. FileNode
// fetches are not counted: only content chunks matter for Skip.
type rtFetches struct {
	blobs int
	byKey map[key.Key]int
}

// rtCountingReader returns a Reader over k that counts every Blob fetch.
func rtCountingReader(st *Store, k key.Key) (*Reader, *rtFetches) {
	r := st.NewReader(k)
	f := &rtFetches{byKey: map[key.Key]int{}}
	get := r.get
	r.get = func(k key.Key) ([]byte, error) {
		if k.Type() == key.Blob {
			f.blobs++
			f.byKey[k]++
		}
		return get(k)
	}
	return r, f
}

// rtThreeEntryDir builds a directory with the entries a (file "alpha"), b
// (directory holding inner.txt) and c (file "charlie") and returns its root
// and the three content keys.
func rtThreeEntryDir(t *testing.T, st *Store) (root, ka, kb, kc key.Key) {
	t.Helper()
	w := st.NewWriter(context.Background())
	var err error
	if ka, err = w.PutBytes([]byte("alpha")); err != nil {
		t.Fatalf("PutBytes a: %v", err)
	}
	if kc, err = w.PutBytes([]byte("charlie")); err != nil {
		t.Fatalf("PutBytes c: %v", err)
	}
	inner, err := w.PutBytes([]byte("inner"))
	if err != nil {
		t.Fatalf("PutBytes inner: %v", err)
	}
	sub := w.NewDir()
	if err := sub.AddFile("inner.txt", inner); err != nil {
		t.Fatalf("AddFile inner.txt: %v", err)
	}
	if kb, err = sub.Finish(); err != nil {
		t.Fatalf("Finish b: %v", err)
	}
	d := w.NewDir()
	if err := d.AddFile("a", ka); err != nil {
		t.Fatalf("AddFile a: %v", err)
	}
	if err := d.AddDir("b", kb); err != nil {
		t.Fatalf("AddDir b: %v", err)
	}
	if err := d.AddFile("c", kc); err != nil {
		t.Fatalf("AddFile c: %v", err)
	}
	if root, err = d.Finish(); err != nil {
		t.Fatalf("Finish root: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return root, ka, kb, kc
}

func rtNames(entries []fstree.Entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = string(e.Name)
	}
	return names
}

func rtEqualNames(t *testing.T, got []fstree.Entry, want ...string) {
	t.Helper()
	names := rtNames(got)
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

func TestReadFile(t *testing.T) {
	st := rtStore(t)
	small := []byte("hello, amber")
	big := rtRandom(t, 3<<20)

	w := st.NewWriter(context.Background())
	kSmall, err := w.PutBytes(small)
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	kEmpty, err := w.PutBytes(nil)
	if err != nil {
		t.Fatalf("PutBytes empty: %v", err)
	}
	kBig, err := w.PutStream(bytes.NewReader(big))
	if err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if kSmall.Type() != key.Blob {
		t.Fatalf("small key type = %v, want Blob", kSmall.Type())
	}
	if kEmpty.Type() != key.Blob || kEmpty.Length() != 0 {
		t.Fatalf("empty key = %v/%d, want Blob/0", kEmpty.Type(), kEmpty.Length())
	}
	if kBig.Type() != key.FileNode {
		t.Fatalf("3 MiB key type = %v, want FileNode", kBig.Type())
	}

	cases := []struct {
		name string
		k    key.Key
		want []byte
	}{
		{"blob", kSmall, small},
		{"fileNode", kBig, big},
		{"empty", kEmpty, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ReadFile(tc.k)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("ReadFile returned %d bytes that differ from the %d bytes written", len(got), len(tc.want))
			}
		})
	}
}

func TestReadFile_NotAFile(t *testing.T) {
	st := rtStore(t)
	root, _, _, _ := rtThreeEntryDir(t, st)
	if _, err := st.ReadFile(root); err == nil {
		t.Fatal("ReadFile of a directory object succeeded, want error")
	}
}

func TestWriteContent(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)
	var buf bytes.Buffer
	if err := st.WriteContent(&buf, k); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("WriteContent wrote %d bytes that differ from the %d bytes written", buf.Len(), len(data))
	}
}

func TestReader_SmallBuffers(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)

	r := st.NewReader(k)
	var got bytes.Buffer
	buf := make([]byte, 7)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("Reader produced %d bytes that differ from the %d bytes written", got.Len(), len(data))
	}
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("Read after EOF = %d, %v; want 0, io.EOF", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReader_Skip(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)
	leaves := rtLeaves(t, st, k)
	if len(leaves) < 3 {
		t.Fatalf("3 MiB file split into %d chunks, want at least 3", len(leaves))
	}

	type skipCase struct {
		name      string
		n         int64
		wantFetch int // Blob fetches performed by Skip itself
	}
	var cases []skipCase
	var off int64
	for i, l := range leaves {
		cases = append(cases, skipCase{fmt.Sprintf("boundary%d", i), off, 0})
		if l.Length() >= 2 {
			cases = append(cases, skipCase{fmt.Sprintf("mid%d", i), off + int64(l.Length())/2, 1})
		}
		off += int64(l.Length())
	}
	if off != int64(len(data)) {
		t.Fatalf("chunk lengths sum to %d, want %d", off, len(data))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fetched := rtCountingReader(st, k)
			if err := r.Skip(tc.n); err != nil {
				t.Fatalf("Skip(%d): %v", tc.n, err)
			}
			if fetched.blobs != tc.wantFetch {
				t.Fatalf("Skip(%d) fetched %d chunks, want %d", tc.n, fetched.blobs, tc.wantFetch)
			}
			tail, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll after Skip(%d): %v", tc.n, err)
			}
			if !bytes.Equal(tail, data[tc.n:]) {
				t.Fatalf("tail after Skip(%d) is %d bytes and differs from data[%d:]", tc.n, len(tail), tc.n)
			}
		})
	}
}

func TestReader_SkipAfterRead(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)
	leaves := rtLeaves(t, st, k)
	if len(leaves) < 3 {
		t.Fatalf("3 MiB file split into %d chunks, want at least 3", len(leaves))
	}
	first := int64(leaves[0].Length())
	if first < 100 {
		t.Fatalf("first chunk is %d bytes, want at least 100", first)
	}

	t.Run("intoLaterChunk", func(t *testing.T) {
		r := st.NewReader(k)
		head := make([]byte, 100)
		if _, err := io.ReadFull(r, head); err != nil {
			t.Fatalf("ReadFull: %v", err)
		}
		if !bytes.Equal(head, data[:100]) {
			t.Fatal("head differs from data[:100]")
		}
		if err := r.Skip(first - 100 + 5); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(tail, data[first+5:]) {
			t.Fatalf("tail is %d bytes and differs from data[%d:]", len(tail), first+5)
		}
	})
	t.Run("toChunkEnd", func(t *testing.T) {
		r := st.NewReader(k)
		head := make([]byte, 10)
		if _, err := io.ReadFull(r, head); err != nil {
			t.Fatalf("ReadFull: %v", err)
		}
		if err := r.Skip(first - 10); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(tail, data[first:]) {
			t.Fatalf("tail is %d bytes and differs from data[%d:]", len(tail), first)
		}
	})
	t.Run("withinChunk", func(t *testing.T) {
		r, fetched := rtCountingReader(st, k)
		head := make([]byte, 10)
		if _, err := io.ReadFull(r, head); err != nil {
			t.Fatalf("ReadFull: %v", err)
		}
		if err := r.Skip(5); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		next := make([]byte, 10)
		if _, err := io.ReadFull(r, next); err != nil {
			t.Fatalf("ReadFull: %v", err)
		}
		if !bytes.Equal(next, data[15:25]) {
			t.Fatal("bytes after Skip(5) differ from data[15:25]")
		}
		if fetched.blobs != 1 {
			t.Fatalf("fetched %d chunks, want 1", fetched.blobs)
		}
	})
}

func TestReader_SkipPastEnd(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)
	size := int64(len(data))
	for _, n := range []int64{size, size + 1, 10 * size} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r := st.NewReader(k)
			if err := r.Skip(n); err != nil {
				t.Fatalf("Skip(%d): %v", n, err)
			}
			buf := make([]byte, 16)
			if got, err := r.Read(buf); got != 0 || err != io.EOF {
				t.Fatalf("Read after Skip(%d) = %d, %v; want 0, io.EOF", n, got, err)
			}
			if err := r.Skip(1); err != nil {
				t.Fatalf("Skip after EOF: %v", err)
			}
			if got, err := r.Read(buf); got != 0 || err != io.EOF {
				t.Fatalf("Read after second Skip = %d, %v; want 0, io.EOF", got, err)
			}
		})
	}
	t.Run("negative", func(t *testing.T) {
		r := st.NewReader(k)
		if err := r.Skip(-1); err == nil {
			t.Fatal("Skip(-1) succeeded, want error")
		}
	})
}

// rtTruncate wraps a Reader's get so the Blob bad comes back one byte short
// of its key's length, while every other key passes through unchanged. It
// proves Skip and fill report an error instead of panicking on a truncated
// or corrupt stored object.
func rtTruncate(r *Reader, bad key.Key) {
	get := r.get
	r.get = func(k key.Key) ([]byte, error) {
		data, err := get(k)
		if err != nil || k != bad {
			return data, err
		}
		return data[:len(data)-1], nil
	}
}

func TestReader_TruncatedObject(t *testing.T) {
	st := rtStore(t)
	data := rtRandom(t, 3<<20)
	k := rtPut(t, st, data)
	leaves := rtLeaves(t, st, k)
	if len(leaves) < 2 {
		t.Fatalf("3 MiB file split into %d chunks, want at least 2", len(leaves))
	}
	bad := leaves[1]
	if bad.Length() < 1 {
		t.Fatalf("leaf 1 is empty, want at least 1 byte to truncate")
	}
	wantErr := fmt.Sprintf("store: object %s is %d bytes, key says %d", bad, bad.Length()-1, bad.Length())

	t.Run("skip", func(t *testing.T) {
		r := st.NewReader(k)
		rtTruncate(r, bad)
		if err := r.Skip(int64(leaves[0].Length()) + 1); err == nil {
			t.Fatal("Skip into a truncated chunk succeeded, want error")
		} else if err.Error() != wantErr {
			t.Fatalf("Skip error = %q, want %q", err.Error(), wantErr)
		}
		// The error is sticky.
		if err := r.Skip(1); err == nil || err.Error() != wantErr {
			t.Fatalf("Skip after the error = %v, want the same sticky error", err)
		}
	})

	t.Run("read", func(t *testing.T) {
		r := st.NewReader(k)
		rtTruncate(r, bad)
		_, err := io.ReadAll(r)
		if err == nil {
			t.Fatal("ReadAll through a truncated chunk succeeded, want error")
		}
		if err.Error() != wantErr {
			t.Fatalf("ReadAll error = %q, want %q", err.Error(), wantErr)
		}
	})
}

func TestReader_MultiLevelTree(t *testing.T) {
	st := rtStore(t)
	w := st.NewWriter(context.Background())
	blob := func(n int) fstree.Object {
		obj, err := fstree.EncodeBlob(rtRandom(t, n))
		if err != nil {
			t.Fatalf("EncodeBlob: %v", err)
		}
		if err := w.Emit(obj); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		return obj
	}
	node := func(children ...key.Key) fstree.Object {
		obj, err := fstree.EncodeFileNode(children)
		if err != nil {
			t.Fatalf("EncodeFileNode: %v", err)
		}
		if err := w.Emit(obj); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		return obj
	}
	// root -> [b0 (empty), n1 -> [b1, b2], n2 -> [b3], b4]
	b0 := blob(0)
	b1 := blob(1000)
	b2 := blob(2000)
	b3 := blob(3000)
	b4 := blob(4000)
	n1 := node(b1.Key, b2.Key)
	n2 := node(b3.Key)
	root := node(b0.Key, n1.Key, n2.Key, b4.Key)
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := bytes.Join([][]byte{b1.Bytes, b2.Bytes, b3.Bytes, b4.Bytes}, nil)
	if root.Key.Length() != uint64(len(want)) {
		t.Fatalf("root length = %d, want %d", root.Key.Length(), len(want))
	}

	t.Run("readAll", func(t *testing.T) {
		got, err := io.ReadAll(st.NewReader(root.Key))
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("Reader output differs from the concatenated leaves")
		}
		got, err = st.ReadFile(root.Key)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("ReadFile output differs from the concatenated leaves")
		}
	})
	t.Run("skipWholeSubtree", func(t *testing.T) {
		r, fetched := rtCountingReader(st, root.Key)
		if err := r.Skip(3000); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		if fetched.blobs != 0 {
			t.Fatalf("Skip(3000) fetched %d chunks (%v), want none", fetched.blobs, fetched.byKey)
		}
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(tail, want[3000:]) {
			t.Fatal("tail after skipping n1 differs from want[3000:]")
		}
		if fetched.byKey[b1.Key] != 0 || fetched.byKey[b2.Key] != 0 {
			t.Fatalf("chunks under the skipped subtree were fetched: %v", fetched.byKey)
		}
	})
	t.Run("skipIntoSubtree", func(t *testing.T) {
		r, fetched := rtCountingReader(st, root.Key)
		if err := r.Skip(1500); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		if fetched.blobs != 1 || fetched.byKey[b2.Key] != 1 {
			t.Fatalf("Skip(1500) fetched %v, want only b2 once", fetched.byKey)
		}
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(tail, want[1500:]) {
			t.Fatal("tail after Skip(1500) differs from want[1500:]")
		}
	})
	t.Run("skipIntoTrailingLeaf", func(t *testing.T) {
		r, fetched := rtCountingReader(st, root.Key)
		if err := r.Skip(6500); err != nil {
			t.Fatalf("Skip: %v", err)
		}
		if fetched.blobs != 1 || fetched.byKey[b4.Key] != 1 {
			t.Fatalf("Skip(6500) fetched %v, want only b4 once", fetched.byKey)
		}
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(tail, want[6500:]) {
			t.Fatal("tail after Skip(6500) differs from want[6500:]")
		}
	})
}

func TestReader_Empty(t *testing.T) {
	st := rtStore(t)
	k := rtPut(t, st, nil)
	buf := make([]byte, 4)
	r := st.NewReader(k)
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("Read of empty file = %d, %v; want 0, io.EOF", n, err)
	}
	r = st.NewReader(k)
	if err := r.Skip(3); err != nil {
		t.Fatalf("Skip on empty file: %v", err)
	}
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("Read after Skip on empty file = %d, %v; want 0, io.EOF", n, err)
	}
}

func TestReader_NotAFile(t *testing.T) {
	st := rtStore(t)
	root, _, _, _ := rtThreeEntryDir(t, st)
	r := st.NewReader(root)
	_, err := r.Read(make([]byte, 4))
	if err == nil || err == io.EOF {
		t.Fatalf("Read of a directory object = %v, want a non-EOF error", err)
	}
	if err := r.Skip(1); err == nil {
		t.Fatal("Skip after a read error succeeded, want the sticky error")
	}
}

func TestLookup(t *testing.T) {
	st := rtStore(t)
	root, ka, kb, kc := rtThreeEntryDir(t, st)
	cases := []struct {
		name string
		mode uint64
		want key.Key
	}{
		{"a", ModeFile, ka},
		{"b", ModeDir, kb},
		{"c", ModeFile, kc},
	}
	for _, tc := range cases {
		e, err := st.Lookup(root, tc.name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", tc.name, err)
		}
		if string(e.Name) != tc.name {
			t.Fatalf("Lookup(%q).Name = %q", tc.name, e.Name)
		}
		if e.Mode != tc.mode {
			t.Fatalf("Lookup(%q).Mode = %#o, want %#o", tc.name, e.Mode, tc.mode)
		}
		if e.UID != 0 || e.GID != 0 || e.Mtime != 0 {
			t.Fatalf("Lookup(%q) uid/gid/mtime = %d/%d/%d, want zero", tc.name, e.UID, e.GID, e.Mtime)
		}
		if !bytes.Equal(e.ContentKey, tc.want[:]) {
			t.Fatalf("Lookup(%q).ContentKey = %x, want %s", tc.name, e.ContentKey, tc.want)
		}
	}

	inner, err := st.Lookup(kb, "inner.txt")
	if err != nil {
		t.Fatalf("Lookup in subdirectory: %v", err)
	}
	if inner.Mode != ModeFile {
		t.Fatalf("inner.txt mode = %#o, want %#o", inner.Mode, ModeFile)
	}

	for _, name := range []string{"zzz", "", "aa", "B"} {
		_, err := st.Lookup(root, name)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Lookup(%q) error = %v, want ErrNotFound", name, err)
		}
	}

	if _, err := st.Lookup(ka, "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup in a file object error = %v, want a non-ErrNotFound error", err)
	}
}

func TestLookupKey(t *testing.T) {
	st := rtStore(t)
	root, ka, kb, kc := rtThreeEntryDir(t, st)
	for name, want := range map[string]key.Key{"a": ka, "b": kb, "c": kc} {
		got, err := st.LookupKey(root, name)
		if err != nil {
			t.Fatalf("LookupKey(%q): %v", name, err)
		}
		if got != want {
			t.Fatalf("LookupKey(%q) = %s, want %s", name, got, want)
		}
	}
	if _, err := st.LookupKey(root, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupKey(missing) error = %v, want ErrNotFound", err)
	}

	// An entry without a content key (a symlink) is an error that is not
	// ErrNotFound. Such entries never come from the Dir builder, so build one
	// with fstree directly through the writer's Emit.
	w := st.NewWriter(context.Background())
	db := fstree.NewDirBuilder(chunkers.NewItemChunker(ItemBits))
	link := fstree.Entry{Name: []byte("link"), Mode: 0o120777, LinkTarget: []byte("target")}
	if err := db.AddEntry(w.Emit, link); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	linkRoot, err := db.Finish(w.Emit)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = st.LookupKey(linkRoot, "link")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupKey of a symlink entry error = %v, want a non-ErrNotFound error", err)
	}
}

func TestListDir(t *testing.T) {
	st := rtStore(t)
	root, ka, kb, kc := rtThreeEntryDir(t, st)

	entries, more, err := st.ListDir(root, "", 10)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if more {
		t.Fatal("ListDir of 3 entries with limit 10 reports more")
	}
	rtEqualNames(t, entries, "a", "b", "c")
	wantKeys := []key.Key{ka, kb, kc}
	wantModes := []uint64{ModeFile, ModeDir, ModeFile}
	for i, e := range entries {
		if !bytes.Equal(e.ContentKey, wantKeys[i][:]) {
			t.Fatalf("entry %q content key = %x, want %s", e.Name, e.ContentKey, wantKeys[i])
		}
		if e.Mode != wantModes[i] {
			t.Fatalf("entry %q mode = %#o, want %#o", e.Name, e.Mode, wantModes[i])
		}
	}

	entries, more, err = st.ListDir(root, "a", 10)
	if err != nil {
		t.Fatalf("ListDir after a: %v", err)
	}
	if more {
		t.Fatal("ListDir after a reports more")
	}
	rtEqualNames(t, entries, "b", "c")

	entries, more, err = st.ListDir(root, "bb", 10)
	if err != nil {
		t.Fatalf("ListDir after bb: %v", err)
	}
	if more {
		t.Fatal("ListDir after bb reports more")
	}
	rtEqualNames(t, entries, "c")

	entries, more, err = st.ListDir(root, "c", 10)
	if err != nil {
		t.Fatalf("ListDir after c: %v", err)
	}
	if len(entries) != 0 || more {
		t.Fatalf("ListDir after the last entry = %v, more=%v; want empty, false", rtNames(entries), more)
	}

	if _, _, err := st.ListDir(ka, "", 10); err == nil {
		t.Fatal("ListDir of a file object succeeded, want error")
	}
}

func TestListDir_Pagination(t *testing.T) {
	st := rtStore(t)
	root, _, _, _ := rtThreeEntryDir(t, st)

	page, more, err := st.ListDir(root, "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !more {
		t.Fatal("page 1 of 3 entries with limit 2 reports no more")
	}
	rtEqualNames(t, page, "a", "b")

	page, more, err = st.ListDir(root, string(page[len(page)-1].Name), 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if more {
		t.Fatal("page 2 reports more")
	}
	rtEqualNames(t, page, "c")

	page, more, err = st.ListDir(root, "", 3)
	if err != nil {
		t.Fatalf("limit 3: %v", err)
	}
	if more {
		t.Fatal("limit equal to the entry count reports more")
	}
	rtEqualNames(t, page, "a", "b", "c")

	for _, limit := range []int{0, -1} {
		page, more, err = st.ListDir(root, "", limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if more {
			t.Fatalf("limit %d reports more", limit)
		}
		rtEqualNames(t, page, "a", "b", "c")
	}
}

func TestListDir_LargeDirectory(t *testing.T) {
	st := rtStore(t)
	const n = 1500
	w := st.NewWriter(context.Background())
	content, err := w.PutBytes([]byte("x"))
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	d := w.NewDir()
	want := make([]string, n)
	for i := range n {
		want[i] = fmt.Sprintf("e%04d", i)
		if err := d.AddFile(want[i], content); err != nil {
			t.Fatalf("AddFile %s: %v", want[i], err)
		}
	}
	root, err := d.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if root.Type() != key.DirNode {
		t.Fatalf("%d-entry directory root is a %v, want DirNode", n, root.Type())
	}

	all, more, err := st.ListDir(root, "", 0)
	if err != nil {
		t.Fatalf("ListDir unlimited: %v", err)
	}
	if more {
		t.Fatal("unlimited ListDir reports more")
	}
	rtEqualNames(t, all, want...)

	var got []string
	after := ""
	pages := 0
	for {
		page, more, err := st.ListDir(root, after, 7)
		if err != nil {
			t.Fatalf("page after %q: %v", after, err)
		}
		if len(page) > 7 {
			t.Fatalf("page after %q has %d entries, limit is 7", after, len(page))
		}
		got = append(got, rtNames(page)...)
		pages++
		if !more {
			break
		}
		if len(page) != 7 {
			t.Fatalf("more=true with a short page of %d after %q", len(page), after)
		}
		after = string(page[len(page)-1].Name)
	}
	if wantPages := (n + 6) / 7; pages != wantPages {
		t.Fatalf("paginated in %d pages, want %d", pages, wantPages)
	}
	if len(got) != n {
		t.Fatalf("pagination yielded %d names, want %d", len(got), n)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paginated name %d = %q, want %q", i, got[i], want[i])
		}
	}

	rest, more, err := st.ListDir(root, "e1400", 0)
	if err != nil {
		t.Fatalf("ListDir after e1400: %v", err)
	}
	if more {
		t.Fatal("unlimited ListDir after e1400 reports more")
	}
	rtEqualNames(t, rest, want[1401:]...)

	if _, err := st.Lookup(root, "e1234"); err != nil {
		t.Fatalf("Lookup in a multi-level directory: %v", err)
	}
	if _, err := st.Lookup(root, "e1500"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup(e1500) error = %v, want ErrNotFound", err)
	}
	k, err := st.LookupKey(root, "e0000")
	if err != nil {
		t.Fatalf("LookupKey(e0000): %v", err)
	}
	if k != content {
		t.Fatalf("LookupKey(e0000) = %s, want %s", k, content)
	}
}
