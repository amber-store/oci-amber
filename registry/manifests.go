package registry

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/amber-store/oci-amber/oci"
)

// maxManifestSize caps the body of PUT /v2/<name>/manifests/<reference>.
// Larger bodies are refused with 413 before anything is stored.
const maxManifestSize = 4 << 20

// manifestLocation is the canonical URL path of a stored manifest.
func manifestLocation(name string, d oci.Digest) string {
	return "/v2/" + name + "/manifests/" + d.String()
}

// handleManifests serves HEAD, GET, PUT and DELETE
// /v2/<name>/manifests/<reference>. The reference is a tag or a digest;
// image.Store tells them apart and reports a malformed one as DIGEST_INVALID
// (digest-shaped but not sha256), MANIFEST_INVALID (bad tag on PUT) or
// ErrNotFound (bad tag on the other methods: nothing can exist under it),
// which handleError renders.
func (s *server) handleManifests(w http.ResponseWriter, r *http.Request, rt route) {
	if rt.rest == "" {
		s.notFound(w)
		return
	}
	switch r.Method {
	case http.MethodHead:
		s.serveManifest(w, r, rt.name, rt.rest, false)
	case http.MethodGet:
		s.serveManifest(w, r, rt.name, rt.rest, true)
	case http.MethodPut:
		s.putManifest(w, r, rt.name, rt.rest)
	case http.MethodDelete:
		s.deleteManifest(w, r, rt.name, rt.rest)
	default:
		methodNotAllowed(w, r, http.MethodHead, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// serveManifest answers HEAD (withBody false) and GET for one manifest. The
// headers come from the stored meta.json; GET streams the exact pushed
// bytes, sha256-verified by image.Image.WriteTo. The Accept header is
// ignored: real clients accept the concrete type returned.
func (s *server) serveManifest(w http.ResponseWriter, r *http.Request, name, reference string, withBody bool) {
	im, err := s.images.Open(name, reference)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", im.Meta.MediaType)
	h.Set("Content-Length", strconv.FormatInt(im.Meta.Size, 10))
	h.Set("Docker-Content-Digest", im.Meta.Digest.String())
	if !withBody {
		w.WriteHeader(http.StatusOK)
		return
	}

	bw := &bodyWriter{w: w, status: http.StatusOK}
	ctx := r.Context()
	err = im.WriteTo(ctx, bw)
	if err == nil {
		bw.finish()
		return
	}
	if isClientGone(r, err) {
		s.log.Debug("manifest pull abandoned by the client", "repo", name, "reference", reference, "written", bw.n, "error", err)
		return
	}
	if !bw.started {
		h.Del("Content-Type")
		h.Del("Content-Length")
		h.Del("Docker-Content-Digest")
		s.handleError(w, r, err)
		return
	}
	// Bytes are already on the wire (a digest mismatch is only known at the
	// end): cut the connection so the client sees a truncated body rather
	// than a clean, wrong one. This means a corrupt store and needs operator
	// attention.
	s.log.Error("manifest pull failed after the response started; aborting connection",
		"repo", name, "reference", reference, "digest", im.Meta.Digest.String(), "written", bw.n, "error", err)
	panic(http.ErrAbortHandler)
}

// manifestTooLarge answers 413 for a body above maxManifestSize.
func manifestTooLarge(w http.ResponseWriter) {
	writeErrorStatus(w, http.StatusRequestEntityTooLarge,
		oci.NewError(oci.CodeManifestInvalid, "manifest exceeds the %d byte limit", maxManifestSize))
}

// putManifest reads the body under the 4 MiB cap and hands it to
// image.Store.Put, which validates the reference and the manifest, resolves
// every referenced blob and child, builds and publishes the image root and
// emits the image log line.
func (s *server) putManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	if r.ContentLength > maxManifestSize {
		manifestTooLarge(w)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManifestSize))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			manifestTooLarge(w)
			return
		}
		writeError(w, oci.NewError(oci.CodeManifestInvalid, "reading manifest body: %v", err))
		return
	}
	meta, err := s.images.Put(r.Context(), name, reference, r.Header.Get("Content-Type"), body)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Location", manifestLocation(name, meta.Digest))
	h.Set("Docker-Content-Digest", meta.Digest.String())
	if meta.Subject != nil {
		// Tells clients the referrers API is native, so they do not fall
		// back to maintaining tag-schema indexes.
		h.Set("OCI-Subject", meta.Subject.Digest.String())
	}
	w.WriteHeader(http.StatusCreated)
}

// deleteManifest drops a tag (by tag) or a manifest with its tags and
// referrer ref (by digest); image.Store.Delete decides which. Objects and
// blob refs are left alone.
func (s *server) deleteManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	if err := s.images.Delete(name, reference); err != nil {
		s.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
