package blob

import (
	"fmt"

	"github.com/amber-store/oci-amber/oci"
)

// RawRefusedError reports that a blob could only be stored raw and the
// store is configured to refuse that (Options.AllowRaw is false). Reason
// says why the blob is not a prism and Err carries the detail, zrecipe's
// error for instance. It is returned by Put; nothing is published.
type RawRefusedError struct {
	Digest oci.Digest
	Format string
	Reason RawReason
	Err    error
}

func (e *RawRefusedError) Error() string {
	why := string(e.Reason)
	if e.Err != nil {
		why += ": " + e.Err.Error()
	}
	return fmt.Sprintf("blob: %s cannot be stored as a prism (%s); raw layers are refused, start with --allow-raw to store them", e.Digest, why)
}

func (e *RawRefusedError) Unwrap() error { return e.Err }

// refuseRaw decides whether a blob that cannot be a prism may be stored
// raw. A not-tar blob (a config, an attestation, a non-tar artifact) is
// always stored raw: there is nothing else it could ever be. Every other
// reason arises only for a blob that looked like a tar or a compressed
// tar, in practice a layer, and keeping such a layer raw is what
// Options.AllowRaw opts into; without it the outcome is logged at error
// level, with the attributes the fallback lines carry, and returned as a
// *RawRefusedError for Put to hand back unchanged. meta supplies the
// digest, size and format; err is the detail behind reason, if any.
func (b *Store) refuseRaw(meta Meta, reason RawReason, err error) error {
	if reason == ReasonNotTar || b.opts.AllowRaw {
		return nil
	}
	b.log.Error("layer refused", "digest", meta.Digest, "size", meta.Size, "format", meta.Format, "reason", reason, "error", err)
	return &RawRefusedError{Digest: meta.Digest, Format: meta.Format, Reason: reason, Err: err}
}
