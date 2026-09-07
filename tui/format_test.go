package tui

import (
	"testing"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1023: "1023 B", 1024: "1.0 KiB", 1900727: "1.8 MiB", 4388352: "4.2 MiB", 5 << 30: "5.0 GiB", 1048575: "1.0 MiB", 1073741823: "1.0 GiB", 1048530: "1.0 MiB"}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1900727: "1,900,727", -1234567: "-1,234,567"}
	for in, want := range cases {
		if got := FormatCount(in); got != want {
			t.Errorf("FormatCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDurations(t *testing.T) {
	if got := FormatClock(42 * time.Second); got != "0:42" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatClock(12*time.Minute + 5*time.Second); got != "12:05" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatClock(time.Hour + 2*time.Minute + 3*time.Second); got != "1:02:03" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := FormatShort(42*time.Second + 300*time.Millisecond); got != "42s" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatShort(2*time.Minute + 10*time.Second); got != "2m10s" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatShort(time.Hour + 3*time.Minute); got != "1h03m" {
		t.Errorf("FormatShort = %q", got)
	}
	if got := FormatETA(2*time.Minute + 10*time.Second); got != "~2m10s" {
		t.Errorf("FormatETA = %q", got)
	}
	if got := FormatETA(90 * time.Minute); got != ">1h" {
		t.Errorf("FormatETA = %q", got)
	}
	if got := FormatETA(0); got != "~0s" {
		t.Errorf("FormatETA(0) = %q", got)
	}
}

func TestShortDigest(t *testing.T) {
	d := oci.DigestOfBytes([]byte("x"))
	if got := ShortDigest(d); len(got) != 8 || got != d.Hex()[:8] {
		t.Errorf("ShortDigest = %q", got)
	}
}
