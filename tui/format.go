// Package tui renders an import's progress: a Bubble Tea program for a
// terminal, periodic status lines otherwise, and the report both end with.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

// FormatBytes renders n in binary units with one decimal, bytes exact.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n) / unit
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		// 1023.95 is the point where "%.1f" would round up to "1024.0": roll
		// over to the next unit before that happens rather than after.
		if f < 1023.95 {
			return fmt.Sprintf("%.1f %s", f, suffix)
		}
		f /= unit
	}
	return fmt.Sprintf("%.1f EiB", f)
}

// FormatCount renders n with thousands separators.
func FormatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// hms decomposes d, rounded to the second, into hours, minutes and seconds.
func hms(d time.Duration) (h, m, s int) {
	d = d.Round(time.Second)
	h = int(d / time.Hour)
	m = int(d/time.Minute) % 60
	s = int(d/time.Second) % 60
	return h, m, s
}

// FormatClock renders elapsed time as m:ss or h:mm:ss.
func FormatClock(d time.Duration) string {
	h, m, s := hms(d)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// FormatShort renders a duration as 42s, 2m10s or 1h03m.
func FormatShort(d time.Duration) string {
	h, m, s := hms(d)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// FormatETA renders an estimate: ~ plus FormatShort, or >1h.
func FormatETA(d time.Duration) string {
	if d > time.Hour {
		return ">1h"
	}
	return "~" + FormatShort(d)
}

// ShortDigest is the first eight hex characters of d.
func ShortDigest(d oci.Digest) string {
	h := d.Hex()
	if len(h) > 8 {
		h = h[:8]
	}
	return h
}
