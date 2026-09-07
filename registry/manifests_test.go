package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

// descriptor is a JSON descriptor object for the manifests built in tests.
type descriptor = map[string]any

// newDescriptor builds the descriptor of a blob or manifest.
func newDescriptor(mediaType string, d oci.Digest, size int) descriptor {
	return descriptor{"mediaType": mediaType, "digest": d.String(), "size": size}
}

// storeBlob puts data straight into the blob store and returns its descriptor.
func (e *testEnv) storeBlob(t *testing.T, mediaType string, data []byte) descriptor {
	t.Helper()
	return newDescriptor(mediaType, e.putBlob(t, data), len(data))
}

// storeConfig stores the JSON config fixture as an image config blob.
func (e *testEnv) storeConfig(t *testing.T) descriptor {
	t.Helper()
	return e.storeBlob(t, oci.MediaTypeOCIConfig, rawFixture())
}

// layerFixture is incompressible data prefixed so it can never be taken for
// a compressed stream or a tar; the blob store keeps it raw, which is all a
// manifest test needs.
func layerFixture(seed uint64, n int) []byte {
	return append([]byte("RAW:"), randomBytes(seed, n)...)
}

// manifestJSON builds an OCI image manifest around config and layers. extra
// overrides or adds top-level fields; a nil value removes the field.
func manifestJSON(t *testing.T, config descriptor, layers []descriptor, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIManifest,
		"config":        config,
		"layers":        append([]descriptor{}, layers...),
	}
	for k, v := range extra {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return body
}

// indexJSON builds an OCI image index over children; extra works as in
// manifestJSON.
func indexJSON(t *testing.T, children []descriptor, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     oci.MediaTypeOCIIndex,
		"manifests":     append([]descriptor{}, children...),
	}
	for k, v := range extra {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return body
}

// manifestPath is the URL path of a manifest reference.
func manifestPath(name, reference string) string {
	return "/v2/" + name + "/manifests/" + reference
}

// ctOf is a headers map carrying one Content-Type.
func ctOf(mediaType string) map[string]string {
	return map[string]string{"Content-Type": mediaType}
}

// putManifest PUTs body under name/reference with the given Content-Type
// ("" sends none) and requires a 201.
func (e *testEnv) putManifest(t *testing.T, name, reference, contentType string, body []byte) *response {
	t.Helper()
	var headers map[string]string
	if contentType != "" {
		headers = ctOf(contentType)
	}
	resp := e.do(t, http.MethodPut, manifestPath(name, reference), body, headers)
	assertStatus(t, resp, http.StatusCreated)
	return resp
}

func TestPutManifestByTag(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	layer := e.storeBlob(t, "application/vnd.oci.image.layer.v1.tar+gzip", layerFixture(41, 1500))
	body := manifestJSON(t, cfg, []descriptor{layer}, nil)
	want := oci.DigestOfBytes(body)

	// Pushed twice: re-pushing an identical manifest is idempotent.
	for i := 0; i < 2; i++ {
		resp := e.putManifest(t, "library/app", "v1", oci.MediaTypeOCIManifest, body)
		assertHeader(t, resp, "Location", manifestPath("library/app", want.String()))
		assertHeader(t, resp, "Docker-Content-Digest", want.String())
		assertHeader(t, resp, "OCI-Subject", "")
		if len(resp.body) != 0 {
			t.Fatalf("push %d: body = %q, want empty", i, resp.body)
		}
	}
}

