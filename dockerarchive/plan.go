package dockerarchive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// PlanBlob is one config, layer or other blob to store.
type PlanBlob struct {
	Digest    oci.Digest
	Size      int64
	MediaType string
	Present   bool // already in the store; skip it
}

// PlanManifest is one manifest or index to publish. Synthesized marks a
// pruned index whose body this package produced; Attestation marks a
// BuildKit attestation manifest (by the annotation on the descriptor that
// referenced it), which describes no root filesystem.
type PlanManifest struct {
	Digest      oci.Digest
	MediaType   string
	Body        []byte
	IsIndex     bool
	Synthesized bool
	Attestation bool
}

// PlanEntry is one index.json entry: the image the archive was saved
// from, with the names to publish it under and its manifests in publish
// order (children first, itself last).
type PlanEntry struct {
	Digest       oci.Digest
	Names        []Name
	IsIndex      bool
	Platforms    int // children with a platform (or without any annotation), indexes only
	Attestations int // children annotated as attestation manifests
	Manifests    []oci.Digest
}

// Plan is what an import stores: unique blobs in first-use order, unique
// manifests in publish order, and the entries with their names.
type Plan struct {
	Blobs     []PlanBlob
	Manifests []PlanManifest
	Entries   []PlanEntry
}

// PlanOptions configure Plan. Names overrides the archive's RepoTags and is
// only allowed for a single-entry archive. Present, when set, is asked for
// every blob; a true answer marks the blob present.
type PlanOptions struct {
	Names   []string
	Present func(oci.Digest) (bool, error)
}

// node is a present manifest or index as resolved from the archive.
type node struct {
	desc        oci.Descriptor // as referenced, digest replaced for a pruned index
	body        []byte
	manifest    *oci.Manifest
	synthesized bool
	children    []*node // present children, indexes only
}

// Plan resolves every index.json entry, prunes absent children, collects
// blobs and manifests and assigns names.
func (a *Archive) Plan(opts PlanOptions) (*Plan, error) {
	var roots []*node
	seenRoot := map[oci.Digest]bool{}
	for i, d := range a.Index.Manifests {
		n, present, err := a.resolve(d)
		if err != nil {
			return nil, fmt.Errorf("dockerarchive: %s entry %d: %w", IndexFile, i, err)
		}
		if !present {
			return nil, fmt.Errorf("dockerarchive: %s entry %d: manifest %s is not in the archive", IndexFile, i, d.Digest)
		}
		if seenRoot[n.desc.Digest] {
			continue // the same image listed twice (e.g. one image tagged more than once)
		}
		seenRoot[n.desc.Digest] = true
		roots = append(roots, n)
	}
	p := &Plan{}
	seenBlob := map[oci.Digest]bool{}
	seenManifest := map[oci.Digest]bool{}
	for _, r := range roots {
		e := PlanEntry{Digest: r.desc.Digest, IsIndex: r.manifest.IsIndex()}
		var walk func(n *node)
		walk = func(n *node) {
			for _, c := range n.children {
				walk(c)
			}
			for _, bd := range n.manifest.BlobDescriptors() {
				if !seenBlob[bd.Digest] {
					seenBlob[bd.Digest] = true
					size, _ := a.Size(bd.Digest)
					p.Blobs = append(p.Blobs, PlanBlob{Digest: bd.Digest, Size: size, MediaType: bd.MediaType})
				}
			}
			if !seenManifest[n.desc.Digest] {
				seenManifest[n.desc.Digest] = true
				p.Manifests = append(p.Manifests, PlanManifest{
					Digest:      n.desc.Digest,
					MediaType:   n.desc.MediaType,
					Body:        n.body,
					IsIndex:     n.manifest.IsIndex(),
					Synthesized: n.synthesized,
					Attestation: n.desc.IsAttestation(),
				})
			}
			e.Manifests = append(e.Manifests, n.desc.Digest)
		}
		walk(r)
		if e.IsIndex {
			for _, c := range r.children {
				if c.desc.IsAttestation() {
					e.Attestations++
				} else {
					e.Platforms++
				}
			}
		}
		p.Entries = append(p.Entries, e)
	}
	if err := a.assignNames(p, roots, opts.Names); err != nil {
		return nil, err
	}
	if opts.Present != nil {
		for i := range p.Blobs {
			ok, err := opts.Present(p.Blobs[i].Digest)
			if err != nil {
				return nil, fmt.Errorf("dockerarchive: checking whether %s is stored: %w", p.Blobs[i].Digest, err)
			}
			p.Blobs[i].Present = ok
		}
	}
	return p, nil
}

