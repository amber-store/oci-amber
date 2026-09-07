package registry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
)

const (
	// fsPrefix is the path prefix of the rootfs API.
	fsPrefix = "/fs/"
	// mediaTypeTar is the content type of a directory served as a tar.
	mediaTypeTar = "application/x-tar"
)

// fsRoute is a parsed /fs/<repo>:<tag>/<path> or /fs/<repo>@<digest>/<path>.
type fsRoute struct {
	repo, reference, path string
}

// parseFSPath splits the part of the request path after /fs/. The first
// segment holding '@' (checked first, since a digest carries a ':') or ':'
// ends the reference; the segments after it form the rootfs path. ok is
// false when no segment holds a separator.
func parseFSPath(rest string) (fsRoute, bool) {
	segs := strings.Split(rest, "/")
	for i, seg := range segs {
		sep := strings.IndexByte(seg, '@')
		if sep < 0 {
			sep = strings.IndexByte(seg, ':')
		}
		if sep < 0 {
			continue
		}
		repo := strings.Join(segs[:i], "/")
		if i > 0 {
			repo += "/"
		}
		repo += seg[:sep]
		return fsRoute{repo: repo, reference: seg[sep+1:], path: strings.Join(segs[i+1:], "/")}, true
	}
	return fsRoute{}, false
}

// displayPath renders a cleaned rootfs path for messages.
func displayPath(p string) string { return "/" + p }

// handleFS serves GET and HEAD under /fs/: a directory as a JSON listing or,
// with format=tar, as a tar; a regular file as bytes with ranges; any other
// entry as its JSON description.
func (s *server) handleFS(w http.ResponseWriter, r *http.Request, rest string) {
	rt, ok := parseFSPath(rest)
	if !ok {
		s.notFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, http.MethodGet, http.MethodHead)
		return
	}
	if err := oci.ValidateRepository(rt.repo); err != nil {
		writeError(w, oci.NewError(oci.CodeNameInvalid, "invalid repository name %q: %v", rt.repo, err))
		return
	}
	q := r.URL.Query()
	im, err := s.openRootfsImage(r, rt.repo, rt.reference, q.Get("platform"))
	if err != nil {
		s.handleError(w, r, err)
		return
	}
	fs, ok := im.FS()
	if !ok {
		rootfsUnavailable(w, im)
		return
	}
	p := rootfs.Clean(rt.path)
	ent, err := fs.Stat(p)
	if err != nil {
		s.fsError(w, r, p, err)
		return
	}
	format := q.Get("format")
	switch {
	case format != "" && format != "json" && format != "tar":
		writeError(w, oci.NewError(oci.CodePathInvalid, "unknown format %q: use json or tar", format))
	case ent.IsDir():
		if format == "tar" {
			s.serveTar(w, r, fs, p, ent)
		} else {
			s.serveListing(w, r, fs, p, ent)
		}
	case format == "tar":
		writeError(w, oci.NewError(oci.CodePathInvalid, "%s is not a directory; format=tar applies to directories", displayPath(p)))
	case ent.IsRegular():
		s.serveFile(w, r, fs, p, ent)
	default:
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(w, http.StatusOK, fsEntryJSON(ent))
	}
}

