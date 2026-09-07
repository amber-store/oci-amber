package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/amber-store/core/key"
	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/keyedmutex"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/rootfs"
	"github.com/amber-store/oci-amber/store"
)

// Store keeps manifests and indexes as image roots in an amber store and
// maintains the tag, manifest and referrer references that name them.
type Store struct {
	st    *store.Store
	blobs *blob.Store
	log   *slog.Logger
	// repos serializes the publishing part of Put, and Delete, per
	// repository, so a delete's tag scan never interleaves with a publish
	// in the same repository. Put's rootfs build runs before taking it.
	repos keyedmutex.Mutex[string]
}

// New returns a Store over st. blobs resolves config and layer digests and
// supplies the per-blob accounting consumed by the image log line. A nil log
// uses slog.Default.
func New(st *store.Store, blobs *blob.Store, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{st: st, blobs: blobs, log: log}
}

// Image is an opened image root.
type Image struct {
	Meta      Meta
	root      key.Key
	manifest  key.Key
	rootfs    key.Key
	hasRootfs bool
	st        *store.Store
}

// Root returns the image root key.
func (im *Image) Root() key.Key { return im.root }

// Rootfs returns the key of the image's rootfs/ directory, when Meta.Rootfs
// says one is present.
func (im *Image) Rootfs() (key.Key, bool) { return im.rootfs, im.hasRootfs }

// FS returns a reader over the image's rootfs/ tree, when it has one.
func (im *Image) FS() (*rootfs.FS, bool) {
	if !im.hasRootfs {
		return nil, false
	}
	return rootfs.NewFS(im.st, im.rootfs), true
}

// WriteTo streams the stored manifest bytes to w while hashing them, and
// returns ErrDigestMismatch if they do not hash to Meta.Digest. The bytes
// have been written by then, so a caller serving HTTP must abort the
// response rather than finish it cleanly. ctx is checked between reads.
func (im *Image) WriteTo(ctx context.Context, w io.Writer) error {
	r := im.st.NewReader(im.manifest)
	defer r.Close()
	h := sha256.New()
	mw := io.MultiWriter(w, h)
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := mw.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("image: reading manifest %s: %w", im.manifest, err)
		}
	}
	if oci.DigestFromSum(h.Sum(nil)) != im.Meta.Digest {
		return ErrDigestMismatch
	}
	return nil
}

