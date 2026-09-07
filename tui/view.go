package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/draganm/oci-amber/blob"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/importer"
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// RenderView renders one frame for width columns. bar draws a progress bar
// for a fraction; spinner is the current spinner frame. It is a pure
// function so tests can render snapshots without a terminal.
func RenderView(s importer.Snapshot, title string, width int, bar func(float64) string, spinner string) string {
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	b.WriteString(header(title, s.Elapsed, width))
	switch s.Phase {
	case importer.PhaseChecking, importer.PhaseIdle:
		fmt.Fprintf(&b, "  checking archive  %s  %3.0f%%\n", bar(s.Checked), s.Checked*100)
	case importer.PhaseBlobs:
		c := s.Counts
		counts := fmt.Sprintf("%d done · %d already present · %d in flight · %d pending", c.Done, c.Present, c.InFlight, c.Pending)
		if c.Raw > 0 {
			counts += fmt.Sprintf(" · %d raw", c.Raw)
		}
		fmt.Fprintf(&b, "  blobs  %s\n", styleDim.Render(counts))
		for _, r := range s.Blobs {
			if r.State != importer.BlobInFlight {
				continue
			}
			b.WriteString(blobLine(r, bar))
		}
		eta := "ETA estimating"
		if s.ETAKnown {
			eta = "ETA " + FormatETA(s.ETA)
		}
		fmt.Fprintf(&b, "\n  %s  %3.0f%%   %s\n", bar(s.Fraction), s.Fraction*100, eta)
	case importer.PhaseManifests, importer.PhaseDone:
		b.WriteString("  publishing manifests\n")
		for _, m := range s.Manifests {
			b.WriteString(manifestLine(m, spinner))
		}
	}
	b.WriteString("\n  " + styleDim.Render("q or ctrl-c to cancel") + "\n")
	return clampWidth(b.String(), width)
}

// header is the first line of a view: the title on the left, the elapsed
// time on the right, and the title shortened with … when both do not fit
// in width, so the clock is never pushed off the screen.
func header(title string, elapsed time.Duration, width int) string {
	clock := "elapsed " + FormatClock(elapsed)
	title = truncate(title, width-lipgloss.Width(clock)-2)
	gap := max(2, width-lipgloss.Width(title)-lipgloss.Width(clock))
	return styleTitle.Render(title) + strings.Repeat(" ", gap) + styleDim.Render(clock) + "\n\n"
}

// truncate shortens s to width cells, ending it with … when it had to.
func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func blobLine(r importer.BlobRow, bar func(float64) string) string {
	stage := string(r.Stage)
	if stage == "" {
		stage = "queued"
	}
	detail := fmt.Sprintf("%3.0f%%", r.Fraction*100)
	if r.Stage == blob.StageAnalyze && r.Size > 0 && r.Progress >= r.Size {
		detail = "searching…"
	}
	return fmt.Sprintf("  ▸ %s  %-9s  %-9s  %s  %s\n", ShortDigest(r.Digest), FormatBytes(r.Size), stage, bar(r.Fraction), detail)
}

func manifestLine(m importer.ManifestRow, spinner string) string {
	label := ShortDigest(m.Digest)
	if len(m.Names) > 0 {
		names := make([]string, len(m.Names))
		for i, n := range m.Names {
			names[i] = n.String()
		}
		label = strings.Join(names, ", ")
	}
	kind := "manifest"
	if m.IsIndex {
		kind = "index"
	}
	switch m.State {
	case importer.ManifestInFlight:
		what := "building rootfs"
		if m.IsIndex {
			what = "publishing"
		}
		return fmt.Sprintf("  %s %s  %s  %s\n", spinner, label, kind, styleDim.Render(what))
	case importer.ManifestDone:
		out := styleOK.Render("✓")
		rf := ""
		if m.Rootfs != nil && m.Rootfs.Status != image.RootfsNotApplicable {
			switch m.Rootfs.Status {
			case image.RootfsOK, image.RootfsPartial:
				rf = fmt.Sprintf("rootfs %s, %s entries", m.Rootfs.Status, FormatCount(int64(m.Rootfs.Entries)))
			default:
				rf = styleWarn.Render("rootfs " + string(m.Rootfs.Status))
			}
		}
		return fmt.Sprintf("  %s %s  %s  %s\n", out, label, kind, rf)
	default:
		return fmt.Sprintf("  %s %s  %s\n", styleDim.Render("·"), label, kind)
	}
}

// clampWidth truncates every line to width cells so the block never wraps,
// which would break the in-place redraw.
func clampWidth(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = lipgloss.NewStyle().MaxWidth(width).Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// StatusLine is the one-line summary plain mode prints.
func StatusLine(s importer.Snapshot) string {
	switch s.Phase {
	case importer.PhaseChecking, importer.PhaseIdle:
		return fmt.Sprintf("checking archive · %.0f%%", s.Checked*100)
	case importer.PhaseBlobs:
		c := s.Counts
		total := c.Pending + c.InFlight + c.Done + c.Failed
		eta := "ETA estimating"
		if s.ETAKnown {
			eta = "ETA " + FormatETA(s.ETA)
		}
		return fmt.Sprintf("blobs %d/%d · %.0f%% · %s", c.Done, total, s.Fraction*100, eta)
	default:
		done := 0
		for _, m := range s.Manifests {
			if m.State == importer.ManifestDone {
				done++
			}
		}
		return fmt.Sprintf("publishing manifests %d/%d", done, len(s.Manifests))
	}
}
