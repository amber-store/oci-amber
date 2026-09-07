package image

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/amber-store/oci-amber/oci"
)

// Tags returns the tags of repo in bytewise order, which is the
// "ASCIIbetical" (Go sort.Strings) order the distribution spec asks for.
// A repository's tags are exactly the refs with prefix "oci/tag/<repo>:";
// because repository names cannot contain ':' the prefix cannot match a
// neighbouring repository such as "<repo>2" or "<repo>/sub".
//
// A repository that has manifests but no tags yields an empty, non-nil
// slice (the registry encodes it as "tags": []). A repository with neither
// yields ErrNotFound, which the registry maps to NAME_UNKNOWN.
func (s *Store) Tags(repo string) ([]string, error) {
	refs, err := s.st.ListRefs(TagRef(repo, ""))
	if err != nil {
		return nil, fmt.Errorf("image: listing tags of %s: %w", repo, err)
	}
	tags := make([]string, 0, len(refs))
	for _, r := range refs {
		refRepo, tag, ok := ParseTagRef(r.Name)
		if !ok || refRepo != repo {
			continue
		}
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		known, err := s.repoHasManifests(repo)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, ErrNotFound
		}
	}
	slices.Sort(tags)
	return tags, nil
}

// repoHasManifests reports whether at least one oci/manifest/<repo>/<digest>
// ref exists. The prefix "oci/manifest/<repo>/" is also a prefix of every
// nested repository's manifest refs ("oci/manifest/<repo>/sub/<digest>"), so
// every name is parsed and its repository compared exactly.
func (s *Store) repoHasManifests(repo string) (bool, error) {
	refs, err := s.st.ListRefs(ManifestRef(repo, ""))
	if err != nil {
		return false, fmt.Errorf("image: listing manifests of %s: %w", repo, err)
	}
	for _, r := range refs {
		refRepo, _, ok := ParseManifestRef(r.Name)
		if ok && refRepo == repo {
			return true, nil
		}
	}
	return false, nil
}

// Referrers returns one descriptor per manifest or index in repo whose
// subject is subject, sorted by digest. Each descriptor carries the
// mediaType, digest, size, artifactType and annotations recorded in the
// referrer's meta.json at push time. That artifactType is already the
// manifest's own artifactType, or the config media type for an image
// manifest without one, or empty for an index without one, so the descriptor
// follows the distribution spec's rules without re-parsing the manifest.
//
// A non-empty artifactType keeps only descriptors whose artifactType equals
// it exactly. The result is never nil: an unknown subject, a subject nobody
// refers to, or a filter that matches nothing yields an empty slice, which
// the registry serves as an index with an empty manifests list.
func (s *Store) Referrers(repo string, subject oci.Digest, artifactType string) ([]oci.Descriptor, error) {
	refs, err := s.st.ListRefs(ReferrerRef(repo, subject, ""))
	if err != nil {
		return nil, fmt.Errorf("image: listing referrers of %s in %s: %w", subject, repo, err)
	}
	out := make([]oci.Descriptor, 0, len(refs))
	for _, r := range refs {
		refRepo, refSubject, _, ok := ParseReferrerRef(r.Name)
		if !ok || refRepo != repo || refSubject != subject {
			continue
		}
		m, err := s.readMeta(r.Key)
		if err != nil {
			return nil, fmt.Errorf("image: reading referrer %s: %w", r.Name, err)
		}
		if artifactType != "" && m.ArtifactType != artifactType {
			continue
		}
		out = append(out, oci.Descriptor{
			MediaType:    m.MediaType,
			Digest:       m.Digest,
			Size:         m.Size,
			ArtifactType: m.ArtifactType,
			Annotations:  m.Annotations,
		})
	}
	slices.SortFunc(out, func(a, b oci.Descriptor) int { return cmp.Compare(a.Digest, b.Digest) })
	return out, nil
}

// Repositories returns every repository that has at least one tag or
// manifest ref, sorted bytewise and without duplicates; nested names such
// as "library/app" are returned whole. Blob refs are global and do not name
// a repository, and a referrer ref never exists without the referrer's own
// manifest ref, so only the tag and manifest namespaces are scanned. The
// two literal prefixes are the ones TagRef and ManifestRef build on, which
// no builder call can yield bare. The result is never nil.
func (s *Store) Repositories() ([]string, error) {
	seen := make(map[string]struct{})
	tagRefs, err := s.st.ListRefs("oci/tag/")
	if err != nil {
		return nil, fmt.Errorf("image: listing tag refs: %w", err)
	}
	for _, r := range tagRefs {
		if repo, _, ok := ParseTagRef(r.Name); ok {
			seen[repo] = struct{}{}
		}
	}
	manifestRefs, err := s.st.ListRefs("oci/manifest/")
	if err != nil {
		return nil, fmt.Errorf("image: listing manifest refs: %w", err)
	}
	for _, r := range manifestRefs {
		if repo, _, ok := ParseManifestRef(r.Name); ok {
			seen[repo] = struct{}{}
		}
	}
	repos := make([]string, 0, len(seen))
	for repo := range seen {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	return repos, nil
}

// Manifests returns the digest of every manifest or index pushed to repo,
// tagged or not, sorted bytewise. The prefix "oci/manifest/<repo>/" is
// also a prefix of every nested repository's manifest refs, so each name
// is parsed and its repository compared exactly, as repoHasManifests
// does. A repository with no manifest refs yields ErrNotFound.
func (s *Store) Manifests(repo string) ([]oci.Digest, error) {
	refs, err := s.st.ListRefs(ManifestRef(repo, ""))
	if err != nil {
		return nil, fmt.Errorf("image: listing manifests of %s: %w", repo, err)
	}
	digests := make([]oci.Digest, 0, len(refs))
	for _, r := range refs {
		refRepo, d, ok := ParseManifestRef(r.Name)
		if ok && refRepo == repo {
			digests = append(digests, d)
		}
	}
	if len(digests) == 0 {
		return nil, ErrNotFound
	}
	slices.Sort(digests)
	return digests, nil
}
