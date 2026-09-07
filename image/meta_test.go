package image

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

func TestStatsCompressionRatio(t *testing.T) {
	cases := []struct {
		s    Stats
		want float64
	}{
		{Stats{TotalBytes: 100, DiskBytes: 10}, 10},
		{Stats{TotalBytes: 95631872, DiskBytes: 10276044}, 9.30629},
		{Stats{TotalBytes: 100, DiskBytes: 0}, math.Inf(1)},
		{Stats{TotalBytes: 0, DiskBytes: 0}, math.Inf(1)},
	}
	for _, c := range cases {
		got := c.s.CompressionRatio()
		if math.IsInf(c.want, 1) {
			if !math.IsInf(got, 1) {
				t.Errorf("%+v: CompressionRatio = %v, want +Inf", c.s, got)
			}
			continue
		}
		if math.Abs(got-c.want) > 1e-4 {
			t.Errorf("%+v: CompressionRatio = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestStatsDedupedPercent(t *testing.T) {
	cases := []struct {
		s    Stats
		want float64
	}{
		{Stats{LogicalBytes: 200, DedupedBytes: 50}, 25},
		{Stats{LogicalBytes: 327545651, DedupedBytes: 293700000}, 89.6668},
		{Stats{LogicalBytes: 0, DedupedBytes: 0}, 0},
		{Stats{LogicalBytes: 10, DedupedBytes: 10}, 100},
	}
	for _, c := range cases {
		got := c.s.DedupedPercent()
		if math.Abs(got-c.want) > 1e-3 {
			t.Errorf("%+v: DedupedPercent = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestMetaJSONOmitsAbsentFields(t *testing.T) {
	m := Meta{
		Version:   1,
		Kind:      KindManifest,
		MediaType: oci.MediaTypeOCIManifest,
		Digest:    oci.Digest("sha256:" + hexA),
		Size:      1234,
		CreatedAt: time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC),
		Stats:     Stats{TotalBytes: 1, LogicalBytes: 2, DedupedBytes: 3, DiskBytes: 4},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, absent := range []string{`"artifactType"`, `"subject"`, `"annotations"`} {
		if strings.Contains(s, absent) {
			t.Errorf("JSON contains %s, want omitted: %s", absent, s)
		}
	}
	for _, present := range []string{
		`"version":1`, `"kind":"manifest"`, `"mediaType":"` + oci.MediaTypeOCIManifest + `"`,
		`"digest":"sha256:` + hexA + `"`, `"size":1234`, `"createdAt":"2026-09-03T18:00:00Z"`,
		`"stats":{"totalBytes":1,"logicalBytes":2,"dedupedBytes":3,"diskBytes":4}`,
	} {
		if !strings.Contains(s, present) {
			t.Errorf("JSON lacks %s: %s", present, s)
		}
	}
}

func TestMetaJSONRoundTripWithSubject(t *testing.T) {
	m := Meta{
		Version:      1,
		Kind:         KindIndex,
		MediaType:    oci.MediaTypeOCIIndex,
		Digest:       oci.Digest("sha256:" + hexB),
		Size:         99,
		ArtifactType: "application/vnd.example+type",
		Subject:      &oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: oci.Digest("sha256:" + hexA), Size: 7},
		Annotations:  map[string]string{"org.opencontainers.image.created": "x"},
		CreatedAt:    time.Now().UTC(),
		Stats:        Stats{TotalBytes: 10, LogicalBytes: 20, DedupedBytes: 5, DiskBytes: 15},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Meta
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindIndex || back.MediaType != m.MediaType || back.Digest != m.Digest || back.Size != 99 ||
		back.ArtifactType != m.ArtifactType || back.Stats != m.Stats || !back.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v vs %+v", back, m)
	}
	if back.Subject == nil || back.Subject.MediaType != m.Subject.MediaType || back.Subject.Digest != m.Subject.Digest || back.Subject.Size != m.Subject.Size || back.Subject.ArtifactType != m.Subject.ArtifactType {
		t.Fatalf("subject round trip mismatch: %+v", back.Subject)
	}
	if back.Annotations["org.opencontainers.image.created"] != "x" {
		t.Fatalf("annotations round trip mismatch: %v", back.Annotations)
	}
}

func TestLayoutNames(t *testing.T) {
	// The image root's entries must be added in byte order; pin the order the
	// builder relies on.
	names := []string{BlobsDir, ManifestFile, ManifestsDir, MetaFile}
	for i := 1; i < len(names); i++ {
		if !(names[i-1] < names[i]) {
			t.Fatalf("root entry names are not in byte order: %q >= %q", names[i-1], names[i])
		}
	}
}
