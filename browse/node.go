// Package browse is the terminal browser behind `oci-amber browse`: a node
// tree over the images, blobs and root filesystems a store holds, a
// Bubble Tea model that walks it one listing at a time, and a viewer that
// shows files as text, pretty-printed JSON or a hex dump. Nothing in the
// package writes to the store.
package browse

import (
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/store"
)

// Node is one place the browser can be. Every Node is also a Lister, an
// Opener or a Resolver.
type Node interface {
	// Crumb is the node's breadcrumb segment; "" contributes nothing.
	Crumb() string
}

// Lister is a Node that lists rows.
type Lister interface {
	Node
	List() ([]Row, error)
}

// Opener is a Node that is a regular file.
type Opener interface {
	Node
	Open() (*File, error)
}

// Resolver is a Node whose kind is only known once it is followed: a
// symlink in the filesystem view. Resolve returns the Lister or Opener it
// leads to.
type Resolver interface {
	Node
	Resolve() (Node, error)
}

// Row is one line of a listing.
type Row struct {
	Name    string // first column
	Detail  string // annotation, already formatted; may be empty
	Size    int64  // bytes, shown when HasSize
	HasSize bool
	Meta    *RowMeta // ls -l columns for rootfs rows; nil elsewhere
	Info    []KV     // the info popup, in display order
	Child   Node     // what Enter opens; nil when nothing can be opened
	IsDir   bool     // rendered as a directory
}

// RowMeta are the ls -l columns of a rootfs entry.
type RowMeta struct {
	Mode     uint64 // type bits and permissions
	UID, GID uint64
	Mtime    time.Time
	Target   string // symlinks
}

// KV is one label/value pair of an info popup or a viewer status line.
type KV struct{ Key, Value string }

// File is an opened regular file.
type File struct {
	Name   string
	Size   int64
	Key    key.Key
	Labels []KV                 // facts for the viewer's status line
	Open   func() *store.Reader // a fresh reader positioned at the start
}
