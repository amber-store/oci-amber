package blob

import (
	"compress/gzip"
	"context"
	"sync"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

// recorder is an Observer that keeps every call in order.
type recorder struct {
	mu     sync.Mutex
	stages []Stage
	// progress holds, per stage in the order entered, every n reported.
	progress [][]int64
}

func (r *recorder) BlobStage(_ oci.Digest, s Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, s)
	r.progress = append(r.progress, nil)
}

func (r *recorder) BlobProgress(_ oci.Digest, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.progress) == 0 {
		panic("progress before any stage")
	}
	r.progress[len(r.progress)-1] = append(r.progress[len(r.progress)-1], n)
}

// assertStages checks the stage sequence and that, within each stage, the
// counts never decrease and the stages named in reachSize end at size.
func (r *recorder) assertStages(t *testing.T, size int64, want []Stage, reachSize ...Stage) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stages) != len(want) {
		t.Fatalf("stages = %v, want %v", r.stages, want)
	}
	for i := range want {
		if r.stages[i] != want[i] {
			t.Fatalf("stages = %v, want %v", r.stages, want)
		}
		var last int64 = -1
		for _, n := range r.progress[i] {
			if n < last {
				t.Fatalf("stage %s: progress went backwards: %v", want[i], r.progress[i])
			}
			if n > size {
				t.Fatalf("stage %s: progress %d exceeds size %d", want[i], n, size)
			}
			last = n
		}
		for _, s := range reachSize {
			if s == want[i] && last != size {
				t.Fatalf("stage %s ended at %d, want %d", s, last, size)
			}
		}
	}
}

func TestObserverPrismStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(64<<10, 1)), gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindPrism {
		t.Fatalf("kind = %s (%s), want prism", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageConfirm, StageCommit, StageVerify}, StageAnalyze, StageConfirm, StageCommit, StageVerify)
}

func TestObserverPrismWithoutVerify(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: false, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(64<<10, 1)), gzip.DefaultCompression)
	putPrism(t, b, data)
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageConfirm, StageCommit}, StageAnalyze, StageConfirm, StageCommit)
}

func TestObserverRawStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := randomBytes(t, 40<<10) // not a tar: analyze decides raw, then the bytes are stored verbatim
	meta, err := b.Put(context.Background(), spoolOf(data))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotTar {
		t.Fatalf("kind/reason = %s/%s, want raw/not-tar", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageRaw}, StageRaw)
}

func TestObserverStagingDowngrade(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, AllowRaw: true, Observer: rec})
	full := tarBytes(t, "usr/lib/app", textBytes(8<<10, 3))
	data := gzipBytes(t, full[:tarHeaderSize+1024], gzip.DefaultCompression)
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonDecomposeFailed {
		t.Fatalf("kind/reason = %s/%s, want raw/decompose-failed", meta.Kind, meta.RawReason)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageConfirm, StageRaw}, StageAnalyze, StageConfirm, StageRaw)
}

func TestObserverDedupHitReportsNothing(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := gzipBytes(t, tarBytes(t, "etc/motd", textBytes(8<<10, 1)), gzip.DefaultCompression)
	putPrism(t, b, data)
	before := len(rec.stages)
	if _, err := b.Put(context.Background(), spoolOf(data)); err != nil {
		t.Fatal(err)
	}
	if len(rec.stages) != before {
		t.Fatalf("a dedup hit reported stages: %v", rec.stages[before:])
	}
}
