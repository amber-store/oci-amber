package browse

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/amber-store/core/key"
	tarprism "github.com/draganm/tar-prism"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// fileNode is a regular file of the storage tree.
type fileNode struct {
	st     *store.Store
	name   string
	key    key.Key
	labels []KV
}

func (n *fileNode) Crumb() string { return n.name }

func (n *fileNode) Open() (*File, error) {
	if t := n.key.Type(); t != key.Blob && t != key.FileNode {
		return nil, fmt.Errorf("browse: %s is a %s, not file content", n.name, t)
	}
	st, k := n.st, n.key
	return &File{Name: n.name, Size: int64(k.Length()), Key: k, Labels: n.labels, Open: func() *store.Reader { return st.NewReader(k) }}, nil
}

// keyInfo are the info lines every storage row carries.
func keyInfo(name string, k key.Key) []KV {
	return []KV{
		{"name", name},
		{"key", k.String()},
		{"type", k.Type().String()},
		{"length", tui.FormatCount(int64(k.Length())) + " bytes"},
	}
}

// listDir decodes the entries of the directory object dir in name order.
func listDir(st *store.Store, dir key.Key) ([]rootfs.Entry, error) {
	entries, _, err := rootfs.NewFS(st, dir).List("", "", 0)
	return entries, err
}

