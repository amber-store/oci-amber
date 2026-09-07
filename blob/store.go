package blob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	tarprism "github.com/draganm/tar-prism"
	"github.com/jobs-build/amber-store-core/key"

	"github.com/amber-store/oci-amber/keyedmutex"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

var (
	// ErrNotFound reports a digest with no blob reference.
	ErrNotFound = errors.New("blob: not found")
	// ErrDigestMismatch reports served bytes whose sha256 differs from the
	// blob's digest. They have already been written; the caller must abort
	// the response rather than finish it.
	ErrDigestMismatch = errors.New("blob: served bytes do not match digest")
	// ErrReadOnly reports Put or Delete on a Store made by NewReadOnly.
	ErrReadOnly = errors.New("blob: store is read-only")
)

// blobsDirName is the prism blobs directory inside a blob root; it equals
// tar-prism's BlobsDir.
const blobsDirName = "blobs"

// Options configures a Store. Zero values take the defaults of the
// corresponding `oci-amber serve` flags. WorkDir is required.
type Options struct {
	WorkDir               string
	MaxInMemory           int64
	AnalyzeParallelism    int
	AnalyzeTimeout        time.Duration
	MaxConcurrentFinalize int
	// VerifyLimit, when positive, is how many bytes of a layer's compressed
	// form the ingest verifies: once the recompression has reproduced that
	// much, the candidate is accepted and the rest of the layer is taken
	// apart without being recompressed (zrecipe's Options.VerifyLimit,
	// applied in the search and in the confirming pass). A layer whose
	// compression diverges past the limit is stored as a prism and fails
	// its digest check on pull instead of being caught at ingest. Zero
	// verifies every byte.
	VerifyLimit int64
	// VerifyRoundTrip runs the full pull pipeline over every prism before
	// it is published and treats a mismatch as a round-trip failure. It is
	// a diagnostic; the CLI leaves it off.
	VerifyRoundTrip bool
	// AllowRaw lets Put store a blob raw whatever the reason. When false,
	// Put refuses to store a blob raw for any reason other than
	// ReasonNotTar and returns a *RawRefusedError instead: every other
	// reason arises only for a blob that looked like a tar or a compressed
	// tar, in practice a layer, and keeping such a layer verbatim is a
	// choice the operator makes. Blobs that are not tars (configs,
	// attestations, non-tar artifacts) can never be prisms and are stored
	// raw regardless.
	AllowRaw  bool
	RecentTTL time.Duration
	// Observer, when set, receives stage transitions and byte counts from
	// Put. The registry leaves it nil; `oci-amber import` drives its
	// progress display from it.
	Observer Observer
}

// Store puts and serves OCI blobs in an amber store: prisms for
// reproducible compressed tars, verbatim bytes for everything else.
type Store struct {
	st       *store.Store
	opts     Options
	log      *slog.Logger
	finalize chan struct{}                // finalize slots
	digests  keyedmutex.Mutex[oci.Digest] // one finalization per digest at a time
	recentMu sync.Mutex
	recent   map[oci.Digest]recentEntry
	readOnly bool
}

// recentEntry is one row of the recent-uploads table.
type recentEntry struct {
	stats store.Stats
	at    time.Time
}

