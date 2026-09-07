// Package importer stores a planned docker save archive through the blob
// and image stores and tracks progress for a display.
package importer

import (
	"sync"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/oci"
)

// Phase is where an import is.
type Phase int

const (
	PhaseIdle      Phase = iota
	PhaseChecking        // verifying blob digests against the archive
	PhaseBlobs           // storing blobs
	PhaseManifests       // publishing manifests and tags
	PhaseDone
)

// BlobState is one blob's state.
type BlobState int

const (
	BlobPending BlobState = iota
	BlobPresent           // already stored: skipped at planning, or a dedup hit
	BlobInFlight
	BlobDone
	BlobFailed
)

// ManifestState is one manifest's state.
type ManifestState int

const (
	ManifestPending ManifestState = iota
	ManifestInFlight
	ManifestDone
)

// BlobRow is a blob's progress. Fraction is 0..1 of the blob's own work.
type BlobRow struct {
	Digest    oci.Digest
	Size      int64
	MediaType string
	State     BlobState
	Stage     blob.Stage
	Progress  int64 // bytes reported in the current stage
	Fraction  float64
	Kind      blob.Kind      // set when done
	RawReason blob.RawReason // set when done raw
}

// ManifestRow is a manifest's progress.
type ManifestRow struct {
	Digest  oci.Digest
	Names   []dockerarchive.Name // set for entries
	IsIndex bool
	State   ManifestState
	Rootfs  *image.Rootfs // set when done, manifests only
}

// Counts summarize the blob rows.
type Counts struct {
	Pending, InFlight, Done, Present, Raw, Failed int
}

// Snapshot is a consistent copy of the tracker's state for rendering.
type Snapshot struct {
	Phase     Phase
	Checked   float64 // checking phase progress, 0..1
	Blobs     []BlobRow
	Counts    Counts
	Fraction  float64 // blob phase progress, 0..1, present blobs excluded
	Elapsed   time.Duration
	ETA       time.Duration
	ETAKnown  bool
	Manifests []ManifestRow
	Err       error
}

// TrackerOptions configure a Tracker. Verify says whether the blob store
// runs the round-trip check, which decides the stage shares. Now defaults
// to time.Now.
type TrackerOptions struct {
	Verify bool
	Now    func() time.Time
}

// etaWarmup is how long the blob phase must have run before an ETA is
// reported; a rate measured over less is noise.
const etaWarmup = 2 * time.Second

type blobRow struct {
	BlobRow
	stageBase, stageShare float64
	sawStage              bool
}

// Tracker is the shared progress state: the importer records state
// changes, the blob store (through blob.Observer) records stages and byte
// counts, renderers take snapshots. All methods are safe for concurrent
// use.
type Tracker struct {
	opts TrackerOptions
	now  func() time.Time

	mu        sync.Mutex
	phase     Phase
	started   time.Time // Queue
	blobStart time.Time // StartBlobs
	toCheck   int64
	checked   int64
	blobs     []*blobRow
	byDigest  map[oci.Digest]*blobRow
	manifests []ManifestRow
	err       error
}

// NewTracker returns an idle tracker.
func NewTracker(opts TrackerOptions) *Tracker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Tracker{opts: opts, now: opts.Now, byDigest: map[oci.Digest]*blobRow{}}
}

// Queue loads the plan: one row per blob and per manifest, present blobs
// already marked, and enters the checking phase.
func (t *Tracker) Queue(p *dockerarchive.Plan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseChecking
	t.started = t.now()
	for _, pb := range p.Blobs {
		r := &blobRow{BlobRow: BlobRow{Digest: pb.Digest, Size: pb.Size, MediaType: pb.MediaType}}
		if pb.Present {
			r.State = BlobPresent
			r.Fraction = 1
		} else {
			t.toCheck += pb.Size
		}
		t.blobs = append(t.blobs, r)
		t.byDigest[pb.Digest] = r
	}
	names := map[oci.Digest][]dockerarchive.Name{}
	for _, e := range p.Entries {
		names[e.Digest] = e.Names
	}
	for _, pm := range p.Manifests {
		t.manifests = append(t.manifests, ManifestRow{Digest: pm.Digest, Names: names[pm.Digest], IsIndex: pm.IsIndex})
	}
}

// Checked records the bytes verified so far in the checking phase.
func (t *Tracker) Checked(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checked = n
}

// StartBlobs enters the blob phase; the ETA clock starts here.
func (t *Tracker) StartBlobs() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseBlobs
	t.blobStart = t.now()
}

// Start marks d in flight.
func (t *Tracker) Start(d oci.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.byDigest[d]; r != nil {
		r.State = BlobInFlight
	}
}

