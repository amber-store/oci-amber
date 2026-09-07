package browse

import (
	"errors"
	"fmt"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// Browser builds nodes over an open store. It only reads.
type Browser struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
}

// New returns a Browser over st. blobs and images must be stores over the
// same st; blob.NewReadOnly is enough for blobs.
func New(st *store.Store, blobs *blob.Store, images *image.Store) *Browser {
	return &Browser{st: st, blobs: blobs, images: images}
}

// shortRef abbreviates a digest for rows and crumbs: "sha256:4f7c9a1e".
func shortRef(d oci.Digest) string { return d.Algorithm() + ":" + tui.ShortDigest(d) }

// plural renders n with word: "1 tag", "1,234 files".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%s %ss", tui.FormatCount(int64(n)), word)
}

// rootNode is the repository listing.
func (b *Browser) rootNode() *reposNode { return &reposNode{b: b} }

// repoNode is one repository's listing, or image.ErrNotFound.
func (b *Browser) repoNode(repo string) (*repoNode, error) {
	tags, err := b.images.Tags(repo)
	if err != nil {
		return nil, err
	}
	manifests, err := b.images.Manifests(repo)
	if err != nil && !errors.Is(err, image.ErrNotFound) {
		return nil, err
	}
	return &repoNode{b: b, repo: repo, tags: tags, manifests: manifests}, nil
}

// imageNode is the storage root of repo's reference, a tag or a digest,
// or image.ErrNotFound.
func (b *Browser) imageNode(repo, reference string) (*imageRootNode, error) {
	im, err := b.images.Open(repo, reference)
	if err != nil {
		return nil, err
	}
	crumb := ":" + reference
	if oci.IsDigest(reference) {
		crumb = "@" + shortRef(im.Meta.Digest)
	}
	return &imageRootNode{b: b, repo: repo, crumb: crumb, im: im}, nil
}
