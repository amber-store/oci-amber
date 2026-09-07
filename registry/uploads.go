package registry

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// uploadLocation is the URL path of an upload session.
func uploadLocation(name, id string) string {
	return "/v2/" + name + "/blobs/uploads/" + id
}

// uploadRange formats the Range header of upload responses: "0-<index of
// the last byte received>", "0-0" while the session is empty.
func uploadRange(offset int64) string {
	return fmt.Sprintf("0-%d", max(offset-1, 0))
}

// setUploadHeaders sets the headers every upload session response carries.
// Location is included on PATCH too: docker's client follows it for the
// next request.
func setUploadHeaders(w http.ResponseWriter, name string, sess *upload.Session) {
	h := w.Header()
	h.Set("Location", uploadLocation(name, sess.ID))
	h.Set("Docker-Upload-UUID", sess.ID)
	h.Set("Range", uploadRange(sess.Offset()))
}

// writeBlobCreated answers 201 for the blob d: the end of an upload, a
// mount, or an upload skipped because d was already stored.
func writeBlobCreated(w http.ResponseWriter, name string, d oci.Digest) {
	w.Header().Set("Location", blobLocation(name, d))
	w.Header().Set("Docker-Content-Digest", d.String())
	w.WriteHeader(http.StatusCreated)
}

// knownBlob reports whether want is already stored. A hit is recorded as a
// whole-blob dedup of this upload (blob.Store.Reuse), so the caller answers
// 201 without reading the request body at all. net/http takes care of the
// unread body: a small one is drained so the connection stays alive, a
// large one makes it close the connection behind the response, which every
// Go client turns into the 201 rather than a write error. The bytes the
// client would have sent are never compared with want: the blob stored
// under want is the right one whatever the client had.
func (s *server) knownBlob(want oci.Digest) (bool, error) {
	_, err := s.blobs.Reuse(want)
	if errors.Is(err, blob.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// discardSession drops a session. A session that is already gone (swept,
// or cancelled concurrently) is not an error. The session's lock is not
// touched here: its holder releases it on the way out.
func (s *server) discardSession(id string) {
	if err := s.uploads.Remove(id); err != nil && !errors.Is(err, upload.ErrUnknown) {
		s.log.Warn("removing upload session", "id", id, "error", err)
	}
}

// removeSpool deletes a spool's backing file, if any.
func (s *server) removeSpool(sp *upload.Spool, id string) {
	if err := sp.Remove(); err != nil {
		s.log.Warn("removing upload spool", "id", id, "error", err)
	}
}

// handleUpload serves everything under /v2/<name>/blobs/uploads/.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request, rt route) {
	if rt.rest == "" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		s.startUpload(w, r, rt.name)
		return
	}
	if strings.Contains(rt.rest, "/") {
		s.notFound(w)
		return
	}
	id := rt.rest
	switch r.Method {
	case http.MethodPatch:
		s.patchUpload(w, r, rt.name, id)
	case http.MethodPut:
		s.putUpload(w, r, rt.name, id)
	case http.MethodGet:
		s.uploadStatus(w, r, rt.name, id)
	case http.MethodDelete:
		s.cancelUpload(w, r, id)
	default:
		methodNotAllowed(w, r, http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodDelete)
	}
}

// startUpload handles POST: a cross-repository mount when ?mount= names a
// stored blob (blobs are global, so ?from= is not consulted), a monolithic
// upload when ?digest= is present, otherwise a new session.
func (s *server) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	if mount := q.Get("mount"); mount != "" {
		if d, err := oci.ParseDigest(mount); err == nil {
			exists, err := s.blobs.Exists(d)
			if err != nil {
				s.handleError(w, r, err)
				return
			}
			if exists {
				writeBlobCreated(w, name, d)
				return
			}
		}
	}
	if _, monolithic := q["digest"]; monolithic {
		want, err := oci.ParseDigest(q.Get("digest"))
		if err != nil {
			writeError(w, oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", q.Get("digest"), err))
			return
		}
		known, err := s.knownBlob(want)
		if err != nil {
			s.handleError(w, r, err)
			return
		}
		if known {
			writeBlobCreated(w, name, want)
			return
		}
		sess, err := s.uploads.Create()
		if err != nil {
			s.handleError(w, r, err)
			return
		}
		if _, err := sess.Append(r.Body); err != nil {
			s.discardSession(sess.ID)
			s.handleError(w, r, err)
			return
		}
		s.finalize(w, r, name, sess, want, true)
		return
	}
	sess, err := s.uploads.Create()
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	setUploadHeaders(w, name, sess)
	w.WriteHeader(http.StatusAccepted)
}

