package importer

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

const gzipLayer = "application/vnd.oci.image.layer.v1.tar+gzip"

// env is a store with a blob store, an image store and a tracker wired as
// the blob observer, the way the command wires them.
type env struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
	tr     *Tracker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), store.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := NewTracker(TrackerOptions{Verify: true})
	blobs, err := blob.New(st, blob.Options{WorkDir: t.TempDir(), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute, MaxConcurrentFinalize: 2, VerifyRoundTrip: true, RecentTTL: 24 * time.Hour, Observer: tr}, log)
	if err != nil {
		t.Fatal(err)
	}
	return &env{st: st, blobs: blobs, images: image.New(st, blobs, log), tr: tr}
}

// tarGz builds a gzip tar with one file of compressible text.
func tarGz(t *testing.T, name string, size int, seed int64) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	words := []string{"alpha ", "beta ", "gamma ", "delta ", "epsilon\n"}
	var text bytes.Buffer
	for text.Len() < size {
		text.WriteString(words[r.Intn(len(words))])
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(text.Len()), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(text.Bytes())
	tw.Close()
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(tb.Bytes())
	gw.Close()
	return gz.Bytes()
}

// fixture is the busybox-like archive: an index over a present amd64
// image (two layers: a prism and a not-tar raw one), its attestation, and
// two absent platforms; two names in two repositories.
func fixture(t *testing.T) (*dockerarchive.Archive, *dockerarchive.Plan, *env) {
	t.Helper()
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layerA := tarGz(t, "usr/share/words", 200<<10, 1)
	layerB := []byte(strings.Repeat("not a tar, stored raw ", 100))
	img := b.AddImage(config, []archivetest.Layer{{MediaType: gzipLayer, Data: layerA}, {MediaType: gzipLayer, Data: layerB}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	att := b.AddImage(archivetest.Attestation(img))
	idx := b.AddIndex([]oci.Descriptor{img, att, archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
	b.Top(idx)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"registry.example.ch/team/app:v1", "app:latest"}, Layers: []oci.Digest{oci.DigestOfBytes(layerA), oci.DigestOfBytes(layerB)}})
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { arch.Close() })
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	return arch, plan, e
}

