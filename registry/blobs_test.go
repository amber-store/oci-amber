package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"testing"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// putBlob stores data directly through the blob store, bypassing HTTP.
func (e *testEnv) putBlob(t *testing.T, data []byte) oci.Digest {
	t.Helper()
	meta, err := e.blobs.Put(context.Background(), upload.NewMemorySpool(data))
	if err != nil {
		t.Fatalf("blob put: %v", err)
	}
	return meta.Digest
}

func randomBytes(seed uint64, n int) []byte {
	rnd := mathrand.New(mathrand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rnd.Uint64())
	}
	return b
}

// rawFixture is a blob zrecipe classifies as raw: a JSON config, format
// none and not a tar.
func rawFixture() []byte {
	return []byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"]},"rootfs":{"type":"layers","diff_ids":[]}}`)
}

// largeRawFixture is incompressible data, prefixed so it can never be taken
// for a gzip, zstd or tar stream, and large enough to span several
// content-defined chunks.
func largeRawFixture() []byte {
	return append([]byte("RAW:"), randomBytes(3, 200<<10)...)
}

// gzipTarFixture is a gzip layer as Go's compress/gzip writes it, which
// zrecipe reproduces with its go-flate engine, so it is stored as a prism.
func gzipTarFixture(t *testing.T) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	files := []struct {
		name string
		body []byte
	}{
		{"etc/hosts", bytes.Repeat([]byte("127.0.0.1 localhost\n"), 800)},
		{"usr/bin/tool", randomBytes(11, 40<<10)},
		{"var/lib/data.bin", randomBytes(12, 70<<10)},
	}
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func TestBlobHeadAndGet(t *testing.T) {
	e := newTestEnv(t)
	data := rawFixture()
	d := e.putBlob(t, data)
	path := "/v2/library/app/blobs/" + d.String()

	resp := e.do(t, http.MethodHead, path, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(data)))
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	assertHeader(t, resp, "Content-Type", "application/octet-stream")
	assertHeader(t, resp, "Accept-Ranges", "bytes")
	if len(resp.body) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes", len(resp.body))
	}

	resp = e.do(t, http.MethodGet, path, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(data)))
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	assertHeader(t, resp, "Content-Type", "application/octet-stream")
	if !bytes.Equal(resp.body, data) {
		t.Fatalf("GET body differs from pushed bytes")
	}
	if got := oci.DigestOfBytes(resp.body); got != d {
		t.Fatalf("GET body digest %s, want %s", got, d)
	}
}

func TestBlobUnknownAndInvalidDigest(t *testing.T) {
	e := newTestEnv(t)
	unknown := "/v2/library/app/blobs/sha256:" + fakeHex

	resp := e.do(t, http.MethodHead, unknown, nil, nil)
	assertStatus(t, resp, http.StatusNotFound)

	resp = e.do(t, http.MethodGet, unknown, nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUnknown)

	resp = e.do(t, http.MethodDelete, unknown, nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUnknown)

	for _, bad := range []string{"sha256:xyz", "md5:" + fakeHex, "sha512:" + fakeHex + fakeHex, "SHA256:" + fakeHex, fakeHex} {
		resp = e.do(t, http.MethodGet, "/v2/library/app/blobs/"+bad, nil, nil)
		assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	}
}

func TestBlobRangeRaw(t *testing.T) {
	e := newTestEnv(t)
	data := largeRawFixture()
	d := e.putBlob(t, data)
	path := "/v2/library/app/blobs/" + d.String()
	size := len(data)

	resp := e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=2-5"})
	assertStatus(t, resp, http.StatusPartialContent)
	assertHeader(t, resp, "Content-Range", "bytes 2-5/"+strconv.Itoa(size))
	assertHeader(t, resp, "Content-Length", "4")
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	if !bytes.Equal(resp.body, data[2:6]) {
		t.Fatalf("range body = %q, want %q", resp.body, data[2:6])
	}

	// Open-ended range across chunk boundaries.
	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=100000-"})
	assertStatus(t, resp, http.StatusPartialContent)
	assertHeader(t, resp, "Content-Range", "bytes 100000-"+strconv.Itoa(size-1)+"/"+strconv.Itoa(size))
	assertHeader(t, resp, "Content-Length", strconv.Itoa(size-100000))
	if !bytes.Equal(resp.body, data[100000:]) {
		t.Fatal("open-ended range body differs")
	}

	// Suffix range: the last 4 bytes.
	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=-4"})
	assertStatus(t, resp, http.StatusPartialContent)
	assertHeader(t, resp, "Content-Range", "bytes "+strconv.Itoa(size-4)+"-"+strconv.Itoa(size-1)+"/"+strconv.Itoa(size))
	if !bytes.Equal(resp.body, data[size-4:]) {
		t.Fatal("suffix range body differs")
	}

	// Last byte alone; an end beyond the size is clamped.
	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=" + strconv.Itoa(size-1) + "-999999999"})
	assertStatus(t, resp, http.StatusPartialContent)
	assertHeader(t, resp, "Content-Length", "1")
	if !bytes.Equal(resp.body, data[size-1:]) {
		t.Fatal("last-byte range body differs")
	}

	// Unsatisfiable: start at or beyond the size.
	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=" + strconv.Itoa(size) + "-"})
	assertStatus(t, resp, http.StatusRequestedRangeNotSatisfiable)
	assertHeader(t, resp, "Content-Range", "bytes */"+strconv.Itoa(size))

	// Multi-range and malformed ranges get the full body.
	for _, h := range []string{"bytes=0-3,5-6", "bytes=abc", "items=0-3", "bytes=5-2"} {
		resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": h})
		assertStatus(t, resp, http.StatusOK)
		assertHeader(t, resp, "Content-Range", "")
		if !bytes.Equal(resp.body, data) {
			t.Fatalf("Range %q: body differs from full blob", h)
		}
	}

	// HEAD ignores Range.
	resp = e.do(t, http.MethodHead, path, nil, map[string]string{"Range": "bytes=2-5"})
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", strconv.Itoa(size))
}

