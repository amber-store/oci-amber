package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// writeJSON writes v as the JSON body of a response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"errors":[]}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// writeError writes the standard error envelope with the code's default
// HTTP status.
func writeError(w http.ResponseWriter, err *oci.Error) {
	writeErrorStatus(w, err.Code.DefaultStatus(), err)
}

// writeErrorStatus writes the standard error envelope with an explicit
// status, for the cases where the spec departs from the code's default
// (416 for an upload Content-Range mismatch).
func writeErrorStatus(w http.ResponseWriter, status int, err *oci.Error) {
	writeJSON(w, status, oci.ErrorResponse{Errors: []oci.Error{*err}})
}

// writeEmptyErrors writes {"errors":[]} with the given status: unknown
// paths under /v2/, recovered panics and internal failures.
func writeEmptyErrors(w http.ResponseWriter, status int) {
	writeJSON(w, status, oci.ErrorResponse{Errors: []oci.Error{}})
}

// notFound is the answer for everything under /v2/ that is not a route.
func (s *server) notFound(w http.ResponseWriter) {
	writeEmptyErrors(w, http.StatusNotFound)
}

// methodNotAllowed answers 405 with the UNSUPPORTED code and an Allow header.
func methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, oci.NewError(oci.CodeUnsupported, "method %s is not supported for %s", r.Method, r.URL.Path))
}

// handleError maps an error from the stores to a response: *oci.Error
// carries its own code; the stores' not-found sentinels become the matching
// 404 codes; a blob the store refuses to keep raw is the client's problem
// and answers 400 with the store's message, which names --allow-raw;
// anything else is an internal failure, logged and answered with a 500 and
// an empty errors list.
func (s *server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	var oerr *oci.Error
	var refused *blob.RawRefusedError
	switch {
	case errors.As(err, &oerr):
		writeError(w, oerr)
	case errors.As(err, &refused):
		writeError(w, oci.NewError(oci.CodeBlobUploadInvalid, "%v", err))
	case errors.Is(err, blob.ErrNotFound):
		writeError(w, oci.NewError(oci.CodeBlobUnknown, "blob unknown to registry"))
	case errors.Is(err, image.ErrNotFound):
		writeError(w, oci.NewError(oci.CodeManifestUnknown, "manifest unknown to registry"))
	case errors.Is(err, upload.ErrUnknown):
		writeError(w, oci.NewError(oci.CodeBlobUploadUnknown, "blob upload unknown to registry"))
	default:
		if isClientGone(r, err) {
			s.log.Debug("request abandoned by the client", "method", r.Method, "path", r.URL.Path, "error", err)
			if r.Context().Err() != nil {
				// Nobody is left to read a response.
				return
			}
			writeEmptyErrors(w, http.StatusInternalServerError)
			return
		}
		s.log.Log(r.Context(), slog.LevelError, "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeEmptyErrors(w, http.StatusInternalServerError)
	}
}

// isClientGone reports whether err is the peer walking away rather than a
// server fault. A Ctrl-C in the middle of a push cancels the request
// context, but a body read can also fail first: a truncated chunked body
// surfaces as io.ErrUnexpectedEOF and a reset connection as a *net.OpError.
// None of these deserve the error log or an operator's attention.
//
// The match is against *net.OpError, the concrete type the net package
// returns for a failed connection, and deliberately not against the
// net.Error interface: syscall.Errno declares both Timeout() and
// Temporary(), so it satisfies net.Error, and every local filesystem error
// wraps one. Matching the interface would classify a full disk, an EACCES
// or a missing upload spool as a client disconnect and hide real server
// faults from the error log.
func isClientGone(r *http.Request, err error) bool {
	if r != nil && r.Context().Err() != nil {
		return true
	}
	var operr *net.OpError
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &operr)
}
