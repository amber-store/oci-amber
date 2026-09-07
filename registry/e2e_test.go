package registry_test

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/registry"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// Media types the fixtures use that the oci package does not name.
const (
	e2eLayerGzip       = "application/vnd.oci.image.layer.v1.tar+gzip"
	e2eLayerTar        = "application/vnd.oci.image.layer.v1.tar"
	e2eDockerConfig    = "application/vnd.docker.container.image.v1+json"
	e2eDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	e2eEmptyConfig     = "application/vnd.oci.empty.v1+json"
	e2eSBOMType        = "application/vnd.example.sbom+json"
	e2eOctetStream     = "application/octet-stream"
)

// Repositories used by the scenario.
const (
	e2eApp  = "library/app"
	e2eBase = "tools/base"
)

// e2eMaxInMemory is the upload-session and zrecipe spool threshold. Layers
// A and C are larger, so both spill paths run and their sessions are backed
// by a file under <work>/oci-amber/uploads/<id>.
const e2eMaxInMemory = 1 << 20

// Regular-file counts of the fixture tars: tar-prism makes one blob per
// regular file (empty files included); directories, symlinks and hard links
// are header-only.
const (
	e2eEntriesA = 6 // bin/app, etc/config.yaml, lib/libfoo.so, share/readme.txt, the PAX-named NOTICE, var/empty
	e2eEntriesB = 5 // etc/hostname, etc/hosts, etc/os-release, etc/empty, .wh.var (both zero-length regular files)
	e2eEntriesC = 3 // bin/app, etc/extra.conf, lib/libfoo.so
)

// TestE2EPushPull drives the registry over HTTP exactly the way real
// clients do, end to end:
//
//   - two OCI image manifests sharing a layer, an OCI index with both as
//     children, an artifact manifest whose subject is the first image, and a
//     Docker v2 manifest in a second repository built entirely from mounts;
//   - every upload style in the wild: chunked PATCH with Content-Range and a
//     416 resume (docker), single PATCH (podman, ggcr), POST + PUT with body
//     (containerd), monolithic POST ?digest=, cross-repository mounts, and a
//     PUT that fails with 500 and is retried on the retained session;
//   - the per-blob and per-image log lines with their accounting;
//   - the amber side: blob root layout, meta.json shape per kind, stats;
//   - a pull that walks from the index tag down to every blob and checks
//     bytes, digests, sizes, media types and headers;
//   - tags/list, _catalog and referrers with pagination and filters;
//   - deletes by tag, by digest and of a blob.
func TestE2EPushPull(t *testing.T) {
	e := newE2EEnv(t)
	e.push()
	e.checkRootfs()
	e.fsAPI()
	e.checkLogs()
	e.storage()
	e.pull()
	e.lists()
	e.deletes()
}

// ---------------------------------------------------------------------------
// Environment

// e2eLogBuffer collects slog JSON records from all handler goroutines.
type e2eLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *e2eLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *e2eLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records returns every record whose msg attribute equals msg, in emission
// order.
func (b *e2eLogBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

type e2eEnv struct {
	t         *testing.T
	c         *e2eClient
	logs      *e2eLogBuffer
	st        *store.Store
	blobs     *blob.Store
	images    *image.Store
	tmp       string
	work      string
	uploadDir string
	fx        *e2eFixtures
	pushed    map[oci.Digest][]byte // every blob and manifest body, by digest
	types     map[oci.Digest]string // media type each manifest must be served with

	m1, m2, idx, art, base oci.Digest // manifest digests recorded during push
}

func newE2EEnv(t *testing.T) *e2eEnv { return newE2EEnvAllowRaw(t, false) }

// newE2EEnvAllowRaw builds the environment with blob.Options.AllowRaw as
// given. The scenario runs with it off: its layers all become prisms and
// its other blobs are not tars, which are stored raw whatever the setting.
// TestE2ERawRefused needs both.
func newE2EEnvAllowRaw(t *testing.T, allowRaw bool) *e2eEnv {
	t.Helper()
	logs := &e2eLogBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	// The same layout `oci-amber serve` builds: the operator names the work
	// directory, the registry owns <work-dir>/oci-amber inside it.
	work := filepath.Join(dir, "work")
	ownDir := filepath.Join(work, "oci-amber")
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               ownDir,
		MaxInMemory:           e2eMaxInMemory,
		AnalyzeParallelism:    2,
		AnalyzeTimeout:        15 * time.Minute,
		MaxConcurrentFinalize: 2,
		VerifyRoundTrip:       true,
		AllowRaw:              allowRaw,
		RecentTTL:             time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	uploadDir := filepath.Join(ownDir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uploads, err := upload.NewManager(uploadDir, e2eMaxInMemory, time.Hour, log)
	if err != nil {
		t.Fatalf("upload.NewManager: %v", err)
	}
	t.Cleanup(func() {
		if err := uploads.Close(); err != nil {
			t.Errorf("uploads.Close: %v", err)
		}
	})
	images := image.New(st, blobs, log)

	srv := httptest.NewServer(registry.New(blobs, images, uploads, log))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{
		t:         t,
		c:         &e2eClient{t: t, base: base, http: srv.Client()},
		logs:      logs,
		st:        st,
		blobs:     blobs,
		images:    images,
		tmp:       dir,
		work:      work,
		uploadDir: uploadDir,
		fx:        newE2EFixtures(t),
		pushed:    map[oci.Digest][]byte{},
		types:     map[oci.Digest]string{},
	}
}

// record remembers a pushed blob so the pull phase can compare bytes.
func (e *e2eEnv) record(d oci.Digest, body []byte) {
	e.t.Helper()
	if got := oci.DigestOfBytes(body); got != d {
		e.t.Fatalf("recorded digest %s does not match body digest %s", d, got)
	}
	e.pushed[d] = body
}

// recordManifest remembers a pushed manifest and the media type it must be
// served with.
func (e *e2eEnv) recordManifest(d oci.Digest, body []byte, mediaType string) {
	e.record(d, body)
	e.types[d] = mediaType
}

// ---------------------------------------------------------------------------
// Fixtures

type e2eFixtures struct {
	big, lib     []byte // random file contents shared between layers A and C
	tarA, layerA []byte // layer A: gzip (Go default level) -> prism
	tarB, layerB []byte // layer B: uncompressed tar -> prism, format none
	tarC, layerC []byte // layer C: gzip (Go best speed), shares big and lib with A
	cfg1, cfg2   []byte // image configs -> raw, not-tar
	m1, m2, idx  []byte // two OCI manifests and the index over them
	empty, sbom  []byte // artifact blobs
	art          []byte // artifact manifest with subject m1
	base         []byte // Docker v2 manifest in the second repository
}

type e2eEntry struct {
	name     string
	data     []byte
	dir      bool
	symlink  string
	hardlink string
}

// e2eTar builds a tar archive with archive/tar. Names longer than 100 bytes
// make the writer emit PAX headers, which tar-prism keeps in the recipe.
func e2eTar(t *testing.T, entries []e2eEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, en := range entries {
		hdr := &tar.Header{Name: en.name, Mode: 0o644, ModTime: time.Unix(1_700_000_000, 0), Uname: "root", Gname: "root"}
		switch {
		case en.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case en.symlink != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = en.symlink
			hdr.Mode = 0o777
		case en.hardlink != "":
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = en.hardlink
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(en.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", en.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(en.data); err != nil {
				t.Fatalf("tar data %s: %v", en.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	return buf.Bytes()
}

func e2eGzip(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func e2eRandom(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func e2eJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func e2eDescriptor(mediaType string, body []byte) oci.Descriptor {
	return oci.Descriptor{MediaType: mediaType, Digest: oci.DigestOfBytes(body), Size: int64(len(body))}
}

func e2ePtr[T any](v T) *T { return &v }

// e2eImageConfig is the subset of an OCI image config the fixtures need.
// The registry never interprets configs; they are just raw blobs to it.
type e2eImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

func e2eConfig(t *testing.T, arch string, tars ...[]byte) []byte {
	t.Helper()
	cfg := e2eImageConfig{Architecture: arch, OS: "linux"}
	cfg.RootFS.Type = "layers"
	for _, tb := range tars {
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, oci.DigestOfBytes(tb).String())
	}
	return e2eJSON(t, cfg)
}

// e2eIndex carries platform fields, which oci.Descriptor does not model.
type e2eIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []e2eIndexEntry `json:"manifests"`
}

type e2eIndexEntry struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}

func newE2EFixtures(t *testing.T) *e2eFixtures {
	t.Helper()
	fx := &e2eFixtures{}
	fx.big = e2eRandom(1, 2<<20)   // spans several 1 MiB max chunks and the 1 MiB spill threshold
	fx.lib = e2eRandom(2, 700<<10) // one more shared file
	readme := bytes.Repeat([]byte("oci-amber end-to-end fixture\n"), 512)
	longName := "share/doc/" + strings.Repeat("a-rather-long-directory-name/", 5) + "NOTICE" // > 100 bytes: PAX

	fx.tarA = e2eTar(t, []e2eEntry{
		{name: "bin/", dir: true},
		{name: "bin/app", data: fx.big},
		{name: "bin/app-link", symlink: "app"},
		{name: "etc/", dir: true},
		{name: "etc/config.yaml", data: []byte("listen: :8080\nworkers: 4\n")},
		{name: "lib/", dir: true},
		{name: "lib/libfoo.so", data: fx.lib},
		{name: "lib/libfoo.so.1", hardlink: "lib/libfoo.so"},
		{name: "share/", dir: true},
		{name: "share/readme.txt", data: readme},
		{name: longName, data: []byte("notice\n")},
		{name: "var/", dir: true},
		{name: "var/empty"},
	})
	fx.layerA = e2eGzip(t, fx.tarA, gzip.DefaultCompression)

	fx.tarB = e2eTar(t, []e2eEntry{
		{name: "etc/", dir: true},
		{name: "etc/hostname", data: []byte("e2e\n")},
		{name: "etc/hosts", data: []byte("127.0.0.1 localhost\n")},
		{name: "etc/os-release", data: []byte("ID=e2e\nVERSION_ID=1\n")},
		{name: "etc/empty"}, // a zero-length file
		{name: ".wh.var"},   // whiteout: removes layer A's var/ from the rootfs
	})
	fx.layerB = fx.tarB

	fx.tarC = e2eTar(t, []e2eEntry{
		{name: "bin/", dir: true},
		{name: "bin/app", data: fx.big},
		{name: "etc/", dir: true},
		{name: "etc/extra.conf", data: []byte("extra = true\n")},
		{name: "lib/", dir: true},
		{name: "lib/libfoo.so", data: fx.lib},
	})
	fx.layerC = e2eGzip(t, fx.tarC, gzip.BestSpeed)

	fx.cfg1 = e2eConfig(t, "amd64", fx.tarA, fx.tarB)
	fx.cfg2 = e2eConfig(t, "arm64", fx.tarA, fx.tarC)

	fx.m1 = e2eJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        e2ePtr(e2eDescriptor(oci.MediaTypeOCIConfig, fx.cfg1)),
		Layers:        []oci.Descriptor{e2eDescriptor(e2eLayerGzip, fx.layerA), e2eDescriptor(e2eLayerTar, fx.layerB)},
		Annotations:   map[string]string{"org.opencontainers.image.ref.name": "v1"},
	})
	fx.m2 = e2eJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        e2ePtr(e2eDescriptor(oci.MediaTypeOCIConfig, fx.cfg2)),
		Layers:        []oci.Descriptor{e2eDescriptor(e2eLayerGzip, fx.layerA), e2eDescriptor(e2eLayerGzip, fx.layerC)},
	})
	fx.idx = e2eJSON(t, e2eIndex{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIIndex,
		Manifests: []e2eIndexEntry{
			{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(fx.m1).String(), Size: int64(len(fx.m1)), Platform: map[string]string{"os": "linux", "architecture": "amd64"}},
			{MediaType: oci.MediaTypeOCIManifest, Digest: oci.DigestOfBytes(fx.m2).String(), Size: int64(len(fx.m2)), Platform: map[string]string{"os": "linux", "architecture": "arm64"}},
		},
	})

	fx.empty = []byte("{}")
	fx.sbom = e2eJSON(t, map[string]any{"bomFormat": "e2e", "components": []string{"bin/app", "lib/libfoo.so"}})
	fx.art = e2eJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		ArtifactType:  e2eSBOMType,
		Config:        e2ePtr(e2eDescriptor(e2eEmptyConfig, fx.empty)),
		Layers:        []oci.Descriptor{e2eDescriptor(e2eSBOMType, fx.sbom)},
		Subject:       e2ePtr(e2eDescriptor(oci.MediaTypeOCIManifest, fx.m1)),
		Annotations:   map[string]string{"org.example.note": "e2e"},
	})

	fx.base = e2eJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeDockerManifest,
		Config:        e2ePtr(e2eDescriptor(e2eDockerConfig, fx.cfg1)),
		Layers:        []oci.Descriptor{e2eDescriptor(e2eDockerLayerGzip, fx.layerA)},
	})
	return fx
}