func TestBlobRangeOnEmptyBlob(t *testing.T) {
	e := newTestEnv(t)
	d := e.putBlob(t, []byte{})
	path := "/v2/library/app/blobs/" + d.String()

	resp := e.do(t, http.MethodGet, path, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", "0")
	if len(resp.body) != 0 {
		t.Fatalf("empty blob GET returned %d bytes", len(resp.body))
	}

	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=0-"})
	assertStatus(t, resp, http.StatusRequestedRangeNotSatisfiable)
	assertHeader(t, resp, "Content-Range", "bytes */0")
}

func TestBlobRangeOnPrismIsFullBody(t *testing.T) {
	e := newTestEnv(t)
	data := gzipTarFixture(t)
	d := e.putBlob(t, data)
	bl, err := e.blobs.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if bl.Meta.Kind != blob.KindPrism {
		t.Fatalf("fixture stored as kind %q (reason %q); a compress/gzip tar must be a prism (Task 8)", bl.Meta.Kind, bl.Meta.RawReason)
	}
	path := "/v2/library/app/blobs/" + d.String()

	resp := e.do(t, http.MethodHead, path, nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Accept-Ranges", "")
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(data)))

	resp = e.do(t, http.MethodGet, path, nil, map[string]string{"Range": "bytes=2-5"})
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Accept-Ranges", "")
	assertHeader(t, resp, "Content-Range", "")
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(data)))
	assertHeader(t, resp, "Docker-Content-Digest", d.String())
	if !bytes.Equal(resp.body, data) {
		t.Fatal("prism GET body differs from pushed bytes")
	}
}

func TestBlobDelete(t *testing.T) {
	e := newTestEnv(t)
	d := e.putBlob(t, rawFixture())
	path := "/v2/library/app/blobs/" + d.String()

	resp := e.do(t, http.MethodDelete, path, nil, nil)
	assertStatus(t, resp, http.StatusAccepted)

	resp = e.do(t, http.MethodHead, path, nil, nil)
	assertStatus(t, resp, http.StatusNotFound)

	resp = e.do(t, http.MethodDelete, path, nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUnknown)
}

func TestBlobMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		resp := e.do(t, m, "/v2/library/app/blobs/sha256:"+fakeHex, []byte("x"), nil)
		assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
		assertHeader(t, resp, "Allow", "HEAD, GET, DELETE")
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		header string
		size   int64
		want   byteRange
		ok     bool
		err    error
	}{
		{"", 10, byteRange{}, false, nil},
		{"bytes=2-5", 10, byteRange{2, 5}, true, nil},
		{"bytes=5-", 10, byteRange{5, 9}, true, nil},
		{"bytes=-3", 10, byteRange{7, 9}, true, nil},
		{"bytes=-30", 10, byteRange{0, 9}, true, nil},
		{"bytes=0-100", 10, byteRange{0, 9}, true, nil},
		{"bytes=0-0", 10, byteRange{0, 0}, true, nil},
		{"bytes=9-9", 10, byteRange{9, 9}, true, nil},
		{"bytes=10-", 10, byteRange{}, false, errUnsatisfiable},
		{"bytes=12-15", 10, byteRange{}, false, errUnsatisfiable},
		{"bytes=-0", 10, byteRange{}, false, errUnsatisfiable},
		{"bytes=0-", 0, byteRange{}, false, errUnsatisfiable},
		{"bytes=-1", 0, byteRange{}, false, errUnsatisfiable},
		{"bytes=1-3,5-6", 10, byteRange{}, false, nil},
		{"bytes=abc", 10, byteRange{}, false, nil},
		{"bytes=5-2", 10, byteRange{}, false, nil},
		{"bytes=-", 10, byteRange{}, false, nil},
		{"items=1-2", 10, byteRange{}, false, nil},
		{"bytes=x-5", 10, byteRange{}, false, nil},
	}
	for _, c := range cases {
		got, ok, err := parseRange(c.header, c.size)
		if got != c.want || ok != c.ok || err != c.err {
			t.Errorf("parseRange(%q, %d) = %+v, %v, %v; want %+v, %v, %v", c.header, c.size, got, ok, err, c.want, c.ok, c.err)
		}
	}
}
