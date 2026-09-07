package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// readFilePrealloc caps the buffer ReadFile allocates up front from a key's
// length field, which is not trusted until the objects behind it were read.
const readFilePrealloc = 16 << 20

// ReadFile returns the whole content of the regular-file object k (a Blob or
// a FileNode) as one slice. It is meant for small files such as meta.json,
// comp.json, recipe.json and manifest bodies; use NewReader or WriteContent
// for anything large. A key of any other type is an error.
func (s *Store) ReadFile(k key.Key) ([]byte, error) {
	var data []byte
	switch k.Type() {
	case key.Blob:
		b, err := s.Get(k)
		if err != nil {
			return nil, fmt.Errorf("store: reading %s: %w", k, err)
		}
		data = b
	case key.FileNode:
		buf := bytes.NewBuffer(make([]byte, 0, int(min(k.Length(), readFilePrealloc))))
		if err := fstree.WriteContent(buf, k, s.Get); err != nil {
			return nil, fmt.Errorf("store: reading %s: %w", k, err)
		}
		data = buf.Bytes()
	default:
		return nil, fmt.Errorf("store: %s is not a file-content object (type %v)", k, k.Type())
	}
	if uint64(len(data)) != k.Length() {
		return nil, fmt.Errorf("store: %s: read %d bytes, key length is %d", k, len(data), k.Length())
	}
	return data, nil
}

// WriteContent streams the content of the regular-file object k to w in
// order, one chunk at a time, without buffering the whole file.
func (s *Store) WriteContent(w io.Writer, k key.Key) error {
	return fstree.WriteContent(w, k, s.Get)
}

// Reader streams the content of a regular-file object (a Blob or a FileNode
// tree) chunk by chunk. It keeps one slice of pending child keys per tree
// level plus the current chunk, so memory is one chunk plus one index node
// per level regardless of file size. A Reader is not safe for concurrent
// use. Errors, including io.EOF, are sticky.
type Reader struct {
	get   func(key.Key) ([]byte, error)
	stack [][]key.Key // pending child keys per level; the last slice is the deepest
	cur   []byte      // unread tail of the current chunk
	err   error       // first error seen, io.EOF once the tree is exhausted
}

var _ io.ReadCloser = (*Reader)(nil)

// NewReader returns a Reader positioned at the start of the file object k.
// Nothing is fetched until the first Read or Skip.
func (s *Store) NewReader(k key.Key) *Reader {
	return &Reader{get: s.Get, stack: [][]key.Key{{k}}}
}

// next pops the next pending key in content order, or reports false when the
// whole tree has been consumed.
func (r *Reader) next() (key.Key, bool) {
	for len(r.stack) > 0 {
		top := r.stack[len(r.stack)-1]
		if len(top) == 0 {
			r.stack = r.stack[:len(r.stack)-1]
			continue
		}
		r.stack[len(r.stack)-1] = top[1:]
		return top[0], true
	}
	return key.Key{}, false
}

// descend fetches the FileNode k and pushes its children as a new level.
func (r *Reader) descend(k key.Key) error {
	data, err := r.get(k)
	if err != nil {
		return fmt.Errorf("store: reading %s: %w", k, err)
	}
	children, err := fstree.DecodeFileNode(data)
	if err != nil {
		return err
	}
	r.stack = append(r.stack, children)
	return nil
}

// fill makes cur non-empty by descending to and fetching the next Blob with
// content. It returns io.EOF when nothing is left.
func (r *Reader) fill() error {
	for len(r.cur) == 0 {
		k, ok := r.next()
		if !ok {
			return io.EOF
		}
		switch k.Type() {
		case key.Blob:
			data, err := r.get(k)
			if err != nil {
				return fmt.Errorf("store: reading %s: %w", k, err)
			}
			if uint64(len(data)) != k.Length() {
				return fmt.Errorf("store: object %s is %d bytes, key says %d", k, len(data), k.Length())
			}
			r.cur = data
		case key.FileNode:
			if err := r.descend(k); err != nil {
				return err
			}
		default:
			return fmt.Errorf("store: %s is not a file-content object (type %v)", k, k.Type())
		}
	}
	return nil
}

