package browse

import (
	"fmt"
	"path"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
)

// fsRootFor returns the filesystem view's root for an image: the rootfs
// root when it has a tree, a platform chooser for an index, otherwise a
// node that explains why there is none.
func fsRootFor(b *Browser, repo string, im *image.Image) Lister {
	if fs, ok := im.FS(); ok {
		return &fsDirNode{st: b.st, fs: fs, labels: []KV{{"image", repo + " " + shortRef(im.Meta.Digest)}}}
	}
	if im.Meta.Kind == image.KindIndex {
		return &fsChooserNode{b: b, repo: repo, im: im}
	}
	return &fsUnavailableNode{rf: im.Meta.Rootfs}
}

// fsDirNode is a directory of the filesystem view. Paths resolve through
// rootfs.FS, so symlinks are followed the way the /fs/ API follows them.
// path is cleaned and "" is the root.
type fsDirNode struct {
	st     *store.Store
	fs     *rootfs.FS
	path   string
	crumb  string
	labels []KV
}

func (n *fsDirNode) Crumb() string { return n.crumb }

func (n *fsDirNode) List() ([]Row, error) {
	entries, _, err := n.fs.List(n.path, "", 0)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		r := entryRow(e)
		p := path.Join(n.path, e.Name)
		switch e.Type() {
		case store.TypeDir:
			r.Child = &fsDirNode{st: n.st, fs: n.fs, path: p, crumb: e.Name, labels: n.labels}
		case store.TypeReg:
			r.Child = &fsFileNode{st: n.st, fs: n.fs, path: p, name: e.Name, labels: n.labels}
		case store.TypeLink:
			r.Child = &fsLinkNode{st: n.st, fs: n.fs, path: p, name: e.Name, target: e.Target, labels: n.labels}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// fsFileNode is a regular file of the filesystem view, opened by path so a
// file reached through a symlink resolves the same way a listing did.
type fsFileNode struct {
	st         *store.Store
	fs         *rootfs.FS
	path, name string
	labels     []KV
}

func (n *fsFileNode) Crumb() string { return n.name }

func (n *fsFileNode) Open() (*File, error) {
	e, err := n.fs.Stat(n.path)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	if !e.IsRegular() {
		return nil, fmt.Errorf("browse: %s is a %s, not a regular file", n.path, e.TypeName())
	}
	st, k := n.st, e.Content
	return &File{Name: n.name, Size: e.Size, Key: k, Labels: append(entryLabels(e), n.labels...),
		Open: func() *store.Reader { return st.NewReader(k) }}, nil
}

// fsLinkNode is a symlink of the filesystem view; Resolve follows it to a
// directory (listed under the link's own name, like `cd link`) or a file.
type fsLinkNode struct {
	st                 *store.Store
	fs                 *rootfs.FS
	path, name, target string
	labels             []KV
}

func (n *fsLinkNode) Crumb() string { return n.name }

func (n *fsLinkNode) Resolve() (Node, error) {
	e, err := n.fs.Stat(n.path)
	if err != nil {
		return nil, fmt.Errorf("browse: %s -> %s: %w", n.name, n.target, err)
	}
	switch e.Type() {
	case store.TypeDir:
		return &fsDirNode{st: n.st, fs: n.fs, path: n.path, crumb: n.name, labels: n.labels}, nil
	case store.TypeReg:
		return &fsFileNode{st: n.st, fs: n.fs, path: n.path, name: n.name, labels: n.labels}, nil
	}
	return nil, fmt.Errorf("browse: %s -> %s is a %s", n.name, n.target, e.TypeName())
}

// fsChooserNode lists an index's children so one platform's filesystem
// can be picked; the child's crumb is its platform.
type fsChooserNode struct {
	b    *Browser
	repo string
	im   *image.Image
}

func (n *fsChooserNode) Crumb() string { return "" }

func (n *fsChooserNode) List() ([]Row, error) {
	m, err := (&imageRootNode{b: n.b, repo: n.repo, im: n.im}).manifest()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(m.Manifests))
	for _, d := range m.Manifests {
		crumb := shortRef(d.Digest)
		if d.Platform != nil {
			crumb = d.Platform.String()
		}
		rows = append(rows, childRow(n.b, n.repo, d, func(im *image.Image) Node {
			switch root := fsRootFor(n.b, n.repo, im).(type) {
			case *fsDirNode:
				root.crumb = crumb
				return root
			case *fsUnavailableNode:
				root.crumb = crumb
				return root
			default:
				return root
			}
		}))
	}
	return rows, nil
}

// fsUnavailableNode stands in for a filesystem the image does not have:
// one row with the status and reason from meta.json.
type fsUnavailableNode struct {
	crumb string
	rf    *image.Rootfs
}

func (n *fsUnavailableNode) Crumb() string { return n.crumb }

func (n *fsUnavailableNode) List() ([]Row, error) {
	status, reason := "no rootfs", "this image root was stored before rootfs views existed"
	if n.rf != nil {
		status = "rootfs " + string(n.rf.Status)
		reason = n.rf.Reason
		if n.rf.Status == image.RootfsNotApplicable {
			reason = "the manifest does not describe a container image"
		}
	}
	return []Row{{Name: status, Detail: reason, Info: []KV{{"status", status}, {"reason", reason}}}}, nil
}
