package dockerarchive

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/amber-store/oci-amber/oci"
)

// Annotations on index.json entries that name the saved image, as
// containerd's exporter (and so docker with the containerd image store)
// writes them: AnnotationRefName carries the tag, AnnotationImageName the
// full docker reference.
const (
	AnnotationRefName   = "org.opencontainers.image.ref.name"
	AnnotationImageName = "io.containerd.image.name"
)

// layoutVersion is the imageLayoutVersion written to oci-layout.
const layoutVersion = "1.0.0"

// Export is one image to save: the manifest or index at Digest in Repo,
// its stored media type, and the tag to record it under; an empty Tag
// saves it as a digest reference, without names.
type Export struct {
	Repo      string
	Digest    oci.Digest
	MediaType string
	Tag       string
}

// Source supplies what Write reads: manifest and index bodies by
// repository and digest, and the bytes of a config or layer blob, which
// Blob streams to w.
type Source interface {
	Manifest(ctx context.Context, repo string, d oci.Digest) ([]byte, error)
	Blob(ctx context.Context, d oci.Digest, w io.Writer) error
}

// WriteOptions configure Write. Platform, when set, chooses which child of
// an index the index's manifest.json entry describes; without it, or when
// no child matches, the entry describes the first child that is an image
// manifest with a config and not an attestation, and an index with no such
// child gets no entry. A manifest without a config (an artifact) gets no
// entry either.
type WriteOptions struct {
	Platform *oci.Platform
	// Parallelism is how many blobs are rebuilt at once, ahead of the
	// archive; less than 1 means 1. Blobs are rebuilt into files under
	// WorkDir ("" is the OS temp directory) and copied into the archive
	// in order, so a save needs room there for about twice Parallelism
	// blobs at a time. The archive's bytes do not depend on either.
	Parallelism int
	WorkDir     string
	// Progress, when set, is called once, with Count and Total, when every
	// manifest has been resolved and before the first byte; then whenever
	// a blob starts or finishes being rebuilt, after every write of one,
	// and after every entry written to the archive. Calls come from
	// several goroutines but never at once.
	Progress func(WriteProgress)
}

// WriteProgress is what WriteOptions.Progress receives: how much of the
// archive's blobs/sha256 entries has been produced and written, and which
// blobs are being rebuilt.
type WriteProgress struct {
	Count int   // blobs/sha256 entries, manifests included
	Total int64 // their bytes
	Done  int   // entries written to the archive whole
	// Produced is how many of Total's bytes have been rebuilt from the
	// source, ahead of their entry in the archive; it is the work done.
	Produced int64
	// Active is the blobs being rebuilt right now, in write order.
	Active []BlobProgress
}

// BlobProgress is one blob being rebuilt: its size and how much of it the
// source has produced.
type BlobProgress struct {
	Digest  oci.Digest
	Size    int64
	Written int64
}

// Write writes a `docker image save` archive of images to w, reading from
// src: blobs/ and blobs/sha256/, every manifest, index, config and layer
// under the images as blobs/sha256/<hex> in digest order, then index.json
// with one entry per image, manifest.json with Docker's legacy entries,
// and oci-layout. Files are 0444 (0644 for the two JSON files),
// directories 0755, modification times the epoch, so the archive is a
// function of its content. Nothing is written until every manifest has
// been read and parsed, so a missing image fails before the first byte.
// Config and layer blobs are rebuilt from src into staged files, several
// at once (WriteOptions.Parallelism), and copied into the archive in
// order; the staged files are gone when Write returns.
func Write(ctx context.Context, w io.Writer, src Source, images []Export, opts WriteOptions) error {
	if len(images) == 0 {
		return errors.New("dockerarchive: nothing to save")
	}
	p := &savePlan{src: src, items: map[oci.Digest]*saveItem{}, legacy: map[oci.Digest]*LegacyEntry{}, progress: opts.Progress}
	for _, img := range images {
		if err := p.add(ctx, img, opts.Platform); err != nil {
			return err
		}
	}
	return p.writeTo(ctx, w, max(1, opts.Parallelism), opts.WorkDir)
}

// saveItem is one blobs/sha256 entry: a manifest read whole and parsed,
// or a blob streamed from the source.
type saveItem struct {
	size     int64
	body     []byte        // manifests and indexes; nil for a streamed blob
	manifest *oci.Manifest // parsed body, for manifests and indexes
}

