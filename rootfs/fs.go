package rootfs

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/tarexport"

	"github.com/amber-store/oci-amber/store"
)

// Errors of FS, each wrapped with the path in question.
var (
	ErrNotFound = errors.New("rootfs: no such path")
	ErrNotDir   = errors.New("rootfs: not a directory")
	ErrNotFile  = errors.New("rootfs: not a regular file")
	ErrLoop     = errors.New("rootfs: too many levels of symbolic links")
)

// Entry is one entry of a stored tree.
type Entry struct {
	Name     string
	Mode     uint64 // type bits and permissions
	UID, GID uint64
	Mtime    int64     // ns since the Unix epoch
	Size     int64     // regular files: the content length
	Target   string    // symlinks
	Rdev     [2]uint64 // devices: major, minor
	Content  key.Key   // regular files and directories: the payload key
}

// Type returns the entry's type bits.
func (e Entry) Type() uint64 { return e.Mode & store.TypeMask }

// IsDir reports a directory.
func (e Entry) IsDir() bool { return e.Type() == store.TypeDir }

// IsRegular reports a regular file.
func (e Entry) IsRegular() bool { return e.Type() == store.TypeReg }

// TypeName names the type: file, dir, symlink, char, block, fifo or socket.
func (e Entry) TypeName() string {
	switch e.Type() {
	case store.TypeReg:
		return "file"
	case store.TypeDir:
		return "dir"
	case store.TypeLink:
		return "symlink"
	case store.TypeChar:
		return "char"
	case store.TypeBlock:
		return "block"
	case store.TypeFIFO:
		return "fifo"
	case store.TypeSocket:
		return "socket"
	}
	return "unknown"
}

// FS reads a stored rootfs tree.
type FS struct {
	st   *store.Store
	root key.Key
}

// NewFS returns an FS over the directory object root.
func NewFS(st *store.Store, root key.Key) *FS { return &FS{st: st, root: root} }

// Root returns the tree's root key.
func (f *FS) Root() key.Key { return f.root }

// Clean normalizes a request path with URL semantics: rooted cleaning, so
// ".." never leaves the root, repeated and trailing slashes vanish, and ""
// or "/" is the root, returned as "".
func Clean(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// Stat resolves p, following symlinks in every component, and returns the
// entry reached. The root is a directory entry with an empty name.
func (f *FS) Stat(p string) (Entry, error) { return f.resolve(p) }

// List returns the entries of the directory at p whose names sort after
// `after` in name order, at most limit of them when limit > 0, and whether
// more follow.
func (f *FS) List(p, after string, limit int) ([]Entry, bool, error) {
	dir, err := f.resolve(p)
	if err != nil {
		return nil, false, err
	}
	return f.ListDir(dir, after, limit)
}

// ListDir is List for an already resolved directory entry.
func (f *FS) ListDir(dir Entry, after string, limit int) ([]Entry, bool, error) {
	if !dir.IsDir() {
		return nil, false, fmt.Errorf("%w: %s", ErrNotDir, dir.Name)
	}
	raw, more, err := f.st.ListDir(dir.Content, after, limit)
	if err != nil {
		return nil, false, fmt.Errorf("rootfs: listing %s: %w", dir.Name, err)
	}
	entries := make([]Entry, 0, len(raw))
	for _, r := range raw {
		e, err := fromFstree(r)
		if err != nil {
			return nil, false, fmt.Errorf("rootfs: %s: %w", dir.Name, err)
		}
		entries = append(entries, e)
	}
	return entries, more, nil
}

// Open returns the regular file at p and a reader over its content.
func (f *FS) Open(p string) (Entry, *store.Reader, error) {
	e, err := f.resolve(p)
	if err != nil {
		return Entry{}, nil, err
	}
	r, err := f.Content(e)
	if err != nil {
		return Entry{}, nil, fmt.Errorf("%w: %s", ErrNotFile, Clean(p))
	}
	return e, r, nil
}

// Content returns a reader over the content of the regular file e.
func (f *FS) Content(e Entry) (*store.Reader, error) {
	if !e.IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrNotFile, e.Name)
	}
	return f.st.NewReader(e.Content), nil
}

