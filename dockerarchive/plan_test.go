package dockerarchive

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/dockerarchive/archivetest"
	"github.com/amber-store/oci-amber/oci"
)

const gzipLayer = "application/vnd.oci.image.layer.v1.tar+gzip"

// busyboxLike builds the observed shape: a top index whose children are a
// present amd64 manifest, its attestation manifest, and two absent
// platforms; manifest.json names the amd64 image.
func busyboxLike(t *testing.T, tags ...string) (*archivetest.Builder, oci.Descriptor, oci.Descriptor, oci.Descriptor) {
	t.Helper()
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("gzip layer bytes")
	img := b.AddImage(config, []archivetest.Layer{{MediaType: gzipLayer, Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, map[string]string{"org.opencontainers.image.version": "1.37"})
	att := b.AddImage(archivetest.Attestation(img))
	idx := b.AddIndex([]oci.Descriptor{
		img,
		att,
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}),
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}),
	}, map[string]string{"keep": "me"})
	b.Top(idx)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: tags, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	return b, img, att, idx
}

func TestPlanPrunesIndexAndNamesEntry(t *testing.T) {
	b, img, att, idx := busyboxLike(t, "registry.example.ch/library/busybox:1.37", "busybox:latest")
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("entries = %+v", p.Entries)
	}
	e := p.Entries[0]
	if !e.IsIndex || e.Platforms != 1 || e.Attestations != 1 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Digest == idx.Digest {
		t.Fatal("pruned index must have a new digest")
	}
	wantNames := []Name{{"library/busybox", "1.37"}, {"busybox", "latest"}}
	if len(e.Names) != 2 || e.Names[0] != wantNames[0] || e.Names[1] != wantNames[1] {
		t.Fatalf("names = %v, want %v", e.Names, wantNames)
	}
	// Publish order: the two children, then the pruned index.
	if len(e.Manifests) != 3 || e.Manifests[2] != e.Digest {
		t.Fatalf("publish order = %v", e.Manifests)
	}
	if e.Manifests[0] != img.Digest || e.Manifests[1] != att.Digest {
		t.Fatalf("children order = %v, want %s, %s", e.Manifests[:2], img.Digest, att.Digest)
	}
	// The synthesized index keeps every other field and only the present children.
	var top *PlanManifest
	for i := range p.Manifests {
		if p.Manifests[i].Digest == e.Digest {
			top = &p.Manifests[i]
		}
	}
	if top == nil || !top.Synthesized || !top.IsIndex || top.MediaType != oci.MediaTypeOCIIndex {
		t.Fatalf("top manifest = %+v", top)
	}
	var pruned struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Manifests     []oci.Descriptor  `json:"manifests"`
		Annotations   map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(top.Body, &pruned); err != nil {
		t.Fatal(err)
	}
	if pruned.SchemaVersion != 2 || pruned.Annotations["keep"] != "me" || len(pruned.Manifests) != 2 {
		t.Fatalf("pruned index = %s", top.Body)
	}
	if pruned.Manifests[0].Digest != img.Digest || pruned.Manifests[0].Annotations["org.opencontainers.image.version"] != "1.37" || pruned.Manifests[1].Digest != att.Digest {
		t.Fatalf("pruned children = %+v", pruned.Manifests)
	}
	if oci.DigestOfBytes(top.Body) != e.Digest {
		t.Fatal("entry digest is not the digest of the synthesized body")
	}
	// Blobs: config, layer, attestation config, in-toto payload; unique, first use.
	if len(p.Blobs) != 4 {
		t.Fatalf("blobs = %+v", p.Blobs)
	}
	if p.Blobs[1].MediaType != gzipLayer || p.Blobs[1].Present {
		t.Fatalf("layer blob = %+v", p.Blobs[1])
	}
	if len(p.Manifests) != 3 {
		t.Fatalf("manifests = %d, want 3", len(p.Manifests))
	}
	for _, pm := range p.Manifests {
		if want := pm.Digest == att.Digest; pm.Attestation != want {
			t.Fatalf("manifest %s: Attestation = %v, want %v", pm.Digest, pm.Attestation, want)
		}
	}
}