// savePlan is what Write has resolved before it starts writing.
type savePlan struct {
	src         Source
	items       map[oci.Digest]*saveItem
	index       []oci.Descriptor // index.json entries, one per Export
	legacy      map[oci.Digest]*LegacyEntry
	legacyOrder []oci.Digest
	progress    func(WriteProgress) // nil when nobody listens

	// mu guards state, which the writer and the stager's workers update.
	mu    sync.Mutex
	state WriteProgress
}

// report sends a copy of the state to the progress callback. The caller
// holds mu, so reports never overlap.
func (p *savePlan) report() {
	if p.progress != nil {
		s := p.state
		s.Active = slices.Clone(s.Active)
		p.progress(s)
	}
}

// startBlob adds d to the active blobs, in write (digest) order.
func (p *savePlan) startBlob(d oci.Digest, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, _ := slices.BinarySearchFunc(p.state.Active, d, func(b BlobProgress, d oci.Digest) int { return strings.Compare(string(b.Digest), string(d)) })
	p.state.Active = slices.Insert(p.state.Active, i, BlobProgress{Digest: d, Size: size})
	p.report()
}

// advance counts n more bytes of d as produced.
func (p *savePlan) advance(d oci.Digest, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i := slices.IndexFunc(p.state.Active, func(b BlobProgress) bool { return b.Digest == d }); i >= 0 {
		p.state.Active[i].Written += n
	}
	p.state.Produced += n
	p.report()
}

// finishBlob removes d from the active blobs.
func (p *savePlan) finishBlob(d oci.Digest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Active = slices.DeleteFunc(p.state.Active, func(b BlobProgress) bool { return b.Digest == d })
	p.report()
}

// entryDone counts one more blobs/sha256 entry written to the archive;
// produced is how many of its bytes were not counted before (a manifest's,
// written from memory).
func (p *savePlan) entryDone(produced int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Done++
	p.state.Produced += produced
	p.report()
}

// add resolves one export: the top-level entry, everything under it, and
// its manifest.json entry.
func (p *savePlan) add(ctx context.Context, img Export, platform *oci.Platform) error {
	m, body, err := p.resolve(ctx, img.Repo, img.Digest)
	if err != nil {
		return err
	}
	desc := oci.Descriptor{MediaType: img.MediaType, Digest: img.Digest, Size: int64(len(body))}
	var name *Name
	if img.Tag != "" {
		name = &Name{Repo: img.Repo, Tag: img.Tag}
		desc.Annotations = map[string]string{
			AnnotationRefName:   img.Tag,
			AnnotationImageName: dockerReference(*name),
		}
	}
	p.index = append(p.index, desc)

	legacyDigest, legacyManifest := img.Digest, m
	if m.IsIndex() {
		legacyDigest, legacyManifest = "", nil
		for _, c := range legacyCandidates(m, platform) {
			cm, _, err := p.resolve(ctx, img.Repo, c.Digest)
			if err != nil {
				return err
			}
			if !cm.IsIndex() && cm.Config != nil {
				legacyDigest, legacyManifest = c.Digest, cm
				break
			}
		}
	}
	if legacyManifest == nil || legacyManifest.Config == nil {
		return nil
	}
	le := p.legacy[legacyDigest]
	if le == nil {
		le = &LegacyEntry{Config: blobPath(legacyManifest.Config.Digest)}
		for _, l := range legacyManifest.Layers {
			le.Layers = append(le.Layers, blobPath(l.Digest))
		}
		p.legacy[legacyDigest] = le
		p.legacyOrder = append(p.legacyOrder, legacyDigest)
	}
	if name != nil && !slices.Contains(le.RepoTags, name.String()) {
		le.RepoTags = append(le.RepoTags, name.String())
	}
	return nil
}