// ---------------------------------------------------------------------------
// HTTP client that behaves like the real ones

type e2eClient struct {
	t    *testing.T
	base *url.URL
	http *http.Client
}

func (c *e2eClient) url(path string) string { return c.base.String() + path }

// resolve turns a Location header into an absolute URL the way ggcr,
// containerd and containers/image do: relative to the request URL.
func (c *e2eClient) resolve(resp *http.Response) string {
	c.t.Helper()
	loc := resp.Header.Get("Location")
	if loc == "" {
		c.t.Fatalf("%s %s: response %d has no Location header", resp.Request.Method, resp.Request.URL, resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		c.t.Fatalf("Location %q: %v", loc, err)
	}
	return resp.Request.URL.ResolveReference(u).String()
}

func (c *e2eClient) do(method, target string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, target, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: reading body: %v", method, target, err)
	}
	return resp, data
}

func (c *e2eClient) expect(resp *http.Response, body []byte, status int) {
	c.t.Helper()
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s: status %d, want %d; body %q", resp.Request.Method, resp.Request.URL, resp.StatusCode, status, body)
	}
}

func (c *e2eClient) expectHeader(resp *http.Response, name, want string) {
	c.t.Helper()
	if got := resp.Header.Get(name); got != want {
		c.t.Fatalf("%s %s: header %s = %q, want %q", resp.Request.Method, resp.Request.URL, name, got, want)
	}
}

func (c *e2eClient) expectLength(resp *http.Response, want int) {
	c.t.Helper()
	if resp.ContentLength != int64(want) {
		c.t.Fatalf("%s %s: Content-Length %d, want %d", resp.Request.Method, resp.Request.URL, resp.ContentLength, want)
	}
}

// expectError checks the status and the standard error envelope and returns
// the single error it carries.
func (c *e2eClient) expectError(resp *http.Response, body []byte, status int, code oci.ErrorCode) oci.Error {
	c.t.Helper()
	c.expect(resp, body, status)
	c.expectHeader(resp, "Content-Type", "application/json")
	var env oci.ErrorResponse
	if err := json.Unmarshal(body, &env); err != nil {
		c.t.Fatalf("%s %s: body %q is not an error envelope: %v", resp.Request.Method, resp.Request.URL, body, err)
	}
	if len(env.Errors) != 1 || env.Errors[0].Code != code {
		c.t.Fatalf("%s %s: errors = %+v, want exactly one %s", resp.Request.Method, resp.Request.URL, env.Errors, code)
	}
	return env.Errors[0]
}

// expectEmptyErrors checks the status and the `{"errors":[]}` envelope the
// registry uses for internal failures.
func (c *e2eClient) expectEmptyErrors(resp *http.Response, body []byte, status int) {
	c.t.Helper()
	c.expect(resp, body, status)
	var env oci.ErrorResponse
	if err := json.Unmarshal(body, &env); err != nil {
		c.t.Fatalf("%s %s: body %q is not an error envelope: %v", resp.Request.Method, resp.Request.URL, body, err)
	}
	if len(env.Errors) != 0 {
		c.t.Fatalf("%s %s: errors = %+v, want an empty list", resp.Request.Method, resp.Request.URL, env.Errors)
	}
}

// withDigest adds ?digest=<d> to an upload URL.
func (c *e2eClient) withDigest(uploadURL string, d oci.Digest) string {
	c.t.Helper()
	u, err := url.Parse(uploadURL)
	if err != nil {
		c.t.Fatal(err)
	}
	q := u.Query()
	q.Set("digest", d.String())
	u.RawQuery = q.Encode()
	return u.String()
}

// startUpload is POST /v2/<name>/blobs/uploads/ and returns the absolute
// upload URL and the session id.
func (c *e2eClient) startUpload(name string) (uploadURL, uuid string) {
	c.t.Helper()
	resp, body := c.do(http.MethodPost, c.url("/v2/"+name+"/blobs/uploads/"), nil, map[string]string{"Content-Type": "application/json"})
	c.expect(resp, body, http.StatusAccepted)
	c.expectHeader(resp, "Range", "0-0")
	uuid = resp.Header.Get("Docker-Upload-UUID")
	if uuid == "" {
		c.t.Fatalf("POST upload: no Docker-Upload-UUID")
	}
	uploadURL = c.resolve(resp)
	if want := c.url("/v2/" + name + "/blobs/uploads/" + uuid); !strings.HasPrefix(uploadURL, want) {
		c.t.Fatalf("POST upload: Location %q, want prefix %q", uploadURL, want)
	}
	return uploadURL, uuid
}

// patch appends chunk to the session; contentRange may be "" (podman,
// ggcr). It returns the next upload URL from the Location header, which
// every client follows, and the Range header.
func (c *e2eClient) patch(uploadURL string, chunk []byte, contentRange string) (next, rng string) {
	c.t.Helper()
	hdr := map[string]string{"Content-Type": e2eOctetStream}
	if contentRange != "" {
		hdr["Content-Range"] = contentRange
	}
	resp, body := c.do(http.MethodPatch, uploadURL, chunk, hdr)
	c.expect(resp, body, http.StatusAccepted)
	if resp.Header.Get("Docker-Upload-UUID") == "" {
		c.t.Fatalf("PATCH %s: no Docker-Upload-UUID", uploadURL)
	}
	return c.resolve(resp), resp.Header.Get("Range")
}

// sessionStatus is GET on a session, which resuming clients use to learn
// the offset: 204 with the cumulative Range and the session id.
func (c *e2eClient) sessionStatus(uploadURL, uuid string, size int) {
	c.t.Helper()
	resp, body := c.do(http.MethodGet, uploadURL, nil, nil)
	c.expect(resp, body, http.StatusNoContent)
	c.expectHeader(resp, "Range", fmt.Sprintf("0-%d", max(0, size-1)))
	c.expectHeader(resp, "Docker-Upload-UUID", uuid)
}

// finishUpload is PUT <uploadURL>?digest=<d> with an optional final body.
// Afterwards the session must be gone: finalization removes it from the
// map, so its id answers 404 BLOB_UPLOAD_UNKNOWN.
func (c *e2eClient) finishUpload(name, uploadURL string, d oci.Digest, body []byte) {
	c.t.Helper()
	resp, rb := c.do(http.MethodPut, c.withDigest(uploadURL, d), body, map[string]string{"Content-Type": e2eOctetStream})
	c.expect(resp, rb, http.StatusCreated)
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	if got, want := c.resolve(resp), c.url("/v2/"+name+"/blobs/"+d.String()); got != want {
		c.t.Fatalf("PUT upload: Location %q, want %q", got, want)
	}
	resp, rb = c.do(http.MethodGet, uploadURL, nil, nil)
	c.expectError(resp, rb, http.StatusNotFound, oci.CodeBlobUploadUnknown)
}

// pushChunked uploads like docker: one PATCH per chunk with Content-Range,
// then an empty PUT. After the first chunk it replays that chunk the way a
// client that lost the response would, expects 416 carrying the current
// Range, asks the session for its offset with GET, and continues from there.
func (c *e2eClient) pushChunked(name string, data []byte, chunk int) oci.Digest {
	c.t.Helper()
	d := oci.DigestOfBytes(data)
	uploadURL, uuid := c.startUpload(name)
	replayed := false
	for sent := 0; sent < len(data); {
		end := min(sent+chunk, len(data))
		contentRange := fmt.Sprintf("%d-%d", sent, end-1)
		next, rng := c.patch(uploadURL, data[sent:end], contentRange)
		if want := fmt.Sprintf("0-%d", end-1); rng != want {
			c.t.Fatalf("PATCH Range = %q, want %q", rng, want)
		}
		if !replayed && end < len(data) {
			replayed = true
			resp, body := c.do(http.MethodPatch, next, data[sent:end], map[string]string{"Content-Type": e2eOctetStream, "Content-Range": contentRange})
			c.expectError(resp, body, http.StatusRequestedRangeNotSatisfiable, oci.CodeBlobUploadInvalid)
			c.expectHeader(resp, "Range", rng)
			c.sessionStatus(next, uuid, end)
		}
		uploadURL = next
		sent = end
	}
	c.finishUpload(name, uploadURL, d, nil)
	return d
}

// pushSinglePatch uploads like podman and ggcr: one PATCH without
// Content-Range, then an empty PUT.
func (c *e2eClient) pushSinglePatch(name string, data []byte) oci.Digest {
	c.t.Helper()
	d := oci.DigestOfBytes(data)
	uploadURL, _ := c.startUpload(name)
	next, rng := c.patch(uploadURL, data, "")
	if want := fmt.Sprintf("0-%d", max(0, len(data)-1)); rng != want {
		c.t.Fatalf("PATCH Range = %q, want %q", rng, want)
	}
	c.finishUpload(name, next, d, nil)
	return d
}

// pushPutBody uploads like containerd: POST, then one PUT carrying the
// whole body.
func (c *e2eClient) pushPutBody(name string, data []byte) oci.Digest {
	c.t.Helper()
	d := oci.DigestOfBytes(data)
	uploadURL, _ := c.startUpload(name)
	c.finishUpload(name, uploadURL, d, data)
	return d
}

// pushMonolithic is the single-request POST ?digest= upload.
func (c *e2eClient) pushMonolithic(name string, data []byte) oci.Digest {
	c.t.Helper()
	d := oci.DigestOfBytes(data)
	resp, body := c.do(http.MethodPost, c.url("/v2/"+name+"/blobs/uploads/?digest="+url.QueryEscape(d.String())), data, map[string]string{"Content-Type": e2eOctetStream})
	c.expect(resp, body, http.StatusCreated)
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	if got, want := c.resolve(resp), c.url("/v2/"+name+"/blobs/"+d.String()); got != want {
		c.t.Fatalf("monolithic POST: Location %q, want %q", got, want)
	}
	return d
}