func TestRunStoresEverything(t *testing.T) {
	arch, plan, e := fixture(t)
	rep, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entry := plan.Entries[0]
	for _, n := range entry.Names {
		im, err := e.images.Open(n.Repo, n.Tag)
		if err != nil {
			t.Fatalf("open %s: %v", n, err)
		}
		if im.Meta.Kind != image.KindIndex || im.Meta.Digest != entry.Digest {
			t.Fatalf("%s: %+v", n, im.Meta)
		}
		for _, d := range entry.Manifests[:len(entry.Manifests)-1] {
			if _, err := e.images.Open(n.Repo, d.String()); err != nil {
				t.Fatalf("child %s in %s: %v", d, n.Repo, err)
			}
		}
	}
	// The platform image got its rootfs; the layers are one prism and one raw.
	child, _ := e.images.Open("app", entry.Manifests[0].String())
	if child.Meta.Rootfs == nil || child.Meta.Rootfs.Status != image.RootfsUnavailable {
		// layer B is raw, so the rootfs is unavailable; that is the expected, honest outcome
		t.Fatalf("rootfs = %+v", child.Meta.Rootfs)
	}
	kinds := map[blob.Kind]int{}
	for _, pb := range plan.Blobs {
		bl, err := e.blobs.Open(pb.Digest)
		if err != nil {
			t.Fatalf("blob %s: %v", pb.Digest, err)
		}
		kinds[bl.Meta.Kind]++
	}
	if kinds[blob.KindPrism] != 1 || kinds[blob.KindRaw] != 4 { // config, layer B, attestation config, in-toto payload
		t.Fatalf("kinds = %v", kinds)
	}
	// Report.
	if rep.Blobs.Processed != 5 || rep.Blobs.Prism != 1 || rep.Blobs.Raw != 4 || rep.Blobs.Present != 0 {
		t.Fatalf("blob counts = %+v", rep.Blobs)
	}
	if rep.Blobs.RawReasons[blob.ReasonNotTar] != 4 {
		t.Fatalf("raw reasons = %v", rep.Blobs.RawReasons)
	}
	var wantCompressed int64
	for _, pb := range plan.Blobs {
		wantCompressed += pb.Size
	}
	for _, pm := range plan.Manifests {
		wantCompressed += int64(len(pm.Body))
	}
	if rep.Compressed != wantCompressed {
		t.Fatalf("Compressed = %d, want %d", rep.Compressed, wantCompressed)
	}
	if rep.Uncompressed <= rep.Compressed {
		t.Fatalf("Uncompressed = %d should exceed Compressed = %d (layer A inflates)", rep.Uncompressed, rep.Compressed)
	}
	if rep.Added <= 0 || rep.Added >= rep.Compressed*2 {
		t.Fatalf("Added = %d, want on the order of Compressed = %d", rep.Added, rep.Compressed)
	}
	if ratio, ok := rep.DedupRatio(); !ok || ratio <= 0 {
		t.Fatalf("ratio = %v,%v", ratio, ok)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Platforms != 1 || rep.Entries[0].Attestations != 1 || len(rep.Entries[0].Rootfs) != 1 {
		t.Fatalf("entry report = %+v", rep.Entries[0])
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Counts.Done != 5 || s.Err != nil {
		t.Fatalf("tracker: %+v", s)
	}
}

func TestRunAgainIsAllPresentAndAddsNothing(t *testing.T) {
	arch, plan, e := fixture(t)
	if _, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	plan2, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker(TrackerOptions{Verify: true})
	rep, err := New(e.blobs, e.images, arch, plan2, tr2, Options{Workers: 2}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blobs.Present != 5 || rep.Blobs.Processed != 0 {
		t.Fatalf("second run counts = %+v", rep.Blobs)
	}
	// Re-publishing a manifest rewrites its meta.json (a fresh createdAt)
	// and the root nodes above it, the same few hundred bytes a re-push
	// costs the registry; no blob content is written.
	if rep.Added <= 0 || rep.Added >= 4096 {
		t.Fatalf("second run added %d bytes, want only manifest metadata", rep.Added)
	}
	if rep.Uncompressed == 0 {
		t.Fatal("uncompressed size must come from the stored metas for present blobs")
	}
}

func TestRunCancelledPublishesNoTag(t *testing.T) {
	arch, plan, e := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.images.Open("app", "latest"); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("tag published after a cancelled run: %v", err)
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Err == nil {
		t.Fatalf("tracker after cancel: %+v", s)
	}
}

func TestRunCorruptBlobFailsBeforeWriting(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	good := tarGz(t, "a", 4<<10, 2)
	claimed := oci.DigestOfBytes([]byte("what the path claims"))
	b.AddBlobAs(claimed, good)
	m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
		Layers: []oci.Descriptor{{MediaType: gzipLayer, Digest: claimed, Size: int64(len(good))}}}
	body, _ := json.Marshal(m)
	b.Top(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: b.AddBlob(body), Size: int64(len(body))})
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"x:1"}})
	path, _ := b.WriteFile(t.TempDir(), "bad.tar")
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 1}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	for _, pb := range plan.Blobs {
		if ok, _ := e.blobs.Exists(pb.Digest); ok {
			t.Fatalf("blob %s was stored despite the corrupt archive", pb.Digest)
		}
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Err == nil {
		t.Fatalf("tracker: %+v", s)
	}
}

// TestRunPutFailureCancelsTheRun reaches the worker pool (unlike the two
// tests above, which both fail earlier, in check) and forces one Put to
// fail through the putHook test seam, so the pool's failure and
// cancellation branches — tr.Fail/firstErr/cancel, and the feeder's
// <-ctx.Done() case — actually run.
func TestRunPutFailureCancelsTheRun(t *testing.T) {
	arch, plan, e := fixture(t)
	hookErr := errors.New("boom")
	// The largest blob is fed first, so failing it leaves the rest pending.
	failDigest := slices.MaxFunc(plan.Blobs, func(a, b dockerarchive.PlanBlob) int { return cmp.Compare(a.Size, b.Size) }).Digest
	putHook = func(ctx context.Context, pb dockerarchive.PlanBlob) error {
		if pb.Digest == failDigest {
			return hookErr
		}
		return nil
	}
	t.Cleanup(func() { putHook = nil })

	_, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 1}).Run(context.Background())
	if !errors.Is(err, hookErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, hookErr)
	}

	snap := e.tr.Snapshot()
	var sawFailed, sawPending bool
	for _, r := range snap.Blobs {
		switch {
		case r.Digest == failDigest:
			if r.State != BlobFailed {
				t.Fatalf("failed blob state = %v, want BlobFailed", r.State)
			}
			sawFailed = true
		case r.State == BlobPending:
			sawPending = true
		}
	}
	if !sawFailed {
		t.Fatal("failed blob's row not found")
	}
	if !sawPending {
		t.Fatal("want at least one other blob still pending: the feeder should have stopped early")
	}
	if snap.Phase != PhaseDone || snap.Err == nil {
		t.Fatalf("tracker: %+v", snap)
	}
	if _, err := e.images.Open("app", "latest"); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("tag published after a failed run: %v", err)
	}
}