// patchUpload appends the body to the session. A Content-Range, when sent,
// must start at the current offset.
func (s *server) patchUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	unlock := s.sessLocks.Lock(id)
	defer unlock()
	sess, err := s.uploads.Get(id)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	if cr := r.Header.Get("Content-Range"); cr != "" {
		start, _, err := parseContentRange(cr)
		if err != nil {
			writeError(w, oci.NewError(oci.CodeBlobUploadInvalid, "invalid Content-Range %q: %v", cr, err))
			return
		}
		if off := sess.Offset(); start != off {
			setUploadHeaders(w, name, sess)
			writeErrorStatus(w, http.StatusRequestedRangeNotSatisfiable,
				oci.NewError(oci.CodeBlobUploadInvalid, "Content-Range starts at %d but the upload is at offset %d", start, off))
			return
		}
	}
	if _, err := sess.Append(r.Body); err != nil {
		s.handleError(w, r, err)
		return
	}
	setUploadHeaders(w, name, sess)
	w.WriteHeader(http.StatusAccepted)
}

// putUpload appends the optional body and finalizes the session. A digest
// that is already stored ends the upload right there: the session and
// whatever was PATCHed into it are dropped, the body stays unread.
func (s *server) putUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	unlock := s.sessLocks.Lock(id)
	defer unlock()
	sess, err := s.uploads.Get(id)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	dq := r.URL.Query().Get("digest")
	if dq == "" {
		writeError(w, oci.NewError(oci.CodeDigestInvalid, "digest parameter is required to complete an upload"))
		return
	}
	want, err := oci.ParseDigest(dq)
	if err != nil {
		writeError(w, oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", dq, err))
		return
	}
	known, err := s.knownBlob(want)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	if known {
		s.discardSession(sess.ID)
		writeBlobCreated(w, name, want)
		return
	}
	if _, err := sess.Append(r.Body); err != nil {
		s.handleError(w, r, err)
		return
	}
	s.finalize(w, r, name, sess, want, false)
}

// finalize checks the session's digest against the requested one, stores
// the blob (blob.Store.Put does the analysis, ingest and publication, and
// the whole-blob dedup for a digest that was published while this upload
// was in flight) and answers 201. monolithic marks a POST ?digest= upload,
// whose session id the client never learns, so it is discarded on every
// failure; a PUT's session survives internal failures so the client can
// retry the PUT. A digest mismatch discards the session either way: its
// bytes can never become valid. So does the store refusing to keep the
// blob raw: the same bytes would be refused again.
func (s *server) finalize(w http.ResponseWriter, r *http.Request, name string, sess *upload.Session, want oci.Digest, monolithic bool) {
	sp, err := sess.Spool()
	if err != nil {
		if monolithic {
			s.discardSession(sess.ID)
		}
		s.handleError(w, r, err)
		return
	}
	if got := sp.Digest(); got != want {
		s.discardSession(sess.ID)
		s.removeSpool(sp, sess.ID)
		e := oci.NewError(oci.CodeDigestInvalid, "uploaded content has digest %s, not %s", got, want)
		e.Detail = map[string]string{"expected": want.String(), "actual": got.String()}
		writeError(w, e)
		return
	}
	meta, err := s.blobs.Put(r.Context(), sp)
	if err != nil {
		var refused *blob.RawRefusedError
		if monolithic || errors.As(err, &refused) {
			s.discardSession(sess.ID)
			s.removeSpool(sp, sess.ID)
		}
		s.handleError(w, r, err)
		return
	}
	s.discardSession(sess.ID)
	s.removeSpool(sp, sess.ID)
	writeBlobCreated(w, name, meta.Digest)
}

// uploadStatus handles GET: the current offset of a session.
func (s *server) uploadStatus(w http.ResponseWriter, r *http.Request, name, id string) {
	unlock := s.sessLocks.Lock(id)
	defer unlock()
	sess, err := s.uploads.Get(id)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	setUploadHeaders(w, name, sess)
	w.WriteHeader(http.StatusNoContent)
}

// cancelUpload handles DELETE: drops the session and its spilled file.
func (s *server) cancelUpload(w http.ResponseWriter, r *http.Request, id string) {
	unlock := s.sessLocks.Lock(id)
	defer unlock()
	err := s.uploads.Remove(id)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseContentRange parses the Content-Range of an upload PATCH. The
// distribution spec uses "<start>-<end>"; the HTTP forms
// "bytes <start>-<end>/<total>" and "bytes=<start>-<end>" are tolerated.
func parseContentRange(h string) (start, end int64, err error) {
	v := strings.TrimSpace(h)
	v = strings.TrimPrefix(v, "bytes=")
	v = strings.TrimPrefix(v, "bytes ")
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	a, b, ok := strings.Cut(v, "-")
	if !ok {
		return 0, 0, errors.New("expected <start>-<end>")
	}
	start, err = strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid start %q", a)
	}
	end, err = strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err != nil || end < start {
		return 0, 0, fmt.Errorf("invalid end %q", b)
	}
	return start, end, nil
}