// mount is POST ?mount=<d>&from=<from>. A hit (201) returns true. On a miss
// the registry starts an ordinary upload, which is cancelled the way
// containers/image does, and false is returned.
func (c *e2eClient) mount(name string, d oci.Digest, from string) bool {
	c.t.Helper()
	target := c.url("/v2/" + name + "/blobs/uploads/?mount=" + url.QueryEscape(d.String()) + "&from=" + url.QueryEscape(from))
	resp, body := c.do(http.MethodPost, target, nil, nil)
	switch resp.StatusCode {
	case http.StatusCreated:
		c.expectHeader(resp, "Docker-Content-Digest", d.String())
		if got, want := c.resolve(resp), c.url("/v2/"+name+"/blobs/"+d.String()); got != want {
			c.t.Fatalf("mount: Location %q, want %q", got, want)
		}
		return true
	case http.StatusAccepted:
		if resp.Header.Get("Docker-Upload-UUID") == "" {
			c.t.Fatalf("mount miss: no Docker-Upload-UUID")
		}
		uploadURL := c.resolve(resp)
		if !strings.Contains(uploadURL, "/v2/"+name+"/blobs/uploads/") {
			c.t.Fatalf("mount miss: Location %q is not an upload URL", uploadURL)
		}
		resp, body = c.do(http.MethodDelete, uploadURL, nil, nil)
		c.expect(resp, body, http.StatusNoContent)
		return false
	default:
		c.t.Fatalf("mount: status %d, want 201 or 202; body %q", resp.StatusCode, body)
		return false
	}
}

func (c *e2eClient) headBlob(name string, d oci.Digest, size int) http.Header {
	c.t.Helper()
	resp, body := c.do(http.MethodHead, c.url("/v2/"+name+"/blobs/"+d.String()), nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectLength(resp, size)
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	c.expectHeader(resp, "Content-Type", e2eOctetStream)
	return resp.Header
}

func (c *e2eClient) getBlob(name string, d oci.Digest, want []byte) http.Header {
	c.t.Helper()
	resp, body := c.do(http.MethodGet, c.url("/v2/"+name+"/blobs/"+d.String()), nil, nil)
	c.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, want) {
		c.t.Fatalf("GET blob %s: body differs from the pushed bytes (%d vs %d bytes)", d, len(body), len(want))
	}
	if got := oci.DigestOfBytes(body); got != d {
		c.t.Fatalf("GET blob %s: body hashes to %s", d, got)
	}
	c.expectLength(resp, len(want))
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	c.expectHeader(resp, "Content-Type", e2eOctetStream)
	return resp.Header
}

// putManifest pushes body under reference (tag or digest) and returns the
// digest and the response headers. contentType "" sends no Content-Type.
func (c *e2eClient) putManifest(name, reference, contentType string, body []byte) (oci.Digest, http.Header) {
	c.t.Helper()
	hdr := map[string]string{}
	if contentType != "" {
		hdr["Content-Type"] = contentType
	}
	resp, rb := c.do(http.MethodPut, c.url("/v2/"+name+"/manifests/"+reference), body, hdr)
	c.expect(resp, rb, http.StatusCreated)
	d := oci.DigestOfBytes(body)
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	if got, want := c.resolve(resp), c.url("/v2/"+name+"/manifests/"+d.String()); got != want {
		c.t.Fatalf("PUT manifest: Location %q, want %q", got, want)
	}
	return d, resp.Header
}

// getManifest fetches reference with GET and HEAD, checks bytes, digest,
// length and media type, and returns the parsed manifest.
func (c *e2eClient) getManifest(name, reference string, want []byte, wantType string) *oci.Manifest {
	c.t.Helper()
	d := oci.DigestOfBytes(want)
	resp, body := c.do(http.MethodGet, c.url("/v2/"+name+"/manifests/"+reference), nil, nil)
	c.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, want) {
		c.t.Fatalf("GET manifest %s/%s: body %q differs from the pushed bytes %q", name, reference, body, want)
	}
	c.expectHeader(resp, "Content-Type", wantType)
	c.expectHeader(resp, "Docker-Content-Digest", d.String())
	c.expectLength(resp, len(want))

	head, hb := c.do(http.MethodHead, c.url("/v2/"+name+"/manifests/"+reference), nil, nil)
	c.expect(head, hb, http.StatusOK)
	c.expectHeader(head, "Content-Type", wantType)
	c.expectHeader(head, "Docker-Content-Digest", d.String())
	c.expectLength(head, len(want))

	m, err := oci.ParseManifest(body)
	if err != nil {
		c.t.Fatalf("GET manifest %s/%s: %v", name, reference, err)
	}
	return m
}

func (c *e2eClient) delete(path string, status int) {
	c.t.Helper()
	resp, body := c.do(http.MethodDelete, c.url(path), nil, nil)
	c.expect(resp, body, status)
}

// nextLink returns the absolute URL of a `<url>; rel="next"` Link header,
// or "" when the response has none.
func (c *e2eClient) nextLink(resp *http.Response) string {
	c.t.Helper()
	link := resp.Header.Get("Link")
	if link == "" {
		return ""
	}
	end := strings.Index(link, ">")
	if !strings.HasPrefix(link, "<") || end < 0 || !strings.Contains(link[end:], `rel="next"`) {
		c.t.Fatalf("malformed Link header %q", link)
	}
	u, err := url.Parse(link[1:end])
	if err != nil {
		c.t.Fatalf("Link URL %q: %v", link[1:end], err)
	}
	return resp.Request.URL.ResolveReference(u).String()
}

type e2eTagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func (c *e2eClient) tags(target string) (e2eTagList, string) {
	c.t.Helper()
	resp, body := c.do(http.MethodGet, target, nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/json")
	var tl e2eTagList
	if err := json.Unmarshal(body, &tl); err != nil {
		c.t.Fatalf("GET %s: %v: %q", target, err, body)
	}
	if tl.Tags == nil {
		c.t.Fatalf("GET %s: tags is null, want an array: %q", target, body)
	}
	return tl, c.nextLink(resp)
}

func (c *e2eClient) catalog(target string) ([]string, string) {
	c.t.Helper()
	resp, body := c.do(http.MethodGet, target, nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/json")
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		c.t.Fatalf("GET %s: %v: %q", target, err, body)
	}
	if cat.Repositories == nil {
		c.t.Fatalf("GET %s: repositories is null, want an array: %q", target, body)
	}
	return cat.Repositories, c.nextLink(resp)
}

func (c *e2eClient) referrers(name string, subject oci.Digest, artifactType string) ([]oci.Descriptor, http.Header) {
	c.t.Helper()
	target := c.url("/v2/" + name + "/referrers/" + subject.String())
	if artifactType != "" {
		target += "?artifactType=" + url.QueryEscape(artifactType)
	}
	resp, body := c.do(http.MethodGet, target, nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", oci.MediaTypeOCIIndex)
	m, err := oci.ParseManifest(body)
	if err != nil {
		c.t.Fatalf("referrers body %q: %v", body, err)
	}
	if !m.IsIndex() || m.Manifests == nil {
		c.t.Fatalf("referrers body %q is not an index with a manifests array", body)
	}
	return m.Manifests, resp.Header
}

// pushInterrupted uploads data (which must exceed e2eMaxInMemory, so the
// session is backed by <work>/oci-amber/uploads/<id>), then makes the finalizing PUT
// fail with an I/O error on the spool by renaming the file away. The
// registry must answer 500 with the empty envelope, publish nothing, log
// the failure at error level and keep the session with all its bytes, so
// that the client's retry of the same PUT succeeds once the file is back.
func (e *e2eEnv) pushInterrupted(name string, data []byte) oci.Digest {
	t, c := e.t, e.c
	t.Helper()
	if len(data) <= e2eMaxInMemory {
		t.Fatalf("pushInterrupted needs a blob larger than the %d byte in-memory limit, got %d bytes", e2eMaxInMemory, len(data))
	}
	d := oci.DigestOfBytes(data)
	uploadURL, uuid := c.startUpload(name)
	next, rng := c.patch(uploadURL, data, "")
	if want := fmt.Sprintf("0-%d", len(data)-1); rng != want {
		t.Fatalf("PATCH Range = %q, want %q", rng, want)
	}
	spool := filepath.Join(e.uploadDir, uuid)
	if _, err := os.Stat(spool); err != nil {
		t.Fatalf("session %s did not spill to %s: %v", uuid, spool, err)
	}
	hidden := filepath.Join(e.tmp, "hidden-spool")
	if err := os.Rename(spool, hidden); err != nil {
		t.Fatal(err)
	}

	resp, body := c.do(http.MethodPut, c.withDigest(next, d), nil, map[string]string{"Content-Type": e2eOctetStream})
	c.expectEmptyErrors(resp, body, http.StatusInternalServerError)
	// Nothing was published.
	resp, body = c.do(http.MethodHead, c.url("/v2/"+name+"/blobs/"+d.String()), nil, nil)
	c.expect(resp, body, http.StatusNotFound)
	// The session survived with every byte it had.
	c.sessionStatus(next, uuid, len(data))
	// The failure was logged at error level.
	logged := false
	for _, rec := range e.logs.records(t, "request failed") {
		if rec["level"] == "ERROR" && rec["method"] == http.MethodPut {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no error-level \"request failed\" record for the failed PUT")
	}

	if err := os.Rename(hidden, spool); err != nil {
		t.Fatal(err)
	}
	c.finishUpload(name, next, d, nil)
	return d
}

// ---------------------------------------------------------------------------
// Phase 1: push

func (e *e2eEnv) push() {
	t, c, fx := e.t, e.c, e.fx
	t.Helper()
	t.Log("phase: push")

	// Every client starts with the version check.
	resp, body := c.do(http.MethodGet, c.url("/v2/"), nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Docker-Distribution-API-Version", "registry/2.0")

	// ggcr checks whether the manifest is already there before pushing.
	resp, body = c.do(http.MethodHead, c.url("/v2/"+e2eApp+"/manifests/v1"), nil, nil)
	c.expect(resp, body, http.StatusNotFound)

	// Blobs that do not exist yet: HEAD is a bare 404, GET carries the envelope.
	cfg1 := oci.DigestOfBytes(fx.cfg1)
	resp, body = c.do(http.MethodHead, c.url("/v2/"+e2eApp+"/blobs/"+cfg1.String()), nil, nil)
	c.expect(resp, body, http.StatusNotFound)
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/blobs/"+cfg1.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeBlobUnknown)

	// Image 1: config monolithic, layer A chunked with a resume, layer B single PATCH.
	e.record(c.pushMonolithic(e2eApp, fx.cfg1), fx.cfg1)
	e.record(c.pushChunked(e2eApp, fx.layerA, 1<<20), fx.layerA)
	e.record(c.pushSinglePatch(e2eApp, fx.layerB), fx.layerB)
	for _, b := range [][]byte{fx.cfg1, fx.layerA, fx.layerB} {
		c.headBlob(e2eApp, oci.DigestOfBytes(b), len(b))
	}
	// Pushing a blob that already exists is idempotent (whole-blob dedup).
	if d := c.pushSinglePatch(e2eApp, fx.layerB); d != oci.DigestOfBytes(fx.layerB) {
		t.Fatalf("re-push of layer B returned %s", d)
	}
	e.m1, _ = c.putManifest(e2eApp, "v1", oci.MediaTypeOCIManifest, fx.m1)
	e.recordManifest(e.m1, fx.m1, oci.MediaTypeOCIManifest)
	// Re-pushing an identical manifest is idempotent too.
	if d, _ := c.putManifest(e2eApp, "v1", oci.MediaTypeOCIManifest, fx.m1); d != e.m1 {
		t.Fatalf("re-push of manifest 1 returned %s, want %s", d, e.m1)
	}

	// Image 2: config via POST + PUT with body, layer A mounted, layer C
	// through a PUT that fails with 500 and is retried on the same session.
	e.record(c.pushPutBody(e2eApp, fx.cfg2), fx.cfg2)
	if !c.mount(e2eApp, oci.DigestOfBytes(fx.layerA), e2eApp) {
		t.Fatalf("mount of an existing blob was not a hit")
	}
	if c.mount(e2eApp, oci.DigestOfBytes([]byte("never uploaded")), e2eApp) {
		t.Fatalf("mount of an unknown blob was a hit")
	}
	e.record(e.pushInterrupted(e2eApp, fx.layerC), fx.layerC)
	// By digest, with a Content-Type parameter the registry must strip.
	e.m2, _ = c.putManifest(e2eApp, oci.DigestOfBytes(fx.m2).String(), oci.MediaTypeOCIManifest+"; charset=utf-8", fx.m2)
	e.recordManifest(e.m2, fx.m2, oci.MediaTypeOCIManifest)
	// By digest with the wrong digest.
	resp, body = c.do(http.MethodPut, c.url("/v2/"+e2eApp+"/manifests/"+e.m1.String()), fx.m2, map[string]string{"Content-Type": oci.MediaTypeOCIManifest})
	c.expectError(resp, body, http.StatusBadRequest, oci.CodeDigestInvalid)
	// A manifest that names a blob the registry does not have.
	missing := e2eJSON(t, oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIManifest,
		Config:        e2ePtr(e2eDescriptor(oci.MediaTypeOCIConfig, []byte("missing config"))),
		Layers:        []oci.Descriptor{e2eDescriptor(e2eLayerGzip, fx.layerA)},
	})
	resp, body = c.do(http.MethodPut, c.url("/v2/"+e2eApp+"/manifests/broken"), missing, map[string]string{"Content-Type": oci.MediaTypeOCIManifest})
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestBlobUnknown)
	// Invalid tags: the spec's TAG_INVALID is reported as MANIFEST_INVALID
	// with a message starting "invalid tag" (TAG_INVALID is not a standard
	// code). A leading '-' and a 129-character tag both fail the grammar.
	for _, tag := range []string{"-bad", strings.Repeat("a", 129)} {
		resp, body = c.do(http.MethodPut, c.url("/v2/"+e2eApp+"/manifests/"+tag), fx.m1, map[string]string{"Content-Type": oci.MediaTypeOCIManifest})
		oe := c.expectError(resp, body, http.StatusBadRequest, oci.CodeManifestInvalid)
		if !strings.HasPrefix(oe.Message, "invalid tag") {
			t.Fatalf("PUT manifest with tag %q: message %q, want prefix \"invalid tag\"", tag, oe.Message)
		}
		resp, body = c.do(http.MethodHead, c.url("/v2/"+e2eApp+"/manifests/"+tag), nil, nil)
		c.expect(resp, body, http.StatusNotFound)
	}

	// The index over both images, by tag.
	e.idx, _ = c.putManifest(e2eApp, "latest", oci.MediaTypeOCIIndex, fx.idx)
	e.recordManifest(e.idx, fx.idx, oci.MediaTypeOCIIndex)

	// An artifact referring to image 1, pushed without a Content-Type: the
	// registry falls back to the manifest's own mediaType.
	e.record(c.pushMonolithic(e2eApp, fx.empty), fx.empty)
	e.record(c.pushSinglePatch(e2eApp, fx.sbom), fx.sbom)
	var hdr http.Header
	e.art, hdr = c.putManifest(e2eApp, oci.DigestOfBytes(fx.art).String(), "", fx.art)
	if got := hdr.Get("OCI-Subject"); got != e.m1.String() {
		t.Fatalf("PUT artifact: OCI-Subject = %q, want %q", got, e.m1)
	}
	e.recordManifest(e.art, fx.art, oci.MediaTypeOCIManifest)

	// A second repository built from mounts alone, with a Docker v2 manifest.
	for _, b := range [][]byte{fx.cfg1, fx.layerA} {
		if !c.mount(e2eBase, oci.DigestOfBytes(b), e2eApp) {
			t.Fatalf("mount of %s into %s was not a hit", oci.DigestOfBytes(b), e2eBase)
		}
	}
	e.base, _ = c.putManifest(e2eBase, "base", oci.MediaTypeDockerManifest, fx.base)
	e.recordManifest(e.base, fx.base, oci.MediaTypeDockerManifest)

	// Nothing is left behind in the work directory once uploads are finalized.
	var leftovers []string
	err := filepath.WalkDir(e.work, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", e.work, err)
	}
	if len(leftovers) > 0 {
		t.Fatalf("files left under the work directory after all uploads finished: %v", leftovers)
	}
}

// ---------------------------------------------------------------------------
// Phase 2: log lines and accounting

func e2eNum(t *testing.T, rec map[string]any, key string) float64 {
	t.Helper()
	v, ok := rec[key]
	if !ok {
		t.Fatalf("log record lacks %q: %v", key, rec)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("log key %q is %T (%v), want a number", key, v, v)
	}
	return f
}

func e2eStr(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	v, ok := rec[key]
	if !ok {
		t.Fatalf("log record lacks %q: %v", key, rec)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("log key %q is %T (%v), want a string", key, v, v)
	}
	return s
}

func e2eAbsent(t *testing.T, rec map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := rec[k]; ok {
			t.Fatalf("log record carries %q=%v, want it absent: %v", k, v, rec)
		}
	}
}