// New returns a Store over st. It creates <WorkDir>/spool, the directory
// zrecipe spills to, if it does not exist and removes what a previous
// process left in it. Only the contents of that one directory are deleted:
// neither it nor WorkDir is ever removed, because WorkDir is derived from
// an operator-supplied path (see cmd/oci-amber).
func New(st *store.Store, opts Options, log *slog.Logger) (*Store, error) {
	if st == nil {
		return nil, errors.New("blob: nil store")
	}
	if opts.WorkDir == "" {
		return nil, errors.New("blob: WorkDir is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.MaxInMemory <= 0 {
		opts.MaxInMemory = 64 << 20
	}
	if opts.AnalyzeParallelism <= 0 {
		opts.AnalyzeParallelism = 2
	}
	if opts.AnalyzeTimeout <= 0 {
		opts.AnalyzeTimeout = 15 * time.Minute
	}
	if opts.MaxConcurrentFinalize <= 0 {
		opts.MaxConcurrentFinalize = max(1, runtime.NumCPU()/2)
	}
	if opts.RecentTTL <= 0 {
		opts.RecentTTL = time.Hour
	}
	spool := filepath.Join(opts.WorkDir, spoolDirName)
	if err := os.MkdirAll(spool, 0o755); err != nil {
		return nil, fmt.Errorf("blob: creating %s: %w", spool, err)
	}
	if err := emptyDir(spool); err != nil {
		return nil, fmt.Errorf("blob: emptying %s: %w", spool, err)
	}
	return &Store{
		st:       st,
		opts:     opts,
		log:      log,
		finalize: make(chan struct{}, opts.MaxConcurrentFinalize),
		recent:   make(map[oci.Digest]recentEntry),
	}, nil
}

// NewReadOnly returns a Store over st that only reads: Open, Exists and
// the pull path work, Put and Delete return ErrReadOnly. It needs no work
// directory and creates or deletes nothing, so a tool that only looks at
// a store (`oci-amber browse`) opens it without side effects. A nil log
// uses slog.Default.
func NewReadOnly(st *store.Store, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{st: st, log: log, readOnly: true, recent: make(map[oci.Digest]recentEntry)}
}

// emptyDir removes every entry of dir, leaving dir itself in place.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Exists reports whether oci/blob/<d> is published.
func (b *Store) Exists(d oci.Digest) (bool, error) {
	_, err := b.st.Resolve(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Open resolves oci/blob/<d> and reads its meta.json (a few hundred
// bytes); nothing else is fetched.
func (b *Store) Open(d oci.Digest) (*Blob, error) {
	root, err := b.st.Resolve(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	meta, err := b.readMeta(root)
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w", d, err)
	}
	if meta.Digest != d {
		return nil, fmt.Errorf("blob: %s: %s names %s", d, MetaFile, meta.Digest)
	}
	return &Blob{Meta: meta, root: root, store: b}, nil
}

// readMeta reads meta.json of a blob root.
func (b *Store) readMeta(root key.Key) (Meta, error) {
	k, err := b.st.LookupKey(root, MetaFile)
	if err != nil {
		return Meta{}, fmt.Errorf("%s: %w", MetaFile, err)
	}
	data, err := b.st.ReadFile(k)
	if err != nil {
		return Meta{}, fmt.Errorf("reading %s: %w", MetaFile, err)
	}
	return decodeMeta(data)
}

// Delete removes oci/blob/<d>; the objects become garbage for amber's
// collector. It waits for a finalization of the same digest to finish.
func (b *Store) Delete(d oci.Digest) error {
	if b.readOnly {
		return ErrReadOnly
	}
	unlock := b.digests.Lock(d)
	defer unlock()
	err := b.st.DeleteRef(RefName(d))
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// TakeRecent returns and removes the recent-uploads entry for d: the stats
// of a Put in this process within RecentTTL.
func (b *Store) TakeRecent(d oci.Digest) (store.Stats, bool) {
	b.recentMu.Lock()
	defer b.recentMu.Unlock()
	b.purgeRecentLocked(time.Now())
	e, ok := b.recent[d]
	if !ok {
		return store.Stats{}, false
	}
	delete(b.recent, d)
	return e.stats, true
}

// recordRecent remembers d's stats for RecentTTL, dropping expired rows.
func (b *Store) recordRecent(d oci.Digest, stats store.Stats) {
	b.recentMu.Lock()
	defer b.recentMu.Unlock()
	now := time.Now()
	b.purgeRecentLocked(now)
	b.recent[d] = recentEntry{stats: stats, at: now}
}

// purgeRecentLocked drops rows older than RecentTTL. The caller holds
// recentMu.
func (b *Store) purgeRecentLocked(now time.Time) {
	for d, e := range b.recent {
		if now.Sub(e.at) > b.opts.RecentTTL {
			delete(b.recent, d)
		}
	}
}

// buildRoot writes meta.json and the blob root directory through w. files
// maps entry names to content keys; blobsDir, when non-zero, becomes the
// blobs/ directory entry. Entries are added in byte order as the directory
// builder requires.
func (b *Store) buildRoot(w *store.Writer, meta Meta, files map[string]key.Key, blobsDir key.Key) (key.Key, error) {
	data, err := encodeMeta(meta)
	if err != nil {
		return key.Key{}, err
	}
	metaKey, err := w.PutBytes(data)
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: storing %s: %w", MetaFile, err)
	}
	entries := make(map[string]key.Key, len(files)+2)
	for name, k := range files {
		entries[name] = k
	}
	entries[MetaFile] = metaKey
	hasBlobs := blobsDir != key.Key{}
	if hasBlobs {
		entries[blobsDirName] = blobsDir
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	dir := w.NewDir()
	for _, name := range names {
		var err error
		if hasBlobs && name == blobsDirName {
			err = dir.AddDir(name, entries[name])
		} else {
			err = dir.AddFile(name, entries[name])
		}
		if err != nil {
			return key.Key{}, fmt.Errorf("blob: adding %s to blob root: %w", name, err)
		}
	}
	root, err := dir.Finish()
	if err != nil {
		return key.Key{}, fmt.Errorf("blob: finishing blob root: %w", err)
	}
	return root, nil
}

// Put finalizes an upload (spec "Blob finalization" steps 2-9): whole-blob
// dedup, finalize slot, analyze, stage and classify, then the staged pack's
// commit or the raw ingest through the accounting writer, blob root,
// oci/blob/<digest> ref, recent-uploads entry, log line, spool removal. A
// prism whose staging or round-trip check fails is downgraded to raw with
// the recorded reason. Every raw outcome but not-tar needs Options.AllowRaw;
// without it Put returns a *RawRefusedError (see refuseRaw). On any error
// nothing is published and the spool is left in place so the caller can
// decide what to do with the session; on success the spool's backing file
// is removed.
func (b *Store) Put(ctx context.Context, sp *upload.Spool) (*Meta, error) {
	if sp == nil {
		return nil, errors.New("blob: nil spool")
	}
	if b.readOnly {
		return nil, ErrReadOnly
	}
	d := sp.Digest()
	size := sp.Size()
	start := time.Now()

	unlock := b.digests.Lock(d)
	defer unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	existing, err := b.Open(d)
	switch {
	case err == nil:
		// Step 2: whole-blob dedup.
		meta := b.dedupHit(existing.Meta)
		if err := sp.Remove(); err != nil {
			b.log.Warn("removing spool", "digest", d, "error", err)
		}
		return &meta, nil
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}

	// Step 3: finalize slot.
	select {
	case b.finalize <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.finalize }()

	// Steps 4 and 5: analyze and classify.
	b.observeStage(d, StageAnalyze)
	dec, err := b.analyze(ctx, sp)
	if err != nil {
		return nil, err
	}
	defer dec.close()
	meta := Meta{
		Version:    MetaVersion,
		Digest:     d,
		Size:       size,
		Format:     dec.format,
		UploadedAt: time.Now().UTC(),
	}
	kind, reason := dec.kind, dec.reason
	var root key.Key
	switch kind {
	case KindPrism:
		// The confirming pass and the commit, then the round-trip check.
		res, params, stats, perr := b.finalizePrism(ctx, dec, d, size)
		var fb *rawFallback
		switch {
		case perr == nil:
			meta.Kind = KindPrism
			meta.DiffID = res.diffID
			meta.UncompressedSize = res.uncompressedSize
			meta.Entries = res.entries
			meta.Engine = params.Engine
			meta.EngineVersion = params.EngineVersion
			meta.Stats = stats
			root, err = b.writeRoot(ctx, meta, map[string]key.Key{
				tarprism.RecipeFile: res.recipe,
				tarprism.IndexFile:  res.index,
				CompFile:            res.comp,
			}, res.blobs)
			if err != nil {
				return nil, err
			}
		case errors.As(perr, &fb):
			// Downgrade: the prism objects are left to GC and the verbatim
			// bytes are stored below with the fallback reason, unless the
			// store refuses raw layers.
			if err := b.refuseRaw(meta, fb.reason, fb.err); err != nil {
				return nil, err
			}
			kind, reason = KindRaw, fb.reason
		default:
			return nil, perr
		}
	case KindRaw:
		if err := b.refuseRaw(meta, reason, dec.err); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("blob: unknown kind %q", kind)
	}
	if kind == KindRaw {
		// Step 7: raw path, also the target of a prism downgrade.
		root, meta, err = b.finalizeRaw(ctx, sp, meta, reason)
		if err != nil {
			return nil, err
		}
	}

	// Step 9: publish, record, log, discard the spool.
	if err := b.st.Publish(RefName(d), root); err != nil {
		return nil, fmt.Errorf("blob: publishing %s: %w", d, err)
	}
	b.recordRecent(d, meta.Stats)
	b.logStored(meta, time.Since(start))
	if err := sp.Remove(); err != nil {
		b.log.Warn("removing spool", "digest", d, "error", err)
	}
	return &meta, nil
}

// Reuse is Put's whole-blob dedup check for an upload whose digest is known
// before its bytes are: a monolithic POST or a PUT that names the digest.
// When oci/blob/<d> is published it does what Put does on a dedup hit (the
// recent-uploads entry and the "blob already present" line) and returns
// the blob's Meta carrying the stats of a fully deduplicated upload, so the
// caller can answer without reading the upload at all. Otherwise it returns
// ErrNotFound and records nothing. It does not serialize on the digest: a
// finalization of d still in flight is not published yet, so the upload
// goes ahead and Put's own check, which is serialized, catches it.
func (b *Store) Reuse(d oci.Digest) (*Meta, error) {
	existing, err := b.Open(d)
	if err != nil {
		return nil, err
	}
	meta := b.dedupHit(existing.Meta)
	return &meta, nil
}

// dedupHit records an upload of a blob that is already published: the
// recent-uploads entry counts it as fully deduplicated and the hit is
// logged. It returns existing with those stats.
func (b *Store) dedupHit(existing Meta) Meta {
	stats := store.Stats{LogicalBytes: existing.Size, DedupedBytes: existing.Size}
	b.recordRecent(existing.Digest, stats)
	b.log.Info("blob already present", "digest", existing.Digest, "size", existing.Size)
	existing.Stats = stats
	return existing
}

// finalizeRaw stores the verbatim spool bytes under the given reason and
// builds the blob root. The ingest runs through its own accounting writer,
// whose Stats become meta.Stats.
func (b *Store) finalizeRaw(ctx context.Context, sp *upload.Spool, meta Meta, reason RawReason) (key.Key, Meta, error) {
	b.observeStage(meta.Digest, StageRaw)
	w := b.st.NewWriter(ctx)
	rawKey, err := b.ingestRaw(ctx, w, sp)
	if err != nil {
		w.Abort()
		return key.Key{}, meta, err
	}
	stats, err := w.Close()
	if err != nil {
		return key.Key{}, meta, err
	}
	meta.Kind = KindRaw
	meta.RawReason = reason
	meta.DiffID = ""
	meta.UncompressedSize = 0
	meta.Entries = 0
	meta.Engine = ""
	meta.EngineVersion = ""
	meta.Stats = stats
	root, err := b.writeRoot(ctx, meta, map[string]key.Key{RawFile: rawKey}, key.Key{})
	if err != nil {
		return key.Key{}, meta, err
	}
	return root, meta, nil
}

// writeRoot builds the blob root through its own writer, so that the stats
// recorded in meta.json are the ingest's and exclude the root itself.
func (b *Store) writeRoot(ctx context.Context, meta Meta, files map[string]key.Key, blobsDir key.Key) (key.Key, error) {
	w := b.st.NewWriter(ctx)
	root, err := b.buildRoot(w, meta, files, blobsDir)
	if err != nil {
		w.Abort()
		return key.Key{}, err
	}
	if _, err := w.Close(); err != nil {
		return key.Key{}, err
	}
	return root, nil
}

// logStored emits the per-blob log line.
func (b *Store) logStored(meta Meta, took time.Duration) {
	attrs := []any{
		"digest", meta.Digest,
		"size", meta.Size,
		"kind", meta.Kind,
		"format", meta.Format,
	}
	if meta.Kind == KindPrism {
		attrs = append(attrs, "engine", meta.Engine, "entries", meta.Entries)
	} else {
		attrs = append(attrs, "raw_reason", meta.RawReason)
	}
	attrs = append(attrs,
		"logical_bytes", meta.Stats.LogicalBytes,
		"deduped_bytes", meta.Stats.DedupedBytes,
		"disk_bytes", meta.Stats.DiskBytes,
		"duration", took,
	)
	b.log.Info("blob stored", attrs...)
}
