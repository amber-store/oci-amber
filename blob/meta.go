// Package blob stores one OCI blob per amber reference. A blob root is a
// directory object holding meta.json plus either the verbatim bytes ("raw")
// or the zrecipe/tar-prism decomposition ("prism").
package blob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

// Kind says how a blob's bytes are kept in the store.
type Kind string

const (
	// KindPrism: the blob was taken apart with zrecipe and tar-prism. The
	// root holds comp.json, recipe.bin, recipe.json and blobs/.
	KindPrism Kind = "prism"
	// KindRaw: the root holds the uploaded bytes verbatim under "raw".
	KindRaw Kind = "raw"
)

// RawReason records why a blob was stored raw. It is empty for prisms.
type RawReason string

const (
	ReasonNotReproducible RawReason = "not-reproducible"
	ReasonUnsupported     RawReason = "unsupported"
	ReasonCorrupt         RawReason = "corrupt"
	ReasonNotTar          RawReason = "not-tar"
	ReasonAnalyzeTimeout  RawReason = "analyze-timeout"
	ReasonRoundTripFailed RawReason = "roundtrip-failed"
	ReasonDecomposeFailed RawReason = "decompose-failed"
)

// MetaVersion is the schema version written to meta.json.
const MetaVersion = 1

// Meta is the content of a blob root's meta.json.
type Meta struct {
	Version          int         `json:"version"`
	Digest           oci.Digest  `json:"digest"`
	Size             int64       `json:"size"`
	Kind             Kind        `json:"kind"`
	Format           string      `json:"format"` // "gzip" | "zstd" | "none"
	RawReason        RawReason   `json:"rawReason,omitempty"`
	DiffID           oci.Digest  `json:"diffId,omitempty"`
	UncompressedSize int64       `json:"uncompressedSize,omitempty"`
	Entries          int         `json:"entries,omitempty"`
	Engine           string      `json:"engine,omitempty"`
	EngineVersion    string      `json:"engineVersion,omitempty"`
	UploadedAt       time.Time   `json:"uploadedAt"`
	Stats            store.Stats `json:"stats"`
}

// Names inside a blob root and under the work directory. The prism-only
// root entries (recipe.bin, recipe.json, blobs/) are tar-prism's own
// constants.
const (
	MetaFile = "meta.json"
	CompFile = "comp.json"
	RawFile  = "raw"

	// spoolDirName is zrecipe's temp directory under Options.WorkDir; New
	// empties it at startup.
	spoolDirName = "spool"
)

// RefName returns the amber reference name that points at d's blob root.
func RefName(d oci.Digest) string { return "oci/blob/" + d.String() }

// encodeMeta renders m as indented JSON with a trailing newline, the shape
// zrecipe's Params.Write and tar-prism's index file use.
func encodeMeta(m Meta) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("blob: encoding meta: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeMeta parses meta.json and checks its schema version.
func decodeMeta(b []byte) (Meta, error) {
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("blob: decoding meta: %w", err)
	}
	if m.Version != MetaVersion {
		return Meta{}, fmt.Errorf("blob: unsupported meta version %d (want %d)", m.Version, MetaVersion)
	}
	return m, nil
}
