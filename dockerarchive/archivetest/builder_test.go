package archivetest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/amber-store/oci-amber/oci"
)

func TestBuilderWritesDockerSaveShape(t *testing.T) {
	b := New()
	img := b.AddImage([]byte(`{"os":"linux"}`), []Layer{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Data: []byte("layer")}}, &oci.Platform{OS: "linux", Architecture: "amd64"}, nil)
	idx := b.AddIndex([]oci.Descriptor{img, AbsentManifest(oci.Platform{OS: "linux", Architecture: "arm64"})}, nil)
	b.Top(idx)
	b.Legacy(LegacyEntry{Config: oci.DigestOfBytes([]byte(`{"os":"linux"}`)), RepoTags: []string{"app:v1"}, Layers: []oci.Digest{oci.DigestOfBytes([]byte("layer"))}})

	var names []string
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b.Bytes()))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			files[h.Name] = data
		}
	}
	// blobs: config, layer, manifest, index = 4; plus the two dirs and three files.
	if len(names) != 2+4+3 {
		t.Fatalf("entries: %v", names)
	}
	if names[0] != "blobs/" || names[1] != "blobs/sha256/" {
		t.Fatalf("directories first: %v", names[:2])
	}
	last := names[len(names)-3:]
	if last[0] != "index.json" || last[1] != "manifest.json" || last[2] != "oci-layout" {
		t.Fatalf("trailing files: %v", last)
	}
	var index oci.Manifest
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != idx.Digest {
		t.Fatalf("index.json = %s", files["index.json"])
	}
	var legacy []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0].Config != "blobs/sha256/"+oci.DigestOfBytes([]byte(`{"os":"linux"}`)).Hex() || legacy[0].RepoTags[0] != "app:v1" {
		t.Fatalf("manifest.json = %s", files["manifest.json"])
	}
	if string(files["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Fatalf("oci-layout = %s", files["oci-layout"])
	}
	if _, ok := files["blobs/sha256/"+img.Digest.Hex()]; !ok {
		t.Fatal("image manifest blob missing")
	}
}