// e2eIsInf reports whether a compression_ratio value is slog's rendering
// of +Inf: the JSON handler cannot encode it and writes the string
// "!ERROR:json: unsupported value: +Inf".
func e2eIsInf(v any) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, "+Inf")
}

// e2eRatio returns a finite compression_ratio, or a huge number for +Inf.
func e2eRatio(t *testing.T, rec map[string]any) float64 {
	t.Helper()
	if f, ok := rec["compression_ratio"].(float64); ok {
		return f
	}
	if e2eIsInf(rec["compression_ratio"]) {
		return float64(1 << 62)
	}
	t.Fatalf("compression_ratio = %v (%T), want a positive number or +Inf", rec["compression_ratio"], rec["compression_ratio"])
	return 0
}

func (e *e2eEnv) blobLog(d oci.Digest) map[string]any {
	e.t.Helper()
	for _, rec := range e.logs.records(e.t, "blob stored") {
		if rec["digest"] == d.String() {
			return rec
		}
	}
	e.t.Fatalf("no \"blob stored\" log record for %s", d)
	return nil
}

// imageLogs returns every "image pushed" record for d, in push order.
func (e *e2eEnv) imageLogs(d oci.Digest) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, rec := range e.logs.records(e.t, "image pushed") {
		if rec["digest"] == d.String() {
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		e.t.Fatalf("no \"image pushed\" log record for %s", d)
	}
	return out
}