// resolve reads the manifest d points at. present is false when its blob
// is not in the archive. An index with no present child, and an image
// manifest with some blob missing, are errors.
func (a *Archive) resolve(d oci.Descriptor) (*node, bool, error) {
	if !a.Has(d.Digest) {
		return nil, false, nil
	}
	body, err := a.ReadBlob(d.Digest)
	if err != nil {
		return nil, false, err
	}
	m, err := oci.ParseManifest(body)
	if err != nil {
		return nil, false, fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	if d.MediaType == "" {
		d.MediaType = m.EffectiveMediaType("")
	}
	n := &node{desc: d, body: body, manifest: m}
	if !m.IsIndex() {
		var missing []string
		for _, bd := range m.BlobDescriptors() {
			if !a.Has(bd.Digest) {
				missing = append(missing, bd.Digest.String())
			}
		}
		if len(missing) > 0 {
			return nil, false, fmt.Errorf("manifest %s: blobs missing from the archive: %s", d.Digest, strings.Join(missing, ", "))
		}
		return n, true, nil
	}
	var keep []int
	for i, cd := range m.Manifests {
		c, present, err := a.resolve(cd)
		if err != nil {
			return nil, false, err
		}
		if !present {
			continue
		}
		keep = append(keep, i)
		n.children = append(n.children, c)
	}
	if len(keep) == 0 {
		return nil, false, fmt.Errorf("index %s: no child manifest has its blobs in the archive", d.Digest)
	}
	if len(keep) < len(m.Manifests) {
		pruned, err := pruneIndex(body, keep, n.children)
		if err != nil {
			return nil, false, fmt.Errorf("index %s: %w", d.Digest, err)
		}
		n.body = pruned
		n.synthesized = true
		n.desc.Digest = oci.DigestOfBytes(pruned)
		n.desc.Size = int64(len(pruned))
		m, err = oci.ParseManifest(pruned)
		if err != nil {
			return nil, false, fmt.Errorf("index %s: re-parsing the pruned index: %w", d.Digest, err)
		}
		n.manifest = m
	}
	return n, true, nil
}

// pruneIndex rewrites body keeping only the manifests at the given
// positions, each replaced by its resolved child's descriptor so that a
// pruned nested index is referenced by its new digest. Every other field
// is carried over untouched through json.RawMessage.
func pruneIndex(body []byte, keep []int, children []*node) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	var manifests []json.RawMessage
	if err := json.Unmarshal(top["manifests"], &manifests); err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(keep))
	for j, i := range keep {
		if i >= len(manifests) {
			return nil, errors.New("manifests array shorter than parsed")
		}
		raw := manifests[i]
		if children[j].synthesized {
			var desc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &desc); err != nil {
				return nil, err
			}
			desc["digest"], _ = json.Marshal(children[j].desc.Digest)
			desc["size"], _ = json.Marshal(children[j].desc.Size)
			rewritten, err := json.Marshal(desc)
			if err != nil {
				return nil, fmt.Errorf("manifests[%d]: %w", i, err)
			}
			kept = append(kept, rewritten)
			continue
		}
		kept = append(kept, raw)
	}
	var err error
	if top["manifests"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	return json.Marshal(top)
}

// assignNames fills every entry's Names from manifest.json, or from the
// override, and rejects an entry left without a name.
func (a *Archive) assignNames(p *Plan, roots []*node, override []string) error {
	if len(override) > 0 {
		if len(p.Entries) != 1 {
			return fmt.Errorf("dockerarchive: --name applies to a single-image archive, this one holds %d images", len(p.Entries))
		}
		for _, s := range override {
			n, err := ParseName(s)
			if err != nil {
				return fmt.Errorf("dockerarchive: --name %v", err)
			}
			p.Entries[0].Names = appendName(p.Entries[0].Names, n)
		}
		return nil
	}
	// Map every present image manifest's config digest to every root that
	// uses it: the same config can be reachable from more than one root
	// (e.g. shared by two distinct top-level indexes), and manifest.json
	// has to name all of them.
	configRoot := map[oci.Digest][]int{}
	for i, r := range roots {
		var walk func(n *node)
		walk = func(n *node) {
			if n.manifest.Config != nil {
				configRoot[n.manifest.Config.Digest] = append(configRoot[n.manifest.Config.Digest], i)
			}
			for _, c := range n.children {
				walk(c)
			}
		}
		walk(r)
	}
	for _, le := range a.Legacy {
		hex := strings.TrimPrefix(le.Config, blobPrefix)
		d, err := oci.ParseDigest("sha256:" + hex)
		if err != nil {
			return fmt.Errorf("dockerarchive: %s: Config %q is not a blob path", ManifestFile, le.Config)
		}
		is, ok := configRoot[d]
		if !ok {
			return fmt.Errorf("dockerarchive: %s names config %s, which no manifest in %s uses", ManifestFile, d, IndexFile)
		}
		if len(is) > 1 {
			return fmt.Errorf("dockerarchive: RepoTags %v apply to %d images that share config %s; save the images separately or pass --name to a single-image archive", le.RepoTags, len(is), d)
		}
		for _, tag := range le.RepoTags {
			n, ok, err := nameFromRepoTag(tag)
			if err != nil {
				return fmt.Errorf("dockerarchive: %s: %w", ManifestFile, err)
			}
			if !ok {
				continue
			}
			for _, i := range is {
				p.Entries[i].Names = appendName(p.Entries[i].Names, n)
			}
		}
	}
	for i, e := range p.Entries {
		if len(e.Names) == 0 {
			return fmt.Errorf("dockerarchive: image %s (entry %d) has no RepoTags; pass --name repo:tag", e.Digest, i)
		}
	}
	return nil
}

func appendName(names []Name, n Name) []Name {
	for _, have := range names {
		if have == n {
			return names
		}
	}
	return append(names, n)
}