func TestPutManifestByDigest(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	d := oci.DigestOfBytes(body)

	resp := e.putManifest(t, "library/app", d.String(), oci.MediaTypeOCIManifest, body)
	assertHeader(t, resp, "Location", manifestPath("library/app", d.String()))
	assertHeader(t, resp, "Docker-Content-Digest", d.String())

	// The digest must match the body.
	wrong := oci.DigestOfBytes([]byte("something else"))
	resp = e.do(t, http.MethodPut, manifestPath("library/app", wrong.String()), body, ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)

	// Digest-shaped but not sha256, wrong length, or not hex.
	for _, ref := range []string{"sha512:" + fakeHex + fakeHex, "sha256:" + fakeHex[:62], "sha256:xyz"} {
		resp = e.do(t, http.MethodPut, manifestPath("library/app", ref), body, ctOf(oci.MediaTypeOCIManifest))
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	}

	// Nothing was stored under the wrong digest.
	resp = e.do(t, http.MethodGet, manifestPath("library/app", wrong.String()), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
}

func TestPutManifestWithSubject(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	// The subject does not have to exist.
	subject := oci.DigestOfBytes([]byte("subject that was never pushed"))
	body := manifestJSON(t, cfg, []descriptor{cfg}, map[string]any{
		"artifactType": "application/vnd.example.sbom",
		"subject":      newDescriptor(oci.MediaTypeOCIManifest, subject, 123),
	})
	d := oci.DigestOfBytes(body)
	resp := e.putManifest(t, "library/app", d.String(), oci.MediaTypeOCIManifest, body)
	assertHeader(t, resp, "OCI-Subject", subject.String())
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	assertHeader(t, resp, "Location", manifestPath("library/app", d.String()))
}

func TestPutManifestMissingBlob(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	missing := oci.DigestOfBytes([]byte("never uploaded"))
	layer := newDescriptor("application/vnd.oci.image.layer.v1.tar+gzip", missing, 14)
	body := manifestJSON(t, cfg, []descriptor{layer}, nil)
	resp := e.do(t, http.MethodPut, manifestPath("library/app", "v1"), body, ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestBlobUnknown)

	var env oci.ErrorResponse
	if err := json.Unmarshal(resp.body, &env); err != nil {
		t.Fatalf("decode %q: %v", resp.body, err)
	}
	detail, _ := env.Errors[0].Detail.(map[string]any)
	if detail["digest"] != missing.String() {
		t.Fatalf("detail = %v, want digest %s", env.Errors[0].Detail, missing)
	}

	// Nothing was stored under the tag.
	resp = e.do(t, http.MethodGet, manifestPath("library/app", "v1"), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
}

func TestPutManifestInvalid(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)

	// An invalid tag is MANIFEST_INVALID (TAG_INVALID is not a standard code).
	for _, tag := range []string{"-leading-dash", ".dot", strings.Repeat("a", 129)} {
		resp := e.do(t, http.MethodPut, manifestPath("library/app", tag), body, ctOf(oci.MediaTypeOCIManifest))
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeManifestInvalid)
	}

	// Invalid JSON.
	resp := e.do(t, http.MethodPut, manifestPath("library/app", "v1"), []byte("{not json"), ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeManifestInvalid)

	// A schemaVersion other than 2.
	resp = e.do(t, http.MethodPut, manifestPath("library/app", "v1"), manifestJSON(t, cfg, nil, map[string]any{"schemaVersion": 1}), ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeManifestInvalid)

	// A descriptor whose digest is not sha256.
	bad := manifestJSON(t, cfg, []descriptor{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "sha512:" + fakeHex + fakeHex, "size": 1}}, nil)
	resp = e.do(t, http.MethodPut, manifestPath("library/app", "v1"), bad, ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeManifestInvalid)

	// None of the rejected pushes created the tag.
	resp = e.do(t, http.MethodGet, manifestPath("library/app", "v1"), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
}

func TestPutManifestTooLarge(t *testing.T) {
	e := newTestEnv(t)
	body := bytes.Repeat([]byte("x"), maxManifestSize+1)
	path := manifestPath("library/app", "v1")

	// A Content-Length above the cap is refused before the body is read.
	resp := e.do(t, http.MethodPut, path, body, ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusRequestEntityTooLarge, oci.CodeManifestInvalid)

	// Hiding the reader's type keeps NewRequest from learning the length, so
	// the body is sent chunked and the MaxBytesReader path is exercised.
	req, err := http.NewRequest(http.MethodPut, e.srv.URL+path, struct{ io.Reader }{bytes.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", oci.MediaTypeOCIManifest)
	raw, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("chunked PUT: %v", err)
	}
	chunkedBody, err := io.ReadAll(raw.Body)
	raw.Body.Close()
	if err != nil {
		t.Fatalf("chunked PUT: reading response: %v", err)
	}
	resp = &response{Response: raw, body: chunkedBody}
	assertErrorCode(t, resp, http.StatusRequestEntityTooLarge, oci.CodeManifestInvalid)

	// Exactly the cap is read and parsed (and rejected as not JSON), not refused.
	resp = e.do(t, http.MethodPut, path, body[:maxManifestSize], ctOf(oci.MediaTypeOCIManifest))
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeManifestInvalid)
}

