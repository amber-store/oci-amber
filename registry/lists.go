package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// tagsResponse is the body of GET /v2/<name>/tags/list.
type tagsResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// catalogResponse is the body of GET /v2/_catalog.
type catalogResponse struct {
	Repositories []string `json:"repositories"`
}

// referrersIndex is the image index returned by the referrers API. Unlike
// oci.Manifest it never omits manifests, so an empty result is "manifests":[].
type referrersIndex struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Manifests     []oci.Descriptor `json:"manifests"`
}

// pageSize reads the n query parameter: -1 when absent, otherwise a
// non-negative integer. Anything else is an error.
func pageSize(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("n")
	if raw == "" {
		return -1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid n %q: must be a non-negative integer", raw)
	}
	return n, nil
}

// paginate returns the items that sort strictly after last (all of them when
// last is empty), truncated to n entries when n >= 0 (n < 0 means no limit).
// next is the last item of the returned page when more items remain after it
// and n > 0; it is "" when the page is the end of the list or when n == 0.
// page is never nil, so it encodes as [] rather than null. items must be
// sorted, which image.Store guarantees.
func paginate(items []string, n int, last string) (page []string, next string) {
	start := len(items)
	for i, it := range items {
		if it > last {
			start = i
			break
		}
	}
	page = items[start:]
	if n >= 0 && n < len(page) {
		page = page[:n]
		if n > 0 {
			next = page[n-1]
		}
	}
	if page == nil {
		page = []string{}
	}
	return page, next
}

// nextLink formats the Link header for the page after last.
func nextLink(path string, n int, last string) string {
	return fmt.Sprintf("<%s?n=%d&last=%s>; rel=\"next\"", path, n, url.QueryEscape(last))
}

// writeJSONAs writes v as a JSON body with an explicit content type, for the
// referrers index, which must be served as an OCI image index.
func writeJSONAs(w http.ResponseWriter, status int, contentType string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeEmptyErrors(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// invalidPageSize answers a malformed n. The standard code list has no
// pagination code, so UNSUPPORTED is sent with a 400.
func invalidPageSize(w http.ResponseWriter, err error) {
	writeErrorStatus(w, http.StatusBadRequest, oci.NewError(oci.CodeUnsupported, "%v", err))
}

// handleTags serves GET /v2/<name>/tags/list.
func (s *server) handleTags(w http.ResponseWriter, r *http.Request, rt route) {
	if rt.rest != "list" {
		s.notFound(w)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	n, err := pageSize(r)
	if err != nil {
		invalidPageSize(w, err)
		return
	}
	tags, err := s.images.Tags(rt.name)
	if err != nil {
		if errors.Is(err, image.ErrNotFound) {
			writeError(w, oci.NewError(oci.CodeNameUnknown, "repository %s is not known", rt.name))
			return
		}
		s.handleError(w, r, err)
		return
	}
	page, next := paginate(tags, n, r.URL.Query().Get("last"))
	if next != "" {
		w.Header().Set("Link", nextLink("/v2/"+rt.name+"/tags/list", n, next))
	}
	writeJSON(w, http.StatusOK, tagsResponse{Name: rt.name, Tags: page})
}

// handleReferrers serves GET /v2/<name>/referrers/<digest>. The answer is
// always 200 with an image index; the artifactType query parameter filters
// and is acknowledged with OCI-Filters-Applied.
func (s *server) handleReferrers(w http.ResponseWriter, r *http.Request, rt route) {
	if rt.rest == "" {
		s.notFound(w)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	subject, err := oci.ParseDigest(rt.rest)
	if err != nil {
		writeError(w, oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", rt.rest, err))
		return
	}
	artifactType := r.URL.Query().Get("artifactType")
	list, err := s.images.Referrers(rt.name, subject, artifactType)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	if list == nil {
		list = []oci.Descriptor{}
	}
	if artifactType != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	writeJSONAs(w, http.StatusOK, oci.MediaTypeOCIIndex, referrersIndex{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIIndex,
		Manifests:     list,
	})
}

// handleCatalog serves GET /v2/_catalog.
func (s *server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	n, err := pageSize(r)
	if err != nil {
		invalidPageSize(w, err)
		return
	}
	repos, err := s.images.Repositories()
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	page, next := paginate(repos, n, r.URL.Query().Get("last"))
	if next != "" {
		w.Header().Set("Link", nextLink("/v2/_catalog", n, next))
	}
	writeJSON(w, http.StatusOK, catalogResponse{Repositories: page})
}
