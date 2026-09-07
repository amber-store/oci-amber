package importer

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func digest(s string) oci.Digest { return oci.DigestOfBytes([]byte(s)) }

func plan(sizes map[string]int64, present ...string) *dockerarchive.Plan {
	p := &dockerarchive.Plan{}
	for name, size := range sizes {
		pb := dockerarchive.PlanBlob{Digest: digest(name), Size: size, MediaType: "layer"}
		for _, pr := range present {
			if pr == name {
				pb.Present = true
			}
		}
		p.Blobs = append(p.Blobs, pb)
	}
	p.Manifests = []dockerarchive.PlanManifest{{Digest: digest("m1"), IsIndex: false}, {Digest: digest("idx"), IsIndex: true}}
	p.Entries = []dockerarchive.PlanEntry{{Digest: digest("idx"), Names: []dockerarchive.Name{{Repo: "app", Tag: "v1"}}, IsIndex: true, Manifests: []oci.Digest{digest("m1"), digest("idx")}}}
	return p
}

func approx(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestTrackerFractionsThroughPrismStages(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "b": 3000}))
	if s := tr.Snapshot(); s.Phase != PhaseChecking || s.Counts.Pending != 2 {
		t.Fatalf("after Queue: %+v", s)
	}
	tr.Checked(2000)
	approx(t, "Checked", tr.Snapshot().Checked, 0.5)
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobProgress(digest("a"), 500)
	s := tr.Snapshot()
	row := rowFor(t, s, "a")
	approx(t, "analyze half", row.Fraction, 0.175) // 0.35 share × 0.5
	approx(t, "overall", s.Fraction, 175.0/4000)
	if s.Counts.InFlight != 1 || s.Counts.Pending != 1 {
		t.Fatalf("counts = %+v", s.Counts)
	}
	tr.BlobProgress(digest("a"), 1000)
	tr.BlobStage(digest("a"), blob.StageConfirm)
	approx(t, "confirm start", rowFor(t, tr.Snapshot(), "a").Fraction, 0.35)
	tr.BlobProgress(digest("a"), 1000)
	tr.BlobStage(digest("a"), blob.StageCommit)
	approx(t, "commit start", rowFor(t, tr.Snapshot(), "a").Fraction, 0.65)
	tr.BlobProgress(digest("a"), 1000)
	tr.BlobStage(digest("a"), blob.StageVerify)
	approx(t, "verify start", rowFor(t, tr.Snapshot(), "a").Fraction, 0.85)
	tr.BlobProgress(digest("a"), 500)
	approx(t, "verify half", rowFor(t, tr.Snapshot(), "a").Fraction, 0.925)
	tr.Done(digest("a"), &blob.Meta{Digest: digest("a"), Size: 1000, Kind: blob.KindPrism})
	s = tr.Snapshot()
	if r := rowFor(t, s, "a"); r.State != BlobDone || r.Fraction != 1 || r.Kind != blob.KindPrism {
		t.Fatalf("done row = %+v", r)
	}
	approx(t, "overall after a", s.Fraction, 0.25)
}

func TestTrackerWithoutVerifyStages(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: false, Now: time.Now})
	tr.Queue(plan(map[string]int64{"a": 100}))
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobStage(digest("a"), blob.StageConfirm)
	approx(t, "confirm start, no verify", rowFor(t, tr.Snapshot(), "a").Fraction, 0.4)
	tr.BlobStage(digest("a"), blob.StageCommit)
	tr.BlobProgress(digest("a"), 50)
	approx(t, "commit half, no verify", rowFor(t, tr.Snapshot(), "a").Fraction, 0.875)
}

func TestTrackerRawTakesTheRemainder(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: true, Now: time.Now})
	tr.Queue(plan(map[string]int64{"a": 100, "b": 100, "c": 100}))
	tr.StartBlobs()
	// raw right after analyze: 0.35 + 0.65 × n/size
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobStage(digest("a"), blob.StageRaw)
	tr.BlobProgress(digest("a"), 50)
	approx(t, "raw after analyze", rowFor(t, tr.Snapshot(), "a").Fraction, 0.675)
	// raw after confirm (decompose downgrade): 0.65 + 0.35 × n/size
	tr.Start(digest("b"))
	tr.BlobStage(digest("b"), blob.StageAnalyze)
	tr.BlobStage(digest("b"), blob.StageConfirm)
	tr.BlobProgress(digest("b"), 100)
	tr.BlobStage(digest("b"), blob.StageRaw)
	tr.BlobProgress(digest("b"), 50)
	approx(t, "raw after confirm", rowFor(t, tr.Snapshot(), "b").Fraction, 0.825)
	// progress in a stage never pushes the fraction past the stage's end
	tr.BlobProgress(digest("b"), 100)
	approx(t, "raw complete", rowFor(t, tr.Snapshot(), "b").Fraction, 1)
	tr.Done(digest("b"), &blob.Meta{Digest: digest("b"), Size: 100, Kind: blob.KindRaw, RawReason: blob.ReasonNotTar})
	s := tr.Snapshot()
	if s.Counts.Raw != 1 || rowFor(t, s, "b").RawReason != blob.ReasonNotTar {
		t.Fatalf("raw accounting: %+v", s.Counts)
	}
}