// entryRow renders a rootfs entry's ls -l columns and info; the caller
// sets Child.
func entryRow(e rootfs.Entry) Row {
	mtime := time.Unix(0, e.Mtime).UTC()
	r := Row{Name: e.Name, IsDir: e.IsDir(), Meta: &RowMeta{Mode: e.Mode, UID: e.UID, GID: e.GID, Mtime: mtime, Target: e.Target}}
	info := []KV{
		{"name", e.Name},
		{"type", e.TypeName()},
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
		{"mtime", mtime.Format(time.RFC3339)},
	}
	switch e.Type() {
	case store.TypeReg:
		r.Size, r.HasSize = e.Size, true
		info = append(info, KV{"size", tui.FormatCount(e.Size) + " bytes"}, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeDir:
		info = append(info, KV{"key", e.Content.String()}, KV{"key type", e.Content.Type().String()})
	case store.TypeLink:
		info = append(info, KV{"target", e.Target})
	case store.TypeChar, store.TypeBlock:
		info = append(info, KV{"device", fmt.Sprintf("%d, %d", e.Rdev[0], e.Rdev[1])})
	}
	r.Info = info
	return r
}

// entryLabels are the viewer status facts of a rootfs file.
func entryLabels(e rootfs.Entry) []KV {
	return []KV{
		{"mode", fmt.Sprintf("%04o", e.Mode&^store.TypeMask)},
		{"owner", fmt.Sprintf("%d:%d", e.UID, e.GID)},
	}
}

// amberDirNode is a directory of the storage tree the browser has no
// special knowledge of: rootfs/ and everything under it, or an entry a
// future layout adds. Rows carry the entry's own metadata; symlinks are
// shown, not followed.
type amberDirNode struct {
	st     *store.Store
	name   string
	dir    key.Key
	labels []KV // inherited by files: the image or layer they belong to
}

func (n *amberDirNode) Crumb() string { return n.name }

func (n *amberDirNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s: %w", n.name, err)
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		r := entryRow(e)
		switch e.Type() {
		case store.TypeDir:
			r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: n.labels}
		case store.TypeReg:
			r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: append(entryLabels(e), n.labels...)}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// blobKind summarizes how a blob is stored: "prism gzip go-flate",
// "prism none", "raw not-tar".
func blobKind(m blob.Meta) string {
	if m.Kind == blob.KindRaw {
		return "raw " + string(m.RawReason)
	}
	s := "prism " + m.Format
	if m.Engine != "" {
		s += " " + m.Engine
	}
	return s
}

// blobLabels are the viewer status facts of a blob's parts.
func blobLabels(m blob.Meta) []KV {
	return []KV{{"blob", shortRef(m.Digest)}, {"kind", blobKind(m)}}
}

// partDetails describes the fixed entries of a blob root.
var partDetails = map[string]string{
	blob.MetaFile:       "blob metadata",
	blob.CompFile:       "zrecipe compression parameters",
	tarprism.IndexFile:  "tar-prism index: where each blob splices into the recipe",
	tarprism.RecipeFile: "tar recipe: every byte that is not file content",
	blob.RawFile:        "the blob's bytes, verbatim",
}

// blobRootNode is a blob root: the parts of a prism, or meta.json and the
// verbatim bytes of a raw blob.
type blobRootNode struct {
	st *store.Store
	bl *blob.Blob
}

func (n *blobRootNode) Crumb() string { return shortRef(n.bl.Meta.Digest) }

func (n *blobRootNode) List() ([]Row, error) {
	entries, err := listDir(n.st, n.bl.Root())
	if err != nil {
		return nil, fmt.Errorf("browse: listing blob root %s: %w", n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Name == tarprism.BlobsDir && e.IsDir():
			rows = append(rows, Row{
				Name: e.Name, Detail: plural(n.bl.Meta.Entries, "file"), IsDir: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &prismBlobsNode{st: n.st, bl: n.bl, dir: e.Content},
			})
		case e.IsRegular():
			rows = append(rows, Row{
				Name: e.Name, Detail: partDetails[e.Name], Size: e.Size, HasSize: true,
				Info:  keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.st, name: e.Name, key: e.Content, labels: labels},
			})
		default:
			r := entryRow(e)
			if e.IsDir() {
				r.Child = &amberDirNode{st: n.st, name: e.Name, dir: e.Content, labels: labels}
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// prismBlobsNode is the blobs/ directory of a prism: one numbered file per
// regular file of the layer's tar, annotated with the tar entry's name
// from recipe.json.
type prismBlobsNode struct {
	st  *store.Store
	bl  *blob.Blob
	dir key.Key
}

func (n *prismBlobsNode) Crumb() string { return tarprism.BlobsDir }

func (n *prismBlobsNode) List() ([]Row, error) {
	prism, err := n.bl.Prism()
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	idx, err := prism.Index()
	if err != nil {
		return nil, fmt.Errorf("browse: reading %s of %s: %w", tarprism.IndexFile, n.bl.Meta.Digest, err)
	}
	byBlob := make(map[string]tarprism.Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		if name, ok := strings.CutPrefix(e.Blob, tarprism.BlobsDir+"/"); ok {
			byBlob[name] = e
		}
	}
	entries, err := listDir(n.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s of %s: %w", tarprism.BlobsDir, n.bl.Meta.Digest, err)
	}
	labels := blobLabels(n.bl.Meta)
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		if !e.IsRegular() {
			rows = append(rows, entryRow(e))
			continue
		}
		r := Row{Name: e.Name, Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content)}
		fileLabels := labels
		if te, ok := byBlob[e.Name]; ok {
			r.Detail = te.Name
			r.Info = append(r.Info, KV{"tar entry", te.Name}, KV{"recipe offset", tui.FormatCount(te.Offset)})
			fileLabels = append([]KV{{"file", te.Name}}, labels...)
		}
		r.Child = &fileNode{st: n.st, name: e.Name, key: e.Content, labels: fileLabels}
		rows = append(rows, r)
	}
	return rows, nil
}

// imageRootNode is an image root: the manifest, its meta.json, the blobs
// and child manifests it references, and the rootfs tree when it has one.
// crumb is ":tag" or "@sha256:…" as the image was reached.
type imageRootNode struct {
	b     *Browser
	repo  string
	crumb string
	im    *image.Image
}

func (n *imageRootNode) Crumb() string { return n.crumb }

// manifest parses the stored manifest bytes.
func (n *imageRootNode) manifest() (*oci.Manifest, error) {
	var buf bytes.Buffer
	if err := n.im.WriteTo(context.Background(), &buf); err != nil {
		return nil, fmt.Errorf("browse: reading manifest %s: %w", n.im.Meta.Digest, err)
	}
	m, err := oci.ParseManifest(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("browse: manifest %s: %w", n.im.Meta.Digest, err)
	}
	return m, nil
}

// rootfsDetail summarizes a meta.json rootfs field: "ok · 1,204 entries",
// "partial · 1,200 entries · 4 skipped", "unavailable".
func rootfsDetail(r *image.Rootfs) string {
	if r == nil {
		return ""
	}
	s := string(r.Status)
	if r.Status == image.RootfsOK || r.Status == image.RootfsPartial {
		s += " · " + tui.FormatCount(int64(r.Entries)) + " entries"
	}
	if r.SkippedCount > 0 {
		s += fmt.Sprintf(" · %d skipped", r.SkippedCount)
	}
	return s
}

func (n *imageRootNode) List() ([]Row, error) {
	m, err := n.manifest()
	if err != nil {
		return nil, err
	}
	entries, err := listDir(n.b.st, n.im.Root())
	if err != nil {
		return nil, fmt.Errorf("browse: listing image root %s: %w", n.im.Meta.Digest, err)
	}
	meta := n.im.Meta
	labels := []KV{{"image", n.repo + " " + shortRef(meta.Digest)}}
	uniqueBlobs := map[oci.Digest]bool{}
	for _, d := range m.BlobDescriptors() {
		uniqueBlobs[d.Digest] = true
	}
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Name == image.ManifestFile && e.IsRegular():
			rows = append(rows, Row{Name: e.Name, Detail: meta.MediaType, Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}})
		case e.Name == image.MetaFile && e.IsRegular():
			rows = append(rows, Row{Name: e.Name, Detail: "kind, digest, stats and rootfs status", Size: e.Size, HasSize: true, Info: keyInfo(e.Name, e.Content),
				Child: &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}})
		case e.Name == image.BlobsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: plural(len(uniqueBlobs), "blob"), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &imageBlobsNode{b: n.b, im: n.im, m: m, dir: e.Content}})
		case e.Name == image.ManifestsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: plural(len(m.Manifests), "child manifest"), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &imageManifestsNode{b: n.b, repo: n.repo, m: m}})
		case e.Name == image.RootfsDir && e.IsDir():
			rows = append(rows, Row{Name: e.Name, Detail: rootfsDetail(meta.Rootfs), IsDir: true, Info: keyInfo(e.Name, e.Content),
				Child: &amberDirNode{st: n.b.st, name: e.Name, dir: e.Content, labels: labels}})
		default:
			r := entryRow(e)
			switch e.Type() {
			case store.TypeDir:
				r.Child = &amberDirNode{st: n.b.st, name: e.Name, dir: e.Content, labels: labels}
			case store.TypeReg:
				r.Child = &fileNode{st: n.b.st, name: e.Name, key: e.Content, labels: labels}
			}
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// blobRow is a blob's row under an image: its role in the manifest, how it
// is stored and its sizes. It opens the blob root.
func blobRow(st *store.Store, bl *blob.Blob, role string) Row {
	m := bl.Meta
	var parts []string
	if role != "" {
		parts = append(parts, role)
	}
	parts = append(parts, blobKind(m))
	if m.Kind == blob.KindPrism {
		parts = append(parts, plural(m.Entries, "file"))
		if m.UncompressedSize > 0 {
			parts = append(parts, tui.FormatBytes(m.UncompressedSize)+" uncompressed")
		}
	}
	info := []KV{{"digest", m.Digest.String()}, {"kind", string(m.Kind)}, {"format", m.Format}}
	if m.Engine != "" {
		info = append(info, KV{"engine", strings.TrimSpace(m.Engine + " " + m.EngineVersion)})
	}
	if m.RawReason != "" {
		info = append(info, KV{"raw reason", string(m.RawReason)})
	}
	info = append(info, KV{"size", tui.FormatCount(m.Size) + " bytes"})
	if m.UncompressedSize > 0 {
		info = append(info, KV{"uncompressed", tui.FormatCount(m.UncompressedSize) + " bytes"})
	}
	if m.DiffID != "" {
		info = append(info, KV{"diff id", m.DiffID.String()})
	}
	if m.Kind == blob.KindPrism {
		info = append(info, KV{"files", tui.FormatCount(int64(m.Entries))})
	}
	info = append(info, KV{"uploaded", m.UploadedAt.UTC().Format(time.RFC3339)}, KV{"root key", bl.Root().String()})
	return Row{Name: shortRef(m.Digest), Detail: strings.Join(parts, " · "), Size: m.Size, HasSize: true, IsDir: true, Info: info,
		Child: &blobRootNode{st: st, bl: bl}}
}

