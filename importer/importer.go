package importer

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// putHook is a test seam: when non-nil, putBlob calls it right after
// marking the blob in flight and, on a non-nil error, fails the blob
// without ever calling blob.Store.Put — the way blob/prism.go's
// roundTripCheck lets tests force a failure. Nil in production.
var putHook func(ctx context.Context, pb dockerarchive.PlanBlob) error

// publishHook is a test seam: when non-nil, the publish phase calls it
// right before an image manifest's first Put into a repository and, on a
// non-nil error, fails the run without calling image.Store.Put. Nil in
// production.
var publishHook func(ctx context.Context, repo string, pm dockerarchive.PlanManifest) error

// Options configure an Importer. Workers is how many blobs are finalized,
// and then how many image manifests are published, at once; the command
// passes --max-concurrent-finalize.
type Options struct {
	Workers int
}

// Importer stores one plan.
type Importer struct {
	blobs  *blob.Store
	images *image.Store
	arch   *dockerarchive.Archive
	plan   *dockerarchive.Plan
	tr     *Tracker
	opts   Options
}

// New returns an importer over the given stores and plan. tr must be the
// blob store's Observer for stage progress to show.
func New(blobs *blob.Store, images *image.Store, arch *dockerarchive.Archive, plan *dockerarchive.Plan, tr *Tracker, opts Options) *Importer {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	return &Importer{blobs: blobs, images: images, arch: arch, plan: plan, tr: tr, opts: opts}
}

