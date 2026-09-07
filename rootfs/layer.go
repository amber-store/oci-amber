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
			return e
		}
		name := strings.TrimPrefix(base, whiteoutPrefix)
		if name == "" {
			return e.skip("whiteout without a name")
		}
		e.kind, e.path = kindWhiteout, joinPath(dir, name)
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
