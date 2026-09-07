// Package archivetest builds `docker image save` archives in memory for
// tests: an OCI layout tar with the file order Docker 29 writes (blobs
// first, then index.json, manifest.json and oci-layout).
package archivetest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/amber-store/oci-amber/oci"
)

// Layer is one layer to add to an image.
type Layer struct {
	MediaType string
	Data      []byte
}

// LegacyEntry is one manifest.json entry.
type LegacyEntry struct {
	Config   oci.Digest
	RepoTags []string
	Layers   []oci.Digest
}

type blob struct {
	digest oci.Digest
	data   []byte
}

// Builder accumulates blobs and the three top-level files.
type Builder struct {
	blobs   []blob
	top     []oci.Descriptor
	legacy  []LegacyEntry
	noIndex bool
}

// New returns an empty builder.
func New() *Builder { return &Builder{} }

// AddBlob adds data under its sha256.
func (b *Builder) AddBlob(data []byte) oci.Digest {
	d := oci.DigestOfBytes(data)
	b.AddBlobAs(d, data)
	return d
}

// AddBlobAs adds data under d, which need not match; use it to build a
// corrupt archive.
func (b *Builder) AddBlobAs(d oci.Digest, data []byte) {
	for _, existing := range b.blobs {
		if existing.digest == d {
			return
		}
	}
	b.blobs = append(b.blobs, blob{digest: d, data: data})
}

// AddImage adds config, every layer and an OCI image manifest over them,
// and returns the manifest's descriptor carrying platform and annotations.
func (b *Builder) AddImage(config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string) oci.Descriptor {
	m := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
	}
	for _, l := range layers {
		m.Layers = append(m.Layers, oci.Descriptor{MediaType: l.MediaType, Digest: b.AddBlob(l.Data), Size: int64(len(l.Data))})
	}
	body := mustJSON(m)
	return oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: b.AddBlob(body), Size: int64(len(body)), Platform: platform, Annotations: annotations}
}

// AddIndex adds an OCI index over children (present or absent) and returns
// its descriptor.
func (b *Builder) AddIndex(children []oci.Descriptor, annotations map[string]string) oci.Descriptor {
	m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: children, Annotations: annotations}
	body := mustJSON(m)
	return oci.Descriptor{MediaType: oci.MediaTypeOCIIndex, Digest: b.AddBlob(body), Size: int64(len(body))}
}

// AbsentManifest is a descriptor for a platform whose manifest was not
// saved: a plausible digest that is in no archive.
func AbsentManifest(p oci.Platform) oci.Descriptor {
	body := []byte("absent " + p.String())
	return oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(body), Size: 610, Platform: &p}
}

// Attestation returns the parts of a BuildKit attestation manifest for
// target: an empty config, one in-toto layer, the unknown/unknown platform
// and the vnd.docker.reference annotations. Pass them to AddImage.
func Attestation(target oci.Descriptor) (config []byte, layers []Layer, platform *oci.Platform, annotations map[string]string) {
	config = []byte(`{"architecture":"unknown","os":"unknown","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	layers = []Layer{{MediaType: "application/vnd.in-toto+json", Data: []byte(`{"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://spdx.dev/Document","subject":[]}`)}}
	platform = &oci.Platform{OS: "unknown", Architecture: "unknown"}
	annotations = map[string]string{
		"vnd.docker.reference.digest": target.Digest.String(),
		"vnd.docker.reference.type":   "attestation-manifest",
	}
	return
}

// Top sets index.json's manifests.
func (b *Builder) Top(entries ...oci.Descriptor) { b.top = entries }

// Legacy sets manifest.json's entries.
func (b *Builder) Legacy(entries ...LegacyEntry) { b.legacy = entries }

// NoIndex omits index.json, producing a legacy-only archive.
func (b *Builder) NoIndex() { b.noIndex = true }

// Bytes renders the archive.
func (b *Builder) Bytes() []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := func(name string) {
		must(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}))
	}
	file := func(name string, mode int64, data []byte) {
		must(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}))
		_, err := tw.Write(data)
		must(err)
	}
	dir("blobs/")
	dir("blobs/sha256/")
	for _, bl := range b.blobs {
		file("blobs/sha256/"+bl.digest.Hex(), 0o444, bl.data)
	}
	if !b.noIndex {
		file("index.json", 0o644, mustJSON(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: b.top}))
	}
	type legacyJSON struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	legacy := make([]legacyJSON, 0, len(b.legacy))
	for _, e := range b.legacy {
		l := legacyJSON{Config: "blobs/sha256/" + e.Config.Hex(), RepoTags: e.RepoTags}
		for _, d := range e.Layers {
			l.Layers = append(l.Layers, "blobs/sha256/"+d.Hex())
		}
		legacy = append(legacy, l)
	}
	file("manifest.json", 0o644, mustJSON(legacy))
	file("oci-layout", 0o444, []byte(`{"imageLayoutVersion":"1.0.0"}`))
	must(tw.Close())
	return buf.Bytes()
}

// WriteFile renders the archive to <dir>/<name> and returns the path.
func (b *Builder) WriteFile(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, b.Bytes(), 0o644)
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	must(err)
	return out
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
