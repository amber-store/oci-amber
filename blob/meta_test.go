package blob

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
)

func TestRefName(t *testing.T) {
	d := oci.DigestOfBytes([]byte("x"))
	if got, want := RefName(d), "oci/blob/"+d.String(); got != want {
		t.Fatalf("RefName = %q, want %q", got, want)
	}
}

func TestMetaRoundTripPrism(t *testing.T) {
	in := Meta{
		Version:          MetaVersion,
		Digest:           oci.DigestOfBytes([]byte("layer")),
		Size:             5,
		Kind:             KindPrism,
		Format:           "gzip",
		DiffID:           oci.DigestOfBytes([]byte("tar")),
		UncompressedSize: 3,
		Entries:          2,
		Engine:           "gnu-gzip",
		EngineVersion:    "1.14",
		UploadedAt:       time.Date(2026, 9, 3, 18, 0, 0, 123456789, time.UTC),
		Stats:            store.Stats{LogicalBytes: 10, NewLogicalBytes: 4, DedupedBytes: 6, DiskBytes: 3, ObjectsNew: 1, ObjectsDeduped: 2},
	}
	b, err := encodeMeta(in)
	if err != nil {
		t.Fatalf("encodeMeta: %v", err)
	}
	s := string(b)
	if !strings.HasSuffix(s, "}\n") {
		t.Fatalf("meta must be indented JSON ending in a newline: %q", s)
	}
	if strings.Contains(s, "rawReason") {
		t.Fatalf("prism meta must omit rawReason: %s", s)
	}
	for _, want := range []string{
		`"version": 1`, `"kind": "prism"`, `"format": "gzip"`,
		`"digest": "` + in.Digest.String() + `"`, `"size": 5`,
		`"diffId": "` + in.DiffID.String() + `"`, `"uncompressedSize": 3`, `"entries": 2`,
		`"engine": "gnu-gzip"`, `"engineVersion": "1.14"`,
		`"uploadedAt": "2026-09-03T18:00:00.123456789Z"`,
		`"logicalBytes": 10`, `"newLogicalBytes": 4`, `"dedupedBytes": 6`, `"diskBytes": 3`, `"objectsNew": 1`, `"objectsDeduped": 2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("meta JSON lacks %s:\n%s", want, s)
		}
	}
	out, err := decodeMeta(b)
	if err != nil {
		t.Fatalf("decodeMeta: %v", err)
	}
	metaEqual(t, in, out)
}

func TestMetaRawOmitsPrismFields(t *testing.T) {
	in := Meta{Version: MetaVersion, Digest: oci.DigestOfBytes(nil), Kind: KindRaw, Format: "none", RawReason: ReasonNotTar, UploadedAt: time.Now().UTC()}
	b, err := encodeMeta(in)
	if err != nil {
		t.Fatalf("encodeMeta: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"rawReason": "not-tar"`) {
		t.Fatalf("raw meta must carry rawReason: %s", s)
	}
	for _, absent := range []string{"diffId", "uncompressedSize", "entries", "engine", "engineVersion"} {
		if strings.Contains(s, absent) {
			t.Errorf("raw meta must omit %s: %s", absent, s)
		}
	}
	out, err := decodeMeta(b)
	if err != nil {
		t.Fatalf("decodeMeta: %v", err)
	}
	metaEqual(t, in, out)
}

func TestDecodeMetaRejectsOtherVersions(t *testing.T) {
	if _, err := decodeMeta([]byte(`{"version":2,"digest":"sha256:00"}`)); err == nil {
		t.Fatal("version 2 must be rejected")
	}
	if _, err := decodeMeta([]byte(`not json`)); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

// metaEqual compares two Meta values, comparing UploadedAt with Equal so a
// value that went through JSON compares equal to its source.
func metaEqual(t *testing.T, want, got Meta) {
	t.Helper()
	if !want.UploadedAt.Equal(got.UploadedAt) {
		t.Fatalf("uploadedAt: want %v, got %v", want.UploadedAt, got.UploadedAt)
	}
	want.UploadedAt, got.UploadedAt = time.Time{}, time.Time{}
	if want != got {
		t.Fatalf("meta mismatch:\n want %+v\n  got %+v", want, got)
	}
}
