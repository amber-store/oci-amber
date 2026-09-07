package image

import (
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestTagRefRoundTrip(t *testing.T) {
	cases := []struct{ repo, tag, want string }{
		{"app", "v1", "oci/tag/app:v1"},
		{"library/app", "v1.0.3", "oci/tag/library/app:v1.0.3"},
		{"a/b/c-d.e_f", "sha256-abc.sig", "oci/tag/a/b/c-d.e_f:sha256-abc.sig"},
		{"app", "_under", "oci/tag/app:_under"},
	}
	for _, c := range cases {
		got := TagRef(c.repo, c.tag)
		if got != c.want {
			t.Errorf("TagRef(%q,%q) = %q, want %q", c.repo, c.tag, got, c.want)
		}
		repo, tag, ok := ParseTagRef(got)
		if !ok || repo != c.repo || tag != c.tag {
			t.Errorf("ParseTagRef(%q) = %q,%q,%v", got, repo, tag, ok)
		}
	}
	for _, bad := range []string{
		"oci/tag/",                        // empty
		"oci/tag/app",                     // no colon
		"oci/tag/:v1",                     // empty repo
		"oci/tag/app:",                    // empty tag
		"oci/tag/App:v1",                  // invalid repo
		"oci/tag/app:-v1",                 // invalid tag
		"oci/manifest/app/sha256:" + hexA, // wrong prefix
		"tag/app:v1",
		"oci/tag/app:" + strings.Repeat("x", 129), // tag too long
	} {
		if _, _, ok := ParseTagRef(bad); ok {
			t.Errorf("ParseTagRef(%q) ok, want !ok", bad)
		}
	}
}

func TestManifestRefRoundTrip(t *testing.T) {
	d := oci.Digest("sha256:" + hexA)
	cases := []struct{ repo, want string }{
		{"app", "oci/manifest/app/sha256:" + hexA},
		{"library/app", "oci/manifest/library/app/sha256:" + hexA},
		{"x/y.z/w_1", "oci/manifest/x/y.z/w_1/sha256:" + hexA},
	}
	for _, c := range cases {
		got := ManifestRef(c.repo, d)
		if got != c.want {
			t.Errorf("ManifestRef(%q) = %q, want %q", c.repo, got, c.want)
		}
		repo, pd, ok := ParseManifestRef(got)
		if !ok || repo != c.repo || pd != d {
			t.Errorf("ParseManifestRef(%q) = %q,%q,%v", got, repo, pd, ok)
		}
	}
	for _, bad := range []string{
		"oci/manifest/",
		"oci/manifest/app",                       // no digest
		"oci/manifest/app/sha256:zz",             // bad digest
		"oci/manifest/app/sha512:" + hexA + hexA, // wrong algorithm
		"oci/manifest//sha256:" + hexA,           // empty repo
		"oci/manifest/App/sha256:" + hexA,        // invalid repo
		"oci/tag/app:v1",
	} {
		if _, _, ok := ParseManifestRef(bad); ok {
			t.Errorf("ParseManifestRef(%q) ok, want !ok", bad)
		}
	}
}

func TestReferrerRefRoundTrip(t *testing.T) {
	subject := oci.Digest("sha256:" + hexA)
	referrer := oci.Digest("sha256:" + hexB)
	for _, repo := range []string{"app", "library/app", "a/b/c"} {
		got := ReferrerRef(repo, subject, referrer)
		want := "oci/referrer/" + repo + "/sha256:" + hexA + "/sha256:" + hexB
		if got != want {
			t.Errorf("ReferrerRef(%q) = %q, want %q", repo, got, want)
		}
		r, s, ref, ok := ParseReferrerRef(got)
		if !ok || r != repo || s != subject || ref != referrer {
			t.Errorf("ParseReferrerRef(%q) = %q,%q,%q,%v", got, r, s, ref, ok)
		}
	}
	for _, bad := range []string{
		"oci/referrer/app/sha256:" + hexA,                  // only one digest
		"oci/referrer/app/sha256:" + hexA + "/sha256:zz",   // bad referrer digest
		"oci/referrer/app/sha256:zz/sha256:" + hexB,        // bad subject digest
		"oci/referrer//sha256:" + hexA + "/sha256:" + hexB, // empty repo
		"oci/manifest/app/sha256:" + hexA,
	} {
		if _, _, _, ok := ParseReferrerRef(bad); ok {
			t.Errorf("ParseReferrerRef(%q) ok, want !ok", bad)
		}
	}
}

func TestRefPrefixesDoNotCollide(t *testing.T) {
	// "library/app" tags must not be matched by a scan for "library/ap".
	if strings.HasPrefix(TagRef("library/app", "v1"), TagRef("library/ap", "")) {
		t.Fatal("tag prefix of a shorter repo name matches a longer repo")
	}
	if !strings.HasPrefix(TagRef("library/app", "v1"), TagPrefix+"library/app:") {
		t.Fatal("tag ref does not start with the repo's tag prefix")
	}
}