// legacyCandidates orders an index's children for the manifest.json entry:
// the ones matching platform first, then the rest, attestations excluded.
func legacyCandidates(m *oci.Manifest, platform *oci.Platform) []oci.Descriptor {
	var matching, rest []oci.Descriptor
	for _, c := range m.Manifests {
		if c.IsAttestation() {
			continue
		}
		if platform != nil && c.Platform != nil && platform.Matches(*c.Platform) {
			matching = append(matching, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(matching, rest...)
}

// resolve reads and parses the manifest or index d in repo, records it and
// everything it references, and returns it. A manifest already resolved
// is returned from the plan.
func (p *savePlan) resolve(ctx context.Context, repo string, d oci.Digest) (*oci.Manifest, []byte, error) {
	if it := p.items[d]; it != nil && it.manifest != nil {
		return it.manifest, it.body, nil
	}
	body, err := p.src.Manifest(ctx, repo, d)
	if err != nil {
		return nil, nil, fmt.Errorf("dockerarchive: reading manifest %s of %s: %w", d, repo, err)
	}
	m, err := oci.ParseManifest(body)
	if err != nil {
		return nil, nil, fmt.Errorf("dockerarchive: manifest %s of %s: %w", d, repo, err)
	}
	p.items[d] = &saveItem{size: int64(len(body)), body: body, manifest: m}
	for _, bd := range m.BlobDescriptors() {
		if _, ok := p.items[bd.Digest]; !ok {
			p.items[bd.Digest] = &saveItem{size: bd.Size}
		}
	}
	for _, c := range m.Manifests {
		if _, _, err := p.resolve(ctx, repo, c.Digest); err != nil {
			return nil, nil, err
		}
	}
	return m, body, nil
}

// writeTo streams the archive, rebuilding blobs on workers goroutines into
// files under workDir ahead of their entries.
func (p *savePlan) writeTo(ctx context.Context, w io.Writer, workers int, workDir string) error {
	tw := tar.NewWriter(w)
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	file := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := dir("blobs/"); err != nil {
		return fmt.Errorf("dockerarchive: writing archive: %w", err)
	}
	if err := dir("blobs/sha256/"); err != nil {
		return fmt.Errorf("dockerarchive: writing archive: %w", err)
	}
	digests := slices.Sorted(maps.Keys(p.items))
	var blobs []oci.Digest // the entries the stager rebuilds, in order
	p.mu.Lock()
	p.state.Count = len(digests)
	for _, d := range digests {
		p.state.Total += p.items[d].size
		if p.items[d].body == nil {
			blobs = append(blobs, d)
		}
	}
	p.report()
	p.mu.Unlock()

	outer := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := p.stage(ctx, cancel, blobs, workers, workDir)
	if err != nil {
		return err
	}
	defer st.close()
	buf := make([]byte, 1<<20)
	for _, d := range digests {
		it := p.items[d]
		if it.body != nil {
			if err := file(blobPath(d), 0o444, it.body); err != nil {
				return fmt.Errorf("dockerarchive: writing manifest %s: %w", d, err)
			}
			p.entryDone(it.size)
			continue
		}
		s, err := st.next()
		if err != nil {
			return st.failure(outer, err)
		}
		if s.err != nil {
			return st.failure(outer, s.err)
		}
		if s.n != it.size {
			return fmt.Errorf("dockerarchive: blob %s is %d bytes, its descriptor says %d", d, s.n, it.size)
		}
		if err := tw.WriteHeader(&tar.Header{Name: blobPath(d), Typeflag: tar.TypeReg, Mode: 0o444, Size: it.size}); err != nil {
			return fmt.Errorf("dockerarchive: writing blob %s: %w", d, err)
		}
		if err := copyStaged(tw, s.path, buf); err != nil {
			return fmt.Errorf("dockerarchive: writing blob %s: %w", d, err)
		}
		os.Remove(s.path)
		p.entryDone(0)
	}
	index, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: p.index})
	if err != nil {
		return fmt.Errorf("dockerarchive: encoding %s: %w", IndexFile, err)
	}
	if err := file(IndexFile, 0o644, index); err != nil {
		return fmt.Errorf("dockerarchive: writing %s: %w", IndexFile, err)
	}
	if len(p.legacyOrder) > 0 {
		entries := make([]LegacyEntry, 0, len(p.legacyOrder))
		for _, d := range p.legacyOrder {
			entries = append(entries, *p.legacy[d])
		}
		legacy, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("dockerarchive: encoding %s: %w", ManifestFile, err)
		}
		if err := file(ManifestFile, 0o644, legacy); err != nil {
			return fmt.Errorf("dockerarchive: writing %s: %w", ManifestFile, err)
		}
	}
	layout, _ := json.Marshal(struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}{layoutVersion})
	if err := file(LayoutFile, 0o444, layout); err != nil {
		return fmt.Errorf("dockerarchive: writing %s: %w", LayoutFile, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("dockerarchive: writing archive: %w", err)
	}
	return nil
}

// blobPath is the archive path of d.
func blobPath(d oci.Digest) string { return blobPrefix + d.Hex() }

// dockerReference is the name docker would give n on load: a leading
// registry host is kept, anything else goes under docker.io, and a single
// component under docker.io/library.
func dockerReference(n Name) string {
	first, _, found := strings.Cut(n.Repo, "/")
	switch {
	case found && isHost(first):
		return n.String()
	case found:
		return "docker.io/" + n.String()
	default:
		return "docker.io/library/" + n.String()
	}
}

// copyStaged copies the staged file at path into w through buf.
func copyStaged(w io.Writer, path string, buf []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyBuffer(w, f, buf)
	return err
}