func (e *e2eEnv) checkLogs() {
	t, fx := e.t, e.fx
	t.Helper()
	t.Log("phase: logs")

	// Per blob: classification, size, and which keys each kind carries.
	for _, tc := range []struct {
		body         []byte
		kind, format string
		entries      int
	}{
		{fx.layerA, string(blob.KindPrism), "gzip", e2eEntriesA},
		{fx.layerB, string(blob.KindPrism), "none", e2eEntriesB},
		{fx.layerC, string(blob.KindPrism), "gzip", e2eEntriesC},
		{fx.cfg1, string(blob.KindRaw), "none", 0},
		{fx.cfg2, string(blob.KindRaw), "none", 0},
	} {
		d := oci.DigestOfBytes(tc.body)
		rec := e.blobLog(d)
		if got := e2eStr(t, rec, "kind"); got != tc.kind {
			t.Fatalf("blob %s: kind %q, want %q (record %v)", d, got, tc.kind, rec)
		}
		if got := e2eStr(t, rec, "format"); got != tc.format {
			t.Fatalf("blob %s: format %q, want %q", d, got, tc.format)
		}
		if got := e2eNum(t, rec, "size"); got != float64(len(tc.body)) {
			t.Fatalf("blob %s: size %v, want %d", d, got, len(tc.body))
		}
		logical, deduped, disk := e2eNum(t, rec, "logical_bytes"), e2eNum(t, rec, "deduped_bytes"), e2eNum(t, rec, "disk_bytes")
		if logical < float64(len(tc.body)) || deduped < 0 || deduped > logical || disk < 0 {
			t.Fatalf("blob %s: inconsistent accounting logical=%v deduped=%v disk=%v", d, logical, deduped, disk)
		}
		if e2eNum(t, rec, "duration") < 0 {
			t.Fatalf("blob %s: negative duration", d)
		}
		if tc.kind == string(blob.KindRaw) {
			if got := e2eStr(t, rec, "raw_reason"); got != string(blob.ReasonNotTar) {
				t.Fatalf("blob %s: raw_reason %q, want %q", d, got, blob.ReasonNotTar)
			}
			e2eAbsent(t, rec, "engine", "entries")
			continue
		}
		e2eAbsent(t, rec, "raw_reason")
		// zrecipe reports no engine for an uncompressed input (Format
		// none): there is no compressor whose output has to be reproduced.
		// Every compressed prism names the engine that reproduces it.
		engine := e2eStr(t, rec, "engine")
		if tc.format == "none" && engine != "" {
			t.Fatalf("blob %s: engine %q on an uncompressed prism, want empty", d, engine)
		}
		if tc.format != "none" && engine == "" {
			t.Fatalf("blob %s: empty engine", d)
		}
		if got := e2eNum(t, rec, "entries"); got != float64(tc.entries) {
			t.Fatalf("blob %s: entries %v, want %d", d, got, tc.entries)
		}
	}
	// Layer C shares its two big files with layer A: file-level dedup shows
	// in its own line (spec: above 90 % deduplicated, disk well below size).
	recC := e.blobLog(oci.DigestOfBytes(fx.layerC))
	if logical, deduped := e2eNum(t, recC, "logical_bytes"), e2eNum(t, recC, "deduped_bytes"); deduped < 0.9*logical {
		t.Fatalf("layer C: deduped_bytes %v of logical_bytes %v, want >= 90%%", deduped, logical)
	}
	if disk := e2eNum(t, recC, "disk_bytes"); disk >= float64(len(fx.layerC))/10 {
		t.Fatalf("layer C: disk_bytes %v, want well below its %d bytes", disk, len(fx.layerC))
	}

	// Per image: every key from the spec plus manifests and duration, then
	// the arithmetic.
	keys := []string{"repo", "reference", "digest", "kind", "blobs", "manifests", "total_bytes", "logical_bytes", "deduped_bytes", "deduped_percent", "disk_bytes", "compression_ratio", "duration"}
	check := func(rec map[string]any, d oci.Digest, repo, reference, kind string, blobs, manifests, total int) {
		t.Helper()
		for _, k := range keys {
			if _, ok := rec[k]; !ok {
				t.Fatalf("image %s: log record lacks %q: %v", d, k, rec)
			}
		}
		if got := e2eStr(t, rec, "repo"); got != repo {
			t.Fatalf("image %s: repo %q, want %q", d, got, repo)
		}
		if got := e2eStr(t, rec, "reference"); got != reference {
			t.Fatalf("image %s: reference %q, want %q", d, got, reference)
		}
		if got := e2eStr(t, rec, "kind"); got != kind {
			t.Fatalf("image %s: kind %q, want %q", d, got, kind)
		}
		if got := e2eNum(t, rec, "blobs"); got != float64(blobs) {
			t.Fatalf("image %s: blobs %v, want %d", d, got, blobs)
		}
		if got := e2eNum(t, rec, "manifests"); got != float64(manifests) {
			t.Fatalf("image %s: manifests %v, want %d", d, got, manifests)
		}
		if got := e2eNum(t, rec, "total_bytes"); got != float64(total) {
			t.Fatalf("image %s: total_bytes %v, want %d", d, got, total)
		}
		logical, deduped, disk := e2eNum(t, rec, "logical_bytes"), e2eNum(t, rec, "deduped_bytes"), e2eNum(t, rec, "disk_bytes")
		if logical <= 0 || deduped < 0 || deduped > logical || disk < 0 {
			t.Fatalf("image %s: inconsistent accounting logical=%v deduped=%v disk=%v", d, logical, deduped, disk)
		}
		pct := e2eNum(t, rec, "deduped_percent")
		if want := 100 * deduped / logical; pct < want-0.1 || pct > want+0.1 {
			t.Fatalf("image %s: deduped_percent %v, want %.1f", d, pct, want)
		}
		ratio := e2eRatio(t, rec)
		if disk > 0 {
			if want := float64(total) / disk; ratio < want-0.01 || ratio > want+0.01 {
				t.Fatalf("image %s: compression_ratio %v, want %.2f", d, ratio, want)
			}
		} else if !e2eIsInf(rec["compression_ratio"]) {
			t.Fatalf("image %s: disk_bytes 0 but compression_ratio %v, want +Inf", d, rec["compression_ratio"])
		}
		if e2eNum(t, rec, "duration") < 0 {
			t.Fatalf("image %s: negative duration", d)
		}
	}

	// Image 1 was pushed twice: the first line carries the real numbers, the
	// second (identical re-push) finds every object present.
	m1s := e.imageLogs(e.m1)
	if len(m1s) != 2 {
		t.Fatalf("image 1: %d \"image pushed\" records, want 2 (push and identical re-push)", len(m1s))
	}
	total1 := len(fx.cfg1) + len(fx.layerA) + len(fx.layerB) + len(fx.m1)
	check(m1s[0], e.m1, e2eApp, "v1", string(image.KindManifest), 3, 0, total1)
	if disk := e2eNum(t, m1s[0], "disk_bytes"); disk < float64(len(fx.big))/2 {
		t.Fatalf("image 1: disk_bytes %v is too small for %d bytes of fresh random content", disk, len(fx.big))
	}
	check(m1s[1], e.m1, e2eApp, "v1", string(image.KindManifest), 3, 0, total1)
	if disk, pct := e2eNum(t, m1s[1], "disk_bytes"), e2eNum(t, m1s[1], "deduped_percent"); disk != 0 || pct != 100 || !e2eIsInf(m1s[1]["compression_ratio"]) {
		t.Fatalf("image 1 re-push: disk_bytes=%v deduped_percent=%v compression_ratio=%v, want 0, 100 and +Inf", disk, pct, m1s[1]["compression_ratio"])
	}

	// Image 2 shares layer A (already present, counted fully deduplicated)
	// and the two big files of layer C (deduplicated at the file level), so
	// almost nothing new hits the disk.
	m2 := e.imageLogs(e.m2)[0]
	check(m2, e.m2, e2eApp, e.m2.String(), string(image.KindManifest), 3, 0, len(fx.cfg2)+len(fx.layerA)+len(fx.layerC)+len(fx.m2))
	if deduped := e2eNum(t, m2, "deduped_bytes"); deduped < float64(len(fx.layerA)+len(fx.big)) {
		t.Fatalf("image 2: deduped_bytes %v, want at least %d (layer A whole plus the shared big file)", deduped, len(fx.layerA)+len(fx.big))
	}
	if disk := e2eNum(t, m2, "disk_bytes"); disk >= float64(len(fx.layerC))/4 {
		t.Fatalf("image 2: disk_bytes %v, want well below the %d bytes of layer C", disk, len(fx.layerC))
	}
	if pct := e2eNum(t, m2, "deduped_percent"); pct < 80 {
		t.Fatalf("image 2: deduped_percent %v, want >= 80", pct)
	}

	// The index adds its own bytes to the children's totals.
	idx := e.imageLogs(e.idx)[0]
	check(idx, e.idx, e2eApp, "latest", string(image.KindIndex), 0, 2, len(fx.idx)+int(e2eNum(t, m1s[0], "total_bytes"))+int(e2eNum(t, m2, "total_bytes")))
	if got, want := e2eNum(t, idx, "deduped_bytes"), e2eNum(t, m1s[0], "deduped_bytes")+e2eNum(t, m2, "deduped_bytes"); got < want {
		t.Fatalf("index: deduped_bytes %v, want at least the children's %v", got, want)
	}

	check(e.imageLogs(e.art)[0], e.art, e2eApp, e.art.String(), string(image.KindManifest), 2, 0, len(fx.empty)+len(fx.sbom)+len(fx.art))

	// The second repository reuses everything: only the manifest is new.
	base := e.imageLogs(e.base)[0]
	check(base, e.base, e2eBase, "base", string(image.KindManifest), 2, 0, len(fx.cfg1)+len(fx.layerA)+len(fx.base))
	if deduped := e2eNum(t, base, "deduped_bytes"); deduped < float64(len(fx.cfg1)+len(fx.layerA)) {
		t.Fatalf("base: deduped_bytes %v, want at least %d", deduped, len(fx.cfg1)+len(fx.layerA))
	}
	if disk := e2eNum(t, base, "disk_bytes"); disk <= 0 || disk >= 64<<10 {
		t.Fatalf("base: disk_bytes %v, want only the manifest's own objects", disk)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: how the blobs were stored

// blobRoot resolves a blob's ref, opens it through blob.Store, and returns
// the blob, the root's entry names and the keys of its meta.json as stored.
func (e *e2eEnv) blobRoot(d oci.Digest) (*blob.Blob, []string, map[string]json.RawMessage) {
	t := e.t
	t.Helper()
	root, err := e.st.Resolve(blob.RefName(d))
	if err != nil {
		t.Fatalf("resolving %s: %v", blob.RefName(d), err)
	}
	bl, err := e.blobs.Open(d)
	if err != nil {
		t.Fatalf("blob.Open(%s): %v", d, err)
	}
	if bl.Root() != root {
		t.Fatalf("blob %s: Open returned root %v, the ref resolves to %v", d, bl.Root(), root)
	}
	entries, more, err := e.st.ListDir(root, "", 100)
	if err != nil || more {
		t.Fatalf("listing the blob root of %s: more=%v err=%v", d, more, err)
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, string(en.Name))
	}
	mk, err := e.st.LookupKey(root, blob.MetaFile)
	if err != nil {
		t.Fatalf("blob %s: %s: %v", d, blob.MetaFile, err)
	}
	raw, err := e.st.ReadFile(mk)
	if err != nil {
		t.Fatalf("blob %s: reading %s: %v", d, blob.MetaFile, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("blob %s: %s is not a JSON object: %v\n%s", d, blob.MetaFile, err, raw)
	}
	var stored blob.Meta
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("blob %s: decoding %s: %v", d, blob.MetaFile, err)
	}
	if stored.Digest != bl.Meta.Digest || stored.Kind != bl.Meta.Kind || stored.Size != bl.Meta.Size || stored.Stats != bl.Meta.Stats {
		t.Fatalf("blob %s: stored meta %+v differs from what Open returns %+v", d, stored, bl.Meta)
	}
	return bl, names, fields
}

func (e *e2eEnv) storage() {
	t, fx := e.t, e.fx
	t.Helper()
	t.Log("phase: storage")

	prismEntries := []string{"blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json"}
	rawEntries := []string{"meta.json", "raw"}
	prismOnly := []string{"diffId", "uncompressedSize", "entries"}
	// engine and engineVersion are omitempty and name the compressor: a
	// compressed prism carries them, an uncompressed one (format none) has
	// no compressor to name, and a raw blob carries neither.
	prismCompressedOnly := []string{"engine", "engineVersion"}

	// Prisms: the tar-prism directory plus comp.json and meta.json; meta.json
	// has no rawReason and carries the prism-only fields with the values the
	// fixtures predict.
	for _, tc := range []struct {
		body, tar []byte
		format    string
		entries   int
	}{
		{fx.layerA, fx.tarA, "gzip", e2eEntriesA},
		{fx.layerB, fx.tarB, "none", e2eEntriesB},
		{fx.layerC, fx.tarC, "gzip", e2eEntriesC},
	} {
		d := oci.DigestOfBytes(tc.body)
		bl, names, fields := e.blobRoot(d)
		if !slices.Equal(names, prismEntries) {
			t.Fatalf("blob %s: root entries %v, want %v", d, names, prismEntries)
		}
		if v, has := fields["rawReason"]; has {
			t.Fatalf("blob %s: prism %s carries rawReason %s, want the key absent", d, blob.MetaFile, v)
		}
		for _, k := range prismOnly {
			if _, has := fields[k]; !has {
				t.Fatalf("blob %s: prism %s lacks %q", d, blob.MetaFile, k)
			}
		}
		for _, k := range prismCompressedOnly {
			_, has := fields[k]
			if want := tc.format != "none"; has != want {
				t.Fatalf("blob %s: prism %s has %q = %v, want present = %v for format %s", d, blob.MetaFile, k, has, want, tc.format)
			}
		}
		m := bl.Meta
		if m.Kind != blob.KindPrism || m.Format != tc.format || m.RawReason != "" {
			t.Fatalf("blob %s: kind=%s format=%s rawReason=%q, want prism/%s/\"\"", d, m.Kind, m.Format, m.RawReason, tc.format)
		}
		if m.Size != int64(len(tc.body)) || m.DiffID != oci.DigestOfBytes(tc.tar) || m.UncompressedSize != int64(len(tc.tar)) || m.Entries != tc.entries || (m.Engine == "") != (tc.format == "none") {
			t.Fatalf("blob %s: size=%d diffId=%s uncompressedSize=%d entries=%d engine=%q; want %d, %s, %d, %d and an engine only for a compressed prism",
				d, m.Size, m.DiffID, m.UncompressedSize, m.Entries, m.Engine, len(tc.body), oci.DigestOfBytes(tc.tar), len(tc.tar), tc.entries)
		}
		t.Logf("blob %s: prism format=%s engine=%s entries=%d stats=%+v", d, m.Format, m.Engine, m.Entries, m.Stats)
	}
	// Layer C's own stats show the file-level dedup against layer A.
	blC, _, _ := e.blobRoot(oci.DigestOfBytes(fx.layerC))
	if s := blC.Meta.Stats; s.DedupedBytes < int64(len(fx.big)+len(fx.lib)) || s.DedupedBytes != s.LogicalBytes-s.NewLogicalBytes || s.DiskBytes >= int64(len(fx.layerC))/10 {
		t.Fatalf("layer C stats %+v: want dedupedBytes >= %d, dedupedBytes == logical - new, diskBytes well below %d", s, len(fx.big)+len(fx.lib), len(fx.layerC))
	}

	// Raw blobs: meta.json and raw; meta.json carries rawReason and none of
	// the prism-only fields.
	for _, body := range [][]byte{fx.cfg1, fx.cfg2, fx.empty, fx.sbom} {
		d := oci.DigestOfBytes(body)
		bl, names, fields := e.blobRoot(d)
		if !slices.Equal(names, rawEntries) {
			t.Fatalf("blob %s: root entries %v, want %v", d, names, rawEntries)
		}
		if got := string(fields["rawReason"]); got != `"not-tar"` {
			t.Fatalf("blob %s: raw %s rawReason = %s, want \"not-tar\"", d, blob.MetaFile, got)
		}
		for _, k := range prismOnly {
			if v, has := fields[k]; has {
				t.Fatalf("blob %s: raw %s carries %q=%s, want it absent", d, blob.MetaFile, k, v)
			}
		}
		m := bl.Meta
		if m.Kind != blob.KindRaw || m.Format != "none" || m.RawReason != blob.ReasonNotTar || m.Size != int64(len(body)) {
			t.Fatalf("blob %s: kind=%s format=%s rawReason=%q size=%d, want raw/none/not-tar/%d", d, m.Kind, m.Format, m.RawReason, m.Size, len(body))
		}
	}

	// The stats in meta.json cover exactly the ingest objects: the blob root
	// and meta.json are written afterwards, through a separate writer. cfg1
	// was the first upload and is smaller than the minimum chunk, so its
	// ingest is a single Blob object of exactly len(cfg1) bytes; counting the
	// root or meta.json would add objects and hundreds of bytes.
	bl1, _, _ := e.blobRoot(oci.DigestOfBytes(fx.cfg1))
	s := bl1.Meta.Stats
	want := store.Stats{LogicalBytes: int64(len(fx.cfg1)), NewLogicalBytes: int64(len(fx.cfg1)), DedupedBytes: 0, DiskBytes: s.DiskBytes, ObjectsNew: 1, ObjectsDeduped: 0}
	if s.DiskBytes <= 0 || s != want {
		t.Fatalf("cfg1 stats %+v, want %+v with diskBytes > 0 (the root and meta.json must not be counted)", s, want)
	}
}

// ---------------------------------------------------------------------------
// Phase 4: pull

// pullImage fetches a manifest by reference and everything under it,
// comparing every body with what was pushed.
func (e *e2eEnv) pullImage(name, reference string, d oci.Digest) {
	t, c := e.t, e.c
	t.Helper()
	want, ok := e.pushed[d]
	if !ok {
		t.Fatalf("no pushed body recorded for %s", d)
	}
	m := c.getManifest(name, reference, want, e.types[d])
	if m.IsIndex() {
		for _, child := range m.Manifests {
			if int64(len(e.pushed[child.Digest])) != child.Size {
				t.Fatalf("index child %s: descriptor size %d, pushed %d", child.Digest, child.Size, len(e.pushed[child.Digest]))
			}
			if e.types[child.Digest] != child.MediaType {
				t.Fatalf("index child %s: descriptor mediaType %q, served %q", child.Digest, child.MediaType, e.types[child.Digest])
			}
			e.pullImage(name, child.Digest.String(), child.Digest)
		}
		return
	}
	for _, desc := range m.BlobDescriptors() {
		body, ok := e.pushed[desc.Digest]
		if !ok {
			t.Fatalf("manifest %s names blob %s that was never pushed", d, desc.Digest)
		}
		if int64(len(body)) != desc.Size {
			t.Fatalf("blob %s: descriptor size %d, pushed %d", desc.Digest, desc.Size, len(body))
		}
		c.headBlob(name, desc.Digest, len(body))
		c.getBlob(name, desc.Digest, body)
	}
}

func (e *e2eEnv) pull() {
	t, c, fx := e.t, e.c, e.fx
	t.Helper()
	t.Log("phase: pull")

	// Walk from the index tag down to every blob of both children.
	e.pullImage(e2eApp, "latest", e.idx)
	// The same images by tag and by digest.
	e.pullImage(e2eApp, "v1", e.m1)
	e.pullImage(e2eApp, e.m1.String(), e.m1)
	e.pullImage(e2eApp, e.m2.String(), e.m2)
	// The artifact, served with the media type from its own body.
	e.pullImage(e2eApp, e.art.String(), e.art)
	// The Docker manifest in the second repository; blobs are global.
	e.pullImage(e2eBase, "base", e.base)
	c.getBlob(e2eBase, oci.DigestOfBytes(fx.layerC), fx.layerC)

	// Unknown references.
	resp, body := c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/manifests/nope"), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eBase+"/manifests/"+e.m1.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)

	// Raw blobs advertise and honour single ranges.
	cfg1 := oci.DigestOfBytes(fx.cfg1)
	if hdr := c.headBlob(e2eApp, cfg1, len(fx.cfg1)); hdr.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("raw blob: Accept-Ranges = %q, want bytes", hdr.Get("Accept-Ranges"))
	}
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/blobs/"+cfg1.String()), nil, map[string]string{"Range": "bytes=2-5"})
	c.expect(resp, body, http.StatusPartialContent)
	if !bytes.Equal(body, fx.cfg1[2:6]) {
		t.Fatalf("range body %q, want %q", body, fx.cfg1[2:6])
	}
	c.expectHeader(resp, "Content-Range", fmt.Sprintf("bytes 2-5/%d", len(fx.cfg1)))
	c.expectLength(resp, 4)
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/blobs/"+cfg1.String()), nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", len(fx.cfg1), len(fx.cfg1)+9)})
	c.expect(resp, body, http.StatusRequestedRangeNotSatisfiable)
	c.expectHeader(resp, "Content-Range", fmt.Sprintf("bytes */%d", len(fx.cfg1)))

	// Prism blobs answer ranges with the full body and do not advertise them.
	layerA := oci.DigestOfBytes(fx.layerA)
	if hdr := c.headBlob(e2eApp, layerA, len(fx.layerA)); hdr.Get("Accept-Ranges") != "" {
		t.Fatalf("prism blob: Accept-Ranges = %q, want none", hdr.Get("Accept-Ranges"))
	}
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/blobs/"+layerA.String()), nil, map[string]string{"Range": "bytes=0-9"})
	c.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, fx.layerA) {
		t.Fatalf("ranged GET of a prism blob did not return the full body (%d of %d bytes)", len(body), len(fx.layerA))
	}
	c.expectLength(resp, len(fx.layerA))
}

