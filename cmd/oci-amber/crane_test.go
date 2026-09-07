package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

const (
	craneLayerGzip  = "application/vnd.oci.image.layer.v1.tar+gzip"
	craneLayoutFile = `{"imageLayoutVersion": "1.0.0"}`
	craneTimeout    = 2 * time.Minute
)

// TestCraneSmoke drives the real server wiring (`run`) with the real crane
// binary: push an image from an OCI layout, read it back in every way crane
// offers, append a layer, list, delete, restart the server on the same
// store and pull again. It is skipped when crane is not on PATH; the Nix
// dev shell provides it.
func TestCraneSmoke(t *testing.T) {
	if _, err := exec.LookPath("crane"); err != nil {
		t.Skip("crane is not on PATH; run under `nix develop` to exercise the real-client smoke test")
	}
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "store")
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	// An empty DOCKER_CONFIG keeps crane away from the developer's credentials.
	dockerCfg := filepath.Join(tmp, "docker")
	if err := os.MkdirAll(dockerCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "DOCKER_CONFIG="+dockerCfg)

	addr, stop := craneServer(t, storeDir, logs)
	repo := addr + "/crane/app"
	img := craneBuildLayout(t, filepath.Join(tmp, "layout"))
	manifestDigest := oci.DigestOfBytes(img.manifest)
	layerDigest := oci.DigestOfBytes(img.layer)
	configDigest := oci.DigestOfBytes(img.config)

	// crane push of an OCI layout: HEAD manifest, HEAD blobs, POST + PATCH +
	// PUT per blob, PUT manifest. It prints the pushed reference by digest.
	out := craneRun(t, env, "push", "--insecure", img.dir, repo+":v1")
	if !strings.Contains(out, "@"+manifestDigest.String()) {
		t.Fatalf("crane push printed %q, want the reference with digest %s", out, manifestDigest)
	}

	// The registry resolves the tag to the digest crane computed locally.
	if got := strings.TrimSpace(craneRun(t, env, "digest", "--insecure", repo+":v1")); got != manifestDigest.String() {
		t.Fatalf("crane digest = %q, want %s", got, manifestDigest)
	}
	// The manifest bytes are served verbatim.
	if got := craneRun(t, env, "manifest", "--insecure", repo+":v1"); !bytes.Equal(bytes.TrimRight([]byte(got), "\n"), img.manifest) {
		t.Fatalf("crane manifest = %q, want %q", got, img.manifest)
	}
	// crane validate pulls every layer and checks digests, sizes and diff IDs
	// against the config.
	craneRun(t, env, "validate", "--insecure", "--remote", repo+":v1")

	// Pull into an OCI layout and compare every blob byte for byte.
	pulled := filepath.Join(tmp, "pulled")
	craneRun(t, env, "pull", "--insecure", "--format=oci", repo+":v1", pulled)
	craneCompareLayout(t, pulled, img.blobs())

	// Storage classification, visible through the API: raw blobs advertise
	// ranges, prisms do not. Go's gzip is reproducible, so the layer is a prism.
	if got := craneHeadBlob(t, addr, "crane/app", configDigest).Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("config blob: Accept-Ranges = %q, want bytes (raw)", got)
	}
	if got := craneHeadBlob(t, addr, "crane/app", layerDigest).Get("Accept-Ranges"); got != "" {
		t.Fatalf("layer blob: Accept-Ranges = %q, want none (prism)", got)
	}

	// crane append compresses a plain tarball itself and pushes a new image on
	// top of v1: the existing layer is found with HEAD and not re-uploaded.
	layer2 := filepath.Join(tmp, "layer2.tar")
	if err := os.WriteFile(layer2, craneTar(t, map[string][]byte{"etc/extra": []byte("extra\n"), "usr/share/data": craneRandom(11, 100<<10)}), 0o644); err != nil {
		t.Fatal(err)
	}
	craneRun(t, env, "append", "--insecure", "-b", repo+":v1", "-f", layer2, "-t", repo+":v2")
	craneRun(t, env, "validate", "--insecure", "--remote", repo+":v2")
	v2Manifest := bytes.TrimRight([]byte(craneRun(t, env, "manifest", "--insecure", repo+":v2")), "\n")
	m2, err := oci.ParseManifest(v2Manifest)
	if err != nil {
		t.Fatalf("v2 manifest: %v", err)
	}
	if len(m2.Layers) != 2 || m2.Layers[0].Digest != layerDigest {
		t.Fatalf("v2 layers = %+v, want the v1 layer %s followed by the appended one", m2.Layers, layerDigest)
	}
	appendedLayer := m2.Layers[1].Digest

	// Tags and catalog.
	if got := strings.Fields(craneRun(t, env, "ls", "--insecure", repo)); !slices.Equal(got, []string{"v1", "v2"}) {
		t.Fatalf("crane ls = %v, want [v1 v2]", got)
	}
	if got := strings.Fields(craneRun(t, env, "catalog", "--insecure", addr)); !slices.Equal(got, []string{"crane/app"}) {
		t.Fatalf("crane catalog = %v, want [crane/app]", got)
	}

	// Delete a tag; the other image stays valid.
	craneRun(t, env, "delete", "--insecure", repo+":v1")
	if got := strings.Fields(craneRun(t, env, "ls", "--insecure", repo)); !slices.Equal(got, []string{"v2"}) {
		t.Fatalf("crane ls after delete = %v, want [v2]", got)
	}
	craneRun(t, env, "validate", "--insecure", "--remote", repo+":v2")

	// Restart on the same store: everything pushed survives, and a fresh
	// pull still reproduces the original layer bytes.
	stop()
	addr2, stop2 := craneServer(t, storeDir, logs)
	repo2 := addr2 + "/crane/app"
	if got := strings.Fields(craneRun(t, env, "ls", "--insecure", repo2)); !slices.Equal(got, []string{"v2"}) {
		t.Fatalf("crane ls after restart = %v, want [v2]", got)
	}
	if got := bytes.TrimRight([]byte(craneRun(t, env, "manifest", "--insecure", repo2+":v2")), "\n"); !bytes.Equal(got, v2Manifest) {
		t.Fatalf("v2 manifest after restart = %q, want %q", got, v2Manifest)
	}
	craneRun(t, env, "validate", "--insecure", "--remote", repo2+":v2")
	pulled2 := filepath.Join(tmp, "pulled2")
	craneRun(t, env, "pull", "--insecure", "--format=oci", repo2+":v2", pulled2)
	craneCompareLayout(t, pulled2, map[oci.Digest][]byte{layerDigest: img.layer, oci.DigestOfBytes(v2Manifest): v2Manifest})
	stop2()

	// With the server stopped, look at how the blobs were stored.
	metas := craneInspect(t, storeDir, layerDigest, configDigest, appendedLayer)
	if m := metas[layerDigest]; m.Kind != blob.KindPrism || m.Format != "gzip" {
		t.Fatalf("Go-gzip layer stored as kind=%s format=%s reason=%q, want prism/gzip", m.Kind, m.Format, m.RawReason)
	}
	if m := metas[configDigest]; m.Kind != blob.KindRaw || m.RawReason != blob.ReasonNotTar {
		t.Fatalf("config stored as kind=%s reason=%q, want raw/not-tar", m.Kind, m.RawReason)
	}
	// crane compresses with Go's gzip at best speed over incompressible
	// data. zrecipe v0.1.0 (then named comp-prysm) accepted such streams
	// with parameters it could not reproduce, so this layer used to fall
	// back to raw; v0.2.0 fixed that and the layer must be a prism now.
	if m := metas[appendedLayer]; m.Kind != blob.KindPrism || m.Format != "gzip" {
		t.Fatalf("crane-compressed layer %s stored as kind=%s format=%s reason=%q, want prism/gzip", appendedLayer, m.Kind, m.Format, m.RawReason)
	}
}

