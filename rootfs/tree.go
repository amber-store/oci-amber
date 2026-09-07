package rootfs

import (
	"fmt"
	"path"
	"strings"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

// maxSymlinkHops bounds one path resolution, like the kernel's 40.
const maxSymlinkHops = 40

// node is one entry of the merged tree.
type node struct {
	mode     uint64 // type bits and permissions
	uid, gid uint64
	mtime    int64
	content  key.Key   // TypeReg
	link     string    // TypeLink
	rdev     [2]uint64 // TypeChar, TypeBlock
	xattrs   map[string][]byte
	children map[string]*node // TypeDir
	implicit bool             // a directory created for a child, no header seen
}

func newDir() *node {
	return &node{mode: store.TypeDir | 0o755, children: map[string]*node{}, implicit: true}
}

func (n *node) typ() uint64 { return n.mode & store.TypeMask }

// skipError says why an entry was left out of the tree.
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

var errLoop = &skipError{"symlink loop"}

// tree is the merged filesystem being built.
type tree struct{ root *node }

func newTree() *tree { return &tree{root: newDir()} }

// splitPath returns the components of a cleaned path; the root has none.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// splitParent splits a non-root cleaned path into its parent and last
// component.
func splitParent(p string) (dir, name string) {
	dir, name = path.Dir(p), path.Base(p)
	if dir == "." {
		dir = ""
	}
	return dir, name
}

// resolveDir walks comps from the root and returns the stack of directories
// ending at the one reached. Symlinks are followed: an absolute target
// restarts at the root, ".." pops (never above the root), more than
// maxSymlinkHops links is errLoop. With create set, a missing component
// becomes an implicit directory and a component that is neither a
// directory nor a symlink is replaced by one (the later entry wins);
// without it, such a component ends the walk with a nil stack.
func (t *tree) resolveDir(comps []string, create bool) ([]*node, error) {
	stack := []*node{t.root}
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
		child := dir.children[c]
		switch {
		case child == nil:
			if !create {
				return nil, nil
			}
			child = newDir()
			dir.children[c] = child
		case child.typ() == store.TypeLink:
			hops++
			if hops > maxSymlinkHops {
				return nil, errLoop
			}
			target := child.link
			if strings.HasPrefix(target, "/") {
				stack = []*node{t.root}
			}
			comps = append(strings.Split(strings.TrimPrefix(target, "/"), "/"), comps...)
			continue
		case child.typ() != store.TypeDir:
			if !create {
				return nil, nil
			}
			child = newDir()
			dir.children[c] = child
		}
		stack = append(stack, child)
	}
	return stack, nil
}

// parentOf resolves the directory that holds p's last component, creating
// implicit directories on the way, and returns it with that name.
func (t *tree) parentOf(p string) (*node, string, error) {
	dir, name := splitParent(p)
	stack, err := t.resolveDir(splitPath(dir), true)
	if err != nil {
		return nil, "", err
	}
	return stack[len(stack)-1], name, nil
}

// lookup returns the node at p, following symlinks in parent components but
// not at the last one, or nil when there is none.
func (t *tree) lookup(p string) (*node, error) {
	if p == "" {
		return t.root, nil
	}
	dir, name := splitParent(p)
	stack, err := t.resolveDir(splitPath(dir), false)
	if err != nil || stack == nil {
		return nil, err
	}
	return stack[len(stack)-1].children[name], nil
}

// put places a file, directory, symlink, device or fifo at its path. A
// directory over a directory keeps the children and takes the metadata;
// anything else replaces the old subtree. A directory entry for the root is
// ignored, any other root entry is a skip.
func (t *tree) put(e entry) error {
	if e.path == "" {
		if e.kind == kindDir {
			return nil
		}
		return &skipError{"root is not a directory"}
	}
	parent, name, err := t.parentOf(e.path)
	if err != nil {
		return err
	}
	n := &node{mode: e.mode, uid: e.uid, gid: e.gid, mtime: e.mtime, xattrs: e.xattrs}
	switch e.kind {
	case kindFile:
		n.mode |= store.TypeReg
		n.content = e.content
	case kindDir:
		n.mode |= store.TypeDir
		n.children = map[string]*node{}
		if old := parent.children[name]; old != nil && old.typ() == store.TypeDir {
			n.children = old.children
		}
	case kindSymlink:
		n.mode |= store.TypeLink
		n.link = e.target
	case kindChar:
		n.mode |= store.TypeChar
		n.rdev = e.rdev
	case kindBlock:
		n.mode |= store.TypeBlock
		n.rdev = e.rdev
	case kindFIFO:
		n.mode |= store.TypeFIFO
	default:
		return fmt.Errorf("rootfs: put of kind %d", e.kind)
	}
	parent.children[name] = n
	return nil
}

// link places a hard link: the target's payload and type bits with e's
// permission bits, ownership, mtime and xattrs. A missing target or a
// directory is a skip.
func (t *tree) link(e entry) error {
	target, err := t.lookup(e.target)
	if err != nil {
		return err
	}
	if target == nil {
		return &skipError{"hard link target not found"}
	}
	if target.typ() == store.TypeDir {
		return &skipError{"hard link to a directory"}
	}
	parent, name, err := t.parentOf(e.path)
	if err != nil {
		return err
	}
	n := *target
	n.mode = target.typ() | e.mode
	n.uid, n.gid, n.mtime, n.xattrs = e.uid, e.gid, e.mtime, e.xattrs
	parent.children[name] = &n
	return nil
}

// whiteout removes the entry at e.path, whatever it is; a missing one is a
// no-op.
func (t *tree) whiteout(e entry) error {
	if e.path == "" {
		return nil
	}
	dir, name := splitParent(e.path)
	stack, err := t.resolveDir(splitPath(dir), false)
	if err != nil || stack == nil {
		return err
	}
	delete(stack[len(stack)-1].children, name)
	return nil
}

// opaque removes every child of the directory at e.path; a missing or
// non-directory path is a no-op.
func (t *tree) opaque(e entry) error {
	stack, err := t.resolveDir(splitPath(e.path), false)
	if err != nil || stack == nil {
		return err
	}
	stack[len(stack)-1].children = map[string]*node{}
	return nil
}