func TestManifestContentTypeSelection(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	child := manifestJSON(t, cfg, nil, nil)
	childDigest := oci.DigestOfBytes(child)
	e.putManifest(t, "library/ct", childDigest.String(), oci.MediaTypeOCIManifest, child)

	cases := []struct {
		name        string
		contentType string // sent with the PUT; "" sends none
		body        []byte
		want        string // Content-Type of the GET
	}{
		{
			"content type parameters are dropped",
			oci.MediaTypeOCIManifest + "; charset=utf-8",
			manifestJSON(t, cfg, nil, nil),
			oci.MediaTypeOCIManifest,
		},
		{
			"no content type uses the manifest's mediaType",
			"",
			manifestJSON(t, cfg, nil, map[string]any{"mediaType": oci.MediaTypeDockerManifest}),
			oci.MediaTypeDockerManifest,
		},
		{
			"no content type and no mediaType is an OCI manifest",
			"",
			manifestJSON(t, cfg, nil, map[string]any{"mediaType": nil}),
			oci.MediaTypeOCIManifest,
		},
		{
			"no content type and no mediaType with manifests is an OCI index",
			"",
			indexJSON(t, []descriptor{newDescriptor(oci.MediaTypeOCIManifest, childDigest, len(child))}, map[string]any{"mediaType": nil}),
			oci.MediaTypeOCIIndex,
		},
	}
	for i, tc := range cases {
		tag := "t" + strconv.Itoa(i)
		e.putManifest(t, "library/ct", tag, tc.contentType, tc.body)
		resp := e.do(t, http.MethodGet, manifestPath("library/ct", tag), nil, nil)
		assertStatus(t, resp, http.StatusOK)
		if got := resp.Header.Get("Content-Type"); got != tc.want {
			t.Errorf("%s: Content-Type = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGetManifestByTagAndDigest(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	layer := e.storeBlob(t, "application/vnd.oci.image.layer.v1.tar+gzip", layerFixture(42, 3000))

	cases := []struct{ name, mediaType string }{
		{"library/oci", oci.MediaTypeOCIManifest},
		{"library/docker", oci.MediaTypeDockerManifest},
	}
	for _, tc := range cases {
		body := manifestJSON(t, cfg, []descriptor{layer}, map[string]any{"mediaType": tc.mediaType})
		d := oci.DigestOfBytes(body)
		e.putManifest(t, tc.name, "v1", tc.mediaType, body)

		for _, ref := range []string{"v1", d.String()} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				// Accept is ignored: the stored type comes back whatever the client lists.
				resp := e.do(t, method, manifestPath(tc.name, ref), nil, map[string]string{"Accept": oci.MediaTypeOCIIndex})
				assertStatus(t, resp, http.StatusOK)
				assertHeader(t, resp, "Content-Type", tc.mediaType)
				assertHeader(t, resp, "Content-Length", strconv.Itoa(len(body)))
				assertHeader(t, resp, "Docker-Content-Digest", d.String())
				if method == http.MethodGet && !bytes.Equal(resp.body, body) {
					t.Fatalf("GET %s %s: body differs from the pushed bytes", tc.name, ref)
				}
				if method == http.MethodHead && len(resp.body) != 0 {
					t.Fatalf("HEAD %s %s: body = %q, want empty", tc.name, ref, resp.body)
				}
			}
		}
	}
}

func TestGetManifestUnknown(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	e.putManifest(t, "library/app", "v1", oci.MediaTypeOCIManifest, manifestJSON(t, cfg, nil, nil))

	for _, path := range []string{
		manifestPath("library/app", "nope"),
		manifestPath("library/app", "sha256:"+fakeHex),
		manifestPath("other/repo", "v1"),
		manifestPath("library/app", strings.Repeat("a", 129)), // not a valid tag, so nothing can exist under it
	} {
		resp := e.do(t, http.MethodGet, path, nil, nil)
		assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
		resp = e.do(t, http.MethodHead, path, nil, nil)
		assertStatus(t, resp, http.StatusNotFound)
	}

	// Digest-shaped but malformed: 400, not 404.
	for _, ref := range []string{"sha256:" + fakeHex[:62], "sha512:" + fakeHex + fakeHex, "sha256:xyz"} {
		resp := e.do(t, http.MethodGet, manifestPath("library/app", ref), nil, nil)
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	}
}

func TestDeleteManifestByTag(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	d := oci.DigestOfBytes(body)
	e.putManifest(t, "library/app", "v1", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "library/app", "v2", oci.MediaTypeOCIManifest, body)

	resp := e.do(t, http.MethodDelete, manifestPath("library/app", "v1"), nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
	resp = e.do(t, http.MethodGet, manifestPath("library/app", "v1"), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)

	// The other tag and the digest survive.
	for _, ref := range []string{"v2", d.String()} {
		resp := e.do(t, http.MethodGet, manifestPath("library/app", ref), nil, nil)
		assertStatus(t, resp, http.StatusOK)
	}

	// Deleting the tag again is MANIFEST_UNKNOWN.
	resp = e.do(t, http.MethodDelete, manifestPath("library/app", "v1"), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
}

func TestDeleteManifestByDigest(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	d := oci.DigestOfBytes(body)
	e.putManifest(t, "library/app", "v1", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "library/app", "v2", oci.MediaTypeOCIManifest, body)

	resp := e.do(t, http.MethodDelete, manifestPath("library/app", d.String()), nil, nil)
	assertStatus(t, resp, http.StatusAccepted)

	// The manifest and every tag pointing at it are gone.
	for _, ref := range []string{"v1", "v2", d.String()} {
		resp := e.do(t, http.MethodGet, manifestPath("library/app", ref), nil, nil)
		assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)
	}
	resp = e.do(t, http.MethodDelete, manifestPath("library/app", d.String()), nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestUnknown)

	// Blobs are untouched: deleting a manifest only drops references.
	resp = e.do(t, http.MethodHead, "/v2/library/app/blobs/"+cfg["digest"].(string), nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestPutIndex(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	var children []descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		body := manifestJSON(t, cfg, nil, map[string]any{"annotations": map[string]string{"arch": arch}})
		d := oci.DigestOfBytes(body)
		e.putManifest(t, "library/multi", d.String(), oci.MediaTypeOCIManifest, body)
		child := newDescriptor(oci.MediaTypeOCIManifest, d, len(body))
		child["platform"] = map[string]any{"os": "linux", "architecture": arch}
		children = append(children, child)
	}
	index := indexJSON(t, children, nil)
	d := oci.DigestOfBytes(index)
	resp := e.putManifest(t, "library/multi", "latest", oci.MediaTypeOCIIndex, index)
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	assertHeader(t, resp, "Location", manifestPath("library/multi", d.String()))

	resp = e.do(t, http.MethodGet, manifestPath("library/multi", "latest"), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Type", oci.MediaTypeOCIIndex)
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(index)))
	if !bytes.Equal(resp.body, index) {
		t.Fatal("index bytes differ from the pushed bytes")
	}

	// An index whose child is not stored is MANIFEST_BLOB_UNKNOWN.
	missing := oci.DigestOfBytes([]byte("missing child"))
	bad := indexJSON(t, []descriptor{newDescriptor(oci.MediaTypeOCIManifest, missing, 10)}, nil)
	resp = e.do(t, http.MethodPut, manifestPath("library/multi", "broken"), bad, ctOf(oci.MediaTypeOCIIndex))
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeManifestBlobUnknown)
}

func TestManifestMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	for _, m := range []string{http.MethodPost, http.MethodPatch} {
		resp := e.do(t, m, manifestPath("library/app", "v1"), []byte("{}"), nil)
		assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
		assertHeader(t, resp, "Allow", "HEAD, GET, PUT, DELETE")
	}

	// No reference at all names nothing.
	resp := e.do(t, http.MethodGet, "/v2/library/app/manifests/", nil, nil)
	assertEmptyErrors(t, resp, http.StatusNotFound)
}