func TestPlanMarksPresentBlobs(t *testing.T) {
	b, _, _, _ := busyboxLike(t, "busybox:1.37")
	a := openBuilder(t, b)
	layer := oci.DigestOfBytes([]byte("gzip layer bytes"))
	p, err := a.Plan(PlanOptions{Present: func(d oci.Digest) (bool, error) { return d == layer, nil }})
	if err != nil {
		t.Fatal(err)
	}
	present := 0
	for _, bl := range p.Blobs {
		if bl.Present {
			present++
			if bl.Digest != layer {
				t.Fatalf("wrong blob marked present: %s", bl.Digest)
			}
		}
	}
	if present != 1 {
		t.Fatalf("present = %d, want 1", present)
	}
}

func TestPlanNameOverride(t *testing.T) {
	b, _, _, _ := busyboxLike(t, "busybox:1.37")
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{Names: []string{"mirror/busybox:1.37", "mirror/busybox:stable"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Entries[0].Names; len(got) != 2 || got[0] != (Name{"mirror/busybox", "1.37"}) || got[1] != (Name{"mirror/busybox", "stable"}) {
		t.Fatalf("names = %v", got)
	}
	if _, err := a.Plan(PlanOptions{Names: []string{"not valid"}}); err == nil {
		t.Fatal("invalid --name accepted")
	}
}

func TestPlanNoTagsIsAnError(t *testing.T) {
	b, _, _, _ := busyboxLike(t) // RepoTags empty: docker save by image id
	a := openBuilder(t, b)
	_, err := a.Plan(PlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want a hint about --name", err)
	}
}

func TestPlanSameImageListedTwice(t *testing.T) {
	// docker save busybox:1.37 busybox:latest, when both tags name the same
	// image, writes two index.json descriptors with the same digest and one
	// manifest.json entry carrying both RepoTags.
	b, _, _, idx := busyboxLike(t, "busybox:1.37", "busybox:latest")
	b.Top(idx, idx)
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1 for the doubly-listed image", p.Entries)
	}
	want := []Name{{"busybox", "1.37"}, {"busybox", "latest"}}
	if got := p.Entries[0].Names; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestPlanAmbiguousRepoTagsIsAnError(t *testing.T) {
	// Two distinct top-level indexes (distinguished by an annotation, since
	// otherwise pruning the absent platform out of each would leave them
	// byte-identical) share the same present platform manifest, so its
	// config is reachable from both roots. manifest.json's one RepoTags
	// entry then cannot say which root it names.
	b := archivetest.New()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("gzip layer bytes")
	img := b.AddImage(config, []archivetest.Layer{{MediaType: gzipLayer, Data: layer}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	idx1 := b.AddIndex([]oci.Descriptor{
		img,
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}),
	}, map[string]string{"idx": "1"})
	idx2 := b.AddIndex([]oci.Descriptor{
		img,
		archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}),
	}, map[string]string{"idx": "2"})
	b.Top(idx1, idx2)
	b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"busybox:1.37"}, Layers: []oci.Digest{oci.DigestOfBytes(layer)}})
	a := openBuilder(t, b)
	if _, err := a.Plan(PlanOptions{}); err == nil || !strings.Contains(err.Error(), "share config") {
		t.Fatalf("err = %v, want an ambiguous RepoTags error", err)
	}
	// --name does not rescue this: it only applies to a single-image
	// archive, and this archive holds two roots. The fix is to save the
	// images separately.
	if _, err := a.Plan(PlanOptions{Names: []string{"mirror/busybox:1.37"}}); err == nil || !strings.Contains(err.Error(), "single-image") {
		t.Fatalf("--name on a two-root archive: %v, want the single-image error", err)
	}
}

