package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// testEnv is a registry handler served by httptest over a real store on a
// temporary directory.
type testEnv struct {
	srv       *httptest.Server
	server    *server
	blobs     *blob.Store
	images    *image.Store
	uploads   *upload.Manager
	uploadDir string
	client    *http.Client
	log       *slog.Logger
	records   *recorder
}

// recorder keeps every log record the registry emits so a test can assert
// on the level a code path logs at. It tees to the test's own log.
type recorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (rc *recorder) add(r slog.Record) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.recs = append(rc.recs, r.Clone())
}

// count returns how many recorded records carry msg.
func (rc *recorder) count(msg string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	n := 0
	for _, r := range rc.recs {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// atLeast returns the recorded records of the given level or higher.
func (rc *recorder) atLeast(level slog.Level) []slog.Record {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []slog.Record
	for _, r := range rc.recs {
		if r.Level >= level {
			out = append(out, r)
		}
	}
	return out
}

// capturingHandler feeds every record to a recorder and then to the wrapped
// handler. Derived handlers keep capturing, so records logged through
// Logger.With are recorded too.
type capturingHandler struct {
	slog.Handler
	rc *recorder
}

func (h capturingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.rc.add(r)
	return h.Handler.Handle(ctx, r)
}

func (h capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return capturingHandler{Handler: h.Handler.WithAttrs(attrs), rc: h.rc}
}

func (h capturingHandler) WithGroup(name string) slog.Handler {
	return capturingHandler{Handler: h.Handler.WithGroup(name), rc: h.rc}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	rc := &recorder{}
	log := slog.New(capturingHandler{
		Handler: slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}),
		rc:      rc,
	})
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	work := filepath.Join(dir, "work")
	blobs, err := blob.New(st, blob.Options{
		WorkDir:               work,
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
	uploadDir := filepath.Join(work, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uploads, err := upload.NewManager(uploadDir, 1<<20, time.Hour, log)
	if err != nil {
		t.Fatalf("upload.NewManager: %v", err)
	}
	t.Cleanup(func() { uploads.Close() })
	images := image.New(st, blobs, log)
	// The handler is assembled exactly as New does it, in two steps, so the
	// tests can reach the server's own state; TestNewWrapsTheServer covers
	// New itself.
	s := newServer(blobs, images, uploads, log)
	srv := httptest.NewServer(withRecovery(log, s))
	t.Cleanup(srv.Close)
	return &testEnv{
		srv:       srv,
		server:    s,
		blobs:     blobs,
		images:    images,
		uploads:   uploads,
		uploadDir: uploadDir,
		client:    srv.Client(),
		log:       log,
		records:   rc,
	}
}

// response is an http.Response whose body has been read.
type response struct {
	*http.Response
	body []byte
}

func (e *testEnv) do(t *testing.T, method, path string, body []byte, headers map[string]string) *response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return &response{Response: resp, body: b}
}

func assertStatus(t *testing.T, resp *response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d; body %q", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, resp.body)
	}
}

func assertHeader(t *testing.T, resp *response, name, want string) {
	t.Helper()
	if got := resp.Header.Get(name); got != want {
		t.Fatalf("%s %s: header %s = %q, want %q", resp.Request.Method, resp.Request.URL.Path, name, got, want)
	}
}

func assertErrorCode(t *testing.T, resp *response, status int, code oci.ErrorCode) {
	t.Helper()
	assertStatus(t, resp, status)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error response Content-Type = %q, want application/json", ct)
	}
	var env oci.ErrorResponse
	if err := json.Unmarshal(resp.body, &env); err != nil {
		t.Fatalf("error body %q is not an error envelope: %v", resp.body, err)
	}
	if len(env.Errors) != 1 || env.Errors[0].Code != code {
		t.Fatalf("errors = %+v, want exactly one error with code %s", env.Errors, code)
	}
}