// imageBlobsNode is the blobs/ directory of an image root: the config and
// layers in manifest order, each annotated with how it is stored, then any
// entry the manifest does not name.
type imageBlobsNode struct {
	b   *Browser
	im  *image.Image
	m   *oci.Manifest
	dir key.Key
}

func (n *imageBlobsNode) Crumb() string { return image.BlobsDir }

func (n *imageBlobsNode) List() ([]Row, error) {
	entries, err := listDir(n.b.st, n.dir)
	if err != nil {
		return nil, fmt.Errorf("browse: listing %s of %s: %w", image.BlobsDir, n.im.Meta.Digest, err)
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Name] = true
	}
	type slot struct {
		digest oci.Digest
		roles  []string
	}
	var order []*slot
	byDigest := map[oci.Digest]*slot{}
	add := func(d oci.Digest, role string) {
		s := byDigest[d]
		if s == nil {
			s = &slot{digest: d}
			byDigest[d] = s
			order = append(order, s)
		}
		s.roles = append(s.roles, role)
	}
	if n.m.Config != nil {
		add(n.m.Config.Digest, "config")
	}
	for i, l := range n.m.Layers {
		add(l.Digest, fmt.Sprintf("layer %d/%d", i+1, len(n.m.Layers)))
	}
	rows := make([]Row, 0, len(entries))
	for _, s := range order {
		role := strings.Join(s.roles, ", ")
		if !present[s.digest.String()] {
			rows = append(rows, Row{Name: shortRef(s.digest), Detail: role + " · missing from " + image.BlobsDir + "/", Info: []KV{{"digest", s.digest.String()}}})
			continue
		}
		delete(present, s.digest.String())
		rows = append(rows, n.blobRow(s.digest, role))
	}
	for _, e := range entries { // whatever the manifest did not name, in name order
		if !present[e.Name] {
			continue
		}
		d, err := oci.ParseDigest(e.Name)
		if err != nil {
			rows = append(rows, entryRow(e))
			continue
		}
		rows = append(rows, n.blobRow(d, ""))
	}
	return rows, nil
}