// Run checks, stores and publishes the plan and returns the report. On
// error nothing else is published; blobs stored before the failure stay,
// so a re-run resumes through dedup hits.
func (im *Importer) Run(ctx context.Context) (*Report, error) {
	start := time.Now()
	im.tr.Queue(im.plan)
	rep, err := im.run(ctx)
	im.tr.Finish(err)
	if err != nil {
		return nil, err
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

// run performs the three phases of an import in order: checking blob
// digests, storing blobs, and publishing manifests.
func (im *Importer) run(ctx context.Context) (*Report, error) {
	if err := im.check(ctx); err != nil {
		return nil, err
	}
	metas, err := im.storeBlobs(ctx)
	if err != nil {
		return nil, err
	}
	return im.publish(ctx, metas)
}

// check verifies every non-present blob against its digest.
func (im *Importer) check(ctx context.Context) error {
	var done int64
	for _, pb := range im.plan.Blobs {
		if pb.Present {
			continue
		}
		err := im.arch.Verify(ctx, pb.Digest, func(n int64) { im.tr.Checked(done + n) })
		if err != nil {
			return fmt.Errorf("importer: checking %s: %w", pb.Digest, err)
		}
		done += pb.Size
		im.tr.Checked(done)
	}
	return nil
}

// storeBlobs runs Put over the non-present blobs with a worker pool,
// largest first, and returns every blob's meta, present ones read from
// the store. A blob's time is dominated by a single-threaded recompression
// proportional to its size, so feeding the largest first keeps a big
// layer from starting last and running alone after the small ones are
// done (longest-processing-time-first).
func (im *Importer) storeBlobs(ctx context.Context) (map[oci.Digest]*blob.Meta, error) {
	im.tr.StartBlobs()
	var (
		mu    sync.Mutex
		metas = map[oci.Digest]*blob.Meta{}
	)
	err := each(ctx, im.opts.Workers, im.absentLargestFirst(), func(ctx context.Context, pb dockerarchive.PlanBlob) error {
		meta, err := im.putBlob(ctx, pb)
		if err != nil {
			im.tr.Fail(pb.Digest, err)
			return err
		}
		im.tr.Done(pb.Digest, meta)
		mu.Lock()
		metas[pb.Digest] = meta
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, pb := range im.plan.Blobs {
		if !pb.Present {
			continue
		}
		bl, err := im.blobs.Open(pb.Digest)
		if err != nil {
			return nil, fmt.Errorf("importer: reading stored blob %s: %w", pb.Digest, err)
		}
		m := bl.Meta
		metas[pb.Digest] = &m
	}
	return metas, nil
}

// each calls fn for every item on up to workers goroutines, feeding the
// items in order, and returns the first error. After a failure no more
// items are fed and the calls still running see a cancelled context;
// each waits for them before returning. With no failure it returns the
// context's error, if any.
func each[T any](ctx context.Context, workers int, items []T, fn func(context.Context, T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
	)
	jobs := make(chan T)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if err := fn(ctx, it); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case jobs <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// absentLargestFirst lists the plan's non-present blobs by decreasing
// size, plan order among equals. The plan itself keeps its order, which
// the tracker's rows follow.
func (im *Importer) absentLargestFirst() []dockerarchive.PlanBlob {
	var absent []dockerarchive.PlanBlob
	for _, pb := range im.plan.Blobs {
		if !pb.Present {
			absent = append(absent, pb)
		}
	}
	slices.SortStableFunc(absent, func(a, b dockerarchive.PlanBlob) int { return cmp.Compare(b.Size, a.Size) })
	return absent
}

// putBlob stores one blob: it marks the digest in flight, then (absent a
// test hook forcing a failure) opens its section of the archive and puts
// it through the blob store.
func (im *Importer) putBlob(ctx context.Context, pb dockerarchive.PlanBlob) (*blob.Meta, error) {
	im.tr.Start(pb.Digest)
	if putHook != nil {
		if err := putHook(ctx, pb); err != nil {
			return nil, fmt.Errorf("importer: storing %s: %w", pb.Digest, err)
		}
	}
	sec, err := im.arch.Section(pb.Digest)
	if err != nil {
		return nil, err
	}
	meta, err := im.blobs.Put(ctx, upload.NewSectionSpool(sec, 0, pb.Size, pb.Digest))
	if err != nil {
		return nil, fmt.Errorf("importer: storing %s: %w", pb.Digest, err)
	}
	return meta, nil
}

// publishJob is one manifest to publish in one repository under refs.
type publishJob struct {
	repo string
	pm   dockerarchive.PlanManifest
	refs []string
}

// published identifies a manifest's Put into a repository.
type published struct {
	repo   string
	digest oci.Digest
}

// publish puts every manifest in every repository its entry is named in
// and builds the report. Image manifests, whose Put builds the rootfs
// view, go through the worker pool across all entries and repositories,
// so the platforms of a multi-arch image are built at once; the indexes
// follow one entry at a time in plan order, children before parents, as
// an index resolves its children in its repository. The entry's first
// Put supplies its stats; the first repository's Put of each platform
// manifest supplies its rootfs outcome.
func (im *Importer) publish(ctx context.Context, metas map[oci.Digest]*blob.Meta) (*Report, error) {
	im.tr.StartManifests()
	byDigest := map[oci.Digest]dockerarchive.PlanManifest{}
	for _, pm := range im.plan.Manifests {
		byDigest[pm.Digest] = pm
	}
	var images, indexes []publishJob
	for _, e := range im.plan.Entries {
		for _, repo := range reposOf(e.Names) {
			for _, d := range e.Manifests {
				pm, ok := byDigest[d]
				if !ok {
					return nil, fmt.Errorf("importer: plan names manifest %s but does not carry it", d)
				}
				j := publishJob{repo: repo, pm: pm, refs: refsFor(e, d, repo)}
				if pm.IsIndex {
					indexes = append(indexes, j)
				} else {
					images = append(images, j)
				}
			}
		}
	}

	var (
		mu    sync.Mutex
		first = map[published]*image.Meta{}
	)
	err := each(ctx, im.opts.Workers, images, func(ctx context.Context, j publishJob) error {
		if publishHook != nil {
			if err := publishHook(ctx, j.repo, j.pm); err != nil {
				return fmt.Errorf("importer: publishing %s/%s: %w", j.repo, j.refs[0], err)
			}
		}
		meta, err := im.putManifest(ctx, j)
		if err != nil {
			return err
		}
		mu.Lock()
		first[published{j.repo, j.pm.Digest}] = meta
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The indexes were collected entry by entry, repository by repository,
	// in plan order, which puts every child (image manifests above, nested
	// indexes earlier in the list) before its parent.
	for _, j := range indexes {
		meta, err := im.putManifest(ctx, j)
		if err != nil {
			return nil, err
		}
		first[published{j.repo, j.pm.Digest}] = meta
	}

	rep := &Report{Blobs: BlobCounts{RawReasons: map[blob.RawReason]int{}}}
	for _, e := range im.plan.Entries {
		er := EntryReport{Names: e.Names, Digest: e.Digest, IsIndex: e.IsIndex, Platforms: e.Platforms, Attestations: e.Attestations}
		repo := reposOf(e.Names)[0]
		for _, d := range e.Manifests {
			meta := first[published{repo, d}]
			if d == e.Digest {
				er.Stats = meta.Stats
			}
			noteRootfs(&er, byDigest[d], meta)
		}
		rep.Entries = append(rep.Entries, er)
		rep.Added += er.Stats.DiskBytes
		rep.Logical += er.Stats.LogicalBytes
		rep.Deduped += er.Stats.DedupedBytes
	}
	im.account(rep, metas)
	return rep, nil
}

// putManifest publishes the job's manifest under each of its refs and
// returns the first Put's meta.
func (im *Importer) putManifest(ctx context.Context, j publishJob) (*image.Meta, error) {
	var first *image.Meta
	for _, ref := range j.refs {
		im.tr.ManifestStart(j.pm.Digest)
		meta, err := im.images.Put(ctx, j.repo, ref, j.pm.MediaType, j.pm.Body)
		if err != nil {
			return nil, fmt.Errorf("importer: publishing %s/%s: %w", j.repo, ref, err)
		}
		im.tr.ManifestDone(j.pm.Digest, meta)
		if first == nil {
			first = meta
		}
	}
	return first, nil
}

// reposOf lists the distinct repositories among names, in first-use order.
func reposOf(names []dockerarchive.Name) []string {
	var repos []string
	for _, n := range names {
		if !slices.Contains(repos, n.Repo) {
			repos = append(repos, n.Repo)
		}
	}
	return repos
}

// refsFor returns the references d is published under in repo: its digest
// for a child manifest, the entry's tags in that repository for the entry.
func refsFor(e dockerarchive.PlanEntry, d oci.Digest, repo string) []string {
	if d != e.Digest {
		return []string{d.String()}
	}
	var tags []string
	for _, n := range e.Names {
		if n.Repo == repo {
			tags = append(tags, n.Tag)
		}
	}
	return tags
}

// noteRootfs records a platform manifest's rootfs outcome. Indexes have
// none, attestation manifests describe no filesystem even though their
// config looks like an image config, and not-applicable says the same.
func noteRootfs(er *EntryReport, pm dockerarchive.PlanManifest, meta *image.Meta) {
	if pm.IsIndex || pm.Attestation || meta.Rootfs == nil || meta.Rootfs.Status == image.RootfsNotApplicable {
		return
	}
	er.Rootfs = append(er.Rootfs, meta.Rootfs)
}

// account fills the blob counts and byte totals from the plan and metas.
func (im *Importer) account(rep *Report, metas map[oci.Digest]*blob.Meta) {
	snap := im.tr.Snapshot()
	state := map[oci.Digest]BlobState{}
	for _, r := range snap.Blobs {
		state[r.Digest] = r.State
	}
	for _, pb := range im.plan.Blobs {
		rep.Compressed += pb.Size
		m := metas[pb.Digest]
		if m != nil && m.Kind == blob.KindPrism {
			rep.Uncompressed += m.UncompressedSize
		} else {
			rep.Uncompressed += pb.Size
		}
		if pb.Present || state[pb.Digest] == BlobPresent {
			rep.Blobs.Present++
			continue
		}
		rep.Blobs.Processed++
		if m == nil {
			continue
		}
		switch m.Kind {
		case blob.KindPrism:
			rep.Blobs.Prism++
		case blob.KindRaw:
			rep.Blobs.Raw++
			rep.Blobs.RawReasons[m.RawReason]++
		}
	}
	for _, pm := range im.plan.Manifests {
		rep.Compressed += int64(len(pm.Body))
		rep.Uncompressed += int64(len(pm.Body))
	}
}
