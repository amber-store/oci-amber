// Package image stores OCI manifests and image indexes as amber image roots
// and maintains the tag, manifest and referrer references that name them.
package image

import (
	"errors"
	"math"
	"time"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
)

// Kind says whether an image root holds an image manifest or an index.
type Kind string

const (
	KindManifest Kind = "manifest"
	KindIndex    Kind = "index"
)

// Entry names inside an image root.
const (
	ManifestFile = "manifest"  // the exact bytes the client PUT
	MetaFile     = "meta.json" // Meta
	BlobsDir     = "blobs"     // <digest> -> blob root, for config and layers
	ManifestsDir = "manifests" // <digest> -> child image root, index only
	RootfsDir    = "rootfs"    // the merged root filesystem, manifests with status ok or partial
)

// RootfsStatus says whether an image root holds a rootfs/ and why not.
type RootfsStatus string

const (
	RootfsOK            RootfsStatus = "ok"             // rootfs/ holds every entry of every layer
	RootfsPartial       RootfsStatus = "partial"        // rootfs/ is present; Skipped lists what was left out
	RootfsUnavailable   RootfsStatus = "unavailable"    // no rootfs/; Reason says which layer prevented it
	RootfsNotApplicable RootfsStatus = "not-applicable" // the manifest does not describe a container image
)

// Rootfs is the rootfs field of a manifest's meta.json. Entries is the
// number of entries under rootfs/, the root excluded; it is 0 unless Status
// is RootfsOK or RootfsPartial.
type Rootfs struct {
	Status       RootfsStatus  `json:"status"`
	Entries      int           `json:"entries"`
	Reason       string        `json:"reason,omitempty"`
	Skipped      []rootfs.Skip `json:"skipped,omitempty"`
	SkippedCount int           `json:"skippedCount,omitempty"`
}

// ErrNotFound is returned by Open and Delete when the reference does not
// exist.
var ErrNotFound = errors.New("image: not found")

// ErrDigestMismatch is returned by Image.WriteTo when the streamed manifest
// bytes do not hash to the stored digest.
var ErrDigestMismatch = errors.New("image: served bytes do not match digest")

// Stats are the per-image accounting numbers computed at push time and
// written to the image log line and meta.json.
type Stats struct {
	// TotalBytes is the image size as the manifest describes it: config and
	// layer sizes plus the manifest bytes; for an index, the index bytes plus
	// the TotalBytes of every child.
	TotalBytes int64 `json:"totalBytes"`
	// LogicalBytes is the encoded size of every object the image offered to
	// the store (blobs' logical bytes, or their size when already present,
	// plus the manifest's own objects).
	LogicalBytes int64 `json:"logicalBytes"`
	// DedupedBytes is the part of LogicalBytes that was already stored.
	DedupedBytes int64 `json:"dedupedBytes"`
	// DiskBytes is the number of bytes appended to pack segments.
	DiskBytes int64 `json:"diskBytes"`
}

// CompressionRatio is TotalBytes / DiskBytes, +Inf when nothing was written.
func (s Stats) CompressionRatio() float64 {
	if s.DiskBytes == 0 {
		return math.Inf(1)
	}
	return float64(s.TotalBytes) / float64(s.DiskBytes)
}

// DedupedPercent is 100 * DedupedBytes / LogicalBytes, 0 when nothing was
// offered.
func (s Stats) DedupedPercent() float64 {
	if s.LogicalBytes == 0 {
		return 0
	}
	return 100 * float64(s.DedupedBytes) / float64(s.LogicalBytes)
}

// Meta is the meta.json of an image root.
type Meta struct {
	Version      int               `json:"version"`
	Kind         Kind              `json:"kind"`
	MediaType    string            `json:"mediaType"`
	Digest       oci.Digest        `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Subject      *oci.Descriptor   `json:"subject,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	Stats        Stats             `json:"stats"`
	Rootfs       *Rootfs           `json:"rootfs,omitempty"`
}

// metaVersion is the Meta.Version written by this binary.
const metaVersion = 1