// TestRunStoresLargestBlobsFirst pins the feeding order of the worker
// pool: the largest absent blob goes first, so a big layer never starts
// last and runs alone after the small ones have finished. A single worker
// makes the start order the feeding order; the putHook seam records it.
func TestRunStoresLargestBlobsFirst(t *testing.T) {
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	small := tarGz(t, "small", 2<<10, 3)
	large := tarGz(t, "large", 256<<10, 4)
	medium := tarGz(t, "medium", 32<<10, 5)
	layers := []archivetest.Layer{{MediaType: gzipLayer, Data: small}, {MediaType: gzipLayer, Data: large}, {MediaType: gzipLayer, Data: medium}}
	img := b.AddImage(config, layers, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(img)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes(small), oci.DigestOfBytes(large), oci.DigestOfBytes(medium)}})
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[oci.Digest]int64{}
	for _, pb := range plan.Blobs {
		sizes[pb.Digest] = pb.Size
	}
	if slices.IsSortedFunc(plan.Blobs, func(a, b dockerarchive.PlanBlob) int { return cmp.Compare(b.Size, a.Size) }) {
		t.Fatal("plan already lists the blobs largest first; the test would not tell the orders apart")
	}

	var started []oci.Digest
	putHook = func(ctx context.Context, pb dockerarchive.PlanBlob) error {
		started = append(started, pb.Digest)
		return nil
	}
	t.Cleanup(func() { putHook = nil })
	if _, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 1}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(started) != len(plan.Blobs) {
		t.Fatalf("started %d blobs, want %d", len(started), len(plan.Blobs))
	}
	for i := 1; i < len(started); i++ {
		if sizes[started[i]] > sizes[started[i-1]] {
			t.Fatalf("blob %d (%d bytes) started after blob %d (%d bytes); want largest first", i, sizes[started[i]], i-1, sizes[started[i-1]])
		}
	}
}