// Done records Put's result for d. A blob for which no stage was ever
// reported was a dedup hit inside Put and counts as present.
func (t *Tracker) Done(d oci.Digest, m *blob.Meta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil {
		return
	}
	r.State = BlobDone
	if !r.sawStage {
		r.State = BlobPresent
	}
	r.Fraction = 1
	if m != nil {
		r.Kind = m.Kind
		r.RawReason = m.RawReason
	}
}

// Fail records a failed Put for d.
func (t *Tracker) Fail(d oci.Digest, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.byDigest[d]; r != nil {
		r.State = BlobFailed
	}
}

// StartManifests enters the manifest phase.
func (t *Tracker) StartManifests() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseManifests
}

// ManifestStart marks d in flight.
func (t *Tracker) ManifestStart(d oci.Digest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.manifests {
		if t.manifests[i].Digest == d {
			t.manifests[i].State = ManifestInFlight
		}
	}
}

// ManifestDone records a published manifest and its rootfs outcome.
func (t *Tracker) ManifestDone(d oci.Digest, m *image.Meta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.manifests {
		if t.manifests[i].Digest == d {
			t.manifests[i].State = ManifestDone
			if m != nil {
				t.manifests[i].Rootfs = m.Rootfs
			}
		}
	}
}

// Finish ends the run with err (nil on success).
func (t *Tracker) Finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = PhaseDone
	t.err = err
}

// BlobStage implements blob.Observer.
func (t *Tracker) BlobStage(d oci.Digest, s blob.Stage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil {
		return
	}
	prevEnd := r.stageBase + r.stageShare
	if !r.sawStage {
		prevEnd = 0
	}
	r.sawStage = true
	r.Stage = s
	r.Progress = 0
	switch s {
	case blob.StageAnalyze:
		if t.opts.Verify {
			r.stageBase, r.stageShare = 0, 0.35
		} else {
			r.stageBase, r.stageShare = 0, 0.4
		}
	case blob.StageConfirm:
		if t.opts.Verify {
			r.stageBase, r.stageShare = 0.35, 0.3
		} else {
			r.stageBase, r.stageShare = 0.4, 0.35
		}
	case blob.StageCommit:
		if t.opts.Verify {
			r.stageBase, r.stageShare = 0.65, 0.2
		} else {
			r.stageBase, r.stageShare = 0.75, 0.25
		}
	case blob.StageVerify:
		r.stageBase, r.stageShare = 0.85, 0.15
	default: // raw: from wherever the previous stage ended to the end
		r.stageBase, r.stageShare = prevEnd, 1-prevEnd
	}
	r.Fraction = r.stageBase
}

// BlobProgress implements blob.Observer.
func (t *Tracker) BlobProgress(d oci.Digest, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.byDigest[d]
	if r == nil || !r.sawStage {
		return
	}
	r.Progress = n
	part := 1.0
	if r.Size > 0 {
		part = min(1, float64(n)/float64(r.Size))
	}
	r.Fraction = r.stageBase + r.stageShare*part
}

// Snapshot copies the state. Fraction and the ETA are derived here.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := Snapshot{Phase: t.phase, Err: t.err}
	if !t.started.IsZero() {
		s.Elapsed = now.Sub(t.started)
	}
	if t.toCheck > 0 {
		s.Checked = min(1, float64(t.checked)/float64(t.toCheck))
	} else if t.phase > PhaseChecking {
		s.Checked = 1
	}
	var total, progress float64
	s.Blobs = make([]BlobRow, len(t.blobs))
	for i, r := range t.blobs {
		s.Blobs[i] = r.BlobRow
		switch r.State {
		case BlobPending:
			s.Counts.Pending++
		case BlobInFlight:
			s.Counts.InFlight++
		case BlobDone:
			s.Counts.Done++
			if r.Kind == blob.KindRaw {
				s.Counts.Raw++
			}
		case BlobPresent:
			s.Counts.Present++
			continue // outside the estimate
		case BlobFailed:
			s.Counts.Failed++
		}
		total += float64(r.Size)
		progress += float64(r.Size) * r.Fraction
	}
	if total > 0 {
		s.Fraction = progress / total
	} else if t.phase > PhaseBlobs {
		s.Fraction = 1
	}
	if t.phase == PhaseBlobs && !t.blobStart.IsZero() {
		elapsed := now.Sub(t.blobStart)
		if elapsed >= etaWarmup && progress > 0 {
			rate := progress / elapsed.Seconds()
			s.ETA = time.Duration((total - progress) / rate * float64(time.Second))
			s.ETAKnown = true
		}
	}
	s.Manifests = append([]ManifestRow(nil), t.manifests...)
	return s
}
