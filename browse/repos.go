package browse

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/tui"
)

// reposNode is the entry screen: every repository with a tag or a manifest.
type reposNode struct{ b *Browser }

func (n *reposNode) Crumb() string { return "oci-amber" }

// repoRefs are one repository's tags and manifest digests, sorted.
type repoRefs struct {
	tags      []string
	manifests []oci.Digest
}

// catalog scans the tag and manifest namespaces once and groups them by
// repository; names come back in bytewise order.
func (b *Browser) catalog() ([]string, map[string]*repoRefs, error) {
	repos := map[string]*repoRefs{}
	get := func(repo string) *repoRefs {
		r := repos[repo]
		if r == nil {
			r = &repoRefs{}
			repos[repo] = r
		}
		return r
	}
	tagRefs, err := b.st.ListRefs(image.TagPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("browse: %w", err)
	}
	for _, r := range tagRefs {
		if repo, tag, ok := image.ParseTagRef(r.Name); ok {
			rr := get(repo)
			rr.tags = append(rr.tags, tag)
		}
	}
	manifestRefs, err := b.st.ListRefs(image.ManifestPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("browse: %w", err)
	}
	for _, r := range manifestRefs {
		if repo, d, ok := image.ParseManifestRef(r.Name); ok {
			rr := get(repo)
			rr.manifests = append(rr.manifests, d)
		}
	}
	for _, rr := range repos {
		slices.Sort(rr.tags)
		slices.Sort(rr.manifests)
	}
	return slices.Sorted(maps.Keys(repos)), repos, nil
}

func (n *reposNode) List() ([]Row, error) {
	names, repos, err := n.b.catalog()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(names))
	for _, name := range names {
		rr := repos[name]
		rows = append(rows, Row{
			Name:   name,
			Detail: plural(len(rr.tags), "tag") + " · " + plural(len(rr.manifests), "manifest"),
			IsDir:  true,
			Info: []KV{
				{"repository", name},
				{"tags", tui.FormatCount(int64(len(rr.tags)))},
				{"manifests", tui.FormatCount(int64(len(rr.manifests)))},
			},
			Child: &repoNode{b: n.b, repo: name, tags: rr.tags, manifests: rr.manifests},
		})
	}
	return rows, nil
}

// repoNode lists one repository: its tags, then the manifests no tag
// points at, both opened through image.Store.Open for their meta.json.
type repoNode struct {
	b         *Browser
	repo      string
	tags      []string
	manifests []oci.Digest
}

func (n *repoNode) Crumb() string { return n.repo }

func (n *repoNode) List() ([]Row, error) {
	tagged := make(map[oci.Digest]bool, len(n.tags))
	rows := make([]Row, 0, len(n.tags)+len(n.manifests))
	for _, tag := range n.tags {
		im, err := n.b.images.Open(n.repo, tag)
		if err != nil {
			return nil, fmt.Errorf("browse: opening %s:%s: %w", n.repo, tag, err)
		}
		tagged[im.Meta.Digest] = true
		rows = append(rows, imageRow(n.b, n.repo, tag, im))
	}
	for _, d := range n.manifests {
		if tagged[d] {
			continue
		}
		im, err := n.b.images.Open(n.repo, d.String())
		if err != nil {
			return nil, fmt.Errorf("browse: opening %s@%s: %w", n.repo, d, err)
		}
		rows = append(rows, imageRow(n.b, n.repo, d.String(), im))
	}
	return rows, nil
}

// imageRow is an image's row in a repository listing; reference is a tag
// or a digest string.
func imageRow(b *Browser, repo, reference string, im *image.Image) Row {
	m := im.Meta
	name, crumb := reference, ":"+reference
	var parts []string
	if oci.IsDigest(reference) {
		name, crumb = shortRef(m.Digest), "@"+shortRef(m.Digest)
		parts = append(parts, "untagged", string(m.Kind))
	} else {
		parts = append(parts, string(m.Kind), shortRef(m.Digest))
	}
	if rf := m.Rootfs; rf != nil && rf.Status != image.RootfsOK && rf.Status != image.RootfsNotApplicable {
		parts = append(parts, "rootfs "+string(rf.Status))
	}
	info := []KV{
		{"repository", repo},
		{"reference", reference},
		{"digest", m.Digest.String()},
		{"kind", string(m.Kind)},
		{"media type", m.MediaType},
		{"size", tui.FormatCount(m.Stats.TotalBytes) + " bytes"},
		{"created", m.CreatedAt.UTC().Format(time.RFC3339)},
	}
	if m.Rootfs != nil {
		info = append(info, KV{"rootfs", rootfsDetail(m.Rootfs)})
		if m.Rootfs.Reason != "" {
			info = append(info, KV{"rootfs reason", m.Rootfs.Reason})
		}
	}
	info = append(info, KV{"root key", im.Root().String()})
	return Row{Name: name, Detail: strings.Join(parts, " · "), Size: m.Stats.TotalBytes, HasSize: true, IsDir: true, Info: info,
		Child: &imageRootNode{b: b, repo: repo, crumb: crumb, im: im}}
}
