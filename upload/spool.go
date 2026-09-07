// Package upload holds in-progress blob uploads. A Session buffers the bytes
// of one upload in memory and spills them to a file in the work directory
// once they exceed a threshold, keeping a running sha256 over everything
// received. A Manager owns the sessions of one registry process and expires
// the ones that go idle. A Spool is the snapshot a finished upload hands to
// the blob store.
package upload

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/amber-store/oci-amber/oci"
)

// ReaderAtSeeker is the view of an upload that blob finalization consumes: a
// seekable stream that can also be read at arbitrary offsets, so zrecipe
// can evaluate candidate engines in parallel over one open spool.
type ReaderAtSeeker interface {
	io.ReadSeeker
	io.ReaderAt
}

var errSpoolRemoved = errors.New("upload: spool removed")

// Spool is a snapshot of an upload's bytes, backed by a byte slice, by a
// file under the manager's directory, or by a window of a caller-owned
// io.ReaderAt. Its size and digest are fixed when the snapshot is taken;
// bytes appended to the session afterwards are not visible through it.
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

// NewMemorySpool returns a Spool over data. The slice is not copied.
func NewMemorySpool(data []byte) *Spool {
	return &Spool{size: int64(len(data)), digest: oci.DigestOfBytes(data), mem: data}
}

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

// Size is the number of bytes in the spool.
func (sp *Spool) Size() int64 { return sp.size }

// Digest is the sha256 digest of the spool's bytes.
func (sp *Spool) Digest() oci.Digest { return sp.digest }

// Open returns a reader positioned at offset 0. For a memory spool it is a
// *bytes.Reader; for a file spool it is backed by a freshly opened *os.File
// and implements io.Closer, which the caller must close; for a section
// spool it is an *io.SectionReader over the caller's reader, with no Close.
// Open may be called several times and the readers' ReadAt may be used
// concurrently.
//
// A file spool's reader stays valid even if its backing path is unlinked
// after Open returns — by Session.close (a cancel, a finalize, or a sweep)
// or by a later Spool.Remove on the same path. This registry targets Unix,
// where an open file descriptor keeps the underlying inode and its data
// readable until every descriptor referencing it is closed, regardless of
// whether the directory entry still exists; the reader is unaffected and
// the space is only reclaimed once this reader (and any other open
// descriptor for the same file) is closed.
func (sp *Spool) Open() (ReaderAtSeeker, error) {
	if sp.removed {
		return nil, errSpoolRemoved
	}
	if sp.touch != nil {
		sp.touch()
	}
	if sp.ra != nil {
		return io.NewSectionReader(sp.ra, sp.off, sp.size), nil
	}
	if sp.path == "" {
		return bytes.NewReader(sp.mem), nil
	}
	f, err := os.Open(sp.path)
	if err != nil {
		return nil, err
	}
	return &fileReader{SectionReader: io.NewSectionReader(f, 0, sp.size), f: f}, nil
}

// Remove deletes the backing file, if any, and drops the reference to the
// memory buffer. It is idempotent and succeeds when the file is already
// gone. Open fails after Remove.
//
// For a file spool, the backing file is the session's own file (a file
// spool shares its path with the session that produced it), so Remove
// deletes it outright. Call Remove only once the session that produced the
// spool will not be reused: the blob store calls it only after the blob has
// been stored successfully, never after a failed finalize, since a session
// kept alive for a retry still needs those bytes. Any reader already
// obtained from Open before Remove is unaffected — see Open.
func (sp *Spool) Remove() error {
	sp.removed = true
	sp.mem = nil
	sp.ra = nil
	if sp.path == "" {
		return nil
	}
	if err := os.Remove(sp.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// fileReader restricts an open file to the spool's size so the reader is a
// true snapshot even if the session's file grows afterwards. It holds its
// own *os.File, opened fresh in Spool.Open, so it keeps working under
// Unix's unlink-while-open semantics even after the backing path has been
// removed from the directory (see Open).
type fileReader struct {
	*io.SectionReader
	f *os.File
}

// Close closes the underlying file.
func (r *fileReader) Close() error { return r.f.Close() }
