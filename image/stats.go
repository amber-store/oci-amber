package image

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// computeStats produces the per-image accounting for a manifest or index
// being pushed to repo (spec "Accounting and logging / Per image"):
//
//   - TotalBytes: the sizes of the unique config and layer descriptors plus
//     len(body); for an index, len(body) plus every unique child's stored
//     TotalBytes.
//   - Each unique blob: its recent-uploads entry (consumed) when it was
//     finalized in this process, otherwise it was already present and counts
//     logical = deduped = size, disk = 0.
//   - Each unique index child: its stored meta.json stats.
//   - manifestObjects, the Writer stats of the manifest's own objects
//     (manifest bytes, blobs/, manifests/ and the rootfs/ tree), are added.
//
// Child roots are resolved through oci/manifest/<repo>/<digest>; a missing
// child is CodeManifestBlobUnknown.
func (s *Store) computeStats(ctx context.Context, repo string, m *oci.Manifest, body []byte, manifestObjects store.Stats) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	st := Stats{
		TotalBytes:   int64(len(body)),
		LogicalBytes: manifestObjects.LogicalBytes,
		DedupedBytes: manifestObjects.DedupedBytes,
		DiskBytes:    manifestObjects.DiskBytes,
	}
	seenBlobs := make(map[oci.Digest]bool)
	for _, d := range m.BlobDescriptors() {
		if seenBlobs[d.Digest] {
			continue
		}
		seenBlobs[d.Digest] = true
		st.TotalBytes += d.Size
		if recent, ok := s.blobs.TakeRecent(d.Digest); ok {
			st.LogicalBytes += recent.LogicalBytes
			st.DedupedBytes += recent.DedupedBytes
			st.DiskBytes += recent.DiskBytes
			continue
		}
		st.LogicalBytes += d.Size
		st.DedupedBytes += d.Size
	}
	if !m.IsIndex() {
		return st, nil
	}
	seenChildren := make(map[oci.Digest]bool)
	for _, d := range m.Manifests {
		if seenChildren[d.Digest] {
			continue
		}
		seenChildren[d.Digest] = true
		root, err := s.st.Resolve(ManifestRef(repo, d.Digest))
		if errors.Is(err, store.ErrNotFound) {
			return Stats{}, blobUnknown(d.Digest)
		}
		if err != nil {
			return Stats{}, fmt.Errorf("image: resolving child manifest %s: %w", d.Digest, err)
		}
		child, err := s.readMeta(root)
		if err != nil {
			return Stats{}, err
		}
		st.TotalBytes += child.Stats.TotalBytes
		st.LogicalBytes += child.Stats.LogicalBytes
		st.DedupedBytes += child.Stats.DedupedBytes
		st.DiskBytes += child.Stats.DiskBytes
	}
	return st, nil
}

// logPushed emits the image log line at Info level:
//
//	image pushed repo=library/app reference=v1 digest=sha256:… kind=manifest blobs=7 manifests=0 rootfs=ok rootfs_entries=4213 total_bytes=… logical_bytes=… deduped_bytes=… deduped_percent=89.7 disk_bytes=… compression_ratio=9.31 duration=…
//
// Byte counts are raw integers; the two ratios are rounded to one and two
// decimals. compression_ratio is +Inf when nothing was written to disk.
// rootfs is present on manifests only, rootfs_entries when a tree exists.
func (s *Store) logPushed(repo, reference string, m *Meta, blobs, manifests int, d time.Duration) {
	attrs := []any{
		"repo", repo,
		"reference", reference,
		"digest", string(m.Digest),
		"kind", string(m.Kind),
		"blobs", blobs,
		"manifests", manifests,
	}
	if m.Rootfs != nil {
		attrs = append(attrs, "rootfs", string(m.Rootfs.Status))
		if m.Rootfs.Status == RootfsOK || m.Rootfs.Status == RootfsPartial {
			attrs = append(attrs, "rootfs_entries", m.Rootfs.Entries)
		}
	}
	attrs = append(attrs,
		"total_bytes", m.Stats.TotalBytes,
		"logical_bytes", m.Stats.LogicalBytes,
		"deduped_bytes", m.Stats.DedupedBytes,
		"deduped_percent", roundTo(m.Stats.DedupedPercent(), 1),
		"disk_bytes", m.Stats.DiskBytes,
		"compression_ratio", roundTo(m.Stats.CompressionRatio(), 2),
		"duration", d,
	)
	s.log.Info("image pushed", attrs...)
}

// roundTo rounds x to n decimal places; infinities and NaN pass through.
func roundTo(x float64, n int) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
