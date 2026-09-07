package browse

import (
	"bytes"
	"strings"
	"testing"

	tarprism "github.com/draganm/tar-prism"

	"github.com/amber-store/oci-amber/store"
)

func TestBlobRootListsPrismParts(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	rows := mustList(t, &blobRootNode{st: f.st, bl: bl})
	assertNames(t, rows, "blobs", "comp.json", "meta.json", "recipe.bin", "recipe.json")

	blobs := rows[0]
	if !blobs.IsDir || blobs.Detail != "5 files" {
		t.Errorf("blobs row = %+v, want a directory with detail %q", blobs, "5 files")
	}
	if _, ok := blobs.Child.(*prismBlobsNode); !ok {
		t.Errorf("blobs Child is %T, want *prismBlobsNode", blobs.Child)
	}
	meta := rowNamed(t, rows, "meta.json")
	if !meta.HasSize || meta.Size == 0 || meta.Detail != "blob metadata" {
		t.Errorf("meta.json row = %+v", meta)
	}
	data := readAll(t, meta)
	if !bytes.Contains(data, []byte(`"kind": "prism"`)) {
		t.Errorf("meta.json content: %s", data)
	}
	if got := rowNamed(t, rows, "recipe.bin"); got.Detail != "tar recipe: every byte that is not file content" {
		t.Errorf("recipe.bin detail %q", got.Detail)
	}
}

func TestBlobRootListsRawParts(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.rawLayer)
	rows := mustList(t, &blobRootNode{st: f.st, bl: bl})
	assertNames(t, rows, "meta.json", "raw")
	raw := rows[1]
	if raw.Size != 4096 || raw.Detail != "the blob's bytes, verbatim" {
		t.Errorf("raw row = %+v", raw)
	}
	if len(readAll(t, raw)) != 4096 {
		t.Error("raw content length")
	}
}

func TestPrismBlobsAnnotatedWithTarEntries(t *testing.T) {
	f := newFixture(t)
	bl := f.openBlob(f.layerA)
	dir := f.lookupKey(bl.Root(), tarprism.BlobsDir)
	rows := mustList(t, &prismBlobsNode{st: f.st, bl: bl, dir: dir})
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5: %v", len(rows), rowNames(rows))
	}
	byEntry := map[string]Row{}
	for _, r := range rows {
		if !strings.HasPrefix(r.Name, "0000") {
			t.Errorf("row name %q is not a blob number", r.Name)
		}
		byEntry[r.Detail] = r
	}
	for _, want := range []string{"bin/app", "etc/os-release", "etc/config.json", "usr/bin/tool.sh", "var/cache/x"} {
		if _, ok := byEntry[want]; !ok {
			t.Errorf("no row annotated %q", want)
		}
	}
	app := byEntry["bin/app"]
	if app.Size != int64(len(f.bigBinary)) {
		t.Errorf("bin/app size %d, want %d", app.Size, len(f.bigBinary))
	}
	if !bytes.Equal(readAll(t, app), f.bigBinary) {
		t.Error("bin/app content differs")
	}
	o := app.Child.(Opener)
	file, err := o.Open()
	if err != nil {
		t.Fatal(err)
	}
	if file.Labels[0] != (KV{"file", "bin/app"}) || file.Labels[1] != (KV{"blob", shortRef(f.layerA)}) {
		t.Errorf("labels %v", file.Labels)
	}
	if got := rowNamed(t, rows, app.Name).Info; len(got) < 5 || got[4] != (KV{"tar entry", "bin/app"}) {
		t.Errorf("info %v", got)
	}
}

func TestAmberDirShowsRawRootfs(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	root, ok := im.Rootfs()
	if !ok {
		t.Fatal("v1 has no rootfs")
	}
	rows := mustList(t, &amberDirNode{st: f.st, name: "rootfs", dir: root})
	assertNames(t, rows, "bin", "etc", "usr") // var/ was whited out by layer B

	etc := childList(t, rows, "etc")
	assertNames(t, etc, "abs", "config.json", "dangling", "hostname", "link-to-os", "os-release")
	link := rowNamed(t, etc, "link-to-os")
	if link.Meta == nil || link.Meta.Target != "os-release" || link.Child != nil {
		t.Errorf("symlink row = %+v; want target os-release and no Child", link)
	}
	if link.Meta.Mtime.Year() != 2026 {
		t.Errorf("mtime %v", link.Meta.Mtime)
	}
	os := rowNamed(t, etc, "os-release")
	if os.Meta.Mode&0o777 != 0o644 || os.Meta.Mode&store.TypeMask != store.TypeReg {
		t.Errorf("os-release mode %o", os.Meta.Mode)
	}
	if got := string(readAll(t, os)); !strings.HasPrefix(got, "PRETTY_NAME=") {
		t.Errorf("os-release content %q", got)
	}
	if rowNamed(t, etc, "abs").Meta.Target != "/etc/os-release" {
		t.Error("absolute symlink target")
	}
}

func TestFileNodeRefusesDirectoryKeys(t *testing.T) {
	f := newFixture(t)
	im := f.openImage("library/app", "v1")
	n := &fileNode{st: f.st, name: "root", key: im.Root()}
	if _, err := n.Open(); err == nil || !strings.Contains(err.Error(), "not file content") {
		t.Fatalf("Open on a directory key: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := plural(1, "tag"); got != "1 tag" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(1234, "file"); got != "1,234 files" {
		t.Errorf("plural(1234) = %q", got)
	}
	if got := shortRef("sha256:4f7c9a1e0000000000000000000000000000000000000000000000000000abcd"); got != "sha256:4f7c9a1e" {
		t.Errorf("shortRef = %q", got)
	}
}