// craneServer starts `run` on a random loopback port over storeDir and
// returns the address and a stop function that shuts the server down and
// waits for run to return. stop is idempotent and also registered as a
// cleanup.
func craneServer(t *testing.T, storeDir string, logs io.Writer) (addr string, stop func()) {
	t.Helper()
	cfg := testConfig(storeDir, logs)
	addrCh := make(chan net.Addr, 1)
	cfg.OnListen = func(a net.Addr) { addrCh <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	select {
	case a := <-addrCh:
		addr = a.String()
	case err := <-done:
		cancel()
		t.Fatalf("run exited before listening: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the server to listen")
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run returned %v after cancel, want nil", err)
			}
		case <-time.After(shutdownTimeout + 10*time.Second):
			t.Errorf("run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return addr, stop
}

// craneRun runs crane with args and returns its stdout. Both streams are
// logged so -v shows what crane did.
func craneRun(t *testing.T, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), craneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "crane", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	t.Logf("$ crane %s\n%s%s", strings.Join(args, " "), stdout.String(), stderr.String())
	if err != nil {
		t.Fatalf("crane %s: %v", strings.Join(args, " "), err)
	}
	return stdout.String()
}

func craneHeadBlob(t *testing.T, addr, name string, d oci.Digest) http.Header {
	t.Helper()
	resp, err := http.Head("http://" + addr + "/v2/" + name + "/blobs/" + d.String())
	if err != nil {
		t.Fatalf("HEAD blob %s: %v", d, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD blob %s: status %d, want 200", d, resp.StatusCode)
	}
	return resp.Header
}

// craneImage is the OCI layout crane pushes, with the exact bytes of each blob.
type craneImage struct {
	dir      string
	config   []byte
	layer    []byte
	manifest []byte
}

func (img *craneImage) blobs() map[oci.Digest][]byte {
	return map[oci.Digest][]byte{
		oci.DigestOfBytes(img.config):   img.config,
		oci.DigestOfBytes(img.layer):    img.layer,
		oci.DigestOfBytes(img.manifest): img.manifest,
	}
}

// craneConfig is the minimum OCI image config crane validate accepts.
type craneConfig struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	Config       struct{} `json:"config"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type craneIndex struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Manifests     []oci.Descriptor `json:"manifests"`
}

func craneJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func craneRandom(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// craneTar writes files (sorted by name) as regular entries of a tar archive.
func craneTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(files[name])), ModTime: time.Unix(1_700_000_000, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func craneWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// craneBuildLayout writes a single-image OCI layout to dir: a gzip layer
// produced by compress/gzip, a config whose diff_ids match it, the
// manifest, index.json and oci-layout.
func craneBuildLayout(t *testing.T, dir string) *craneImage {
	t.Helper()
	tarBytes := craneTar(t, map[string][]byte{
		"etc/motd":     bytes.Repeat([]byte("hello from oci-amber\n"), 1000),
		"usr/bin/tool": craneRandom(7, 256<<10),
	})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tarBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	layer := gz.Bytes()

	var cfg craneConfig
	cfg.Architecture = "amd64"
	cfg.OS = "linux"
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []string{oci.DigestOfBytes(tarBytes).String()}
	config := craneJSON(t, cfg)

	manifest := craneJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: oci.DigestOfBytes(config), Size: int64(len(config))},
		Layers:        []oci.Descriptor{{MediaType: craneLayerGzip, Digest: oci.DigestOfBytes(layer), Size: int64(len(layer))}},
	})
	index := craneJSON(t, craneIndex{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIIndex,
		Manifests:     []oci.Descriptor{{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(manifest), Size: int64(len(manifest))}},
	})

	img := &craneImage{dir: dir, config: config, layer: layer, manifest: manifest}
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	craneWrite(t, filepath.Join(dir, "oci-layout"), []byte(craneLayoutFile))
	craneWrite(t, filepath.Join(dir, "index.json"), index)
	for d, b := range img.blobs() {
		craneWrite(t, filepath.Join(dir, "blobs", "sha256", d.Hex()), b)
	}
	return img
}

// craneCompareLayout checks that every blob under dir/blobs/sha256 hashes to
// its file name and that every expected blob is present and byte-identical.
func craneCompareLayout(t *testing.T, dir string, want map[oci.Digest][]byte) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("pulled layout %s has no index.json: %v", dir, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("reading pulled layout %s: %v", dir, err)
	}
	got := map[oci.Digest][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		d := oci.DigestOfBytes(b)
		if d.Hex() != e.Name() {
			t.Fatalf("pulled blob %s hashes to %s", e.Name(), d)
		}
		got[d] = b
	}
	for d, w := range want {
		g, ok := got[d]
		if !ok {
			t.Fatalf("pulled layout %s lacks %s", dir, d)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("pulled blob %s differs from the pushed bytes (%d vs %d bytes)", d, len(g), len(w))
		}
	}
}

// craneInspect reopens the (stopped) store and returns the stored Meta of
// each digest, logging how each blob was classified.
func craneInspect(t *testing.T, storeDir string, digests ...oci.Digest) map[oci.Digest]blob.Meta {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(storeDir, store.Options{Logger: log})
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st.Close()
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               t.TempDir(),
		MaxInMemory:           1 << 20,
		AnalyzeParallelism:    1,
		AnalyzeTimeout:        time.Minute,
		MaxConcurrentFinalize: 1,
		VerifyRoundTrip:       true,
		RecentTTL:             time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	out := map[oci.Digest]blob.Meta{}
	for _, d := range digests {
		bl, err := blobs.Open(d)
		if err != nil {
			t.Fatalf("blob %s: %v", d, err)
		}
		t.Logf("blob %s: kind=%s format=%s raw_reason=%q engine=%s entries=%d", d, bl.Meta.Kind, bl.Meta.Format, bl.Meta.RawReason, bl.Meta.Engine, bl.Meta.Entries)
		out[d] = bl.Meta
	}
	return out
}

// TestCraneReadsSavedArchive pushes an image with crane, writes it out
// with the save command and pushes the archive back with crane, which
// reads it as a docker save tarball: manifest.json, the config and the
// layers. It is skipped when crane is not on PATH.
func TestCraneReadsSavedArchive(t *testing.T) {
	if _, err := exec.LookPath("crane"); err != nil {
		t.Skip("crane is not on PATH; run under `nix develop` to exercise the real-client smoke test")
	}
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "store")
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	dockerCfg := filepath.Join(tmp, "docker")
	if err := os.MkdirAll(dockerCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "DOCKER_CONFIG="+dockerCfg)

	addr, stop := craneServer(t, storeDir, logs)
	img := craneBuildLayout(t, filepath.Join(tmp, "layout"))
	craneRun(t, env, "push", "--insecure", img.dir, addr+"/crane/app:v1")
	stop()

	// The store is free: save the image the way docker would.
	saved := filepath.Join(tmp, "saved.tar")
	var stderr bytes.Buffer
	if err := runSave(context.Background(), saveConfig{Store: storeDir, Refs: []string{"crane/app:v1"}, Output: saved, Stdout: io.Discard, Stderr: &stderr}); err != nil {
		t.Fatalf("save: %v\n%s", err, stderr.String())
	}

	// crane push of a tarball rebuilds the manifest from manifest.json's
	// config and layers, so the layer and config digests survive and
	// validate reads every layer back.
	addr2, stop2 := craneServer(t, storeDir, logs)
	defer stop2()
	repo := addr2 + "/crane/loaded"
	craneRun(t, env, "push", "--insecure", saved, repo+":v1")
	craneRun(t, env, "validate", "--insecure", "--remote", repo+":v1")
	m, err := oci.ParseManifest(bytes.TrimRight([]byte(craneRun(t, env, "manifest", "--insecure", repo+":v1")), "\n"))
	if err != nil {
		t.Fatalf("manifest of the re-pushed image: %v", err)
	}
	if m.Config == nil || m.Config.Digest != oci.DigestOfBytes(img.config) {
		t.Errorf("config = %+v, want %s", m.Config, oci.DigestOfBytes(img.config))
	}
	if len(m.Layers) != 1 || m.Layers[0].Digest != oci.DigestOfBytes(img.layer) {
		t.Errorf("layers = %+v, want [%s]", m.Layers, oci.DigestOfBytes(img.layer))
	}
}
