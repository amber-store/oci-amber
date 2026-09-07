package blob

import (
	"context"
	"fmt"
	"io"

	"github.com/amber-store/core/key"

	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// ingestRaw stores the spool's bytes verbatim through w (spec finalization
// step 7) and returns the content key of the root's "raw" file. w is bound
// to ctx, so a cancelled request fails the first Emit; the explicit check
// only saves opening the spool.
func (b *Store) ingestRaw(ctx context.Context, w *store.Writer, sp *upload.Spool) (key.Key, error) {
	if err := ctx.Err(); err != nil {
		return key.Key{}, err
	}
	r, err := sp.Open()
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: opening spool: %w", err)
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	k, err := w.PutStream(b.observeReader(sp.Digest(), r))
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: storing raw bytes: %w", err)
	}
	if got := int64(k.Length()); got != sp.Size() {
		return key.Key{}, fmt.Errorf("blob: stored %d raw bytes, spool holds %d", got, sp.Size())
	}
	return k, nil
}
