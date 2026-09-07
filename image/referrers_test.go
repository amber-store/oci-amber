package image_test

import (
	"cmp"
	"maps"
	"slices"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

const (
	sbomType      = "application/vnd.example.sbom.v1"
	signatureType = "application/vnd.example.signature.v1"
)

func byDigest(a, b oci.Descriptor) int { return cmp.Compare(a.Digest, b.Digest) }

// sameDescriptor compares the fields the referrers API serves, treating nil
// and empty annotations alike.
func sameDescriptor(a, b oci.Descriptor) bool {
	return a.MediaType == b.MediaType && a.Digest == b.Digest && a.Size == b.Size &&
		a.ArtifactType == b.ArtifactType && maps.Equal(a.Annotations, b.Annotations)
}

func assertDescriptors(t *testing.T, got, want []oci.Descriptor) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil descriptors, want a non-nil slice")
	}
	if len(got) != len(want) {
		t.Fatalf("got %d descriptors %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if !sameDescriptor(got[i], want[i]) {
			t.Fatalf("descriptor %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// referrerSet pushes a tagged subject into repo and three referrers to it:
// an SBOM manifest with an explicit artifactType and annotations, a plain
// image manifest whose artifactType must fall back to the config media type,
// and an index (over one extra child manifest) without artifactType, which
// must carry none. It returns the subject descriptor and the expected
// referrer descriptors sorted by digest.
func referrerSet(t *testing.T, env *listEnv, repo string) (oci.Descriptor, []oci.Descriptor) {
	t.Helper()
	subjectMeta, _ := env.put(repo, "v1", env.manifest("", nil, nil))
	subject := descriptorOf(subjectMeta)

	sbomAnnotations := map[string]string{
		"org.opencontainers.image.created": "2026-09-03T18:00:00Z",
		"org.example.sbom.format":          "json",
	}
	sbomMeta, sbomBody := env.put(repo, "", env.manifest(sbomType, &subject, sbomAnnotations))
	plainMeta, plainBody := env.put(repo, "", env.manifest("", &subject, map[string]string{"kind": "plain"}))

	childMeta, _ := env.put(repo, "", env.manifest("", nil, map[string]string{"kind": "child"}))
	index := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeOCIIndex,
		Manifests:     []oci.Descriptor{descriptorOf(childMeta)},
		Subject:       &subject,
		Annotations:   map[string]string{"kind": "index"},
	}
	indexMeta, indexBody := env.put(repo, "", index)

	want := []oci.Descriptor{
		{
			MediaType:    oci.MediaTypeOCIManifest,
			Digest:       sbomMeta.Digest,
			Size:         int64(len(sbomBody)),
			ArtifactType: sbomType,
			Annotations:  sbomAnnotations,
		},
		{
			MediaType:    oci.MediaTypeOCIManifest,
			Digest:       plainMeta.Digest,
			Size:         int64(len(plainBody)),
			ArtifactType: oci.MediaTypeOCIConfig,
			Annotations:  map[string]string{"kind": "plain"},
		},
		{
			MediaType:   oci.MediaTypeOCIIndex,
			Digest:      indexMeta.Digest,
			Size:        int64(len(indexBody)),
			Annotations: map[string]string{"kind": "index"},
		},
	}
	slices.SortFunc(want, byDigest)
	return subject, want
}

func TestReferrersSorted(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	subject, want := referrerSet(t, env, repo)

	got, err := env.images.Referrers(repo, subject.Digest, "")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	if !slices.IsSortedFunc(got, byDigest) {
		t.Fatalf("Referrers not sorted by digest: %+v", got)
	}
	assertDescriptors(t, got, want)
}

func TestReferrersArtifactTypeFilter(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	subject, all := referrerSet(t, env, repo)

	cases := []struct {
		name         string
		artifactType string
	}{
		{"explicit artifactType", sbomType},
		{"config media type fallback", oci.MediaTypeOCIConfig},
		{"no match", signatureType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want []oci.Descriptor
			for _, d := range all {
				if d.ArtifactType == tc.artifactType {
					want = append(want, d)
				}
			}
			got, err := env.images.Referrers(repo, subject.Digest, tc.artifactType)
			if err != nil {
				t.Fatalf("Referrers(%q): %v", tc.artifactType, err)
			}
			assertDescriptors(t, got, want)
			for _, d := range got {
				if d.ArtifactType != tc.artifactType {
					t.Fatalf("filter %q returned %+v", tc.artifactType, d)
				}
			}
		})
	}
}

func TestReferrersUnknownSubject(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	subject, want := referrerSet(t, env, repo)

	// A referrer to the same subject digest in another repository is
	// visible only from that repository.
	otherMeta, otherBody := env.put("library/other", "", env.manifest(signatureType, &subject, nil))
	other := oci.Descriptor{
		MediaType:    oci.MediaTypeOCIManifest,
		Digest:       otherMeta.Digest,
		Size:         int64(len(otherBody)),
		ArtifactType: signatureType,
	}

	cases := []struct {
		name    string
		repo    string
		subject oci.Digest
		want    []oci.Descriptor
	}{
		{"digest nobody refers to", repo, oci.DigestOfBytes([]byte("nobody refers to this")), []oci.Descriptor{}},
		{"unknown repository", "nope/nothing", subject.Digest, []oci.Descriptor{}},
		{"prefix of the repository", "library", subject.Digest, []oci.Descriptor{}},
		{"same subject, other repository", "library/other", subject.Digest, []oci.Descriptor{other}},
		{"own repository unaffected", repo, subject.Digest, want},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.images.Referrers(tc.repo, tc.subject, "")
			if err != nil {
				t.Fatalf("Referrers(%q, %s): %v", tc.repo, tc.subject, err)
			}
			assertDescriptors(t, got, tc.want)
		})
	}
}