func assertEmptyErrors(t *testing.T, resp *response, status int) {
	t.Helper()
	assertStatus(t, resp, status)
	if strings.TrimSpace(string(resp.body)) != `{"errors":[]}` {
		t.Fatalf("body = %q, want {\"errors\":[]}", resp.body)
	}
}

const fakeHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestBaseEndpoint(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(t, http.MethodGet, "/v2/", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Docker-Distribution-API-Version", "registry/2.0")
	assertHeader(t, resp, "Content-Type", "application/json")
	if string(resp.body) != "{}" {
		t.Fatalf("body = %q, want {}", resp.body)
	}

	resp = e.do(t, http.MethodHead, "/v2/", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Docker-Distribution-API-Version", "registry/2.0")

	resp = e.do(t, http.MethodPost, "/v2/", nil, nil)
	assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q, want it to list GET", allow)
	}
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
		want route
	}{
		{"/v2/foo/blobs/sha256:" + fakeHex, true, route{"foo", "blobs", "sha256:" + fakeHex}},
		{"/v2/library/app/manifests/latest", true, route{"library/app", "manifests", "latest"}},
		{"/v2/a/b/c/d/manifests/sha256:" + fakeHex, true, route{"a/b/c/d", "manifests", "sha256:" + fakeHex}},
		{"/v2/foo/blobs/bar/blobs/sha256:" + fakeHex, true, route{"foo/blobs/bar", "blobs", "sha256:" + fakeHex}},
		{"/v2/foo/manifests/blobs/sha256:" + fakeHex, true, route{"foo/manifests", "blobs", "sha256:" + fakeHex}},
		{"/v2/foo/tags/tags/list", true, route{"foo/tags", "tags", "list"}},
		{"/v2/foo/blobs/uploads/", true, route{"foo", "uploads", ""}},
		{"/v2/a/b/blobs/uploads/0123abcd", true, route{"a/b", "uploads", "0123abcd"}},
		{"/v2/foo/blobs/uploads/a/b", true, route{"foo", "uploads", "a/b"}},
		{"/v2/foo/tags/list", true, route{"foo", "tags", "list"}},
		{"/v2/foo/referrers/sha256:" + fakeHex, true, route{"foo", "referrers", "sha256:" + fakeHex}},
		{"/v2/foo/blobs/", true, route{"foo", "blobs", ""}},
		{"/v2/", false, route{}},
		{"/v2/_catalog", false, route{}},
		{"/v2/foo", false, route{}},
		{"/v2/blobs/sha256:" + fakeHex, false, route{}},
		{"/v1/foo/blobs/sha256:" + fakeHex, false, route{}},
		{"/v2/foo/blobs", false, route{}},
	}
	for _, c := range cases {
		got, ok := parsePath(c.path)
		if ok != c.ok || got != c.want {
			t.Errorf("parsePath(%q) = %+v, %v; want %+v, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestInvalidNameRejected(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{
		"/v2/Foo/blobs/sha256:" + fakeHex,
		"/v2/foo_/blobs/uploads/",
		"/v2/foo//bar/manifests/latest",
		"/v2/" + strings.Repeat("a", 256) + "/tags/list",
	} {
		resp := e.do(t, http.MethodGet, path, nil, nil)
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeNameInvalid)
		assertHeader(t, resp, "Docker-Distribution-API-Version", "registry/2.0")
	}
}

func TestUnknownPathsAreEmpty404(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/v2/foo/bar", "/v2/foo", "/v2/foo/blobs", "/v2/foo/blobs/uploads/a/b", "/nope", "/"} {
		resp := e.do(t, http.MethodGet, path, nil, nil)
		assertEmptyErrors(t, resp, http.StatusNotFound)
		assertHeader(t, resp, "Docker-Distribution-API-Version", "registry/2.0")
	}
}

