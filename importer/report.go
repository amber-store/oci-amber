package importer

import (
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// EntryReport describes one published image.
type EntryReport struct {
	Names        []dockerarchive.Name
	Digest       oci.Digest
	IsIndex      bool
	Platforms    int
	Attestations int
	// Rootfs holds the rootfs outcome of the entry's platform manifests
	// (the entry itself when it is a manifest), nil entries excluded.
	Rootfs []*image.Rootfs
	// Stats are the image stats of the entry's first Put: they fold in the
	// blobs stored by this run, the manifest objects and the rootfs trees.
	Stats image.Stats
}

// BlobCounts summarize what happened to the blobs.
type BlobCounts struct {
	Processed  int // stored by this run: not present at planning time and not a dedup hit during the run
	Prism      int
	Raw        int
	Present    int // present at planning time plus dedup hits during the run
	RawReasons map[blob.RawReason]int
}

// Report is the outcome of a run.
type Report struct {
	Duration time.Duration
	Entries  []EntryReport
	Blobs    BlobCounts
	// Compressed is every unique blob's size plus every manifest body, as
	// they are in the archive. Uncompressed replaces prisms' sizes by their
	// decompressed size.
	Compressed   int64
	Uncompressed int64
	// Added is the sum of the entries' DiskBytes: what the run appended to
	// the pack segments. Logical and Deduped are the corresponding sums.
	Added   int64
	Logical int64
	Deduped int64
}

// DedupRatio is Compressed over Added; ok is false when nothing was added.
func (r *Report) DedupRatio() (float64, bool) {
	if r.Added == 0 {
		return 0, false
	}
	return float64(r.Compressed) / float64(r.Added), true
}

// NotWrittenPercent is the share of the compressed bytes that did not
// reach the pack segments. Clamped at 0: metadata overhead (manifests,
// rootfs trees) can make Added exceed Compressed on an all-present
// re-import, which would otherwise render as a negative percentage.
func (r *Report) NotWrittenPercent() float64 {
	if r.Compressed == 0 {
		return 0
	}
	return max(0, 100*(1-float64(r.Added)/float64(r.Compressed)))
}

// ChunksReusedPercent is Deduped over Logical, the registry's
// deduped_percent.
func (r *Report) ChunksReusedPercent() float64 {
	if r.Logical == 0 {
		return 0
	}
	return 100 * float64(r.Deduped) / float64(r.Logical)
}
