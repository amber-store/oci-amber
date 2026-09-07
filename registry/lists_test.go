package registry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

func TestPaginate(t *testing.T) {
	all := []string{"a", "b", "c", "d"}
	cases := []struct {
		name  string
		items []string
		n     int
		last  string
		page  []string
		next  string
	}{
		{"no limit", all, -1, "", []string{"a", "b", "c", "d"}, ""},
		{"first page", all, 2, "", []string{"a", "b"}, "b"},
		{"second page ends list", all, 2, "b", []string{"c", "d"}, ""},
		{"limit beyond end", all, 3, "b", []string{"c", "d"}, ""},
		{"past end", all, 1, "d", []string{}, ""},
		{"n zero", all, 0, "", []string{}, ""},
		{"exact length", all, 4, "", []string{"a", "b", "c", "d"}, ""},
		{"last not in list", all, 1, "bb", []string{"c"}, "c"},
		{"last before first", all, 1, "0", []string{"a"}, "a"},
		{"nil items", nil, 5, "", []string{}, ""},
		{"empty items", []string{}, -1, "", []string{}, ""},
	}
	for _, tc := range cases {
		page, next := paginate(tc.items, tc.n, tc.last)
		if page == nil {
			t.Errorf("%s: page is nil, want a non-nil slice", tc.name)
		}
		if !reflect.DeepEqual(page, tc.page) {
			t.Errorf("%s: page = %v, want %v", tc.name, page, tc.page)
		}
		if next != tc.next {
			t.Errorf("%s: next = %q, want %q", tc.name, next, tc.next)
		}
	}
}

// linkQuery checks the shape of a Link header and its path, and returns the
// query string ("?…") to request the next page with.
func linkQuery(t *testing.T, link, wantPath string) string {
	t.Helper()
	end := strings.Index(link, ">")
	if !strings.HasPrefix(link, "<") || end < 0 || link[end:] != `>; rel="next"` {
		t.Fatalf("malformed Link %q", link)
	}
	u, err := url.Parse(link[1:end])
	if err != nil {
		t.Fatalf("Link URL %q: %v", link[1:end], err)
	}
	if u.Path != wantPath {
		t.Fatalf("Link path = %q, want %q", u.Path, wantPath)
	}
	return "?" + u.RawQuery
}

// tagsOf performs GET /v2/<name>/tags/list<query> and decodes the body.
func (e *testEnv) tagsOf(t *testing.T, name, query string) (repoName string, tags []string, link string) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/v2/"+name+"/tags/list"+query, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Type", "application/json")
	var body struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
	if body.Tags == nil {
		t.Fatalf("tags is null in %s, want an array", resp.body)
	}
	return body.Name, body.Tags, resp.Header.Get("Link")
}

func TestTagsList(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	for _, tag := range []string{"v2", "latest", "v1", "v1.0_rc-1", "V0"} {
		e.putManifest(t, "library/app", tag, oci.MediaTypeOCIManifest, body)
	}
	// Another repository's tags, and a nested one's, do not leak in.
	e.putManifest(t, "library/app2", "other", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "library/app/sub", "nested", oci.MediaTypeOCIManifest, body)

	name, tags, link := e.tagsOf(t, "library/app", "")
	if name != "library/app" {
		t.Errorf("name = %q, want library/app", name)
	}
	want := []string{"V0", "latest", "v1", "v1.0_rc-1", "v2"} // bytewise order: uppercase first
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	if link != "" {
		t.Errorf("unexpected Link %q on an unpaginated list", link)
	}
}