func TestHandleErrorMapping(t *testing.T) {
	s := &server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		err    error
		status int
		code   oci.ErrorCode
	}{
		{blob.ErrNotFound, http.StatusNotFound, oci.CodeBlobUnknown},
		{fmt.Errorf("open: %w", blob.ErrNotFound), http.StatusNotFound, oci.CodeBlobUnknown},
		{image.ErrNotFound, http.StatusNotFound, oci.CodeManifestUnknown},
		{upload.ErrUnknown, http.StatusNotFound, oci.CodeBlobUploadUnknown},
		{oci.NewError(oci.CodeDigestInvalid, "bad"), http.StatusBadRequest, oci.CodeDigestInvalid},
		{fmt.Errorf("wrapped: %w", oci.NewError(oci.CodeNameUnknown, "nope")), http.StatusNotFound, oci.CodeNameUnknown},
		{&blob.RawRefusedError{Digest: oci.DigestOfBytes([]byte("layer")), Format: "gzip", Reason: blob.ReasonNotReproducible, Err: errors.New("zrecipe: not reproducible")}, http.StatusBadRequest, oci.CodeBlobUploadInvalid},
		{fmt.Errorf("put: %w", &blob.RawRefusedError{Digest: oci.DigestOfBytes([]byte("layer")), Format: "gzip", Reason: blob.ReasonDecomposeFailed}), http.StatusBadRequest, oci.CodeBlobUploadInvalid},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v2/x/blobs/sha256:"+fakeHex, nil)
		s.handleError(rec, req, c.err)
		resp := &response{Response: rec.Result(), body: rec.Body.Bytes()}
		resp.Request = req
		assertErrorCode(t, resp, c.status, c.code)
	}

	// A refusal's message is the store's own: it is what tells the client
	// which layer was turned away, why, and about --allow-raw.
	refused := &blob.RawRefusedError{Digest: oci.DigestOfBytes([]byte("layer")), Format: "gzip", Reason: blob.ReasonNotReproducible, Err: errors.New("zrecipe: not reproducible")}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/x/blobs/uploads/abc?digest="+refused.Digest.String(), nil)
	s.handleError(rec, req, refused)
	var env oci.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("refusal body %q: %v", rec.Body.Bytes(), err)
	}
	if len(env.Errors) != 1 || env.Errors[0].Message != refused.Error() {
		t.Fatalf("refusal errors = %+v, want one carrying %q", env.Errors, refused.Error())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v2/x/blobs/sha256:"+fakeHex, nil)
	s.handleError(rec, req, errors.New("disk on fire"))
	resp := &response{Response: rec.Result(), body: rec.Body.Bytes()}
	resp.Request = req
	assertEmptyErrors(t, resp, http.StatusInternalServerError)
}