// blobRow opens d and renders it; a blob that cannot be opened gets a row
// that says so and opens nothing.
func (n *imageBlobsNode) blobRow(d oci.Digest, role string) Row {
	bl, err := n.b.blobs.Open(d)
	if err != nil {
		detail := "unreadable: " + err.Error()
		if role != "" {
			detail = role + " · " + detail
		}
		return Row{Name: shortRef(d), Detail: detail, Info: []KV{{"digest", d.String()}}}
	}
	return blobRow(n.b.st, bl, role)
}

// isAttestation reports a BuildKit attestation manifest, marked by the
// annotation on the descriptor that references it.
func isAttestation(d oci.Descriptor) bool {
	return d.Annotations["vnd.docker.reference.type"] == "attestation-manifest"
}

// childRow is an index child's row: platform, attestation mark, kind and
// rootfs status. child builds what Enter opens from the opened image, so
// the storage view and the filesystem chooser share this.
func childRow(b *Browser, repo string, d oci.Descriptor, child func(*image.Image) Node) Row {
	var parts []string
	if d.Platform != nil {
		parts = append(parts, d.Platform.String())
	}
	if isAttestation(d) {
		parts = append(parts, "attestation")
	}
	info := []KV{{"digest", d.Digest.String()}, {"media type", d.MediaType}, {"size", tui.FormatCount(d.Size) + " bytes"}}
	if d.Platform != nil {
		info = append(info, KV{"platform", d.Platform.String()})
	}
	keys := make([]string, 0, len(d.Annotations))
	for k := range d.Annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		info = append(info, KV{k, d.Annotations[k]})
	}
	im, err := b.images.Open(repo, d.Digest.String())
	if err != nil {
		parts = append(parts, "missing: "+err.Error())
		return Row{Name: shortRef(d.Digest), Detail: strings.Join(parts, " · "), Size: d.Size, HasSize: true, Info: info}
	}
	parts = append(parts, string(im.Meta.Kind))
	if rf := im.Meta.Rootfs; rf != nil && rf.Status != image.RootfsOK && rf.Status != image.RootfsNotApplicable {
		parts = append(parts, "rootfs "+string(rf.Status))
	}
	return Row{Name: shortRef(d.Digest), Detail: strings.Join(parts, " · "), Size: d.Size, HasSize: true, IsDir: true, Info: info, Child: child(im)}
}

// imageManifestsNode is the manifests/ directory of an index root: its
// children in index order.
type imageManifestsNode struct {
	b    *Browser
	repo string
	m    *oci.Manifest
}

func (n *imageManifestsNode) Crumb() string { return image.ManifestsDir }

func (n *imageManifestsNode) List() ([]Row, error) {
	rows := make([]Row, 0, len(n.m.Manifests))
	for _, d := range n.m.Manifests {
		crumb := "@" + shortRef(d.Digest)
		rows = append(rows, childRow(n.b, n.repo, d, func(im *image.Image) Node {
			return &imageRootNode{b: n.b, repo: n.repo, crumb: crumb, im: im}
		}))
	}
	return rows, nil
}
