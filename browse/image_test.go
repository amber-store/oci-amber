package browse

import (
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

func TestReposListsEveryRepository(t *testing.T) {
	f := newFixture(t)
	rows := mustList(t, f.b.rootNode())
	assertNames(t, rows, "library/app", "library/app/sub", "tools/rawimg")
	if rows[0].Detail != "2 tags · 4 manifests" {
		t.Errorf("library/app detail %q", rows[0].Detail)
	}
	if rows[1].Detail != "1 tag · 1 manifest" {
		t.Errorf("library/app/sub detail %q", rows[1].Detail)
	}
	if _, ok := rows[0].Child.(*repoNode); !ok || !rows[0].IsDir {
		t.Errorf("repository row = %+v", rows[0])
	}
}

func TestRepoListsTagsThenUntagged(t *testing.T) {
	f := newFixture(t)
	rows := childList(t, mustList(t, f.b.rootNode()), "library/app")
	untagged := []oci.Digest{f.m2, f.m3}
	slices.Sort(untagged)
	assertNames(t, rows, "latest", "v1", shortRef(untagged[0]), shortRef(untagged[1]))

	latest := rows[0]
	if latest.Detail != "index · "+shortRef(f.idx) || !latest.IsDir || !latest.HasSize {
		t.Errorf("latest row = %+v", latest)
	}
	if rows[1].Detail != "manifest · "+shortRef(f.m1) {
		t.Errorf("v1 detail %q", rows[1].Detail)
	}
	if rows[2].Detail != "untagged · manifest" {
		t.Errorf("untagged detail %q", rows[2].Detail)
	}
	root, ok := rows[1].Child.(*imageRootNode)
	if !ok || root.crumb != ":v1" || root.repo != "library/app" {
		t.Fatalf("v1 Child = %#v", rows[1].Child)
	}
	if u := rows[2].Child.(*imageRootNode); u.crumb != "@"+shortRef(untagged[0]) {
		t.Errorf("untagged crumb %q", u.crumb)
	}

	raw := childList(t, mustList(t, f.b.rootNode()), "tools/rawimg")
	if raw[0].Detail != "manifest · "+shortRef(f.rawM)+" · rootfs unavailable" {
		t.Errorf("rawimg detail %q", raw[0].Detail)
	}
}

func TestImageRootRows(t *testing.T) {
	f := newFixture(t)
	root, err := f.b.imageNode("library/app", "v1")
	if err != nil {
		t.Fatal(err)
	}
	rows := mustList(t, root)
	assertNames(t, rows, "blobs", "manifest", "meta.json", "rootfs")
	if rows[0].Detail != "3 blobs" || !rows[0].IsDir {
		t.Errorf("blobs row = %+v", rows[0])
	}
	if rows[1].Detail != oci.MediaTypeOCIManifest || !rows[1].HasSize {
		t.Errorf("manifest row = %+v", rows[1])
	}
	if !strings.HasPrefix(rows[3].Detail, "ok · ") || !strings.HasSuffix(rows[3].Detail, " entries") {
		t.Errorf("rootfs detail %q", rows[3].Detail)
	}
	if _, ok := rows[3].Child.(*amberDirNode); !ok {
		t.Errorf("rootfs Child is %T", rows[3].Child)
	}
	if got := string(readAll(t, rows[1])); !strings.Contains(got, `"schemaVersion":2`) {
		t.Errorf("manifest bytes: %s", got)
	}

	idx, err := f.b.imageNode("library/app", "latest")
	if err != nil {
		t.Fatal(err)
	}
	rows = mustList(t, idx)
	assertNames(t, rows, "blobs", "manifest", "manifests", "meta.json")
	if rows[0].Detail != "0 blobs" || rows[2].Detail != "2 child manifests" {
		t.Errorf("index rows: %q, %q", rows[0].Detail, rows[2].Detail)
	}

	if _, err := f.b.imageNode("library/app", "nope"); err == nil {
		t.Error("unknown tag must fail")
	}
	byDigest, err := f.b.imageNode("library/app", f.m1.String())
	if err != nil || byDigest.crumb != "@"+shortRef(f.m1) {
		t.Errorf("by digest: %v, crumb %q", err, byDigest.crumb)
	}
}

func TestImageBlobsInManifestOrder(t *testing.T) {
	f := newFixture(t)
	root, _ := f.b.imageNode("library/app", "v1")
	rows := childList(t, mustList(t, root), "blobs")
	assertNames(t, rows, shortRef(f.conf), shortRef(f.layerA), shortRef(f.layerB))
	if rows[0].Detail != "config · raw not-tar" {
		t.Errorf("config detail %q", rows[0].Detail)
	}
	if !strings.HasPrefix(rows[1].Detail, "layer 1/2 · prism gzip go-flate · 5 files · ") || !strings.HasSuffix(rows[1].Detail, " uncompressed") {
		t.Errorf("layer A detail %q", rows[1].Detail)
	}
	if !strings.HasPrefix(rows[2].Detail, "layer 2/2 · prism none · ") {
		t.Errorf("layer B detail %q", rows[2].Detail)
	}
	if rows[1].Size != f.sizes[f.layerA] || !rows[1].IsDir {
		t.Errorf("layer A row = %+v", rows[1])
	}
	if _, ok := rows[1].Child.(*blobRootNode); !ok {
		t.Errorf("layer A Child is %T", rows[1].Child)
	}
	if rows[1].Info[0] != (KV{"digest", f.layerA.String()}) {
		t.Errorf("info %v", rows[1].Info)
	}
}

func TestImageManifestsShowPlatforms(t *testing.T) {
	f := newFixture(t)
	idx, _ := f.b.imageNode("library/app", "latest")
	rows := childList(t, mustList(t, idx), "manifests")
	assertNames(t, rows, shortRef(f.m1), shortRef(f.m2))
	if rows[0].Detail != "linux/amd64 · manifest" || rows[1].Detail != "linux/arm64 · manifest" {
		t.Errorf("details %q, %q", rows[0].Detail, rows[1].Detail)
	}
	child, ok := rows[1].Child.(*imageRootNode)
	if !ok || child.crumb != "@"+shortRef(f.m2) {
		t.Fatalf("child = %#v", rows[1].Child)
	}
	assertNames(t, mustList(t, child), "blobs", "manifest", "meta.json", "rootfs")
}

func TestRepoNodeFromNames(t *testing.T) {
	f := newFixture(t)
	rn, err := f.b.repoNode("library/app/sub")
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, mustList(t, rn), "x")
	if _, err := f.b.repoNode("nobody/here"); err == nil {
		t.Error("unknown repository must fail")
	}
}
