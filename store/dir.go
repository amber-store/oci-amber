package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

var errDirFinished = errors.New("store: dir already finished")

// POSIX file type bits of fstree.Entry.Mode. They are the standard values,
// defined here so that store does not depend on x/sys.
const (
	TypeMask   uint64 = 0o170000
	TypeFIFO   uint64 = 0o010000
	TypeChar   uint64 = 0o020000
	TypeDir    uint64 = 0o040000
	TypeBlock  uint64 = 0o060000
	TypeReg    uint64 = 0o100000
	TypeLink   uint64 = 0o120000
	TypeSocket uint64 = 0o140000
)

// Dir builds one directory object through a Writer. Entries must be added
// in strictly increasing bytewise name order, exactly as fstree.DirBuilder
// requires. AddFile and AddDir add entries with UID, GID and Mtime zero and
// Mode ModeFile or ModeDir, so identical content always yields identical
// keys; AddEntry stores the metadata an entry carries. A Dir is not safe
// for concurrent use; build sibling content concurrently, then add the
// entries from one goroutine.
type Dir struct {
	w    *Writer
	db   *fstree.DirBuilder
	last string
	n    int
	done bool
}

// NewDir starts an empty directory whose objects flow through w.
func (w *Writer) NewDir() *Dir {
	return &Dir{w: w, db: fstree.NewDirBuilder(w.ic)}
}

// AddFile adds a regular-file entry whose content is the Blob or FileNode
// under content (a PutStream result).
func (d *Dir) AddFile(name string, content key.Key) error {
	if t := content.Type(); t != key.Blob && t != key.FileNode {
		return fmt.Errorf("store: dir entry %q: content key %s is a %s, not file content", name, content, t)
	}
	return d.add(name, ModeFile, content)
}

// AddDir adds a subdirectory entry whose content is the DirLeaf or DirNode
// under content (a Dir.Finish result).
func (d *Dir) AddDir(name string, content key.Key) error {
	if t := content.Type(); t != key.DirLeaf && t != key.DirNode {
		return fmt.Errorf("store: dir entry %q: content key %s is a %s, not a directory", name, content, t)
	}
	return d.add(name, ModeDir, content)
}

func (d *Dir) add(name string, mode uint64, content key.Key) error {
	return d.AddEntry(fstree.Entry{Name: []byte(name), Mode: mode, ContentKey: content[:]})
}

// AddEntry adds e with the metadata it carries: mode, uid, gid, mtime and
// xattrs are stored as given. The name is validated and ordered like
// AddFile's. The payload must match the type bits of e.Mode: a file content
// key (Blob or FileNode) for TypeReg, a directory key (DirLeaf or DirNode)
// for TypeDir, a non-empty LinkTarget for TypeLink, exactly two Rdev numbers
// for TypeChar and TypeBlock, and none of those for TypeFIFO and TypeSocket.
// Any other type bits are an error, as are both xattr forms at once.
func (d *Dir) AddEntry(e fstree.Entry) error {
	if d.done {
		return errDirFinished
	}
	name := string(e.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("store: invalid dir entry name %q", name)
	}
	if d.n > 0 && name <= d.last {
		return fmt.Errorf("store: dir entry %q not sorted after %q", name, d.last)
	}
	if err := validatePayload(e); err != nil {
		return fmt.Errorf("store: dir entry %q: %w", name, err)
	}
	if err := d.db.AddEntry(d.w.Emit, e); err != nil {
		return err
	}
	d.last = name
	d.n++
	return nil
}

// validatePayload checks that e carries exactly the payload its type bits
// call for.
func validatePayload(e fstree.Entry) error {
	hasContent, hasLink, hasRdev := len(e.ContentKey) > 0, len(e.LinkTarget) > 0, len(e.Rdev) > 0
	switch typ := e.Mode & TypeMask; typ {
	case TypeReg, TypeDir:
		if hasLink || hasRdev {
			return errors.New("a file or directory carries only a content key")
		}
		k, err := key.Parse(e.ContentKey)
		if err != nil {
			return fmt.Errorf("content key: %w", err)
		}
		if t := k.Type(); typ == TypeReg && t != key.Blob && t != key.FileNode {
			return fmt.Errorf("content key %s is a %s, not file content", k, t)
		} else if typ == TypeDir && t != key.DirLeaf && t != key.DirNode {
			return fmt.Errorf("content key %s is a %s, not a directory", k, t)
		}
	case TypeLink:
		if hasContent || hasRdev || !hasLink {
			return errors.New("a symlink carries exactly a link target")
		}
	case TypeChar, TypeBlock:
		if hasContent || hasLink || len(e.Rdev) != 2 {
			return errors.New("a device carries exactly [major, minor]")
		}
	case TypeFIFO, TypeSocket:
		if hasContent || hasLink || hasRdev {
			return errors.New("a fifo or socket carries no payload")
		}
	default:
		return fmt.Errorf("unsupported type bits %#o", typ)
	}
	if len(e.XattrsIn) > 0 && len(e.XattrsKey) > 0 {
		return errors.New("inline and spilled xattrs are exclusive")
	}
	if len(e.XattrsKey) > 0 {
		k, err := key.Parse(e.XattrsKey)
		if err != nil {
			return fmt.Errorf("xattrs key: %w", err)
		}
		if t := k.Type(); t != key.XattrSet {
			return fmt.Errorf("xattrs key %s is a %s, not an XattrSet", k, t)
		}
	}
	return nil
}

// Finish closes the directory and returns its root key (a DirLeaf for small
// directories, including an empty one; a DirNode once the entries span more
// than one leaf). The Dir cannot be used afterwards.
func (d *Dir) Finish() (key.Key, error) {
	if d.done {
		return key.Key{}, errDirFinished
	}
	d.done = true
	return d.db.Finish(d.w.Emit)
}