func TestPlanMultiImageArchive(t *testing.T) {
	b := archivetest.New()
	cfgA, cfgB := []byte(`{"os":"linux","a":1}`), []byte(`{"os":"linux","b":1}`)
	shared := []byte("shared base layer")
	imgA := b.AddImage(cfgA, []archivetest.Layer{{MediaType: gzipLayer, Data: shared}, {MediaType: gzipLayer, Data: []byte("only a")}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	imgB := b.AddImage(cfgB, []archivetest.Layer{{MediaType: gzipLayer, Data: shared}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	b.Top(imgA, imgB) // manifests directly at the top, as the graph driver writes them
	b.Legacy(
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(cfgA), RepoTags: []string{"a:1"}, Layers: []oci.Digest{oci.DigestOfBytes(shared), oci.DigestOfBytes([]byte("only a"))}},
		archivetest.LegacyEntry{Config: oci.DigestOfBytes(cfgB), RepoTags: []string{"b:1"}, Layers: []oci.Digest{oci.DigestOfBytes(shared)}},
	)
	a := openBuilder(t, b)
	p, err := a.Plan(PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 || p.Entries[0].IsIndex || p.Entries[0].Names[0] != (Name{"a", "1"}) || p.Entries[1].Names[0] != (Name{"b", "1"}) {
		t.Fatalf("entries = %+v", p.Entries)
	}
	if len(p.Entries[0].Manifests) != 1 || p.Entries[0].Manifests[0] != imgA.Digest || p.Entries[1].Manifests[0] != imgB.Digest {
		t.Fatalf("manifest lists = %v / %v", p.Entries[0].Manifests, p.Entries[1].Manifests)
	}
	if len(p.Blobs) != 4 { // cfgA, shared, only a, cfgB; shared once
		t.Fatalf("blobs = %+v", p.Blobs)
	}
	if _, err := a.Plan(PlanOptions{Names: []string{"x:1"}}); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("--name on a two-image archive: %v", err)
	}
}

func TestPlanErrors(t *testing.T) {
	t.Run("all children absent", func(t *testing.T) {
		b := archivetest.New()
		idx := b.AddIndex([]oci.Descriptor{archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
		b.Top(idx)
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil || !strings.Contains(err.Error(), "no child") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("top-level entry missing", func(t *testing.T) {
		b := archivetest.New()
		b.Top(archivetest.AbsentManifest(oci.Platform{OS: "linux", Architecture: "amd64"}))
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("partial manifest", func(t *testing.T) {
		b := archivetest.New()
		config := []byte(`{"os":"linux"}`)
		missing := oci.DigestOfBytes([]byte("never added"))
		m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeOCIManifest,
			Config: &oci.Descriptor{MediaType: oci.MediaTypeOCIConfig, Digest: b.AddBlob(config), Size: int64(len(config))},
			Layers: []oci.Descriptor{{MediaType: gzipLayer, Digest: missing, Size: 11}}}
		body, _ := json.Marshal(m)
		d := b.AddBlob(body)
		b.Top(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: d, Size: int64(len(body))})
		b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes(config), RepoTags: []string{"x:1"}})
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("legacy config matches nothing", func(t *testing.T) {
		b, _, _, _ := busyboxLike(t, "busybox:1.37")
		b.Legacy(archivetest.LegacyEntry{Config: oci.DigestOfBytes([]byte("other")), RepoTags: []string{"x:1"}})
		a := openBuilder(t, b)
		if _, err := a.Plan(PlanOptions{}); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("present callback error", func(t *testing.T) {
		b, _, _, _ := busyboxLike(t, "busybox:1.37")
		a := openBuilder(t, b)
		boom := func(oci.Digest) (bool, error) { return false, strings.NewReader("").UnreadRune() }
		if _, err := a.Plan(PlanOptions{Present: boom}); err == nil {
			t.Fatal("expected the callback's error")
		}
	})
}
