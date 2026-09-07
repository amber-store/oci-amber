package registry

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// blobLocation is the canonical URL path of a stored blob.
func blobLocation(name string, d oci.Digest) string {
	return "/v2/" + name + "/blobs/" + d.String()
}

// handleBlob serves HEAD, GET and DELETE /v2/<name>/blobs/<digest>.
func (s *server) handleBlob(w http.ResponseWriter, r *http.Request, rt route) {
	if rt.rest == "" {
		s.notFound(w)
		return
	}
	d, err := oci.ParseDigest(rt.rest)
	if err != nil {
		writeError(w, oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", rt.rest, err))
		return
	}
	switch r.Method {
	case http.MethodHead:
		s.serveBlob(w, r, d, false)
	case http.MethodGet:
		s.serveBlob(w, r, d, true)
	case http.MethodDelete:
		s.deleteBlob(w, r, d)
	default:
		methodNotAllowed(w, r, http.MethodHead, http.MethodGet, http.MethodDelete)
	}
}

// serveBlob answers HEAD (withBody false) and GET for one blob. Raw blobs
// honour a single Range; prisms always get the full body.
func (s *server) serveBlob(w http.ResponseWriter, r *http.Request, d oci.Digest, withBody bool) {
	bl, err := s.blobs.Open(d)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	size := bl.Meta.Size
	h := w.Header()
	h.Set("Content-Type", oci.MediaTypeOctetStream)
	h.Set("Docker-Content-Digest", d.String())
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	if bl.SupportsRange() {
		h.Set("Accept-Ranges", "bytes")
	}
	if !withBody {
		w.WriteHeader(http.StatusOK)
		return
	}

	status := http.StatusOK
	rng := byteRange{start: 0, end: size - 1}
	if bl.SupportsRange() {
		br, ok, err := parseRange(r.Header.Get("Range"), size)
		if err != nil {
			h.Del("Content-Type")
			h.Del("Docker-Content-Digest")
			h.Set("Content-Length", "0")
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if ok {
			status = http.StatusPartialContent
			rng = br
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", br.start, br.end, size))
			h.Set("Content-Length", strconv.FormatInt(br.end-br.start+1, 10))
		}
	}

	bw := &bodyWriter{w: w, status: status}
	ctx := r.Context()
	if status == http.StatusPartialContent {
		err = bl.WriteRange(ctx, bw, rng.start, rng.end)
	} else {
		err = bl.WriteTo(ctx, bw)
	}
	if err == nil {
		bw.finish()
		return
	}
	if isClientGone(r, err) {
		s.log.Debug("blob pull abandoned by the client", "digest", d, "written", bw.n, "error", err)
		return
	}
	if !bw.started {
		h.Del("Content-Length")
		h.Del("Content-Range")
		h.Del("Docker-Content-Digest")
		h.Del("Accept-Ranges")
		s.handleError(w, r, err)
		return
	}
	// Bytes are already on the wire: cut the connection so the client sees
	// a truncated body rather than a clean, wrong one. A failure here means
	// compressor drift or a corrupt store and needs operator attention.
	s.log.Error("blob pull failed after the response started; aborting connection",
		"digest", d, "kind", bl.Meta.Kind, "written", bw.n, "error", err)
	panic(http.ErrAbortHandler)
}

// deleteBlob drops the blob's reference; the objects become garbage.
func (s *server) deleteBlob(w http.ResponseWriter, r *http.Request, d oci.Digest) {
	if err := s.blobs.Delete(d); err != nil {
		s.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// bodyWriter sends the response status with the first byte written, so a
// stream that fails before producing any output can still be answered with
// an error envelope.
type bodyWriter struct {
	w       http.ResponseWriter
	status  int
	started bool
	n       int64
}

func (b *bodyWriter) Write(p []byte) (int, error) {
	if !b.started {
		b.started = true
		b.w.WriteHeader(b.status)
	}
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

// finish sends the status when the body turned out to be empty.
func (b *bodyWriter) finish() {
	if !b.started {
		b.started = true
		b.w.WriteHeader(b.status)
	}
}

// byteRange is an inclusive byte range of a response body.
type byteRange struct {
	start, end int64
}

var errUnsatisfiable = errors.New("range not satisfiable")

// parseRange interprets a Range request header for a body of size bytes.
// It returns (r, true, nil) for one satisfiable range, clamped to the body;
// (byteRange{}, false, nil) when the header is absent, malformed or lists
// several ranges, in which case the full body is served; and
// errUnsatisfiable when the single range lies entirely beyond the body.
func parseRange(header string, size int64) (byteRange, bool, error) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return byteRange{}, false, nil
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return byteRange{}, false, nil
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		// Suffix range: the last n bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return byteRange{}, false, nil
		}
		if n == 0 || size == 0 {
			return byteRange{}, false, errUnsatisfiable
		}
		n = min(n, size)
		return byteRange{start: size - n, end: size - 1}, true, nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false, nil
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return byteRange{}, false, nil
		}
	}
	if start >= size {
		return byteRange{}, false, errUnsatisfiable
	}
	return byteRange{start: start, end: min(end, size-1)}, true, nil
}