// TestIsClientGoneIgnoresSyscallErrno pins the distinction between a peer
// that walked away and a server-side I/O failure. syscall.Errno declares
// both Timeout() and Temporary(), so it satisfies the net.Error interface
// and every local filesystem error wraps one: matching that interface would
// file a full disk or a missing upload spool as a client disconnect and keep
// real faults out of the error log.
func TestIsClientGoneIgnoresSyscallErrno(t *testing.T) {
	live := httptest.NewRequest(http.MethodPut, "/v2/x/blobs/uploads/abc", nil)

	cancelled := httptest.NewRequest(http.MethodPut, "/v2/x/blobs/uploads/abc", nil)
	ctx, cancel := context.WithCancel(cancelled.Context())
	cancel()
	cancelled = cancelled.WithContext(ctx)

	opErr := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	for _, c := range []struct {
		name string
		req  *http.Request
		err  error
		want bool
	}{
		// Server faults: a live request whose error is a local syscall.
		{"ENOENT on the upload spool", live, fmt.Errorf("blob: opening spool: %w", &os.PathError{Op: "open", Path: "/w/uploads/abc", Err: syscall.ENOENT}), false},
		{"EIO from the store", live, fmt.Errorf("store: appending: %w", &os.PathError{Op: "write", Path: "/s/pack.0", Err: syscall.EIO}), false},
		{"bare ENOENT", live, syscall.ENOENT, false},
		{"bare EIO", live, syscall.EIO, false},
		{"a plain internal error", live, errors.New("disk on fire"), false},
		// Client faults.
		{"a reset connection", live, fmt.Errorf("reading body: %w", opErr), true},
		{"a bare *net.OpError", live, opErr, true},
		{"a truncated chunked body", live, fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF), true},
		{"a cancelled context in the error", live, fmt.Errorf("reading body: %w", context.Canceled), true},
		{"an exceeded deadline in the error", live, fmt.Errorf("reading body: %w", context.DeadlineExceeded), true},
		// A cancelled request wins regardless of the error, including one
		// that would otherwise read as a server fault.
		{"a cancelled request with a syscall error", cancelled, fmt.Errorf("blob: opening spool: %w", syscall.ENOENT), true},
		{"a cancelled request with an internal error", cancelled, errors.New("disk on fire"), true},
	} {
		if got := isClientGone(c.req, c.err); got != c.want {
			t.Errorf("isClientGone(%s) = %v, want %v (error: %v)", c.name, got, c.want, c.err)
		}
	}

	// The consequence that matters: handleError logs a server-side I/O
	// failure at Error level, and a client disconnect below it.
	for _, c := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{"ENOENT on the upload spool", fmt.Errorf("blob: opening spool: %w", &os.PathError{Op: "open", Path: "/w/uploads/abc", Err: syscall.ENOENT}), true},
		{"a reset connection", fmt.Errorf("reading body: %w", opErr), false},
	} {
		rc := &recorder{}
		s := &server{log: slog.New(capturingHandler{Handler: slog.NewTextHandler(io.Discard, nil), rc: rc})}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v2/x/blobs/uploads/abc", nil)
		s.handleError(rec, req, c.err)

		resp := &response{Response: rec.Result(), body: rec.Body.Bytes()}
		resp.Request = req
		assertEmptyErrors(t, resp, http.StatusInternalServerError)

		errs := rc.atLeast(slog.LevelError)
		if got := len(errs) > 0; got != c.wantError {
			t.Errorf("%s: error-level records = %d, want any = %v", c.name, len(errs), c.wantError)
		}
		if c.wantError && len(errs) > 0 && errs[0].Message != "request failed" {
			t.Errorf("%s: error record message = %q, want \"request failed\"", c.name, errs[0].Message)
		}
	}
}

func TestWithRecovery(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	h := withRecovery(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	h.ServeHTTP(rec, req)
	resp := &response{Response: rec.Result(), body: rec.Body.Bytes()}
	resp.Request = req
	assertEmptyErrors(t, resp, http.StatusInternalServerError)
	assertHeader(t, resp, "Content-Type", "application/json")

	// A panic after the response started cannot be turned into an envelope:
	// the connection is aborted instead.
	h = withRecovery(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
		panic("boom")
	}))
	func() {
		defer func() {
			rec := recover()
			if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", rec)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// http.ErrAbortHandler itself passes through untouched.
	h = withRecovery(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			rec := recover()
			if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", rec)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
}

func TestPanicInHandlerThroughServer(t *testing.T) {
	// End to end: a panicking handler behind New's wrapper answers 500 with
	// an empty errors list and the connection stays usable.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(withRecovery(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(string(body)) != `{"errors":[]}` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

func TestNewWrapsTheServer(t *testing.T) {
	// New is the exported entry point: it wires the server behind the
	// recovery wrapper, so the handler it returns answers the base endpoint.
	e := newTestEnv(t)
	h := New(e.blobs, e.images, e.uploads, e.log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{}" {
		t.Fatalf("New handler: status %d body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(apiVersionHeader); got != apiVersionValue {
		t.Fatalf("New handler: %s = %q, want %q", apiVersionHeader, got, apiVersionValue)
	}
}