// WriteTar streams the subtree under the directory at p to w as a PAX tar
// with names relative to that directory; the directory itself is not an
// entry, like `tar -C dir .`.
func (f *FS) WriteTar(w io.Writer, p string) error {
	dir, err := f.resolve(p)
	if err != nil {
		return err
	}
	return f.WriteTarDir(w, dir)
}

// WriteTarDir is WriteTar for an already resolved directory entry.
func (f *FS) WriteTarDir(w io.Writer, dir Entry) error {
	if !dir.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotDir, dir.Name)
	}
	return tarexport.Write(w, dir.Content, f.st.Get)
}

// rootEntry is the synthetic entry of the tree's root, which carries no
// metadata of its own.
func (f *FS) rootEntry() Entry { return Entry{Mode: store.TypeDir | 0o755, Content: f.root} }

// resolve walks Clean(p) from the root like a kernel scoped to the tree:
// symlinks are followed in every component (an absolute target restarts at
// the root, ".." pops and never rises above the root), more than
// maxSymlinkHops links is ErrLoop, a missing component is ErrNotFound and
// a non-directory with components left after it is ErrNotDir. A trailing
// slash on a symlink target is ignored, as Clean ignores one on a request
// path, where a kernel would insist on a directory.
func (f *FS) resolve(p string) (Entry, error) {
	p = Clean(p)
	comps := splitPath(p)
	stack := []Entry{f.rootEntry()}
	hops := 0
	for len(comps) > 0 {
		c := comps[0]
		comps = comps[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		dir := stack[len(stack)-1]
		raw, err := f.st.Lookup(dir.Content, c)
		if errors.Is(err, store.ErrNotFound) {
			return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		if err != nil {
			return Entry{}, fmt.Errorf("rootfs: %s: %w", p, err)
		}
		e, err := fromFstree(raw)
		if err != nil {
			return Entry{}, fmt.Errorf("rootfs: %s: %w", p, err)
		}
		switch e.Type() {
		case store.TypeLink:
			hops++
			if hops > maxSymlinkHops {
				return Entry{}, fmt.Errorf("%w: %s", ErrLoop, p)
			}
			if strings.HasPrefix(e.Target, "/") {
				stack = stack[:1]
			}
			comps = append(strings.Split(strings.TrimPrefix(e.Target, "/"), "/"), comps...)
		case store.TypeDir:
			stack = append(stack, e)
		default:
			if hasComponent(comps) {
				return Entry{}, fmt.Errorf("%w: %s", ErrNotDir, p)
			}
			return e, nil
		}
	}
	return stack[len(stack)-1], nil
}

// hasComponent reports whether comps still names something to descend
// into: anything but "" and ".".
func hasComponent(comps []string) bool {
	for _, c := range comps {
		if c != "" && c != "." {
			return true
		}
	}
	return false
}

// fromFstree decodes a stored directory entry.
func fromFstree(r fstree.Entry) (Entry, error) {
	e := Entry{Name: string(r.Name), Mode: r.Mode, UID: r.UID, GID: r.GID, Mtime: r.Mtime}
	switch e.Type() {
	case store.TypeReg, store.TypeDir:
		k, err := key.Parse(r.ContentKey)
		if err != nil {
			return Entry{}, fmt.Errorf("%s: content key: %w", r.Name, err)
		}
		e.Content = k
		if e.Type() == store.TypeReg {
			e.Size = int64(k.Length())
		}
	case store.TypeLink:
		e.Target = string(r.LinkTarget)
	case store.TypeChar, store.TypeBlock:
		if len(r.Rdev) == 2 {
			e.Rdev = [2]uint64{r.Rdev[0], r.Rdev[1]}
		}
	}
	return e, nil
}
