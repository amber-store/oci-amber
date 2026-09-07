package dockerarchive

import (
	"errors"
	"fmt"
	"strings"

	"github.com/amber-store/oci-amber/oci"
)

// Name is a repository and tag to publish an image under.
type Name struct {
	Repo string
	Tag  string
}

func (n Name) String() string { return n.Repo + ":" + n.Tag }

// ParseName parses a --name value: repo[:tag], taken verbatim (a leading
// registry host is kept), with "latest" when the tag is missing. The tag
// is what follows the last ':' after the last '/'. Digest references are
// rejected: a name has to be a tag.
func ParseName(s string) (Name, error) {
	if s == "" {
		return Name{}, errors.New("empty name")
	}
	if strings.Contains(s, "@") {
		return Name{}, fmt.Errorf("%q: a name must be repo:tag, not a digest reference", s)
	}
	repo, tag := splitTag(s)
	if tag == "" {
		if strings.HasSuffix(s, ":") {
			return Name{}, fmt.Errorf("%q: empty tag", s)
		}
		tag = "latest"
	}
	if err := oci.ValidateRepository(repo); err != nil {
		return Name{}, fmt.Errorf("%q: %v", s, err)
	}
	if err := oci.ValidateTag(tag); err != nil {
		return Name{}, fmt.Errorf("%q: %v", s, err)
	}
	return Name{Repo: repo, Tag: tag}, nil
}

// splitTag splits repo[:tag] at the last ':' that follows the last '/'.
func splitTag(s string) (repo, tag string) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 || strings.IndexByte(s[i:], '/') >= 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// nameFromRepoTag turns a RepoTags entry into a Name: a leading registry
// host (a first path component holding '.' or ':', or "localhost") is
// dropped, a missing tag is "latest". ok is false for a digest reference,
// which names nothing to tag. Invalid repository or tag grammar is an
// error.
func nameFromRepoTag(s string) (Name, bool, error) {
	if strings.Contains(s, "@") {
		return Name{}, false, nil
	}
	if first, rest, found := strings.Cut(s, "/"); found && isHost(first) {
		s = rest
	}
	repo, tag := splitTag(s)
	if tag == "" {
		tag = "latest"
	}
	if err := oci.ValidateRepository(repo); err != nil {
		return Name{}, false, fmt.Errorf("RepoTags entry %q: %v", s, err)
	}
	if err := oci.ValidateTag(tag); err != nil {
		return Name{}, false, fmt.Errorf("RepoTags entry %q: %v", s, err)
	}
	return Name{Repo: repo, Tag: tag}, true, nil
}

// isHost is Docker's rule for the first component of a reference.
func isHost(c string) bool {
	return c == "localhost" || strings.ContainsAny(c, ".:")
}