func TestTagsListPagination(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	for _, tag := range []string{"v2", "latest", "v1"} {
		e.putManifest(t, "library/app", tag, oci.MediaTypeOCIManifest, body)
	}
	const listPath = "/v2/library/app/tags/list"

	// n=1: three pages, each linking to the next.
	_, tags, link := e.tagsOf(t, "library/app", "?n=1")
	if !reflect.DeepEqual(tags, []string{"latest"}) {
		t.Fatalf("page 1 = %v, want [latest]", tags)
	}
	if want := `</v2/library/app/tags/list?n=1&last=latest>; rel="next"`; link != want {
		t.Fatalf("page 1 Link = %q, want %q", link, want)
	}
	_, tags, link = e.tagsOf(t, "library/app", linkQuery(t, link, listPath))
	if !reflect.DeepEqual(tags, []string{"v1"}) {
		t.Fatalf("page 2 = %v, want [v1]", tags)
	}
	if want := `</v2/library/app/tags/list?n=1&last=v1>; rel="next"`; link != want {
		t.Fatalf("page 2 Link = %q, want %q", link, want)
	}
	_, tags, link = e.tagsOf(t, "library/app", linkQuery(t, link, listPath))
	if !reflect.DeepEqual(tags, []string{"v2"}) {
		t.Fatalf("page 3 = %v, want [v2]", tags)
	}
	if link != "" {
		t.Fatalf("page 3 Link = %q, want none", link)
	}

	// n larger than the remainder: the rest, no Link.
	_, tags, link = e.tagsOf(t, "library/app", "?n=5&last=latest")
	if !reflect.DeepEqual(tags, []string{"v1", "v2"}) || link != "" {
		t.Errorf("n=5&last=latest = %v with Link %q, want [v1 v2] and no Link", tags, link)
	}

	// n=0: empty list, no Link.
	_, tags, link = e.tagsOf(t, "library/app", "?n=0")
	if len(tags) != 0 || link != "" {
		t.Errorf("n=0 = %v with Link %q, want empty and no Link", tags, link)
	}

	// last beyond the end: empty list, no Link.
	_, tags, link = e.tagsOf(t, "library/app", "?last=zzz")
	if len(tags) != 0 || link != "" {
		t.Errorf("last=zzz = %v with Link %q, want empty and no Link", tags, link)
	}

	// Invalid n.
	for _, q := range []string{"?n=abc", "?n=-1", "?n=1.5"} {
		resp := e.do(t, http.MethodGet, listPath+q, nil, nil)
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeUnsupported)
	}
}

func TestTagsListUnknownRepo(t *testing.T) {
	e := newTestEnv(t)
	resp := e.do(t, http.MethodGet, "/v2/library/nothing/tags/list", nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeNameUnknown)

	// A repository holding only a by-digest manifest exists but has no tags.
	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	d := oci.DigestOfBytes(body)
	e.putManifest(t, "library/untagged", d.String(), oci.MediaTypeOCIManifest, body)
	name, tags, link := e.tagsOf(t, "library/untagged", "")
	if name != "library/untagged" || len(tags) != 0 || link != "" {
		t.Errorf("untagged repo: name %q tags %v Link %q, want the name, empty tags and no Link", name, tags, link)
	}

	// Deleting the only manifest makes the repository unknown again.
	resp = e.do(t, http.MethodDelete, manifestPath("library/untagged", d.String()), nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
	resp = e.do(t, http.MethodGet, "/v2/library/untagged/tags/list", nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeNameUnknown)
}

func TestTagsListBadPathAndMethod(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/v2/library/app/tags/other", "/v2/library/app/tags/", "/v2/library/app/tags/list/more"} {
		resp := e.do(t, http.MethodGet, path, nil, nil)
		assertEmptyErrors(t, resp, http.StatusNotFound)
	}
	for _, m := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		resp := e.do(t, m, "/v2/library/app/tags/list", []byte("{}"), nil)
		assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
		assertHeader(t, resp, "Allow", "GET")
	}
}

// pushedReferrer records one referrer pushed by pushReferrers.
type pushedReferrer struct {
	digest       oci.Digest
	size         int64
	artifactType string
	annotations  map[string]string
}