// ---------------------------------------------------------------------------
// Phase 5: tags, catalog, referrers

func (e *e2eEnv) lists() {
	t, c, fx := e.t, e.c, e.fx
	t.Helper()
	t.Log("phase: lists")

	// tags/list: sorted, then paginated with Link, then n=0.
	tl, next := c.tags(c.url("/v2/" + e2eApp + "/tags/list"))
	if tl.Name != e2eApp || !slices.Equal(tl.Tags, []string{"latest", "v1"}) || next != "" {
		t.Fatalf("tags/list = %+v with next %q, want name %s tags [latest v1] and no Link", tl, next, e2eApp)
	}
	tl, next = c.tags(c.url("/v2/" + e2eApp + "/tags/list?n=1"))
	if !slices.Equal(tl.Tags, []string{"latest"}) || next == "" {
		t.Fatalf("tags/list?n=1 = %v with next %q, want [latest] and a Link", tl.Tags, next)
	}
	tl, next = c.tags(next)
	if !slices.Equal(tl.Tags, []string{"v1"}) || next != "" {
		t.Fatalf("second page = %v with next %q, want [v1] and no Link", tl.Tags, next)
	}
	tl, next = c.tags(c.url("/v2/" + e2eApp + "/tags/list?n=0"))
	if len(tl.Tags) != 0 || next != "" {
		t.Fatalf("tags/list?n=0 = %v with next %q, want [] and no Link", tl.Tags, next)
	}
	tl, next = c.tags(c.url("/v2/" + e2eBase + "/tags/list"))
	if !slices.Equal(tl.Tags, []string{"base"}) || next != "" {
		t.Fatalf("%s tags = %v with next %q, want [base]", e2eBase, tl.Tags, next)
	}
	resp, body := c.do(http.MethodGet, c.url("/v2/no/such/repo/tags/list"), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeNameUnknown)

	// _catalog: both repositories, sorted, paginated.
	repos, next := c.catalog(c.url("/v2/_catalog"))
	if !slices.Equal(repos, []string{e2eApp, e2eBase}) || next != "" {
		t.Fatalf("_catalog = %v with next %q, want [%s %s] and no Link", repos, next, e2eApp, e2eBase)
	}
	repos, next = c.catalog(c.url("/v2/_catalog?n=1"))
	if !slices.Equal(repos, []string{e2eApp}) || next == "" {
		t.Fatalf("_catalog?n=1 = %v with next %q, want [%s] and a Link", repos, next, e2eApp)
	}
	repos, next = c.catalog(next)
	if !slices.Equal(repos, []string{e2eBase}) || next != "" {
		t.Fatalf("second catalog page = %v with next %q, want [%s] and no Link", repos, next, e2eBase)
	}

	// referrers: the artifact points at image 1.
	refs, hdr := c.referrers(e2eApp, e.m1, "")
	if len(refs) != 1 {
		t.Fatalf("referrers of %s = %+v, want exactly the artifact", e.m1, refs)
	}
	got := refs[0]
	if got.Digest != e.art || got.Size != int64(len(fx.art)) || got.MediaType != oci.MediaTypeOCIManifest || got.ArtifactType != e2eSBOMType || got.Annotations["org.example.note"] != "e2e" {
		t.Fatalf("referrer descriptor = %+v, want digest %s size %d mediaType %s artifactType %s annotation org.example.note=e2e", got, e.art, len(fx.art), oci.MediaTypeOCIManifest, e2eSBOMType)
	}
	if v := hdr.Get("OCI-Filters-Applied"); v != "" {
		t.Fatalf("OCI-Filters-Applied = %q without a filter", v)
	}
	refs, hdr = c.referrers(e2eApp, e.m1, e2eSBOMType)
	if len(refs) != 1 || refs[0].Digest != e.art {
		t.Fatalf("filtered referrers = %+v, want the artifact", refs)
	}
	if v := hdr.Get("OCI-Filters-Applied"); v != "artifactType" {
		t.Fatalf("OCI-Filters-Applied = %q, want artifactType", v)
	}
	refs, hdr = c.referrers(e2eApp, e.m1, "application/vnd.example.other")
	if len(refs) != 0 || hdr.Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatalf("unmatched filter: %+v with OCI-Filters-Applied %q, want none and artifactType", refs, hdr.Get("OCI-Filters-Applied"))
	}
	if refs, _ := c.referrers(e2eApp, e.m2, ""); len(refs) != 0 {
		t.Fatalf("referrers of %s = %+v, want none", e.m2, refs)
	}
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/referrers/sha256:nothex"), nil, nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodeDigestInvalid)
}

// ---------------------------------------------------------------------------
// Phase 6: deletes

