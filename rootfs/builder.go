package rootfs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// MaxSkipped is how many skipped entries a Result lists; SkippedCount keeps
// counting past it.
const MaxSkipped = 100

// Skip is one archive entry that was left out of the tree.
type Skip struct {
	Layer  oci.Digest `json:"layer"`
	Path   string     `json:"path"`
	Reason string     `json:"reason"`
}

// Result is a written tree.
type Result struct {
	Root         key.Key
	Entries      int // entries in the tree, the root excluded
	Skipped      []Skip
	SkippedCount int
}

// Builder merges layers into a root filesystem.
type Builder struct {
	t            *tree
	skipped      []Skip
	skippedCount int
}

// New returns a Builder with an empty root.
func New() *Builder { return &Builder{t: newTree()} }

// Apply parses layer and merges it over the layers applied before: its
// whiteouts remove what lower layers put there, then its entries are placed
// in archive order, so a whiteout never touches its own layer. Entries
// that cannot be represented are recorded as skips. A *LayerError means the
// archive could not be parsed and nothing of it was applied; any other
// error is a store failure or the context's.
func (b *Builder) Apply(ctx context.Context, digest oci.Digest, layer Layer) error {
	entries, err := parseLayer(ctx, layer)
	if err != nil {
		var se *storeError
		switch {
		case errors.As(err, &se):
			return fmt.Errorf("rootfs: layer %s: %w", digest, se.err)
		case ctx.Err() != nil:
			return err
		}
		return &LayerError{Layer: digest, Err: err}
	}
	for _, e := range entries {
		switch e.kind {
		case kindWhiteout:
			b.record(digest, e, b.t.whiteout(e))
		case kindOpaque:
			b.record(digest, e, b.t.opaque(e))
		}
	}
	for _, e := range entries {
		switch e.kind {
		case kindWhiteout, kindOpaque:
		case kindSkip:
			b.record(digest, e, &skipError{e.reason})
		case kindHardlink:
			b.record(digest, e, b.t.link(e))
		default:
			b.record(digest, e, b.t.put(e))
		}
	}
	return nil
}

// record notes an entry the tree refused.
func (b *Builder) record(digest oci.Digest, e entry, err error) {
	if err == nil {
		return
	}
	b.skippedCount++
	if len(b.skipped) < MaxSkipped {
		b.skipped = append(b.skipped, Skip{Layer: digest, Path: e.path, Reason: err.Error()})
	}
}

// Write emits the tree through w, bottom-up with every directory's entries
// in bytewise name order, and returns the root key. Regular files reference
// the content keys the layers hold; only directory objects and spilled
// xattr sets are new.
func (b *Builder) Write(w *store.Writer) (Result, error) {
	root, n, err := emitDir(w, b.t.root)
	if err != nil {
		return Result{}, err
	}
	return Result{Root: root, Entries: n, Skipped: b.skipped, SkippedCount: b.skippedCount}, nil
}

func emitDir(w *store.Writer, dir *node) (key.Key, int, error) {
	d := w.NewDir()
	count := 0
	for _, name := range slices.Sorted(maps.Keys(dir.children)) {
		c := dir.children[name]
		e := fstree.Entry{Name: []byte(name), Mode: c.mode, UID: c.uid, GID: c.gid, Mtime: c.mtime}
		switch c.typ() {
		case store.TypeDir:
			k, n, err := emitDir(w, c)
			if err != nil {
				return key.Key{}, 0, err
			}
			e.ContentKey = k[:]
			count += n
		case store.TypeReg:
			e.ContentKey = c.content[:]
		case store.TypeLink:
			e.LinkTarget = []byte(c.link)
		case store.TypeChar, store.TypeBlock:
			e.Rdev = []uint64{c.rdev[0], c.rdev[1]}
		}
		if len(c.xattrs) > 0 {
			inline, spilled, err := w.PutXattrs(c.xattrs)
			if err != nil {
				return key.Key{}, 0, fmt.Errorf("rootfs: xattrs of %s: %w", name, err)
			}
			e.XattrsIn = inline
			if spilled != (key.Key{}) {
				e.XattrsKey = spilled[:]
			}
		}
		if err := d.AddEntry(e); err != nil {
			return key.Key{}, 0, fmt.Errorf("rootfs: %w", err)
		}
		count++
	}
	k, err := d.Finish()
	if err != nil {
		return key.Key{}, 0, fmt.Errorf("rootfs: finishing directory: %w", err)
	}
	return k, count, nil
}
