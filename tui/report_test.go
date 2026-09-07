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

func sampleReport() *importer.Report {
	return &importer.Report{
		Duration: 42 * time.Second,
		Entries: []importer.EntryReport{{
			Names:        []dockerarchive.Name{{Repo: "busybox", Tag: "1.37"}},
			Digest:       oci.DigestOfBytes([]byte("index")),
			IsIndex:      true,
			Platforms:    1,
			Attestations: 1,
			Rootfs:       []*image.Rootfs{{Status: image.RootfsOK, Entries: 1204}},
			Stats:        image.Stats{DiskBytes: 1153024, LogicalBytes: 3000000, DedupedBytes: 1146000},
		}},
		Blobs:        importer.BlobCounts{Processed: 6, Prism: 5, Raw: 1, Present: 2, RawReasons: map[blob.RawReason]int{blob.ReasonNotTar: 1}},
		Compressed:   1900727,
		Uncompressed: 4388352,
		Added:        1153024,
		Logical:      3000000,
		Deduped:      1146000,
	}
}

func TestRenderReport(t *testing.T) {
	out := RenderReport(sampleReport(), "busybox.tar")
	for _, want := range []string{
		"Imported busybox.tar in 42s",
		"busybox:1.37",
		"index, 1 platform + 1 attestation",
		"rootfs ok, 1,204 entries",
		"8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present",
		"Compressed      1.8 MiB     1,900,727 bytes",
		"Uncompressed    4.2 MiB     4,388,352 bytes",
		"Added to CAS    1.1 MiB     1,153,024 bytes",
		"Dedup ratio     1.65x",
		"39.3% not written",
		"Chunks reused   38.2%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderReportNothingAdded(t *testing.T) {
	r := sampleReport()
	r.Added = 0
	r.Blobs = importer.BlobCounts{Present: 8, RawReasons: map[blob.RawReason]int{}}
	out := RenderReport(r, "busybox.tar")
	if !strings.Contains(out, "everything already present") || !strings.Contains(out, "8 already present") {
		t.Errorf("report:\n%s", out)
	}
	r.Added = 286 // a re-import rewrites manifest metadata
	out = RenderReport(r, "busybox.tar")
	if !strings.Contains(out, "everything already present, 286 bytes of manifest metadata rewritten") {
		t.Errorf("report:\n%s", out)
	}
}

func TestRenderReportNoRootfs(t *testing.T) {
	r := sampleReport()
	r.Entries[0].Rootfs = nil
	out := RenderReport(r, "b.tar")
	var entryLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "busybox:1.37") {
			entryLine = line
		}
	}
	if entryLine == "" {
		t.Fatalf("report has no entry line:\n%s", out)
	}
	if !strings.HasSuffix(entryLine, "attestation") {
		t.Errorf("entry line = %q, want it to end right after the kind (no rootfs field, no trailing spaces)", entryLine)
	}
	if strings.Contains(out, "  \n") {
		t.Errorf("report has a line with trailing whitespace:\n%s", out)
	}
}

func TestRenderReportManifestEntryAndSeveralRootfs(t *testing.T) {
	r := sampleReport()
	r.Entries[0].IsIndex = false
	r.Entries[0].Names = append(r.Entries[0].Names, dockerarchive.Name{Repo: "busybox", Tag: "latest"})
	out := RenderReport(r, "b.tar")
	if !strings.Contains(out, "busybox:1.37, busybox:latest") || !strings.Contains(out, "   manifest   ") {
		t.Errorf("report:\n%s", out)
	}
	r.Entries[0].IsIndex = true
	r.Entries[0].Platforms = 2
	r.Entries[0].Rootfs = []*image.Rootfs{{Status: image.RootfsOK, Entries: 5}, {Status: image.RootfsUnavailable}}
	out = RenderReport(r, "b.tar")
	if !strings.Contains(out, "2 platforms") || !strings.Contains(out, "rootfs 1/2 ok") {
		t.Errorf("report:\n%s", out)
	}
}