// multiArchFixture is an archive with two present platforms, each with one
// prism layer of a different shape, an attestation for the first, and two
// names in two repositories.
func multiArchFixture(t *testing.T) (*dockerarchive.Archive, *dockerarchive.Plan, *env) {
	t.Helper()
	b := archivetest.New()
	amdConfig := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	armConfig := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	amdLayer := tarGz(t, "usr/share/words", 64<<10, 6)
	armLayer := tarGz(t, "opt/words", 64<<10, 7)
	amd := b.AddImage(amdConfig, []archivetest.Layer{{MediaType: gzipLayer, Data: amdLayer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	arm := b.AddImage(armConfig, []archivetest.Layer{{MediaType: gzipLayer, Data: armLayer}}, &oci.Platform{OS: "linux", Architecture: "arm64"}, nil)
	att := b.AddImage(archivetest.Attestation(amd))
	idx := b.AddIndex([]oci.Descriptor{amd, arm, att}, nil)
	b.Top(idx)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(amdConfig), RepoTags: []string{"registry.example.ch/team/app:v1", "app:latest"}, Layers: []oci.Digest{oci.DigestOfBytes(amdLayer)}})
	path, err := b.WriteFile(t.TempDir(), "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { arch.Close() })
	e := newEnv(t)
	plan, err := arch.Plan(dockerarchive.PlanOptions{Present: e.blobs.Exists})
	if err != nil {
		t.Fatal(err)
	}
	return arch, plan, e
}

// platformsOf lists the plan's image manifests that describe a platform:
// neither indexes nor attestations.
func platformsOf(plan *dockerarchive.Plan) []oci.Digest {
	var ds []oci.Digest
	for _, pm := range plan.Manifests {
		if !pm.IsIndex && !pm.Attestation {
			ds = append(ds, pm.Digest)
		}
	}
	return ds
}

// TestRunPublishesPlatformManifestsConcurrently holds each platform
// manifest's first publish in the publishHook seam until the other has
// arrived. Both arrive only when the manifests go through the worker
// pool; publishing them one after another would hold the second until
// the first, blocked in the seam, is done. Whatever order they finish
// in, the report and the store come out the same as a sequential run's.
func TestRunPublishesPlatformManifestsConcurrently(t *testing.T) {
	arch, plan, e := multiArchFixture(t)
	entry := plan.Entries[0]
	platforms := platformsOf(plan)
	if len(platforms) != 2 || entry.Platforms != 2 {
		t.Fatalf("platforms = %v, entry = %+v", platforms, entry)
	}
	firstRepo := entry.Names[0].Repo

	arrived := make(chan oci.Digest, 2)
	release := make(chan struct{})
	publishHook = func(ctx context.Context, repo string, pm dockerarchive.PlanManifest) error {
		if repo == firstRepo && slices.Contains(platforms, pm.Digest) {
			arrived <- pm.Digest
			<-release
		}
		return nil
	}
	t.Cleanup(func() { publishHook = nil })

	type result struct {
		rep *Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background())
		done <- result{rep, err}
	}()
	both := true
	var seen []oci.Digest
	for range platforms {
		select {
		case d := <-arrived:
			seen = append(seen, d)
		case <-time.After(5 * time.Second):
			both = false
		}
	}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !both {
		t.Fatalf("only %v arrived while the other platform's publish was held: the manifests are published one after another", seen)
	}

	// The store holds everything, in every repository.
	for _, n := range entry.Names {
		im, err := e.images.Open(n.Repo, n.Tag)
		if err != nil {
			t.Fatalf("open %s: %v", n, err)
		}
		if im.Meta.Kind != image.KindIndex || im.Meta.Digest != entry.Digest {
			t.Fatalf("%s: %+v", n, im.Meta)
		}
		for _, d := range entry.Manifests[:len(entry.Manifests)-1] {
			if _, err := e.images.Open(n.Repo, d.String()); err != nil {
				t.Fatalf("child %s in %s: %v", d, n.Repo, err)
			}
		}
	}
	// The report lists the platforms' rootfs outcomes in plan order.
	er := r.rep.Entries[0]
	if len(r.rep.Entries) != 1 || er.Platforms != 2 || er.Attestations != 1 || len(er.Rootfs) != 2 {
		t.Fatalf("entry report = %+v", er)
	}
	for i, d := range platforms {
		child, err := e.images.Open(firstRepo, d.String())
		if err != nil {
			t.Fatal(err)
		}
		if child.Meta.Rootfs.Status != image.RootfsOK || er.Rootfs[i].Status != image.RootfsOK || er.Rootfs[i].Entries != child.Meta.Rootfs.Entries {
			t.Fatalf("rootfs %d = %+v, stored %+v", i, er.Rootfs[i], child.Meta.Rootfs)
		}
	}
	if er.Rootfs[0].Entries == er.Rootfs[1].Entries {
		t.Fatalf("both platforms have %d entries; the fixture must tell them apart", er.Rootfs[0].Entries)
	}
	if er.Digest != entry.Digest || er.Stats.DiskBytes <= 0 {
		t.Fatalf("entry report = %+v", er)
	}
	snap := e.tr.Snapshot()
	for _, m := range snap.Manifests {
		if m.State != ManifestDone {
			t.Fatalf("manifest %s state = %v, want done", m.Digest, m.State)
		}
	}
	if snap.Phase != PhaseDone || snap.Err != nil {
		t.Fatalf("tracker: %+v", snap)
	}
}

// TestRunPublishFailureCancelsTheRun forces one platform manifest's
// publish to fail through the publishHook seam: the run returns that
// error and no tag is published.
func TestRunPublishFailureCancelsTheRun(t *testing.T) {
	arch, plan, e := multiArchFixture(t)
	platforms := platformsOf(plan)
	hookErr := errors.New("boom")
	publishHook = func(ctx context.Context, repo string, pm dockerarchive.PlanManifest) error {
		if pm.Digest == platforms[1] {
			return hookErr
		}
		return nil
	}
	t.Cleanup(func() { publishHook = nil })

	_, err := New(e.blobs, e.images, arch, plan, e.tr, Options{Workers: 2}).Run(context.Background())
	if !errors.Is(err, hookErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, hookErr)
	}
	for _, n := range plan.Entries[0].Names {
		if _, err := e.images.Open(n.Repo, n.Tag); !errors.Is(err, image.ErrNotFound) {
			t.Fatalf("tag %s published after a failed run: %v", n, err)
		}
	}
	if s := e.tr.Snapshot(); s.Phase != PhaseDone || s.Err == nil {
		t.Fatalf("tracker: %+v", s)
	}
}