// Put validates and stores a manifest or index pushed to repo under
// reference (a tag or a digest) with the request's Content-Type, publishes
// its references, emits the image log line and returns the stored Meta.
//
// Client faults are returned as *oci.Error: CodeNameInvalid for repo,
// CodeDigestInvalid when reference is a digest that is not sha256 or does
// not match the body, CodeManifestInvalid for a bad tag (message "invalid
// tag ...") or an unparsable body, CodeManifestBlobUnknown with Detail
// {"digest": d} for a config, layer or child manifest that is not stored.
// Any other error is internal; nothing has been published when it is
// returned.
func (s *Store) Put(ctx context.Context, repo, reference, contentType string, body []byte) (*Meta, error) {
	start := time.Now()
	if err := oci.ValidateRepository(repo); err != nil {
		return nil, oci.NewError(oci.CodeNameInvalid, "invalid repository name %q: %v", repo, err)
	}
	digest := oci.DigestOfBytes(body)
	tag := ""
	if oci.IsDigest(reference) {
		d, err := oci.ParseDigest(reference)
		if err != nil {
			return nil, oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", reference, err)
		}
		if d != digest {
			return nil, oci.NewError(oci.CodeDigestInvalid, "manifest digest is %s, not %s", digest, d)
		}
	} else {
		if err := oci.ValidateTag(reference); err != nil {
			return nil, oci.NewError(oci.CodeManifestInvalid, "invalid tag %q: %v", reference, err)
		}
		tag = reference
	}
	m, err := oci.ParseManifest(body)
	if err != nil {
		return nil, err
	}
	if m.Subject != nil {
		if _, err := oci.ParseDigest(string(m.Subject.Digest)); err != nil {
			return nil, oci.NewError(oci.CodeManifestInvalid, "invalid subject digest %q: %v", m.Subject.Digest, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	meta := Meta{
		Version:      metaVersion,
		Kind:         KindManifest,
		MediaType:    m.EffectiveMediaType(contentType),
		Digest:       digest,
		Size:         int64(len(body)),
		ArtifactType: m.EffectiveArtifactType(),
		Subject:      m.Subject,
		Annotations:  m.Annotations,
		CreatedAt:    time.Now().UTC(),
	}
	if m.IsIndex() {
		meta.Kind = KindIndex
	}

	// Blob resolution and the rootfs build (image manifests only) read
	// nothing the repository lock guards, so they run before it: the
	// platform manifests of one image pushed at once build their trees at
	// the same time instead of queueing on the repository. The rootfs goes
	// through the accounting writer that pass one continues with.
	blobRoots, err := s.resolveBlobs(m)
	if err != nil {
		return nil, err
	}
	w := s.st.NewWriter(ctx)
	defer w.Abort()
	var rootfsKey key.Key
	if !m.IsIndex() {
		meta.Rootfs, rootfsKey, err = s.buildRootfs(ctx, w, repo, digest, m)
		if err != nil {
			return nil, err
		}
	}

	unlock := s.repos.Lock(repo)
	defer unlock()

	var childRoots map[oci.Digest]key.Key
	if m.IsIndex() {
		childRoots, err = s.resolveChildren(repo, m)
		if err != nil {
			return nil, err
		}
	}

	// Pass one, continued: the manifest's own objects (manifest bytes,
	// blobs/ and manifests/).
	manifestKey, err := w.PutBytes(body)
	if err != nil {
		return nil, fmt.Errorf("image: storing manifest: %w", err)
	}
	blobsKey, err := buildRefDir(w, blobRoots)
	if err != nil {
		return nil, fmt.Errorf("image: building %s/: %w", BlobsDir, err)
	}
	var manifestsKey key.Key
	if m.IsIndex() {
		manifestsKey, err = buildRefDir(w, childRoots)
		if err != nil {
			return nil, fmt.Errorf("image: building %s/: %w", ManifestsDir, err)
		}
	}
	objStats, err := w.Close()
	if err != nil {
		return nil, fmt.Errorf("image: writing manifest objects: %w", err)
	}

	meta.Stats, err = s.computeStats(ctx, repo, m, body, objStats)
	if err != nil {
		return nil, err
	}

	// Pass two: meta.json and the root. Excluded from the accounting because
	// meta.json carries the numbers.
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("image: encoding %s: %w", MetaFile, err)
	}
	metaBytes = append(metaBytes, '\n')
	w2 := s.st.NewWriter(ctx)
	defer w2.Abort()
	metaKey, err := w2.PutBytes(metaBytes)
	if err != nil {
		return nil, fmt.Errorf("image: storing %s: %w", MetaFile, err)
	}
	root := w2.NewDir()
	if err := root.AddDir(BlobsDir, blobsKey); err != nil {
		return nil, fmt.Errorf("image: building root: %w", err)
	}
	if err := root.AddFile(ManifestFile, manifestKey); err != nil {
		return nil, fmt.Errorf("image: building root: %w", err)
	}
	if m.IsIndex() {
		if err := root.AddDir(ManifestsDir, manifestsKey); err != nil {
			return nil, fmt.Errorf("image: building root: %w", err)
		}
	}
	if err := root.AddFile(MetaFile, metaKey); err != nil {
		return nil, fmt.Errorf("image: building root: %w", err)
	}
	if rootfsKey != (key.Key{}) {
		if err := root.AddDir(RootfsDir, rootfsKey); err != nil {
			return nil, fmt.Errorf("image: building root: %w", err)
		}
	}
	rootKey, err := root.Finish()
	if err != nil {
		return nil, fmt.Errorf("image: building root: %w", err)
	}
	if _, err := w2.Close(); err != nil {
		return nil, fmt.Errorf("image: writing root: %w", err)
	}

	// Publish: manifest ref, then tag, then referrer.
	if err := s.st.Publish(ManifestRef(repo, digest), rootKey); err != nil {
		return nil, fmt.Errorf("image: publishing manifest ref: %w", err)
	}
	if tag != "" {
		if err := s.st.Publish(TagRef(repo, tag), rootKey); err != nil {
			return nil, fmt.Errorf("image: publishing tag ref: %w", err)
		}
	}
	if m.Subject != nil {
		if err := s.st.Publish(ReferrerRef(repo, m.Subject.Digest, digest), rootKey); err != nil {
			return nil, fmt.Errorf("image: publishing referrer ref: %w", err)
		}
	}
	s.logPushed(repo, reference, &meta, len(blobRoots), len(childRoots), time.Since(start))
	s.logRootfs(repo, digest, meta.Rootfs)
	return &meta, nil
}

// resolveBlobs maps every unique config and layer digest of m to its blob
// root, or reports the first missing one as CodeManifestBlobUnknown.
func (s *Store) resolveBlobs(m *oci.Manifest) (map[oci.Digest]key.Key, error) {
	roots := make(map[oci.Digest]key.Key)
	for _, d := range m.BlobDescriptors() {
		if _, ok := roots[d.Digest]; ok {
			continue
		}
		k, err := s.st.Resolve(blob.RefName(d.Digest))
		if errors.Is(err, store.ErrNotFound) {
			return nil, blobUnknown(d.Digest)
		}
		if err != nil {
			return nil, fmt.Errorf("image: resolving blob %s: %w", d.Digest, err)
		}
		roots[d.Digest] = k
	}
	return roots, nil
}

// resolveChildren maps every unique child digest of the index m to the
// child's image root in repo, or reports the first missing one as
// CodeManifestBlobUnknown.
func (s *Store) resolveChildren(repo string, m *oci.Manifest) (map[oci.Digest]key.Key, error) {
	roots := make(map[oci.Digest]key.Key)
	for _, d := range m.Manifests {
		if _, ok := roots[d.Digest]; ok {
			continue
		}
		k, err := s.st.Resolve(ManifestRef(repo, d.Digest))
		if errors.Is(err, store.ErrNotFound) {
			return nil, blobUnknown(d.Digest)
		}
		if err != nil {
			return nil, fmt.Errorf("image: resolving child manifest %s: %w", d.Digest, err)
		}
		roots[d.Digest] = k
	}
	return roots, nil
}

// blobUnknown is the MANIFEST_BLOB_UNKNOWN error naming d in its detail.
func blobUnknown(d oci.Digest) *oci.Error {
	e := oci.NewError(oci.CodeManifestBlobUnknown, "manifest references unknown blob %s", d)
	e.Detail = map[string]string{"digest": string(d)}
	return e
}

// buildRefDir builds a directory with one ModeDir entry per digest in roots,
// named by the digest and pointing at that digest's root, in byte order.
func buildRefDir(w *store.Writer, roots map[oci.Digest]key.Key) (key.Key, error) {
	d := w.NewDir()
	for _, digest := range slices.Sorted(maps.Keys(roots)) {
		if err := d.AddDir(string(digest), roots[digest]); err != nil {
			return key.Key{}, err
		}
	}
	return d.Finish()
}

// readMeta reads and decodes the meta.json of the image root at root.
func (s *Store) readMeta(root key.Key) (Meta, error) {
	k, err := s.st.LookupKey(root, MetaFile)
	if err != nil {
		return Meta{}, fmt.Errorf("image: %s in root %s: %w", MetaFile, root, err)
	}
	b, err := s.st.ReadFile(k)
	if err != nil {
		return Meta{}, fmt.Errorf("image: reading %s of root %s: %w", MetaFile, root, err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("image: decoding %s of root %s: %w", MetaFile, root, err)
	}
	return m, nil
}

// refFor maps (repo, reference) to the reference name that Open and Delete
// resolve. A repository name or tag that cannot exist yields ErrNotFound; a
// digest-shaped reference that is not a sha256 digest yields
// CodeDigestInvalid.
func refFor(repo, reference string) (string, error) {
	if oci.ValidateRepository(repo) != nil {
		return "", ErrNotFound
	}
	if oci.IsDigest(reference) {
		d, err := oci.ParseDigest(reference)
		if err != nil {
			return "", oci.NewError(oci.CodeDigestInvalid, "invalid digest %q: %v", reference, err)
		}
		return ManifestRef(repo, d), nil
	}
	if oci.ValidateTag(reference) != nil {
		return "", ErrNotFound
	}
	return TagRef(repo, reference), nil
}

// Open resolves reference (a tag or a digest) in repo and returns the image
// with its Meta loaded. It returns ErrNotFound when the reference does not
// exist.
func (s *Store) Open(repo, reference string) (*Image, error) {
	name, err := refFor(repo, reference)
	if err != nil {
		return nil, err
	}
	root, err := s.st.Resolve(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("image: resolving %s: %w", name, err)
	}
	meta, err := s.readMeta(root)
	if err != nil {
		return nil, err
	}
	manifestKey, err := s.st.LookupKey(root, ManifestFile)
	if err != nil {
		return nil, fmt.Errorf("image: %s in root %s: %w", ManifestFile, root, err)
	}
	im := &Image{Meta: meta, root: root, manifest: manifestKey, st: s.st}
	if meta.Rootfs != nil && (meta.Rootfs.Status == RootfsOK || meta.Rootfs.Status == RootfsPartial) {
		k, err := s.st.LookupKey(root, RootfsDir)
		if err != nil {
			return nil, fmt.Errorf("image: %s in root %s: %w", RootfsDir, root, err)
		}
		im.rootfs, im.hasRootfs = k, true
	}
	return im, nil
}

// Delete removes references. By tag it deletes only the tag ref. By digest
// it deletes the manifest ref, every tag ref in repo that points at the same
// image (the same root key, or a root whose meta.json carries the same
// digest — a tag left on an older root by a re-push by digest), and the
// manifest's own referrer ref when it has a subject. It returns ErrNotFound
// when the reference does not exist. Objects are left for garbage
// collection.
func (s *Store) Delete(repo, reference string) error {
	name, err := refFor(repo, reference)
	if err != nil {
		return err
	}
	unlock := s.repos.Lock(repo)
	defer unlock()

	if !oci.IsDigest(reference) {
		err := s.st.DeleteRef(name)
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("image: deleting %s: %w", name, err)
		}
		return nil
	}

	root, err := s.st.Resolve(name)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("image: resolving %s: %w", name, err)
	}
	meta, err := s.readMeta(root)
	if err != nil {
		return err
	}
	if err := s.st.DeleteRef(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("image: deleting %s: %w", name, err)
	}
	tags, err := s.st.ListRefs(TagPrefix + repo + ":")
	if err != nil {
		return fmt.Errorf("image: listing tags of %s: %w", repo, err)
	}
	for _, ref := range tags {
		same := ref.Key == root
		if !same {
			tm, err := s.readMeta(ref.Key)
			if err != nil {
				return err
			}
			same = tm.Digest == meta.Digest
		}
		if !same {
			continue
		}
		if err := s.st.DeleteRef(ref.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("image: deleting %s: %w", ref.Name, err)
		}
	}
	if meta.Subject != nil {
		rname := ReferrerRef(repo, meta.Subject.Digest, meta.Digest)
		if err := s.st.DeleteRef(rname); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("image: deleting %s: %w", rname, err)
		}
	}
	return nil
}
