package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/oci"
)

// fixture is a store holding two images imported from one archive:
//
//   - demo/app, tagged v1 and latest: an image manifest with a config and
//     one gzip tar layer, so its rootfs is ok
//   - multi:latest: an index with a linux/amd64 and a linux/arm64 child
//     whose layers are not tars, plus an attestation
type fixture struct {
	store   string
	archive []byte
	// app, amd, arm and att are the manifest descriptors; index the
	// index's. appConfig and appLayer are demo/app's blobs.
	app, amd, arm, att, index oci.Descriptor
	appConfig, appLayer       []byte
	blobs                     map[oci.Digest][]byte // every config and layer
}

// newFixture builds the archive and imports it into a fresh store.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{blobs: map[oci.Digest][]byte{}}
	b := archivetest.New()
	remember := func(data []byte) []byte {
		f.blobs[oci.DigestOfBytes(data)] = data
		return data
	}

	tarBytes := craneTar(t, map[string][]byte{"etc/motd": []byte("hello\n"), "bin/tool": craneRandom(3, 4<<10)})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.appLayer = remember(gz.Bytes())
	f.appConfig = remember(mustJSON(t, map[string]any{
		"architecture": "amd64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{oci.DigestOfBytes(tarBytes).String()}},
	}))
	f.app = b.AddImage(f.appConfig, []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: f.appLayer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)

	amdConfig := remember([]byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
	armConfig := remember([]byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
	amdLayer := remember([]byte("amd64 layer, not a tar"))
	armLayer := remember([]byte("arm64 layer, not a tar"))
	f.amd = b.AddImage(amdConfig, []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: amdLayer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	f.arm = b.AddImage(armConfig, []archivetest.Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: armLayer}}, &oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, nil)
	attConfig, attLayers, attPlatform, attAnnotations := archivetest.Attestation(f.amd)
	remember(attConfig)
	for _, l := range attLayers {
		remember(l.Data)
	}
	f.att = b.AddImage(attConfig, attLayers, attPlatform, attAnnotations)
	f.index = b.AddIndex([]oci.Descriptor{f.amd, f.arm, f.att}, nil)

	b.Top(f.app, f.index)
	b.Legacy(
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(f.appConfig), RepoTags: []string{"demo/app:v1", "demo/app:latest"}, Layers: []oci.Digest{oci.DigestOfBytes(f.appLayer)}},
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(amdConfig), RepoTags: []string{"multi:latest"}, Layers: []oci.Digest{oci.DigestOfBytes(amdLayer)}},
	)
	f.archive = b.Bytes()

	tmp := t.TempDir()
	f.store = filepath.Join(tmp, "store")
	path, err := b.WriteFile(tmp, "fixture.tar")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cfg := importConfig{
		Store: f.store, MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelWarn,
		Archive: path, Progress: "plain", Stdout: io.Discard, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("importing the fixture: %v\n%s", err, stderr.String())
	}
	return f
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
