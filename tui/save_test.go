package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/draganm/oci-amber/dockerarchive"
	"github.com/draganm/oci-amber/oci"
)

func TestSaveTrackerSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	tr := NewSaveTracker(func() time.Time { return now })
	if s := tr.Snapshot(); s.Elapsed != 0 || s.Fraction != 0 || s.ETAKnown {
		t.Fatalf("idle snapshot = %+v", s)
	}
	d := oci.DigestOfBytes([]byte("layer"))
	tr.Progress(dockerarchive.WriteProgress{Count: 2, Total: 1000})
	now = now.Add(time.Second)
	tr.Progress(dockerarchive.WriteProgress{Count: 2, Total: 1000, Written: 250, Blob: d, Size: 600, BlobWritten: 250})
	s := tr.Snapshot()
	if s.Elapsed != time.Second || s.Fraction != 0.25 || s.Blob != d || s.BlobWritten != 250 || s.Count != 2 {
		t.Errorf("after one write: %+v", s)
	}
	if s.ETAKnown {
		t.Errorf("ETA reported before the warmup: %+v", s)
	}
	// 250 bytes in 2 s leaves 750 at 125 B/s: 6 s.
	now = now.Add(time.Second)
	if s = tr.Snapshot(); !s.ETAKnown || s.ETA != 6*time.Second || s.Elapsed != 2*time.Second {
		t.Errorf("after the warmup: %+v", s)
	}
	tr.Progress(dockerarchive.WriteProgress{Count: 2, Total: 1000, Written: 1000, Done: 2})
	if s = tr.Snapshot(); s.Fraction != 1 || !s.ETAKnown || s.ETA != 0 || s.Done != 2 {
		t.Errorf("complete: %+v", s)
	}
}

func TestSaveTrackerETAWaitsForBytes(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	tr := NewSaveTracker(func() time.Time { return now })
	tr.Progress(dockerarchive.WriteProgress{Count: 1, Total: 1000, Blob: oci.DigestOfBytes([]byte("x")), Size: 1000})
	now = now.Add(10 * time.Second)
	if s := tr.Snapshot(); s.ETAKnown || s.Fraction != 0 {
		t.Errorf("no byte written yet: %+v", s)
	}
}

func saveSnapshot() SaveSnapshot {
	return SaveSnapshot{
		WriteProgress: dockerarchive.WriteProgress{
			Count: 12, Total: 1288490188, Done: 3, Written: 536870912,
			Blob: oci.DigestOfBytes([]byte("a")), Size: 239495578, BlobWritten: 91008319,
		},
		Elapsed:  42 * time.Second,
		Fraction: 0.42,
		ETA:      63 * time.Second,
		ETAKnown: true,
	}
}

func TestRenderSaveView(t *testing.T) {
	out := RenderSaveView(saveSnapshot(), "Saving demo/app:v1 → app.tar", 100, plainBar)
	for _, want := range []string{
		"Saving demo/app:v1 → app.tar",
		"elapsed 0:42",
		"blobs  3/12 · 512.0 MiB of 1.2 GiB",
		ShortDigest(oci.DigestOfBytes([]byte("a"))) + "  228.4 MiB  [###.......]   38%",
		"[####......]   42%   ETA ~1m03s",
		"q or ctrl-c to cancel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("line wider than the terminal: %q", line)
		}
	}
}

func TestRenderSaveViewBeforeAndBetweenBlobs(t *testing.T) {
	s := SaveSnapshot{}
	out := RenderSaveView(s, "Saving x", 80, plainBar)
	if !strings.Contains(out, "resolving") || strings.Contains(out, "▸") {
		t.Errorf("nothing resolved yet:\n%s", out)
	}
	s = saveSnapshot()
	s.Blob, s.Size, s.BlobWritten, s.ETAKnown = "", 0, 0, false
	out = RenderSaveView(s, "Saving x", 80, plainBar)
	if strings.Contains(out, "▸") || !strings.Contains(out, "ETA estimating") {
		t.Errorf("between blobs:\n%s", out)
	}
}

func TestSaveStatusLine(t *testing.T) {
	if got, want := SaveStatusLine(saveSnapshot()), "blobs 3/12 · 42% · 512.0 MiB of 1.2 GiB · elapsed 0:42 · ETA ~1m03s"; got != want {
		t.Errorf("SaveStatusLine = %q, want %q", got, want)
	}
	s := saveSnapshot()
	s.ETAKnown = false
	if got := SaveStatusLine(s); !strings.HasSuffix(got, "ETA estimating") {
		t.Errorf("SaveStatusLine = %q", got)
	}
	if got, want := SaveStatusLine(SaveSnapshot{Elapsed: 3 * time.Second}), "resolving references · elapsed 0:03"; got != want {
		t.Errorf("SaveStatusLine = %q, want %q", got, want)
	}
}

func TestRunSavePlainPrintsStatusAndReturnsError(t *testing.T) {
	tr := NewSaveTracker(nil)
	var out bytes.Buffer
	err := RunSavePlain(&out, tr, 10*time.Millisecond, func() error {
		tr.Progress(dockerarchive.WriteProgress{Count: 3, Total: 100, Written: 50, Done: 1})
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "blobs 1/3 · 50%") {
		t.Fatalf("no status line printed:\n%s", out.String())
	}
	boom := errors.New("boom")
	if err := RunSavePlain(&out, tr, time.Hour, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// delayedReader delivers its bytes after delay, then EOF.
type delayedReader struct {
	delay time.Duration
	once  sync.Once
	r     io.Reader
}

func (d *delayedReader) Read(p []byte) (int, error) {
	d.once.Do(func() { time.Sleep(d.delay) })
	return d.r.Read(p)
}

func TestRunSaveRendersOnOutAndCancelsOnQ(t *testing.T) {
	tr := NewSaveTracker(nil)
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := &delayedReader{delay: 100 * time.Millisecond, r: strings.NewReader("q")}
	err := RunSave(tr, "Saving demo/app:v1 → app.tar", cancel, in, &out, func() error {
		tr.Progress(dockerarchive.WriteProgress{Count: 1, Total: 10})
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation q caused", err)
	}
	if !strings.Contains(out.String(), "Saving demo/app:v1") {
		t.Errorf("no frame rendered on out:\n%q", out.String())
	}
}

func TestRenderSaveViewKeepsElapsedOnNarrowTerminal(t *testing.T) {
	out := RenderSaveView(saveSnapshot(), "Saving demo/app:v1 → /a/very/long/path/that/does/not/fit/in/the/terminal/app.tar", 60, plainBar)
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasSuffix(first, "elapsed 0:42") || !strings.Contains(first, "…") || len([]rune(first)) > 60 {
		t.Errorf("header must keep the clock and shorten the title: %q", first)
	}
}