func TestReferrersSubjectAbsent(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	// The subject is never pushed; the referrer must still be listed, since
	// clients may push a manifest and its referrers in either order.
	subject := oci.Descriptor{
		MediaType: oci.MediaTypeOCIManifest,
		Digest:    oci.DigestOfBytes([]byte("pushed later, or never")),
		Size:      22,
	}
	meta, body := env.put(repo, "", env.manifest(signatureType, &subject, nil))

	got, err := env.images.Referrers(repo, subject.Digest, "")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	assertDescriptors(t, got, []oci.Descriptor{{
		MediaType:    oci.MediaTypeOCIManifest,
		Digest:       meta.Digest,
		Size:         int64(len(body)),
		ArtifactType: signatureType,
	}})
}

func TestReferrersAfterDelete(t *testing.T) {
	env := newListEnv(t)
	const repo = "library/app"
	subject, want := referrerSet(t, env, repo)

	// Deleting the SBOM referrer by digest removes its referrer ref and
	// nothing else.
	remaining := []oci.Descriptor{}
	for _, d := range want {
		if d.ArtifactType == sbomType {
			if err := env.images.Delete(repo, string(d.Digest)); err != nil {
				t.Fatalf("Delete(%s): %v", d.Digest, err)
			}
			continue
		}
		remaining = append(remaining, d)
	}
	got, err := env.images.Referrers(repo, subject.Digest, "")
	if err != nil {
		t.Fatalf("Referrers after deleting a referrer: %v", err)
	}
	assertDescriptors(t, got, remaining)

	// Deleting the subject itself leaves its referrers in place: a subject
	// does not have to exist for its referrers to be listed.
	if err := env.images.Delete(repo, string(subject.Digest)); err != nil {
		t.Fatalf("Delete(subject %s): %v", subject.Digest, err)
	}
	got, err = env.images.Referrers(repo, subject.Digest, "")
	if err != nil {
		t.Fatalf("Referrers after deleting the subject: %v", err)
	}
	assertDescriptors(t, got, remaining)
}
