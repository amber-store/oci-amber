package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/amber-store/core/key"

	"github.com/amber-store/oci-amber/oci"
)

// Blob is an opened blob: its meta.json and the key of its root directory.
type Blob struct {
	Meta  Meta
	root  key.Key
	store *Store
}

// Root returns the key of the blob root directory.
func (bl *Blob) Root() key.Key { return bl.root }

// ErrNotPrism reports a raw blob where a prism was needed.
var ErrNotPrism = errors.New("blob: not a prism")

// Prism returns the parts of a prism blob root. A raw blob returns an error
// wrapping ErrNotPrism that names the raw reason.
func (bl *Blob) Prism() (*Prism, error) {
	if bl.Meta.Kind != KindPrism {
		return nil, fmt.Errorf("%w: %s is stored %s (%s)", ErrNotPrism, bl.Meta.Digest, bl.Meta.Kind, bl.Meta.RawReason)
	}
	return bl.store.openSource(bl.root)
}

// SupportsRange reports whether WriteRange can serve this blob. Only raw
// blobs support ranges; prisms are always served whole.
func (bl *Blob) SupportsRange() bool { return bl.Meta.Kind == KindRaw }

// WriteTo streams the whole blob to w. The bytes are hashed on the way out
// and ErrDigestMismatch is returned, after the body has been written, when
// they do not match Meta.Digest. Writes stop within one write once ctx is
// done.
func (bl *Blob) WriteTo(ctx context.Context, w io.Writer) error {
	h := sha256.New()
	out := io.MultiWriter(&cancelWriter{ctx: ctx, w: w}, h)
	var err error
	switch bl.Meta.Kind {
	case KindRaw:
		err = bl.store.writeRaw(out, bl.root)
	case KindPrism:
		params, perr := bl.store.readParams(bl.root)
		if perr != nil {
			err = perr
		} else {
			err = bl.store.writePrism(ctx, out, bl.root, params)
		}
	default:
		err = fmt.Errorf("blob: %s has unknown kind %q", bl.Meta.Digest, bl.Meta.Kind)
	}
	if err != nil {
		return err
	}
	if got := oci.DigestFromSum(h.Sum(nil)); got != bl.Meta.Digest {
		return fmt.Errorf("%w: %s was served as %s", ErrDigestMismatch, bl.Meta.Digest, got)
	}
	return nil
}

// WriteRange writes bytes start..end (inclusive) of a raw blob to w. Whole
// chunks that end before start are skipped without being read. The range
// is validated against Meta.Size before anything is written and is not
// digest-verified.
func (bl *Blob) WriteRange(ctx context.Context, w io.Writer, start, end int64) error {
	if !bl.SupportsRange() {
		return fmt.Errorf("blob: %s is a %s blob and cannot serve ranges", bl.Meta.Digest, bl.Meta.Kind)
	}
	if start < 0 || end < start || end >= bl.Meta.Size {
		return fmt.Errorf("blob: range %d-%d is not satisfiable for %d bytes", start, end, bl.Meta.Size)
	}
	k, err := bl.store.st.LookupKey(bl.root, RawFile)
	if err != nil {
		return fmt.Errorf("blob: %s: %w", RawFile, err)
	}
	r := bl.store.st.NewReader(k)
	defer r.Close()
	if err := r.Skip(start); err != nil {
		return fmt.Errorf("blob: skipping to %d: %w", start, err)
	}
	if _, err := io.CopyN(&cancelWriter{ctx: ctx, w: w}, r, end-start+1); err != nil {
		return err
	}
	return nil
}

// writeRaw streams the verbatim bytes of a raw blob root.
func (b *Store) writeRaw(w io.Writer, root key.Key) error {
	k, err := b.st.LookupKey(root, RawFile)
	if err != nil {
		return fmt.Errorf("blob: %s: %w", RawFile, err)
	}
	return b.st.WriteContent(w, k)
}

// cancelWriter fails writes once ctx is done so a disconnected or cancelled
// pull stops within one write.
type cancelWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