// openRootfsImage resolves reference in repo and, when it is an index, the
// child that platform names. An index without a usable platform is
// CodePlatformUnknown with the children's platforms in the detail.
func (s *server) openRootfsImage(r *http.Request, repo, reference, platform string) (*image.Image, error) {
	im, err := s.images.Open(repo, reference)
	if err != nil {
		return nil, err
	}
	if im.Meta.Kind != image.KindIndex {
		return im, nil
	}
	var body bytes.Buffer
	if err := im.WriteTo(r.Context(), &body); err != nil {
		return nil, err
	}
	idx, err := oci.ParseManifest(body.Bytes())
	if err != nil {
		// Not wrapped: a stored index the parser rejects is a server-side
		// fault, not the client's MANIFEST_INVALID.
		return nil, fmt.Errorf("registry: stored index %s: %v", im.Meta.Digest, err)
	}
	available := []string{}
	for _, d := range idx.Manifests {
		if d.Platform != nil {
			available = append(available, d.Platform.String())
		}
	}
	unknown := func(format string, args ...any) error {
		e := oci.NewError(oci.CodePlatformUnknown, format, args...)
		e.Detail = map[string]any{"platforms": available}
		return e
	}
	if platform == "" {
		return nil, unknown("%s is an index: pick a child with ?platform=os/architecture[/variant]", reference)
	}
	want, err := oci.ParsePlatform(platform)
	if err != nil {
		return nil, unknown("%v", err)
	}
	for _, d := range idx.Manifests {
		if d.Platform == nil || !want.Matches(*d.Platform) {
			continue
		}
		child, err := s.images.Open(repo, d.Digest.String())
		if errors.Is(err, image.ErrNotFound) {
			return nil, unknown("platform %s names %s, which is no longer stored", platform, d.Digest)
		}
		if err != nil {
			return nil, err
		}
		if child.Meta.Kind == image.KindIndex {
			return nil, unknown("platform %s names %s, which is itself an index", platform, d.Digest)
		}
		return child, nil
	}
	return nil, unknown("no child of %s has platform %s", reference, platform)
}

// rootfsUnavailable answers for an image without a rootfs tree.
func rootfsUnavailable(w http.ResponseWriter, im *image.Image) {
	status, reason := "absent", "stored before rootfs views existed"
	if rf := im.Meta.Rootfs; rf != nil {
		status, reason = string(rf.Status), rf.Reason
	}
	e := oci.NewError(oci.CodeRootfsUnavailable, "image %s has no root filesystem view (%s)", im.Meta.Digest, status)
	e.Detail = map[string]string{"status": status, "reason": reason}
	writeError(w, e)
}

// fsError maps a rootfs.FS error: a missing component or a file in the
// middle of the path is PATH_UNKNOWN, a symlink loop PATH_INVALID, anything
// else an internal failure.
func (s *server) fsError(w http.ResponseWriter, r *http.Request, p string, err error) {
	switch {
	case errors.Is(err, rootfs.ErrNotFound), errors.Is(err, rootfs.ErrNotDir):
		writeError(w, oci.NewError(oci.CodePathUnknown, "%v", err))
	case errors.Is(err, rootfs.ErrLoop):
		writeError(w, oci.NewError(oci.CodePathInvalid, "%v", err))
	default:
		s.handleError(w, r, err)
	}
}

// fsListing is the body of a directory listing.
type fsListing struct {
	Path    string    `json:"path"`
	Entries []fsEntry `json:"entries"`
}

// fsEntry is one entry of a listing, or the whole body for a device, fifo
// or socket.
type fsEntry struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Mode   string  `json:"mode"`
	UID    uint64  `json:"uid"`
	GID    uint64  `json:"gid"`
	Mtime  string  `json:"mtime"`
	Size   *int64  `json:"size,omitempty"`
	Target string  `json:"target,omitempty"`
	Major  *uint64 `json:"major,omitempty"`
	Minor  *uint64 `json:"minor,omitempty"`
}

// fsEntryJSON renders an entry: mode as four octal digits, mtime as RFC
// 3339 in UTC, and size, target, major and minor by type.
func fsEntryJSON(e rootfs.Entry) fsEntry {
	out := fsEntry{
		Name:  e.Name,
		Type:  e.TypeName(),
		Mode:  fmt.Sprintf("%04o", e.Mode&0o7777),
		UID:   e.UID,
		GID:   e.GID,
		Mtime: time.Unix(0, e.Mtime).UTC().Format(time.RFC3339Nano),
	}
	switch out.Type {
	case "file":
		size := e.Size
		out.Size = &size
	case "symlink":
		out.Target = e.Target
	case "char", "block":
		major, minor := e.Rdev[0], e.Rdev[1]
		out.Major, out.Minor = &major, &minor
	}
	return out
}

