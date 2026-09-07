package image

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
)

// rootfsHook is a test seam: when non-nil, buildRootfs calls it with the
// repository and digest right before building a tree (never for a reused
// or not-applicable one), so a test can hold a build in place. Nil in
// production.
var rootfsHook func(repo string, digest oci.Digest)

// rootfsApplies reports whether m describes a container image whose layers
// form a root filesystem: an image manifest with an OCI or Docker image
// config. Indexes and artifacts do not.
func rootfsApplies(m *oci.Manifest) bool {
	if m.IsIndex() || m.Config == nil {
		return false
	}
	switch m.Config.MediaType {
	case oci.MediaTypeOCIConfig, oci.MediaTypeDockerConfig:
		return true
	}
	return false
}

// buildRootfs produces the rootfs field of the manifest m being pushed to
// repo and, for ok and partial, the key of its rootfs/ directory, writing
// new objects through w. A root of the same digest already in repo lends
// its tree when it has one. A raw layer or one whose archive cannot be
// parsed makes the field unavailable; nothing is written then. A layer
// whose blob vanished since the manifest was validated is
// CodeManifestBlobUnknown; any other failure is returned as is.
func (s *Store) buildRootfs(ctx context.Context, w *store.Writer, repo string, digest oci.Digest, m *oci.Manifest) (*Rootfs, key.Key, error) {
	if !rootfsApplies(m) {
		return &Rootfs{Status: RootfsNotApplicable}, key.Key{}, nil
	}
	if info, k, ok, err := s.reuseRootfs(repo, digest); err != nil {
		return nil, key.Key{}, err
	} else if ok {
		return info, k, nil
	}
	if rootfsHook != nil {
		rootfsHook(repo, digest)
	}
	start := time.Now()
	b := rootfs.New()
	for _, d := range m.Layers {
		bl, err := s.blobs.Open(d.Digest)
		if errors.Is(err, blob.ErrNotFound) {
			return nil, key.Key{}, blobUnknown(d.Digest)
		}
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: opening layer %s: %w", d.Digest, err)
		}
		prism, err := bl.Prism()
		if errors.Is(err, blob.ErrNotPrism) {
			return &Rootfs{Status: RootfsUnavailable, Reason: fmt.Sprintf("layer %s is stored raw (%s)", d.Digest, bl.Meta.RawReason)}, key.Key{}, nil
		}
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: layer %s: %w", d.Digest, err)
		}
		err = b.Apply(ctx, d.Digest, prism)
		var le *rootfs.LayerError
		if errors.As(err, &le) {
			return &Rootfs{Status: RootfsUnavailable, Reason: err.Error()}, key.Key{}, nil
		}
		if err != nil {
			return nil, key.Key{}, fmt.Errorf("image: building rootfs: %w", err)
		}
	}
	res, err := b.Write(w)
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("image: writing rootfs: %w", err)
	}
	info := &Rootfs{Status: RootfsOK, Entries: res.Entries}
	if res.SkippedCount > 0 {
		info.Status, info.Skipped, info.SkippedCount = RootfsPartial, res.Skipped, res.SkippedCount
	}
	s.log.Debug("rootfs built", "repo", repo, "digest", string(digest), "layers", len(m.Layers),
		"entries", res.Entries, "skipped", res.SkippedCount, "duration", time.Since(start))
	return info, res.Root, nil
}

// reuseRootfs returns the rootfs field and key of the root that
// oci/manifest/<repo>/<digest> already points at, when that root holds a
// tree (status ok or partial). The same digest has the same layers, so
// nothing needs to be rebuilt on a re-push. An unavailable rootfs is not
// reused: a layer stored raw at the time may have been deleted and pushed
// again as a prism since. It runs before Put takes the repository lock;
// should the root be deleted and collected before the re-push publishes,
// the publish fails on the missing objects rather than naming them.
func (s *Store) reuseRootfs(repo string, digest oci.Digest) (*Rootfs, key.Key, bool, error) {
	name := ManifestRef(repo, digest)
	root, err := s.st.Resolve(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, key.Key{}, false, nil
	}
	if err != nil {
		return nil, key.Key{}, false, fmt.Errorf("image: resolving %s: %w", name, err)
	}
	meta, err := s.readMeta(root)
	if err != nil {
		return nil, key.Key{}, false, err
	}
	if meta.Rootfs == nil || (meta.Rootfs.Status != RootfsOK && meta.Rootfs.Status != RootfsPartial) {
		return nil, key.Key{}, false, nil
	}
	k, err := s.st.LookupKey(root, RootfsDir)
	if err != nil {
		return nil, key.Key{}, false, fmt.Errorf("image: %s in root %s: %w", RootfsDir, root, err)
	}
	return meta.Rootfs, k, true, nil
}

// logRootfs warns about a rootfs that is missing or incomplete.
func (s *Store) logRootfs(repo string, digest oci.Digest, r *Rootfs) {
	switch {
	case r == nil:
	case r.Status == RootfsUnavailable:
		s.log.Warn("rootfs unavailable", "repo", repo, "digest", string(digest), "reason", r.Reason)
	case r.Status == RootfsPartial && len(r.Skipped) > 0:
		s.log.Warn("rootfs partial", "repo", repo, "digest", string(digest), "skipped", r.SkippedCount,
			"path", r.Skipped[0].Path, "reason", r.Skipped[0].Reason)
	}
}