// Read implements io.Reader. It never returns more than the remainder of the
// current chunk per call.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.fill(); err != nil {
		r.err = err
		return 0, err
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// Skip advances the reader by n bytes without returning them. Every chunk
// and every whole subtree that ends at or before the new position is passed
// over using its key.Length() alone and is never fetched; only the chunk
// that contains the new position is read. Skipping to or past the end is
// not an error: the next Read reports io.EOF. A negative n is an error.
func (r *Reader) Skip(n int64) error {
	if n < 0 {
		return fmt.Errorf("store: Skip: negative count %d", n)
	}
	if r.err != nil {
		if errors.Is(r.err, io.EOF) {
			return nil
		}
		return r.err
	}
	if int64(len(r.cur)) >= n {
		r.cur = r.cur[n:]
		return nil
	}
	n -= int64(len(r.cur))
	r.cur = nil
	for n > 0 {
		k, ok := r.next()
		if !ok {
			return nil // past the end; Read reports io.EOF
		}
		if k.Length() <= uint64(n) {
			n -= int64(k.Length())
			continue
		}
		switch k.Type() {
		case key.Blob:
			data, err := r.get(k)
			if err != nil {
				r.err = fmt.Errorf("store: reading %s: %w", k, err)
				return r.err
			}
			if uint64(len(data)) != k.Length() {
				r.err = fmt.Errorf("store: object %s is %d bytes, key says %d", k, len(data), k.Length())
				return r.err
			}
			r.cur = data[n:]
			n = 0
		case key.FileNode:
			if err := r.descend(k); err != nil {
				r.err = err
				return err
			}
		default:
			r.err = fmt.Errorf("store: %s is not a file-content object (type %v)", k, k.Type())
			return r.err
		}
	}
	return nil
}

// Close implements io.Closer. Readers hold no resources, so it is a no-op
// that exists so a Reader can be handed out as an io.ReadCloser.
func (r *Reader) Close() error {
	return nil
}

// Lookup returns the entry called name in the directory object dir. Names
// are compared bytewise. A missing name wraps ErrNotFound; a dir that is not
// a directory object is a plain error.
func (s *Store) Lookup(dir key.Key, name string) (fstree.Entry, error) {
	e, err := fstree.LookupEntry(dir, []byte(name), s.Get)
	if err != nil {
		if errors.Is(err, fstree.ErrNotFound) {
			return fstree.Entry{}, fmt.Errorf("store: entry %q in %s: %w", name, dir, ErrNotFound)
		}
		return fstree.Entry{}, err
	}
	return e, nil
}

// LookupKey returns the content key of the entry called name in dir: the
// file object for a regular file or the directory object for a directory.
// A missing name wraps ErrNotFound; an entry without a content key (a
// symlink or special file) is an error that does not wrap ErrNotFound.
func (s *Store) LookupKey(dir key.Key, name string) (key.Key, error) {
	e, err := s.Lookup(dir, name)
	if err != nil {
		return key.Key{}, err
	}
	k, err := key.Parse(e.ContentKey)
	if err != nil {
		return key.Key{}, fmt.Errorf("store: entry %q in %s has no content key: %w", name, dir, err)
	}
	return k, nil
}

// listPage is the page size ListDir uses internally when asked for every
// entry, so a huge directory is still walked one page of objects at a time.
const listPage = 1024

// ListDir returns entries of the directory object dir whose names sort
// strictly after `after` (the empty string starts at the beginning), in name
// order. With limit > 0 at most limit entries are returned and more reports
// whether further entries exist; when more is true the page is full, so the
// last entry's name is the cursor for the next call. With limit <= 0 every
// remaining entry is returned and more is false.
func (s *Store) ListDir(dir key.Key, after string, limit int) (entries []fstree.Entry, more bool, err error) {
	if limit > 0 {
		return fstree.ListEntries(dir, []byte(after), limit, s.Get)
	}
	cursor := []byte(after)
	for {
		page, pageMore, err := fstree.ListEntries(dir, cursor, listPage, s.Get)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, page...)
		if !pageMore {
			return entries, false, nil
		}
		cursor = page[len(page)-1].Name
	}
}
