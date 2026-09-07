// Package registry is the OCI distribution HTTP surface of oci-amber.
package registry

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/keyedmutex"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

const (
	apiVersionHeader = "Docker-Distribution-API-Version"
	apiVersionValue  = "registry/2.0"
)

// server holds the stores the handlers work on. It is reachable only
// through the http.Handler returned by New.
type server struct {
	blobs   *blob.Store
	images  *image.Store
	uploads *upload.Manager
	log     *slog.Logger

	// sessLocks serialises the requests that touch one upload session
	// (PATCH, PUT, GET and DELETE on the same id). Its rows are refcounted
	// and dropped as soon as the last holder releases, so a client that
	// opens a session and disappears leaves nothing behind: no sweeper
	// reaches into this map.
	sessLocks keyedmutex.Mutex[string]
}

// New returns the registry's HTTP handler. Panics in handlers are recovered
// into a 500 with an empty errors list.
func New(blobs *blob.Store, images *image.Store, uploads *upload.Manager, log *slog.Logger) http.Handler {
	s := newServer(blobs, images, uploads, log)
	return withRecovery(s.log, s)
}

// newServer builds the server New wraps. It is separate from New so the
// package's own tests can reach the server's state (its session locks)
// while still serving through the same recovery wrapper.
func newServer(blobs *blob.Store, images *image.Store, uploads *upload.Manager, log *slog.Logger) *server {
	if log == nil {
		log = slog.Default()
	}
	return &server{blobs: blobs, images: images, uploads: uploads, log: log}
}

// route is a parsed /v2/<name>/<kind>/<rest> path. kind is one of "blobs",
// "manifests", "tags", "referrers" or "uploads"; for "uploads" rest is the
// session id, "" for POST /v2/<name>/blobs/uploads/.
type route struct {
	name string
	kind string
	rest string
}

// pathRE is the spec's route grammar. The name group is greedy, so a
// repository whose own name contains a "blobs" or "manifests" segment is
// routed correctly: the last keyword segment wins.
var pathRE = regexp.MustCompile(`^/v2/(.+)/(blobs|manifests|tags|referrers)/(.*)$`)

// parsePath splits a request path into its route. ok is false when the
// path does not have the /v2/<name>/<kind>/<rest> shape.
func parsePath(p string) (route, bool) {
	m := pathRE.FindStringSubmatch(p)
	if m == nil {
		return route{}, false
	}
	rt := route{name: m[1], kind: m[2], rest: m[3]}
	if rt.kind == "blobs" {
		if after, ok := strings.CutPrefix(rt.rest, "uploads/"); ok {
			rt.kind = "uploads"
			rt.rest = after
		}
	}
	return rt, true
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if rest, ok := strings.CutPrefix(p, fsPrefix); ok {
		s.handleFS(w, r, rest)
		return
	}
	w.Header().Set(apiVersionHeader, apiVersionValue)
	if p == "/v2/" || p == "/v2" {
		s.handleBase(w, r)
		return
	}
	if p == "/v2/_catalog" {
		s.handleCatalog(w, r)
		return
	}
	rt, ok := parsePath(p)
	if !ok {
		s.notFound(w)
		return
	}
	if err := oci.ValidateRepository(rt.name); err != nil {
		writeError(w, oci.NewError(oci.CodeNameInvalid, "invalid repository name %q: %v", rt.name, err))
		return
	}
	s.dispatch(w, r, rt)
}

// dispatch routes a validated route to the handler for its kind.
func (s *server) dispatch(w http.ResponseWriter, r *http.Request, rt route) {
	switch rt.kind {
	case "blobs":
		s.handleBlob(w, r, rt)
	case "uploads":
		s.handleUpload(w, r, rt)
	case "manifests":
		s.handleManifests(w, r, rt)
	case "tags":
		s.handleTags(w, r, rt)
	case "referrers":
		s.handleReferrers(w, r, rt)
	default:
		s.notFound(w)
	}
}

// handleBase answers the API version check, GET /v2/.
func (s *server) handleBase(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			io.WriteString(w, "{}")
		}
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodHead)
	}
}

// responseWriter records whether the response header has been sent, so the
// recovery wrapper knows whether an error envelope can still be written.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(p)
}

// Flush keeps streamed bodies flushable through the wrapper.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// withRecovery turns a handler panic into a 500 with an empty errors list
// when the response has not started, and into a connection abort when it
// has. http.ErrAbortHandler passes through untouched so handlers can cut a
// connection deliberately. Every request is logged at debug level.
func withRecovery(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error("handler panic", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				if rw.wroteHeader {
					panic(http.ErrAbortHandler)
				}
				writeEmptyErrors(rw, http.StatusInternalServerError)
			}
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", status, "duration", time.Since(start))
		}()
		next.ServeHTTP(rw, r)
	})
}