// serveListing answers a directory with its entries, paginated with n and
// last like tags/list; the Link header keeps the other query parameters. A
// HEAD sends the content type alone rather than listing for nothing.
func (s *server) serveListing(w http.ResponseWriter, r *http.Request, fs *rootfs.FS, p string, dir rootfs.Entry) {
	n, err := pageSize(r)
	if err != nil {
		invalidPageSize(w, err)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	var entries []rootfs.Entry
	more := false
	if n != 0 {
		entries, more, err = fs.ListDir(dir, r.URL.Query().Get("last"), n)
		if err != nil {
			s.fsError(w, r, p, err)
			return
		}
	}
	if more && n > 0 {
		q := r.URL.Query()
		q.Set("n", strconv.Itoa(n))
		q.Set("last", entries[len(entries)-1].Name)
		w.Header().Set("Link", fmt.Sprintf("<%s?%s>; rel=\"next\"", r.URL.EscapedPath(), q.Encode()))
	}
	out := fsListing{Path: p, Entries: make([]fsEntry, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, fsEntryJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// matchesETag reports whether an If-None-Match header names etag.
func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || candidate == "W/"+etag {
			return true
		}
	}
	return false
}

// serveFile streams a regular file with an ETag, If-None-Match, and a single
// Range honoured like a raw blob's.
func (s *server) serveFile(w http.ResponseWriter, r *http.Request, fs *rootfs.FS, p string, ent rootfs.Entry) {
	etag := `"` + ent.Content.String() + `"`
	h := w.Header()
	h.Set("ETag", etag)
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	size := ent.Size
	h.Set("Content-Type", oci.MediaTypeOctetStream)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Accept-Ranges", "bytes")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	status := http.StatusOK
	rng := byteRange{start: 0, end: size - 1}
	br, ok, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		h.Del("Content-Type")
		h.Del("ETag")
		h.Del("Accept-Ranges")
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
	rd, err := fs.Content(ent)
	if err != nil {
		s.fsError(w, r, p, err)
		return
	}
	defer rd.Close()
	bw := &bodyWriter{w: w, status: status}
	err = copyFileRange(bw, rd, rng)
	if err == nil {
		bw.finish()
		return
	}
	if isClientGone(r, err) {
		s.log.Debug("rootfs file transfer abandoned by the client", "path", r.URL.Path, "written", bw.n, "error", err)
		return
	}
	if !bw.started {
		h.Del("Content-Length")
		h.Del("Content-Range")
		h.Del("ETag")
		h.Del("Accept-Ranges")
		s.handleError(w, r, err)
		return
	}
	s.log.Error("rootfs file transfer failed after the response started; aborting connection",
		"path", r.URL.Path, "written", bw.n, "error", err)
	panic(http.ErrAbortHandler)
}

// copyFileRange writes bytes rng.start..rng.end of rd to w, skipping whole
// chunks before the start without reading them.
func copyFileRange(w io.Writer, rd interface {
	io.Reader
	Skip(int64) error
}, rng byteRange) error {
	if rng.end < rng.start {
		return nil
	}
	if rng.start > 0 {
		if err := rd.Skip(rng.start); err != nil {
			return err
		}
	}
	_, err := io.CopyN(w, rd, rng.end-rng.start+1)
	return err
}

// serveTar streams a directory as a PAX tar. The body has no
// Content-Length; a failure after the first byte aborts the connection.
func (s *server) serveTar(w http.ResponseWriter, r *http.Request, fs *rootfs.FS, p string, dir rootfs.Entry) {
	h := w.Header()
	h.Set("Content-Type", mediaTypeTar)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	bw := &bodyWriter{w: w, status: http.StatusOK}
	err := fs.WriteTarDir(bw, dir)
	if err == nil {
		bw.finish()
		return
	}
	if isClientGone(r, err) {
		s.log.Debug("rootfs tar abandoned by the client", "path", r.URL.Path, "written", bw.n, "error", err)
		return
	}
	if !bw.started {
		h.Del("Content-Type")
		s.fsError(w, r, p, err)
		return
	}
	s.log.Error("rootfs tar failed after the response started; aborting connection",
		"path", r.URL.Path, "written", bw.n, "error", err)
	panic(http.ErrAbortHandler)
}
