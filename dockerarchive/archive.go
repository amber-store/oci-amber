// Package dockerarchive reads the archives `docker image save` writes:
// an OCI image layout (oci-layout, index.json, blobs/sha256/*) inside a
// tar, plus Docker's legacy manifest.json carrying the RepoTags. Blobs are
// read in place from the archive file, never extracted.
package dockerarchive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// Names inside the archive.
const (
	IndexFile    = "index.json"
	ManifestFile = "manifest.json"
	LayoutFile   = "oci-layout"
	blobPrefix   = "blobs/sha256/"
)

// maxTopFile bounds index.json and manifest.json; maxSmallBlob bounds what
// ReadBlob loads whole (manifests, indexes, configs).
const (
	maxTopFile   = 16 << 20
	maxSmallBlob = 64 << 20
)

var (
	// ErrNoIndex reports an archive without index.json: a legacy-only
	// archive, which this package does not read.
	ErrNoIndex = errors.New("dockerarchive: no index.json in the archive; only OCI layout archives are supported (docker 25 or later writes one)")
	// ErrBlobMissing reports a digest with no blobs/sha256 entry.
	ErrBlobMissing = errors.New("dockerarchive: blob not in archive")
)

// LegacyEntry is one element of manifest.json. Paths are archive paths
// ("blobs/sha256/<hex>").
type LegacyEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type section struct{ off, size int64 }

// Archive is an open docker save archive: the blob table plus the parsed
// top-level files.
type Archive struct {
	f      *os.File
	blobs  map[oci.Digest]section
	Index  *oci.Manifest // index.json
	Legacy []LegacyEntry // manifest.json; nil when absent

	// LayoutVersion is the imageLayoutVersion from oci-layout, "" when the
	// archive has none. A missing oci-layout is not an error; index.json is
	// the gate for what counts as a readable archive.
	LayoutVersion string
}

// Open opens the archive at path and scans its headers once: every
// blobs/sha256/<hex> regular file is recorded by offset and size, and
// index.json, manifest.json and oci-layout are read whole. The file stays
// open until Close.
func Open(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a, err := scan(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return a, nil
}

// scan reads the tar headers from f, which must be positioned at 0 and is
// read without buffering so that its offset after Next is the start of
// the entry's content.
func scan(f *os.File) (*Archive, error) {
	a := &Archive{f: f, blobs: make(map[oci.Digest]section)}
	tr := tar.NewReader(f)
	var index, legacy, layout []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dockerarchive: reading tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch {
		case name == IndexFile:
			if index, err = readTop(tr, name, h.Size); err != nil {
				return nil, err
			}
		case name == ManifestFile:
			if legacy, err = readTop(tr, name, h.Size); err != nil {
				return nil, err
			}
		case name == LayoutFile:
			if layout, err = readTop(tr, name, h.Size); err != nil {
				return nil, err
			}
		case strings.HasPrefix(name, blobPrefix):
			hex := name[len(blobPrefix):]
			d, err := oci.ParseDigest("sha256:" + hex)
			if err != nil {
				continue // not a blob path, an unrelated file
			}
			off, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, fmt.Errorf("dockerarchive: locating %s: %w", name, err)
			}
			// A repeated blobs/sha256/<hex> name overwrites the earlier
			// record (last-write-wins); the digest check ReadBlob/Verify
			// run before use catches a wrong body either way.
			a.blobs[d] = section{off: off, size: h.Size}
		}
	}
	if index == nil {
		return nil, ErrNoIndex
	}
	m, err := oci.ParseManifest(index)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: %s: %w", IndexFile, err)
	}
	if !m.IsIndex() {
		return nil, fmt.Errorf("dockerarchive: %s is not an image index", IndexFile)
	}
	a.Index = m
	if legacy != nil {
		if err := json.Unmarshal(legacy, &a.Legacy); err != nil {
			return nil, fmt.Errorf("dockerarchive: %s: %w", ManifestFile, err)
		}
	}
	if layout != nil {
		var l struct {
			ImageLayoutVersion string `json:"imageLayoutVersion"`
		}
		if err := json.Unmarshal(layout, &l); err != nil {
			return nil, fmt.Errorf("dockerarchive: %s: %w", LayoutFile, err)
		}
		a.LayoutVersion = l.ImageLayoutVersion
	}
	return a, nil
}

func readTop(r io.Reader, name string, size int64) ([]byte, error) {
	if size > maxTopFile {
		return nil, fmt.Errorf("dockerarchive: %s is %d bytes, more than %d", name, size, maxTopFile)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: reading %s: %w", name, err)
	}
	return b, nil
}

// Close closes the archive file. Sections obtained before Close fail
// afterwards.
func (a *Archive) Close() error { return a.f.Close() }

// Has reports whether d has a blob in the archive.
func (a *Archive) Has(d oci.Digest) bool {
	_, ok := a.blobs[d]
	return ok
}

// Size returns d's size in the archive.
func (a *Archive) Size(d oci.Digest) (int64, bool) {
	s, ok := a.blobs[d]
	return s.size, ok
}

// Section returns a reader over d's bytes. The reader is an io.ReaderAt
// and safe for concurrent use with other sections.
func (a *Archive) Section(d oci.Digest) (*io.SectionReader, error) {
	s, ok := a.blobs[d]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBlobMissing, d)
	}
	return io.NewSectionReader(a.f, s.off, s.size), nil
}

// ReadBlob reads d whole and verifies the bytes hash to d. It is for the
// small blobs (manifests, indexes, configs); layers go through Section.
func (a *Archive) ReadBlob(d oci.Digest) ([]byte, error) {
	sec, err := a.Section(d)
	if err != nil {
		return nil, err
	}
	if sec.Size() > maxSmallBlob {
		return nil, fmt.Errorf("dockerarchive: blob %s is %d bytes, too large to read whole", d, sec.Size())
	}
	b, err := io.ReadAll(sec)
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: reading %s: %w", d, err)
	}
	if got := oci.DigestOfBytes(b); got != d {
		return nil, fmt.Errorf("dockerarchive: blob %s does not match its name: content is %s", d, got)
	}
	return b, nil
}

// Verify streams d and checks its sha256, calling progress (when not nil)
// with the running byte count. It stops early when ctx is done.
func (a *Archive) Verify(ctx context.Context, d oci.Digest, progress func(n int64)) error {
	sec, err := a.Section(d)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		k, err := sec.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if progress != nil && k > 0 {
			progress(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("dockerarchive: reading %s: %w", d, err)
		}
	}
	if got := oci.DigestFromSum(h.Sum(nil)); got != d {
		return fmt.Errorf("dockerarchive: blob %s does not match its name: content is %s", d, got)
	}
	return nil
}
