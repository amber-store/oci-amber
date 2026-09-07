package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/oci"
)

func plainBar(f float64) string {
	return "[" + strings.Repeat("#", int(f*10)) + strings.Repeat(".", 10-int(f*10)) + "]"
}

func blobSnapshot() importer.Snapshot {
	return importer.Snapshot{
		Phase: importer.PhaseBlobs,
		Blobs: []importer.BlobRow{
			{Digest: oci.DigestOfBytes([]byte("a")), Size: 1900727, State: importer.BlobInFlight, Stage: blob.StageCommit, Fraction: 0.52, Progress: 950000},
			{Digest: oci.DigestOfBytes([]byte("b")), Size: 38 << 20, State: importer.BlobInFlight, Stage: blob.StageAnalyze, Fraction: 0.5, Progress: 38 << 20},
			{Digest: oci.DigestOfBytes([]byte("c")), Size: 10, State: importer.BlobDone, Fraction: 1},
			{Digest: oci.DigestOfBytes([]byte("d")), Size: 10, State: importer.BlobPresent, Fraction: 1},
			{Digest: oci.DigestOfBytes([]byte("e")), Size: 10, State: importer.BlobPending},
		},
		Counts:   importer.Counts{Pending: 1, InFlight: 2, Done: 1, Present: 1, Raw: 1},
		Fraction: 0.41,
		Elapsed:  42 * time.Second,
		ETA:      2*time.Minute + 10*time.Second,
		ETAKnown: true,
	}
}

func TestRenderViewBlobPhase(t *testing.T) {
	out := RenderView(blobSnapshot(), "Importing busybox.tar → busybox:1.37", 100, plainBar, "⠋")
	for _, want := range []string{
		"Importing busybox.tar → busybox:1.37",
		"elapsed 0:42",
		"1 done · 1 already present · 2 in flight · 1 pending · 1 raw",
		ShortDigest(oci.DigestOfBytes([]byte("a"))) + "  1.8 MiB",
		"commit",
		"52%",
		"analyze",
		"searching…",
		"41%",
		"ETA ~2m10s",
		"q or ctrl-c to cancel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ShortDigest(oci.DigestOfBytes([]byte("e")))) {
		t.Errorf("pending blobs must not get a row:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("line wider than the terminal: %q", line)
		}
	}
}

func TestRenderViewCheckingAndManifests(t *testing.T) {
	s := importer.Snapshot{Phase: importer.PhaseChecking, Checked: 0.41, Elapsed: time.Second}
	out := RenderView(s, "t", 80, plainBar, "")
	if !strings.Contains(out, "checking archive") || !strings.Contains(out, "41%") || strings.Contains(out, "ETA") {
		t.Errorf("checking view:\n%s", out)
	}
	s = importer.Snapshot{Phase: importer.PhaseManifests, Manifests: []importer.ManifestRow{
		{Digest: oci.DigestOfBytes([]byte("m")), State: importer.ManifestDone, Rootfs: &image.Rootfs{Status: image.RootfsOK, Entries: 1204}},
		{Digest: oci.DigestOfBytes([]byte("i")), IsIndex: true, State: importer.ManifestInFlight, Names: []dockerarchive.Name{{Repo: "busybox", Tag: "1.37"}}},
	}}
	out = RenderView(s, "t", 80, plainBar, "⠋")
	for _, want := range []string{"rootfs ok, 1,204 entries", "⠋", "busybox:1.37", "publishing"} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest view lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(RenderView(importer.Snapshot{Phase: importer.PhaseBlobs, Fraction: 0.1}, "t", 80, plainBar, ""), "ETA estimating") {
		t.Error("unknown ETA must render as estimating")
	}
}

func TestStatusLine(t *testing.T) {
	got := StatusLine(blobSnapshot())
	if got != "blobs 1/4 · 41% · ETA ~2m10s" {
		t.Errorf("StatusLine = %q", got)
	}
	s := importer.Snapshot{Phase: importer.PhaseChecking, Checked: 0.5}
	if got := StatusLine(s); got != "checking archive · 50%" {
		t.Errorf("StatusLine(checking) = %q", got)
	}
	s = importer.Snapshot{Phase: importer.PhaseManifests, Manifests: []importer.ManifestRow{{State: importer.ManifestDone}, {State: importer.ManifestInFlight}}}
	if got := StatusLine(s); got != "publishing manifests 1/2" {
		t.Errorf("StatusLine(manifests) = %q", got)
	}
}