func (e *e2eEnv) deletes() {
	t, c, fx := e.t, e.c, e.fx
	t.Helper()
	t.Log("phase: delete")

	// Deleting a tag keeps the manifest reachable by digest.
	c.delete("/v2/"+e2eApp+"/manifests/v1", http.StatusAccepted)
	resp, body := c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/manifests/v1"), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	e.pullImage(e2eApp, e.m1.String(), e.m1)
	if tl, _ := c.tags(c.url("/v2/" + e2eApp + "/tags/list")); !slices.Equal(tl.Tags, []string{"latest"}) {
		t.Fatalf("tags after deleting v1 = %v, want [latest]", tl.Tags)
	}

	// Deleting by digest removes the manifest ref. The index bytes are still
	// served (the image root holds the child roots structurally), the
	// surviving child is still pullable by digest, and the deleted child
	// stays 404 by digest.
	c.delete("/v2/"+e2eApp+"/manifests/"+e.m2.String(), http.StatusAccepted)
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/manifests/"+e.m2.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	c.getManifest(e2eApp, "latest", fx.idx, oci.MediaTypeOCIIndex)
	e.pullImage(e2eApp, e.m1.String(), e.m1)
	resp, body = c.do(http.MethodGet, c.url("/v2/"+e2eApp+"/manifests/"+e.m2.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)

	// Deleting the artifact by digest drops it from the referrers list.
	c.delete("/v2/"+e2eApp+"/manifests/"+e.art.String(), http.StatusAccepted)
	if refs, _ := c.referrers(e2eApp, e.m1, ""); len(refs) != 0 {
		t.Fatalf("referrers after deleting the artifact = %+v, want none", refs)
	}

	// Deleting a blob.
	cfg2 := oci.DigestOfBytes(fx.cfg2)
	c.delete("/v2/"+e2eApp+"/blobs/"+cfg2.String(), http.StatusAccepted)
	resp, body = c.do(http.MethodHead, c.url("/v2/"+e2eApp+"/blobs/"+cfg2.String()), nil, nil)
	c.expect(resp, body, http.StatusNotFound)

	// Everything twice is a 404 with the matching code.
	resp, body = c.do(http.MethodDelete, c.url("/v2/"+e2eApp+"/manifests/v1"), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	resp, body = c.do(http.MethodDelete, c.url("/v2/"+e2eApp+"/manifests/"+e.m2.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	resp, body = c.do(http.MethodDelete, c.url("/v2/"+e2eApp+"/blobs/"+cfg2.String()), nil, nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeBlobUnknown)
}

// ---------------------------------------------------------------------------
// Phase 1b: the stored rootfs

// checkRootfs opens image 1 and walks its rootfs/: layer A with layer B's
// files merged in and var/ whited out.
func (e *e2eEnv) checkRootfs() {
	t := e.t
	im, err := e.images.Open(e2eApp, e.m1.String())
	if err != nil {
		t.Fatalf("Open image 1: %v", err)
	}
	if im.Meta.Rootfs == nil || im.Meta.Rootfs.Status != image.RootfsOK {
		t.Fatalf("image 1 rootfs = %+v, want ok", im.Meta.Rootfs)
	}
	root, ok := im.Rootfs()
	if !ok {
		t.Fatal("image 1 has no rootfs key")
	}
	got := map[string]fstree.Entry{}
	var walk func(prefix string, k key.Key)
	walk = func(prefix string, k key.Key) {
		entries, _, err := e.st.ListDir(k, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range entries {
			p := prefix + string(ent.Name)
			got[p] = ent
			if ent.Mode&store.TypeMask == store.TypeDir {
				ck, err := key.Parse(ent.ContentKey)
				if err != nil {
					t.Fatal(err)
				}
				walk(p+"/", ck)
			}
		}
	}
	walk("", root)
	longDir := "share/doc/" + strings.TrimSuffix(strings.Repeat("a-rather-long-directory-name/", 5), "/")
	want := []string{"bin", "bin/app", "bin/app-link", "etc", "etc/config.yaml", "etc/empty", "etc/hostname", "etc/hosts", "etc/os-release",
		"lib", "lib/libfoo.so", "lib/libfoo.so.1", "share", "share/readme.txt", "share/doc"}
	parts := strings.Split(longDir, "/")
	for i := 3; i <= len(parts); i++ {
		want = append(want, strings.Join(parts[:i], "/"))
	}
	want = append(want, longDir+"/NOTICE")
	sort.Strings(want)
	names := make([]string, 0, len(got))
	for p := range got {
		names = append(names, p)
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Fatalf("rootfs paths:\n got %v\nwant %v", names, want)
	}
	if im.Meta.Rootfs.Entries != len(want) {
		t.Fatalf("rootfs entries = %d, want %d", im.Meta.Rootfs.Entries, len(want))
	}
	if e := got["bin/app-link"]; e.Mode&store.TypeMask != store.TypeLink || string(e.LinkTarget) != "app" {
		t.Fatalf("bin/app-link = %+v", e)
	}
	if !bytes.Equal(got["lib/libfoo.so"].ContentKey, got["lib/libfoo.so.1"].ContentKey) {
		t.Fatal("hard link does not share the target's content key")
	}
	app, err := key.Parse(got["bin/app"].ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	if app.Length() != uint64(len(e.fx.big)) {
		t.Fatalf("bin/app is %d bytes, want %d", app.Length(), len(e.fx.big))
	}
	for _, rec := range e.logs.records(t, "image pushed") {
		if rec["digest"] == e.m1.String() && rec["rootfs"] != "ok" {
			t.Fatalf("image 1 push line: %v", rec)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 1c: the rootfs API

// e2eFSEntry mirrors the listing entry the API renders.
type e2eFSEntry struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Mode   string  `json:"mode"`
	UID    uint64  `json:"uid"`
	GID    uint64  `json:"gid"`
	Mtime  string  `json:"mtime"`
	Size   *int64  `json:"size"`
	Target string  `json:"target"`
	Major  *uint64 `json:"major"`
	Minor  *uint64 `json:"minor"`
}

type e2eFSListing struct {
	Path    string       `json:"path"`
	Entries []e2eFSEntry `json:"entries"`
}

func e2eListing(t *testing.T, body []byte) e2eFSListing {
	t.Helper()
	var l e2eFSListing
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("listing is not JSON: %v: %q", err, body)
	}
	if l.Entries == nil {
		t.Fatalf("listing entries is null: %q", body)
	}
	return l
}

func e2eNames(l e2eFSListing) []string {
	names := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	return names
}

// fsAPI walks image 1 over /fs/: listings with pagination, files with
// ranges and ETags, symlinks followed, directories as tars, the index by
// platform, and every error the API defines.
func (e *e2eEnv) fsAPI() {
	t, c, fx := e.t, e.c, e.fx
	m1 := e2eApp + "@" + e.m1.String()
	fsURL := func(ref, p string) string { return c.url("/fs/" + ref + "/" + p) }
	get := func(target string, hdr map[string]string) (*http.Response, []byte) {
		return c.do(http.MethodGet, target, nil, hdr)
	}

	// Root listing: layer A's directories, var/ whited out by layer B.
	resp, body := get(fsURL(m1, ""), nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/json")
	root := e2eListing(t, body)
	if root.Path != "" || !slices.Equal(e2eNames(root), []string{"bin", "etc", "lib", "share"}) {
		t.Fatalf("root listing = %+v", root)
	}
	for _, en := range root.Entries {
		if en.Type != "dir" || en.Mode != "0755" || en.Size != nil {
			t.Fatalf("root entry %+v", en)
		}
	}

	// etc: layer A's config.yaml and layer B's files; the same through
	// pages of two followed by Link.
	resp, body = get(fsURL(m1, "etc"), nil)
	c.expect(resp, body, http.StatusOK)
	etc := e2eListing(t, body)
	if etc.Path != "etc" || !slices.Equal(e2eNames(etc), []string{"config.yaml", "empty", "hostname", "hosts", "os-release"}) {
		t.Fatalf("etc listing = %+v", etc)
	}
	for _, en := range etc.Entries {
		if en.Type != "file" || en.Mode != "0644" || en.Size == nil || en.Mtime != "2023-11-14T22:13:20Z" {
			t.Fatalf("etc entry %+v", en)
		}
	}
	if *etc.Entries[2].Size != int64(len("e2e\n")) || *etc.Entries[1].Size != 0 {
		t.Fatalf("hostname size = %d, empty size = %d", *etc.Entries[2].Size, *etc.Entries[1].Size)
	}
	var paged []e2eFSEntry
	pages := 0
	for next := fsURL(m1, "etc") + "?n=2"; next != ""; {
		resp, body = get(next, nil)
		c.expect(resp, body, http.StatusOK)
		page := e2eListing(t, body)
		if len(page.Entries) > 2 {
			t.Fatalf("page of %d entries, want at most 2", len(page.Entries))
		}
		paged = append(paged, page.Entries...)
		pages++
		next = c.nextLink(resp)
	}
	if pages != 3 || !slices.Equal(e2eNames(e2eFSListing{Entries: paged}), e2eNames(etc)) {
		t.Fatalf("paged listing (%d pages) = %+v", pages, paged)
	}
	resp, body = get(fsURL(m1, "etc")+"?n=0", nil)
	c.expect(resp, body, http.StatusOK)
	if l := e2eListing(t, body); len(l.Entries) != 0 || resp.Header.Get("Link") != "" {
		t.Fatalf("n=0 listing = %+v, Link %q", l, resp.Header.Get("Link"))
	}

	// A file: whole, by range, conditionally, through a symlink and a hard link.
	resp, body = get(fsURL(m1, "bin/app"), nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/octet-stream")
	c.expectHeader(resp, "Accept-Ranges", "bytes")
	c.expectLength(resp, len(fx.big))
	if !bytes.Equal(body, fx.big) {
		t.Fatal("bin/app bytes differ from the fixture")
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("bin/app has no ETag")
	}
	resp, body = get(fsURL(m1, "bin/app"), map[string]string{"If-None-Match": etag})
	c.expect(resp, body, http.StatusNotModified)
	c.expectHeader(resp, "ETag", etag)
	resp, body = get(fsURL(m1, "bin/app"), map[string]string{"Range": "bytes=1048576-1048600"})
	c.expect(resp, body, http.StatusPartialContent)
	c.expectHeader(resp, "Content-Range", fmt.Sprintf("bytes 1048576-1048600/%d", len(fx.big)))
	if !bytes.Equal(body, fx.big[1048576:1048601]) {
		t.Fatal("range bytes differ")
	}
	resp, body = get(fsURL(m1, "bin/app"), map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(fx.big)+10)})
	c.expect(resp, body, http.StatusRequestedRangeNotSatisfiable)
	c.expectHeader(resp, "Content-Range", fmt.Sprintf("bytes */%d", len(fx.big)))
	resp, body = get(fsURL(m1, "bin/app-link"), nil)
	c.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, fx.big) {
		t.Fatal("bin/app-link did not follow the symlink to bin/app")
	}
	resp, body = get(fsURL(m1, "lib/libfoo.so.1"), nil)
	c.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, fx.lib) {
		t.Fatal("lib/libfoo.so.1 differs from the fixture")
	}
	resp, body = c.do(http.MethodHead, fsURL(m1, "bin/app"), nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectLength(resp, len(fx.big))
	if len(body) != 0 {
		t.Fatalf("HEAD returned %d body bytes", len(body))
	}
	resp, body = get(fsURL(m1, "bin/app"), map[string]string{"If-None-Match": "*"})
	c.expect(resp, body, http.StatusNotModified)
	resp, body = get(fsURL(m1, "etc/hosts")+"?format=json", nil)
	c.expect(resp, body, http.StatusOK)
	if string(body) != "127.0.0.1 localhost\n" {
		t.Fatalf("format=json on a file = %q", body)
	}
	// An empty file: a 200 with no bytes, and no range can be satisfied.
	resp, body = get(fsURL(m1, "etc/empty"), nil)
	c.expect(resp, body, http.StatusOK)
	c.expectLength(resp, 0)
	if len(body) != 0 || resp.Header.Get("ETag") == "" {
		t.Fatalf("empty file: %d bytes, ETag %q", len(body), resp.Header.Get("ETag"))
	}
	resp, body = get(fsURL(m1, "etc/empty"), map[string]string{"Range": "bytes=0-"})
	c.expect(resp, body, http.StatusRequestedRangeNotSatisfiable)
	c.expectHeader(resp, "Content-Range", "bytes */0")
	resp, body = c.do(http.MethodHead, fsURL(m1, "etc"), nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/json")
	if len(body) != 0 {
		t.Fatalf("HEAD on a listing returned %d body bytes", len(body))
	}
	if resp.Header.Get("Docker-Distribution-API-Version") != "" {
		t.Fatal("the distribution API version header is set on a /fs/ response")
	}

	// Directories as tars: etc alone, then the whole rootfs.
	resp, body = get(fsURL(m1, "etc")+"?format=tar", nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/x-tar")
	tarred := e2eReadTar(t, body)
	wantEtc := map[string]string{"config.yaml": "listen: :8080\nworkers: 4\n", "empty": "", "hostname": "e2e\n", "hosts": "127.0.0.1 localhost\n", "os-release": "ID=e2e\nVERSION_ID=1\n"}
	if len(tarred) != len(wantEtc) {
		t.Fatalf("etc tar has %d entries: %v", len(tarred), tarred)
	}
	for name, data := range wantEtc {
		if tarred[name] != data {
			t.Fatalf("etc tar %s = %q, want %q", name, tarred[name], data)
		}
	}
	resp, body = get(fsURL(m1, "")+"?format=tar", nil)
	c.expect(resp, body, http.StatusOK)
	whole := e2eReadTar(t, body)
	if whole["bin/app"] != string(fx.big) || whole["etc/hosts"] != wantEtc["hosts"] {
		t.Fatalf("whole rootfs tar has %d entries", len(whole))
	}
	if _, has := whole["var/empty"]; has {
		t.Fatal("whole rootfs tar contains the whited-out var/empty")
	}
	resp, body = c.do(http.MethodHead, fsURL(m1, "etc")+"?format=tar", nil, nil)
	c.expect(resp, body, http.StatusOK)
	c.expectHeader(resp, "Content-Type", "application/x-tar")

	// The index by tag: a platform picks the child.
	idx := e2eApp + ":latest"
	resp, body = get(fsURL(idx, "etc"), nil)
	oe := c.expectError(resp, body, http.StatusBadRequest, oci.CodePlatformUnknown)
	if detail, ok := oe.Detail.(map[string]any); !ok || fmt.Sprint(detail["platforms"]) != "[linux/amd64 linux/arm64]" {
		t.Fatalf("PLATFORM_UNKNOWN detail = %v", oe.Detail)
	}
	resp, body = get(fsURL(idx, "etc/extra.conf")+"?platform=linux/arm64", nil)
	c.expect(resp, body, http.StatusOK)
	if string(body) != "extra = true\n" {
		t.Fatalf("index/arm64 etc/extra.conf = %q", body)
	}
	resp, body = get(fsURL(idx, "etc/hostname")+"?platform=linux/amd64", nil)
	c.expect(resp, body, http.StatusOK)
	if string(body) != "e2e\n" {
		t.Fatalf("index/amd64 etc/hostname = %q", body)
	}
	resp, body = get(fsURL(idx, "etc")+"?platform=linux/s390x", nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodePlatformUnknown)
	resp, body = get(fsURL(idx, "etc")+"?platform=linux", nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodePlatformUnknown)
	resp, body = get(fsURL(idx, "etc")+"?platform=linux/arm64&n=1", nil)
	c.expect(resp, body, http.StatusOK)
	if next := c.nextLink(resp); !strings.Contains(next, "platform=linux%2Farm64") || !strings.Contains(next, "last=config.yaml") {
		t.Fatalf("Link on an index listing = %q", next)
	}

	// Errors.
	resp, body = get(fsURL(e2eApp+"@"+e.art.String(), ""), nil)
	oe = c.expectError(resp, body, http.StatusNotFound, oci.CodeRootfsUnavailable)
	if detail, ok := oe.Detail.(map[string]any); !ok || detail["status"] != "not-applicable" {
		t.Fatalf("ROOTFS_UNAVAILABLE detail = %v", oe.Detail)
	}
	resp, body = get(fsURL(m1, "nope/x"), nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodePathUnknown)
	resp, body = get(fsURL(m1, "etc/hosts/x"), nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodePathUnknown)
	resp, body = get(fsURL(m1, "etc/hosts")+"?format=tar", nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodePathInvalid)
	resp, body = get(fsURL(m1, "etc")+"?format=zip", nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodePathInvalid)
	resp, body = get(fsURL(m1, "etc")+"?n=-1", nil)
	c.expect(resp, body, http.StatusBadRequest)
	resp, body = get(fsURL(e2eApp+":no-such-tag", ""), nil)
	c.expectError(resp, body, http.StatusNotFound, oci.CodeManifestUnknown)
	resp, body = get(fsURL("Bad_Name:v1", ""), nil)
	c.expectError(resp, body, http.StatusBadRequest, oci.CodeNameInvalid)
	resp, body = get(c.url("/fs/library/app/etc"), nil)
	c.expectEmptyErrors(resp, body, http.StatusNotFound)
	resp, body = c.do(http.MethodPost, fsURL(m1, "etc"), nil, nil)
	c.expectError(resp, body, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	c.expectHeader(resp, "Allow", "GET, HEAD")
}

// e2eReadTar reads a tar body into name -> content, directory names without
// their trailing slash.
func e2eReadTar(t *testing.T, body []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(hdr.Name, "/")] = string(data)
	}
	return out
}

// ---------------------------------------------------------------------------
// Raw refusal

// e2eTwoLevelGzip builds one gzip member whose deflate payload comes from
// two compress/flate writers at different levels joined at a sync-flush
// boundary. It inflates fine, but no single-pass encoder emits an empty
// stored block followed by a restarted compressor at another level, so
// zrecipe cannot reproduce it (the blob package's tests use the same
// construction).
func e2eTwoLevelGzip(t *testing.T, part1, part2 []byte) []byte {
	t.Helper()
	var payload bytes.Buffer
	w1, err := flate.NewWriter(&payload, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Write(part1); err != nil {
		t.Fatal(err)
	}
	if err := w1.Flush(); err != nil { // non-final blocks, byte aligned
		t.Fatal(err)
	}
	w2, err := flate.NewWriter(&payload, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write(part2); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil { // final block
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff})
	out.Write(payload.Bytes())
	crc := crc32.NewIEEE()
	crc.Write(part1)
	crc.Write(part2)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc.Sum32())
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(len(part1)+len(part2)))
	out.Write(trailer[:])
	return out.Bytes()
}

// TestE2ERawRefused pushes a layer zrecipe cannot reproduce. Without
// AllowRaw the registry answers 400 BLOB_UPLOAD_INVALID with a message
// that names the layer, the reason and --allow-raw, publishes nothing, and
// drops the session and its spool, since a retry could only be refused
// again; with AllowRaw the same push stores the layer raw as
// not-reproducible, as every push did before the flag existed. The layer
// is bigger than e2eMaxInMemory so the session is file-backed and the
// spool removal is observable on disk.
func TestE2ERawRefused(t *testing.T) {
	tarData := e2eTar(t, []e2eEntry{
		{name: "usr/lib/libbig.so", data: e2eRandom(21, 1200<<10)},
		{name: "etc/motd", data: bytes.Repeat([]byte("welcome\n"), 512)},
	})
	layer := e2eTwoLevelGzip(t, tarData[:len(tarData)/2], tarData[len(tarData)/2:])
	if len(layer) <= e2eMaxInMemory {
		t.Fatalf("fixture is %d bytes, want more than the %d byte in-memory limit", len(layer), e2eMaxInMemory)
	}
	d := oci.DigestOfBytes(layer)

	t.Run("refused", func(t *testing.T) {
		e := newE2EEnvAllowRaw(t, false)
		c := e.c
		// POST then PUT with the body, the containerd style: the one where
		// a session the client could retry on exists.
		uploadURL, uuid := c.startUpload(e2eApp)
		resp, body := c.do(http.MethodPut, c.withDigest(uploadURL, d), layer, map[string]string{"Content-Type": e2eOctetStream})
		oerr := c.expectError(resp, body, http.StatusBadRequest, oci.CodeBlobUploadInvalid)
		for _, want := range []string{d.String(), string(blob.ReasonNotReproducible), "--allow-raw"} {
			if !strings.Contains(oerr.Message, want) {
				t.Errorf("error message %q lacks %q", oerr.Message, want)
			}
		}
		// Nothing was published.
		resp, body = c.do(http.MethodHead, c.url("/v2/"+e2eApp+"/blobs/"+d.String()), nil, nil)
		c.expect(resp, body, http.StatusNotFound)
		// The session and its spilled spool are gone, unlike after an
		// internal failure (see pushInterrupted): the same bytes would only
		// be refused again.
		resp, body = c.do(http.MethodGet, uploadURL, nil, nil)
		c.expectError(resp, body, http.StatusNotFound, oci.CodeBlobUploadUnknown)
		if _, err := os.Stat(filepath.Join(e.uploadDir, uuid)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("spool of the refused session: stat = %v, want not exist", err)
		}
		// The monolithic form is refused the same way.
		resp, body = c.do(http.MethodPost, c.url("/v2/"+e2eApp+"/blobs/uploads/?digest="+url.QueryEscape(d.String())), layer, map[string]string{"Content-Type": e2eOctetStream})
		c.expectError(resp, body, http.StatusBadRequest, oci.CodeBlobUploadInvalid)
		if entries, err := os.ReadDir(e.uploadDir); err != nil || len(entries) != 0 {
			t.Errorf("uploads directory after the refusals: %v, %v; want empty", entries, err)
		}
		// Each refusal is one error-level record from the blob store,
		// carrying what the fallback lines carry, and never a "request
		// failed": the bytes are the client's problem, not a server fault.
		recs := e.logs.records(t, "layer refused")
		if len(recs) != 2 {
			t.Fatalf("%d \"layer refused\" records, want 2", len(recs))
		}
		for _, rec := range recs {
			if rec["level"] != "ERROR" || e2eStr(t, rec, "digest") != d.String() || e2eStr(t, rec, "format") != "gzip" ||
				e2eStr(t, rec, "reason") != string(blob.ReasonNotReproducible) || e2eNum(t, rec, "size") != float64(len(layer)) {
				t.Errorf("layer refused record = %v", rec)
			}
		}
		if n := len(e.logs.records(t, "request failed")); n != 0 {
			t.Errorf("%d \"request failed\" records for refusals, want none", n)
		}
		if n := len(e.logs.records(t, "blob stored")); n != 0 {
			t.Errorf("%d \"blob stored\" records after refusals, want none", n)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		e := newE2EEnvAllowRaw(t, true)
		c := e.c
		if got := c.pushPutBody(e2eApp, layer); got != d {
			t.Fatalf("pushed %s, want %s", got, d)
		}
		rec := e.blobLog(d)
		if e2eStr(t, rec, "kind") != string(blob.KindRaw) || e2eStr(t, rec, "raw_reason") != string(blob.ReasonNotReproducible) || e2eStr(t, rec, "format") != "gzip" {
			t.Fatalf("blob stored record = %v, want raw/not-reproducible/gzip", rec)
		}
		c.getBlob(e2eApp, d, layer)
		if n := len(e.logs.records(t, "layer refused")); n != 0 {
			t.Errorf("%d \"layer refused\" records with AllowRaw, want none", n)
		}
	})
}
