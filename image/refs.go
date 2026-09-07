package image

import (
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// Reference name prefixes. Amber's reference.ValidateName allows '/' and
// ':' and forbids only '@' and control characters, so repository names
// (which cannot contain ':') and tags (which cannot contain '/' or ':')
// separate unambiguously.
const (
	TagPrefix      = "oci/tag/"
	ManifestPrefix = "oci/manifest/"
	ReferrerPrefix = "oci/referrer/"
)

// TagRef is the reference name of a tag: "oci/tag/<repo>:<tag>".
func TagRef(repo, tag string) string {
	return TagPrefix + repo + ":" + tag
}

// ManifestRef is the reference name of a manifest pushed to a repository:
// "oci/manifest/<repo>/<digest>".
func ManifestRef(repo string, d oci.Digest) string {
	return ManifestPrefix + repo + "/" + string(d)
}

// ReferrerRef is the reference name that records referrer -> subject inside a
// repository: "oci/referrer/<repo>/<subject>/<referrer>".
func ReferrerRef(repo string, subject, referrer oci.Digest) string {
	return ReferrerPrefix + repo + "/" + string(subject) + "/" + string(referrer)
}

// ParseTagRef splits a tag reference name into repository and tag. ok is
// false when the name has another prefix or the parts are not a valid
// repository name and tag.
func ParseTagRef(name string) (repo, tag string, ok bool) {
	rest, found := strings.CutPrefix(name, TagPrefix)
	if !found {
		return "", "", false
	}
	repo, tag, found = strings.Cut(rest, ":")
	if !found || repo == "" || tag == "" {
		return "", "", false
	}
	if oci.ValidateRepository(repo) != nil || oci.ValidateTag(tag) != nil {
		return "", "", false
	}
	return repo, tag, true
}

// ParseManifestRef splits a manifest reference name into repository and
// digest by cutting at the last '/'.
func ParseManifestRef(name string) (repo string, d oci.Digest, ok bool) {
	rest, found := strings.CutPrefix(name, ManifestPrefix)
	if !found {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return "", "", false
	}
	repo = rest[:i]
	d, err := oci.ParseDigest(rest[i+1:])
	if err != nil || oci.ValidateRepository(repo) != nil {
		return "", "", false
	}
	return repo, d, true
}

// ParseReferrerRef splits a referrer reference name into repository,
// subject digest and referrer digest by cutting at the last two '/'.
func ParseReferrerRef(name string) (repo string, subject, referrer oci.Digest, ok bool) {
	rest, found := strings.CutPrefix(name, ReferrerPrefix)
	if !found {
		return "", "", "", false
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return "", "", "", false
	}
	referrer, err := oci.ParseDigest(rest[i+1:])
	if err != nil {
		return "", "", "", false
	}
	rest = rest[:i]
	j := strings.LastIndexByte(rest, '/')
	if j <= 0 {
		return "", "", "", false
	}
	subject, err = oci.ParseDigest(rest[j+1:])
	if err != nil {
		return "", "", "", false
	}
	repo = rest[:j]
	if oci.ValidateRepository(repo) != nil {
		return "", "", "", false
	}
	return repo, subject, referrer, true
}
