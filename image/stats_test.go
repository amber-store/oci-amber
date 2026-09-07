package image

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

func parse(t *testing.T, body []byte) *oci.Manifest {
	t.Helper()
	m, err := oci.ParseManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestComputeStatsRecentThenPresent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg, cfgMeta := e.configBlob("stats")
	l1, l1Meta := e.layerBlob(200 << 10)
	l2, l2Meta := e.layerBlob(300 << 10)
	body := manifestBody(t, imageManifest(cfg, l1, l2, l1)) // l1 twice: counted once
	m := parse(t, body)
	objects := store.Stats{LogicalBytes: 1000, DedupedBytes: 400, DiskBytes: 600}
	sizes := cfgMeta.Size + l1Meta.Size + l2Meta.Size
	blobStats := cfgMeta.Stats.Add(l1Meta.Stats).Add(l2Meta.Stats)
	if blobStats.DiskBytes == 0 || blobStats.LogicalBytes < sizes {
		t.Fatalf("test precondition: fresh blobs report stats %+v", blobStats)
	}

	// First computation: every blob was just finalized, so the recent-uploads
	// table supplies its numbers.
	got, err := e.images.computeStats(ctx, "library/app", m, body, objects)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		TotalBytes:   sizes + int64(len(body)),
		LogicalBytes: blobStats.LogicalBytes + objects.LogicalBytes,
		DedupedBytes: blobStats.DedupedBytes + objects.DedupedBytes,
		DiskBytes:    blobStats.DiskBytes + objects.DiskBytes,
	}
	if got != want {
		t.Fatalf("computeStats (recent) = %+v, want %+v", got, want)
	}

	// The entries were consumed: now the blobs count as already present.
	got, err = e.images.computeStats(ctx, "library/app", m, body, objects)
	if err != nil {
		t.Fatal(err)
	}
	want = Stats{
		TotalBytes:   sizes + int64(len(body)),
		LogicalBytes: sizes + objects.LogicalBytes,
		DedupedBytes: sizes + objects.DedupedBytes,
		DiskBytes:    objects.DiskBytes,
	}
	if got != want {
		t.Fatalf("computeStats (already present) = %+v, want %+v", got, want)
	}
}

func TestComputeStatsSkippedBlob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	data := randomBytes(t, 128<<10)
	l, first := e.putBlob(layerMediaType, data)
	if _, ok := e.images.blobs.TakeRecent(l.Digest); !ok {
		t.Fatal("test precondition: first upload not in the recent table")
	}
	// The second upload of the same bytes is skipped by whole-blob dedup and
	// recorded as fully deduplicated.
	second, err := e.blobs.Put(ctx, upload.NewMemorySpool(data))
	if err != nil {
		t.Fatal(err)
	}
	if second.Stats.LogicalBytes != first.Size || second.Stats.DedupedBytes != first.Size || second.Stats.DiskBytes != 0 {
		t.Fatalf("skipped blob stats = %+v, want logical = deduped = %d, disk 0", second.Stats, first.Size)
	}
	cfg, cfgMeta := e.configBlob("skipped")
	body := manifestBody(t, imageManifest(cfg, l))
	objects := store.Stats{LogicalBytes: 10, DedupedBytes: 0, DiskBytes: 10}
	got, err := e.images.computeStats(ctx, "app", parse(t, body), body, objects)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		TotalBytes:   cfgMeta.Size + first.Size + int64(len(body)),
		LogicalBytes: cfgMeta.Stats.LogicalBytes + first.Size + 10,
		DedupedBytes: cfgMeta.Stats.DedupedBytes + first.Size,
		DiskBytes:    cfgMeta.Stats.DiskBytes + 10,
	}
	if got != want {
		t.Fatalf("computeStats (skipped blob) = %+v, want %+v", got, want)
	}
}

func TestComputeStatsIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bodyA, dA, metaA := e.pushChild("library/app", "idx-a")
	bodyB, dB, metaB := e.pushChild("library/app", "idx-b")
	idx := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: []oci.Descriptor{
		{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA))},
		{MediaType: oci.MediaTypeOCIManifest, Digest: dB, Size: int64(len(bodyB))},
		{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA))},
	}}
	body := manifestBody(t, idx)
	objects := store.Stats{LogicalBytes: 300, DedupedBytes: 100, DiskBytes: 200}
	got, err := e.images.computeStats(ctx, "library/app", parse(t, body), body, objects)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		TotalBytes:   int64(len(body)) + metaA.Stats.TotalBytes + metaB.Stats.TotalBytes,
		LogicalBytes: metaA.Stats.LogicalBytes + metaB.Stats.LogicalBytes + 300,
		DedupedBytes: metaA.Stats.DedupedBytes + metaB.Stats.DedupedBytes + 100,
		DiskBytes:    metaA.Stats.DiskBytes + metaB.Stats.DiskBytes + 200,
	}
	if got != want {
		t.Fatalf("computeStats (index) = %+v, want %+v", got, want)
	}

	// A child that is not stored in this repository is MANIFEST_BLOB_UNKNOWN.
	_, err = e.images.computeStats(ctx, "other/repo", parse(t, body), body, objects)
	oe := ociErr(t, err, oci.CodeManifestBlobUnknown)
	if detail, ok := oe.Detail.(map[string]string); !ok || detail["digest"] != string(dA) {
		t.Fatalf("Detail = %#v, want digest %s", oe.Detail, dA)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.images.computeStats(cancelled, "library/app", parse(t, body), body, objects); !errors.Is(err, context.Canceled) {
		t.Fatalf("computeStats with cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestPutStatsFreshAndRepush(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfgData := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	l1Data := randomBytes(t, 256<<10)
	l2Data := randomBytes(t, 256<<10)
	cfg, cfgMeta := e.putBlob(oci.MediaTypeOCIConfig, cfgData)
	l1, l1Meta := e.putBlob(layerMediaType, l1Data)
	l2, l2Meta := e.putBlob(layerMediaType, l2Data)
	body := manifestBody(t, imageManifest(cfg, l1, l2))
	sizes := cfgMeta.Size + l1Meta.Size + l2Meta.Size
	blobStats := cfgMeta.Stats.Add(l1Meta.Stats).Add(l2Meta.Stats)

	// Fresh push: blobs contribute their real stats; the manifest's own
	// objects are all new.
	first := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	if first.Stats.TotalBytes != sizes+int64(len(body)) {
		t.Fatalf("TotalBytes = %d, want %d", first.Stats.TotalBytes, sizes+int64(len(body)))
	}
	if first.Stats.LogicalBytes < blobStats.LogicalBytes+int64(len(body)) {
		t.Fatalf("LogicalBytes = %d, want at least blobs' %d + manifest length %d", first.Stats.LogicalBytes, blobStats.LogicalBytes, len(body))
	}
	if first.Stats.DedupedBytes != blobStats.DedupedBytes {
		t.Fatalf("DedupedBytes = %d, want the blobs' %d (manifest objects are new)", first.Stats.DedupedBytes, blobStats.DedupedBytes)
	}
	if first.Stats.DiskBytes <= blobStats.DiskBytes {
		t.Fatalf("DiskBytes = %d, want more than the blobs' %d", first.Stats.DiskBytes, blobStats.DiskBytes)
	}
	manifestLogical := first.Stats.LogicalBytes - blobStats.LogicalBytes
	if math.IsInf(first.Stats.CompressionRatio(), 1) {
		t.Fatal("fresh push reports an infinite compression ratio")
	}

	// Re-push of the same manifest with nothing re-uploaded: blobs count as
	// already present, manifest objects are fully deduplicated.
	second := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	want := Stats{
		TotalBytes:   first.Stats.TotalBytes,
		LogicalBytes: sizes + manifestLogical,
		DedupedBytes: sizes + manifestLogical,
		DiskBytes:    0,
	}
	if second.Stats != want {
		t.Fatalf("re-push stats = %+v, want %+v", second.Stats, want)
	}
	if !math.IsInf(second.Stats.CompressionRatio(), 1) || second.Stats.DedupedPercent() != 100 {
		t.Fatalf("re-push ratio = %v, percent = %v", second.Stats.CompressionRatio(), second.Stats.DedupedPercent())
	}

	// Re-upload the blobs (whole-blob dedup records them as fully deduplicated)
	// and push under a new tag: the same numbers.
	for _, data := range [][]byte{cfgData, l1Data, l2Data} {
		m, err := e.blobs.Put(ctx, upload.NewMemorySpool(data))
		if err != nil {
			t.Fatal(err)
		}
		if m.Stats.DiskBytes != 0 || m.Stats.DedupedBytes != m.Size {
			t.Fatalf("re-uploaded blob %s stats = %+v, want fully deduplicated", m.Digest, m.Stats)
		}
	}
	third := e.put("library/app", "v2", oci.MediaTypeOCIManifest, body)
	if third.Stats != want {
		t.Fatalf("re-push after blob re-upload stats = %+v, want %+v", third.Stats, want)
	}
}

func TestPutLogLine(t *testing.T) {
	e := newEnv(t)
	cfg, _ := e.configBlob("log")
	l1, _ := e.layerBlob(64 << 10)
	body := manifestBody(t, imageManifest(cfg, l1))
	m := e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)

	line := lastLine(e.logs.String(), `msg="image pushed"`)
	if line == "" {
		t.Fatalf("no image log line in:\n%s", e.logs.String())
	}
	for _, want := range []string{
		"repo=library/app",
		"reference=v1",
		"digest=" + string(m.Digest),
		"kind=manifest",
		"blobs=2",
		"manifests=0",
		fmt.Sprintf("total_bytes=%d", m.Stats.TotalBytes),
		fmt.Sprintf("logical_bytes=%d", m.Stats.LogicalBytes),
		fmt.Sprintf("deduped_bytes=%d", m.Stats.DedupedBytes),
		fmt.Sprintf("deduped_percent=%v", roundTo(m.Stats.DedupedPercent(), 1)),
		fmt.Sprintf("disk_bytes=%d", m.Stats.DiskBytes),
		fmt.Sprintf("compression_ratio=%v", roundTo(m.Stats.CompressionRatio(), 2)),
		"duration=",
	} {
		if !strings.Contains(line, " "+want) {
			t.Errorf("log line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "total_bytes=\"") {
		t.Errorf("byte counts must be raw integers:\n%s", line)
	}

	// A push that wrote nothing logs an infinite ratio and 100 percent.
	e.put("library/app", "v1", oci.MediaTypeOCIManifest, body)
	line = lastLine(e.logs.String(), `msg="image pushed"`)
	for _, want := range []string{" compression_ratio=+Inf", " deduped_percent=100", " disk_bytes=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("re-push log line lacks %q:\n%s", want, line)
		}
	}

	// Pushing by digest logs the digest as the reference; an index logs its
	// child count.
	bodyA, dA, _ := e.pushChild("library/app", "log-a")
	line = lastLine(e.logs.String(), `msg="image pushed"`)
	if !strings.Contains(line, " reference="+string(dA)) {
		t.Errorf("push by digest log line lacks the digest reference:\n%s", line)
	}
	idx := manifestBody(t, oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIIndex, Manifests: []oci.Descriptor{
		{MediaType: oci.MediaTypeOCIManifest, Digest: dA, Size: int64(len(bodyA))},
	}})
	e.put("library/app", "latest", oci.MediaTypeOCIIndex, idx)
	line = lastLine(e.logs.String(), `msg="image pushed"`)
	for _, want := range []string{" kind=index", " blobs=0", " manifests=1", " reference=latest"} {
		if !strings.Contains(line, want) {
			t.Errorf("index log line lacks %q:\n%s", want, line)
		}
	}
}

// lastLine returns the last line of logs containing marker, or "".
func lastLine(logs, marker string) string {
	last := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, marker) {
			last = l
		}
	}
	return last
}