func TestTrackerPresentBlobsAreOutsideTheEstimate(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "big": 9000}, "big"))
	s := tr.Snapshot()
	if s.Counts.Present != 1 || s.Counts.Pending != 1 {
		t.Fatalf("counts = %+v", s.Counts)
	}
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	tr.BlobProgress(digest("a"), 1000)
	approx(t, "overall ignores present", tr.Snapshot().Fraction, 0.35)
	// A dedup hit inside Put reports no stage: Done without a stage counts as present.
	tr2 := NewTracker(TrackerOptions{Verify: true, Now: c.now})
	tr2.Queue(plan(map[string]int64{"a": 1000}))
	tr2.StartBlobs()
	tr2.Start(digest("a"))
	tr2.Done(digest("a"), &blob.Meta{Digest: digest("a"), Size: 1000})
	if s := tr2.Snapshot(); s.Counts.Present != 1 || s.Counts.Done != 0 || rowFor(t, s, "a").State != BlobPresent {
		t.Fatalf("dedup hit: %+v", s.Counts)
	}
}

func TestTrackerETA(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	tr := NewTracker(TrackerOptions{Verify: false, Now: c.now})
	tr.Queue(plan(map[string]int64{"a": 1000, "b": 1000}))
	tr.StartBlobs()
	tr.Start(digest("a"))
	tr.BlobStage(digest("a"), blob.StageAnalyze)
	c.advance(time.Second)
	tr.BlobProgress(digest("a"), 1000) // analyze full: fraction 0.4 of a → 400 of 2000 bytes in 1 s
	if s := tr.Snapshot(); s.ETAKnown {
		t.Fatalf("ETA known before the warm-up: %+v", s)
	}
	c.advance(time.Second) // 2 s elapsed, still 400 done → rate 200 B/s → 1600 left → 8 s
	s := tr.Snapshot()
	if !s.ETAKnown || s.ETA != 8*time.Second {
		t.Fatalf("ETA = %v (known %v), want 8s", s.ETA, s.ETAKnown)
	}
	if s.Elapsed != 2*time.Second {
		t.Fatalf("Elapsed = %v", s.Elapsed)
	}
}

func TestTrackerManifestsAndFinish(t *testing.T) {
	tr := NewTracker(TrackerOptions{Verify: true, Now: time.Now})
	p := plan(map[string]int64{"a": 10})
	tr.Queue(p)
	tr.StartBlobs()
	tr.StartManifests()
	if s := tr.Snapshot(); s.Phase != PhaseManifests || len(s.Manifests) != 2 || s.Manifests[1].Names[0].Repo != "app" {
		t.Fatalf("manifest rows: %+v", s.Manifests)
	}
	tr.ManifestStart(digest("m1"))
	if s := tr.Snapshot(); s.Manifests[0].State != ManifestInFlight {
		t.Fatalf("m1 not in flight: %+v", s.Manifests[0])
	}
	tr.ManifestDone(digest("m1"), &image.Meta{Rootfs: &image.Rootfs{Status: image.RootfsOK, Entries: 42}})
	if s := tr.Snapshot(); s.Manifests[0].State != ManifestDone || s.Manifests[0].Rootfs.Entries != 42 {
		t.Fatalf("m1 not done: %+v", s.Manifests[0])
	}
	boom := errors.New("boom")
	tr.Finish(boom)
	if s := tr.Snapshot(); s.Phase != PhaseDone || s.Err != boom {
		t.Fatalf("after Finish: %+v", s)
	}
}

func rowFor(t *testing.T, s Snapshot, name string) BlobRow {
	t.Helper()
	for _, r := range s.Blobs {
		if r.Digest == digest(name) {
			return r
		}
	}
	t.Fatalf("no row for %s", name)
	return BlobRow{}
}