// pushReferrers pushes a base manifest tagged "base" and two artifact
// manifests whose subject is the base. It returns the subject digest and the
// referrers sorted by digest.
func (e *testEnv) pushReferrers(t *testing.T, name string) (oci.Digest, []pushedReferrer) {
	t.Helper()
	cfg := e.storeConfig(t)
	base := manifestJSON(t, cfg, nil, nil)
	subject := oci.DigestOfBytes(base)
	e.putManifest(t, name, "base", oci.MediaTypeOCIManifest, base)
	subjectDesc := newDescriptor(oci.MediaTypeOCIManifest, subject, len(base))
	var out []pushedReferrer
	for _, at := range []string{"application/vnd.example.sbom", "application/vnd.example.signature"} {
		annotations := map[string]string{"org.example.kind": at}
		body := manifestJSON(t, cfg, []descriptor{cfg}, map[string]any{
			"artifactType": at,
			"subject":      subjectDesc,
			"annotations":  annotations,
		})
		d := oci.DigestOfBytes(body)
		resp := e.putManifest(t, name, d.String(), oci.MediaTypeOCIManifest, body)
		assertHeader(t, resp, "OCI-Subject", subject.String())
		out = append(out, pushedReferrer{digest: d, size: int64(len(body)), artifactType: at, annotations: annotations})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].digest < out[j].digest })
	return subject, out
}

// referrersOf performs GET /v2/<name>/referrers/<digest><query> and decodes
// the index it returns.
func (e *testEnv) referrersOf(t *testing.T, name, digest, query string) (manifests []oci.Descriptor, filters string) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/v2/"+name+"/referrers/"+digest+query, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Type", oci.MediaTypeOCIIndex)
	var index struct {
		SchemaVersion int              `json:"schemaVersion"`
		MediaType     string           `json:"mediaType"`
		Manifests     []oci.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(resp.body, &index); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
	if index.SchemaVersion != 2 || index.MediaType != oci.MediaTypeOCIIndex {
		t.Fatalf("index header = %d %q, want 2 %q", index.SchemaVersion, index.MediaType, oci.MediaTypeOCIIndex)
	}
	if index.Manifests == nil {
		t.Fatalf("manifests is null or absent in %s, want an array", resp.body)
	}
	return index.Manifests, resp.Header.Get("OCI-Filters-Applied")
}

