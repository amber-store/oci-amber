package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/draganm/oci-amber/dockerarchive"
)

// etaWarmup is how long a save must have been writing before an ETA is
// reported; a rate measured over less is noise.
const etaWarmup = 2 * time.Second

// SaveTracker is a save's progress state: dockerarchive.Write reports
// through Progress, renderers take snapshots. It is safe for concurrent
// use.
type SaveTracker struct {
	now func() time.Time

	mu      sync.Mutex
	started time.Time // NewSaveTracker; the elapsed clock
	planned time.Time // the first report; the ETA clock
	p       dockerarchive.WriteProgress
}

// NewSaveTracker returns a tracker whose clock starts now; a nil now means
// time.Now.
func NewSaveTracker(now func() time.Time) *SaveTracker {
	if now == nil {
		now = time.Now
	}
	return &SaveTracker{now: now, started: now()}
}

// Progress records a report from dockerarchive.Write; it is the
// WriteOptions.Progress callback.
func (t *SaveTracker) Progress(p dockerarchive.WriteProgress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.planned.IsZero() {
		t.planned = t.now()
	}
	t.p = p
}

// SaveSnapshot is the tracker's state with the elapsed time, the fraction
// of the bytes written and, once the rate is measurable, the ETA.
type SaveSnapshot struct {
	dockerarchive.WriteProgress
	Elapsed  time.Duration
	Fraction float64
	ETA      time.Duration
	ETAKnown bool
}

// Snapshot copies the state. The ETA is the bytes left over the rate
// measured since the first report, once etaWarmup has passed and a byte
// has been written.
func (t *SaveTracker) Snapshot() SaveSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := SaveSnapshot{WriteProgress: t.p, Elapsed: now.Sub(t.started)}
	if t.p.Total > 0 {
		s.Fraction = min(1, float64(t.p.Written)/float64(t.p.Total))
	}
	if !t.planned.IsZero() && t.p.Written > 0 {
		if elapsed := now.Sub(t.planned); elapsed >= etaWarmup {
			rate := float64(t.p.Written) / elapsed.Seconds()
			s.ETA = time.Duration(float64(t.p.Total-t.p.Written) / rate * float64(time.Second))
			s.ETAKnown = true
		}
	}
	return s
}

// RenderSaveView renders one frame of a save for width columns; bar draws
// a progress bar for a fraction. It is a pure function so tests can render
// snapshots without a terminal.
func RenderSaveView(s SaveSnapshot, title string, width int, bar func(float64) string) string {
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	b.WriteString(header(title, s.Elapsed, width))
	if s.Count == 0 {
		b.WriteString("  resolving references\n")
	} else {
		fmt.Fprintf(&b, "  blobs  %s\n", styleDim.Render(fmt.Sprintf("%d/%d · %s of %s", s.Done, s.Count, FormatBytes(s.Written), FormatBytes(s.Total))))
		if s.Blob != "" {
			part := 1.0
			if s.Size > 0 {
				part = min(1, float64(s.BlobWritten)/float64(s.Size))
			}
			fmt.Fprintf(&b, "  ▸ %s  %-9s  %s  %3.0f%%\n", ShortDigest(s.Blob), FormatBytes(s.Size), bar(part), part*100)
		}
		fmt.Fprintf(&b, "\n  %s  %3.0f%%   %s\n", bar(s.Fraction), s.Fraction*100, etaLabel(s))
	}
	b.WriteString("\n  " + styleDim.Render("q or ctrl-c to cancel") + "\n")
	return clampWidth(b.String(), width)
}

// SaveStatusLine is the one-line summary plain mode prints.
func SaveStatusLine(s SaveSnapshot) string {
	elapsed := "elapsed " + FormatClock(s.Elapsed)
	if s.Count == 0 {
		return "resolving references · " + elapsed
	}
	return fmt.Sprintf("blobs %d/%d · %.0f%% · %s of %s · %s · %s", s.Done, s.Count, s.Fraction*100, FormatBytes(s.Written), FormatBytes(s.Total), elapsed, etaLabel(s))
}

func etaLabel(s SaveSnapshot) string {
	if s.ETAKnown {
		return "ETA " + FormatETA(s.ETA)
	}
	return "ETA estimating"
}
