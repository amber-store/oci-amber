package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
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
func (s *memSink) Write(p []byte) (int, error)     { return s.buf.Write(p) }
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
				entries = append(entries, tarEntry{name: "etc/caps", data: "caps", xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02\x00\x00\x00\x00"}})
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

func TestParseLayerSkipsGNUSparseWithExtension(t *testing.T) {
	st := openStore(t)
	compact := strings.Repeat("d", 1024)
	archive := buildTar(t, tar.FormatGNU, tarEntry{name: "sparse", data: compact}, tarEntry{name: "after", data: "after\n"})
	// The header says one more sparse extension block follows it; the
	// block is empty. tar-prism keeps it in the recipe and archive/tar reads
	// it from there, so the content region still starts where both expect.
	archive = slices.Concat(archive[:512], make([]byte, 512), archive[512:])
	patchHeader(archive, 0, func(blk []byte) {
		blk[156] = tar.TypeGNUSparse
		octal(blk[386:398], 0, 12)
		octal(blk[398:410], 1024, 12)
		blk[482] = 1
		octal(blk[483:495], 4096, 12)
	})
	l := newMemLayer(t, st, archive)
	got := parse(t, l)
	if len(got) != 2 || got[0].kind != kindSkip || got[0].reason != "sparse file" || got[1].kind != kindFile || got[1].content != l.blobs[1] {
		t.Fatalf("entries = %+v", got)
	}
	if l.opened != 0 {
		t.Fatalf("opened %d content readers, want 0", l.opened)
	}
}

func TestParseLayerSkipsPAXSparse(t *testing.T) {
	st := openStore(t)
	// PAX sparse 1.0: the records name the format and the real size, the
	// map is the first block of the file's data. Go's writer refuses to
	// emit GNU.sparse records, so a regular entry carries the PAX payload
	// and its typeflag is patched to 'x'.
	pax := paxPayload(map[string]string{
		"GNU.sparse.major":    "1",
		"GNU.sparse.minor":    "0",
		"GNU.sparse.name":     "sparse",
		"GNU.sparse.realsize": "4096",
	})
	mapBlock := make([]byte, 512)
	copy(mapBlock, "1\n0\n1024\n")
	archive := buildTar(t, tar.FormatUSTAR,
		tarEntry{name: "PaxHeaders/sparse", data: pax},
		tarEntry{name: "GNUSparseFile.0/sparse", data: string(mapBlock) + strings.Repeat("d", 1024)},
		tarEntry{name: "after", data: "after\n"})
	patchHeader(archive, 0, func(blk []byte) { blk[156] = tar.TypeXHeader })
	l := newMemLayer(t, st, archive)
	if len(l.blobs) != 2 {
		t.Fatalf("tar-prism cut %d blobs, want 2 (the sparse data and \"after\")", len(l.blobs))
	}
	got := parse(t, l)
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %+v", len(got), got)
	}
	if got[0].kind != kindSkip || got[0].reason != "sparse file" || got[0].path != "sparse" {
		t.Fatalf("sparse entry = %+v", got[0])
	}
	if got[1].kind != kindFile || got[1].path != "after" || got[1].content != l.blobs[1] {
		t.Fatalf("entry after the sparse file = %+v", got[1])
	}
	// archive/tar reads the map from the start of the content region, which
	// is served for real; nothing else is read.
	if l.opened != 1 {
		t.Fatalf("opened %d content readers, want 1 (the sparse map)", l.opened)
	}
}

// paxPayload renders PAX records as "<len> <key>=<value>\n" lines, where
// len counts the whole line including itself.
func paxPayload(records map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(records)) {
		rec := " " + k + "=" + records[k] + "\n"
		n := len(rec) + 1
		for len(strconv.Itoa(n))+len(rec) != n {
			n = len(strconv.Itoa(n)) + len(rec)
		}
		b.WriteString(strconv.Itoa(n) + rec)
	}
	return b.String()
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
		{"whiteout without a name", tar.Header{Name: "a/.wh.", Typeflag: tar.TypeReg}, entry{kind: kindSkip, path: "a/.wh.", reason: "whiteout without a name"}},
		{"whiteout of any type", tar.Header{Name: "a/.wh.b", Typeflag: tar.TypeSymlink, Linkname: "x"}, entry{kind: kindWhiteout, path: "a/b"}},
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