func TestReferrers(t *testing.T) {
	e := newTestEnv(t)
	subject, want := e.pushReferrers(t, "library/app")

	got, filters := e.referrersOf(t, "library/app", subject.String(), "")
	if filters != "" {
		t.Errorf("OCI-Filters-Applied = %q without a filter, want none", filters)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d referrers, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Digest != want[i].digest || got[i].Size != want[i].size ||
			got[i].MediaType != oci.MediaTypeOCIManifest ||
			got[i].ArtifactType != want[i].artifactType ||
			!reflect.DeepEqual(got[i].Annotations, want[i].annotations) {
			t.Errorf("referrer %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Referrers are scoped to the repository and the subject.
	got, _ = e.referrersOf(t, "library/other", subject.String(), "")
	if len(got) != 0 {
		t.Errorf("referrers in another repository = %+v, want none", got)
	}
	got, _ = e.referrersOf(t, "library/app", want[0].digest.String(), "")
	if len(got) != 0 {
		t.Errorf("referrers of a referrer = %+v, want none", got)
	}

	// Deleting a referrer by digest removes it from the list.
	resp := e.do(t, http.MethodDelete, manifestPath("library/app", want[0].digest.String()), nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
	got, _ = e.referrersOf(t, "library/app", subject.String(), "")
	if len(got) != 1 || got[0].Digest != want[1].digest {
		t.Errorf("after deleting %s: referrers = %+v, want only %s", want[0].digest, got, want[1].digest)
	}
}

func TestReferrersFilter(t *testing.T) {
	e := newTestEnv(t)
	subject, want := e.pushReferrers(t, "library/app")

	got, filters := e.referrersOf(t, "library/app", subject.String(), "?artifactType="+url.QueryEscape("application/vnd.example.sbom"))
	if filters != "artifactType" {
		t.Errorf("OCI-Filters-Applied = %q, want artifactType", filters)
	}
	if len(got) != 1 || got[0].ArtifactType != "application/vnd.example.sbom" {
		t.Fatalf("filtered referrers = %+v, want exactly the sbom", got)
	}
	for _, w := range want {
		if w.artifactType == "application/vnd.example.sbom" && got[0].Digest != w.digest {
			t.Errorf("filtered digest = %s, want %s", got[0].Digest, w.digest)
		}
	}

	// A filter matching nothing: empty, still marked as applied.
	got, filters = e.referrersOf(t, "library/app", subject.String(), "?artifactType="+url.QueryEscape("application/vnd.example.none"))
	if len(got) != 0 || filters != "artifactType" {
		t.Errorf("unmatched filter: %+v with OCI-Filters-Applied %q, want empty and artifactType", got, filters)
	}
}

func TestReferrersEmptyAndBadDigest(t *testing.T) {
	e := newTestEnv(t)
	unknown := oci.DigestOfBytes([]byte("nobody refers to me"))
	got, filters := e.referrersOf(t, "library/app", unknown.String(), "")
	if len(got) != 0 || filters != "" {
		t.Errorf("unknown subject: %+v with OCI-Filters-Applied %q, want empty and no header", got, filters)
	}

	for _, bad := range []string{"latest", "sha256:" + fakeHex[:62], "sha512:" + fakeHex + fakeHex, "sha256:xyz"} {
		resp := e.do(t, http.MethodGet, "/v2/library/app/referrers/"+bad, nil, nil)
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	}

	resp := e.do(t, http.MethodGet, "/v2/library/app/referrers/", nil, nil)
	assertEmptyErrors(t, resp, http.StatusNotFound)

	resp = e.do(t, http.MethodDelete, "/v2/library/app/referrers/"+unknown.String(), nil, nil)
	assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	assertHeader(t, resp, "Allow", "GET")
}

// catalog performs GET /v2/_catalog<query> and decodes the body.
func (e *testEnv) catalog(t *testing.T, query string) ([]string, string) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/v2/_catalog"+query, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Type", "application/json")
	var body struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
	if body.Repositories == nil {
		t.Fatalf("repositories is null in %s, want an array", resp.body)
	}
	return body.Repositories, resp.Header.Get("Link")
}

func TestCatalog(t *testing.T) {
	e := newTestEnv(t)

	repos, link := e.catalog(t, "")
	if len(repos) != 0 || link != "" {
		t.Errorf("empty registry: %v with Link %q, want empty and no Link", repos, link)
	}

	cfg := e.storeConfig(t)
	body := manifestJSON(t, cfg, nil, nil)
	e.putManifest(t, "zeta", "v1", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "alpha/two", "v1", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "alpha/one", "v1", oci.MediaTypeOCIManifest, body)
	e.putManifest(t, "alpha/one", "v2", oci.MediaTypeOCIManifest, body) // second tag, same repository
	e.putManifest(t, "digest/only", oci.DigestOfBytes(body).String(), oci.MediaTypeOCIManifest, body)

	want := []string{"alpha/one", "alpha/two", "digest/only", "zeta"}
	repos, link = e.catalog(t, "")
	if !reflect.DeepEqual(repos, want) || link != "" {
		t.Errorf("catalog = %v with Link %q, want %v and no Link", repos, link, want)
	}

	repos, link = e.catalog(t, "?n=2")
	if !reflect.DeepEqual(repos, want[:2]) {
		t.Errorf("n=2 = %v, want %v", repos, want[:2])
	}
	if wantLink := `</v2/_catalog?n=2&last=alpha%2Ftwo>; rel="next"`; link != wantLink {
		t.Fatalf("n=2 Link = %q, want %q", link, wantLink)
	}
	repos, link = e.catalog(t, linkQuery(t, link, "/v2/_catalog"))
	if !reflect.DeepEqual(repos, want[2:]) || link != "" {
		t.Errorf("second page = %v with Link %q, want %v and no Link", repos, link, want[2:])
	}

	repos, link = e.catalog(t, "?n=0")
	if len(repos) != 0 || link != "" {
		t.Errorf("n=0 = %v with Link %q, want empty and no Link", repos, link)
	}

	resp := e.do(t, http.MethodGet, "/v2/_catalog?n=x", nil, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeUnsupported)
	resp = e.do(t, http.MethodPost, "/v2/_catalog", nil, nil)
	assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	assertHeader(t, resp, "Allow", "GET")
}
