package registry

import (
	"encoding/json"
	"testing"

	"github.com/amber-store/oci-amber/rootfs"
)

func TestParseFSPath(t *testing.T) {
	cases := []struct {
		rest string
		want fsRoute
		ok   bool
	}{
		{"library/app:v1/etc/passwd", fsRoute{"library/app", "v1", "etc/passwd"}, true},
		{"library/app:v1/", fsRoute{"library/app", "v1", ""}, true},
		{"library/app:v1", fsRoute{"library/app", "v1", ""}, true},
		{"app:v1/a/b:c/d", fsRoute{"app", "v1", "a/b:c/d"}, true},
		{"library/app@sha256:0123/usr/bin", fsRoute{"library/app", "sha256:0123", "usr/bin"}, true},
		{"app@sha256:0123", fsRoute{"app", "sha256:0123", ""}, true},
		{":v1/x", fsRoute{"", "v1", "x"}, true},
		{"library/app/etc/passwd", fsRoute{}, false},
		{"", fsRoute{}, false},
	}
	for _, c := range cases {
		got, ok := parseFSPath(c.rest)
		if ok != c.ok || got != c.want {
			t.Errorf("parseFSPath(%q) = %+v, %v; want %+v, %v", c.rest, got, ok, c.want, c.ok)
		}
	}
}

func TestMatchesETag(t *testing.T) {
	const etag = `"abc"`
	cases := map[string]bool{"": false, `"abc"`: true, `"xyz"`: false, `"xyz", "abc"`: true, "*": true, `W/"abc"`: true, `abc`: false}
	for header, want := range cases {
		if got := matchesETag(header, etag); got != want {
			t.Errorf("matchesETag(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestFSEntryJSON(t *testing.T) {
	file := fsEntryJSON(rootfs.Entry{Name: "f", Mode: 0o100644, UID: 1, GID: 2, Mtime: 1_700_000_000_000_000_005, Size: 0})
	if b, _ := json.Marshal(file); string(b) != `{"name":"f","type":"file","mode":"0644","uid":1,"gid":2,"mtime":"2023-11-14T22:13:20.000000005Z","size":0}` {
		t.Fatalf("file: %s", b)
	}
	link := fsEntryJSON(rootfs.Entry{Name: "l", Mode: 0o120777, Target: "/x"})
	if b, _ := json.Marshal(link); string(b) != `{"name":"l","type":"symlink","mode":"0777","uid":0,"gid":0,"mtime":"1970-01-01T00:00:00Z","target":"/x"}` {
		t.Fatalf("symlink: %s", b)
	}
	dev := fsEntryJSON(rootfs.Entry{Name: "null", Mode: 0o020666, Rdev: [2]uint64{1, 0}})
	if b, _ := json.Marshal(dev); string(b) != `{"name":"null","type":"char","mode":"0666","uid":0,"gid":0,"mtime":"1970-01-01T00:00:00Z","major":1,"minor":0}` {
		t.Fatalf("char: %s", b)
	}
	dir := fsEntryJSON(rootfs.Entry{Name: "d", Mode: 0o041777})
	if b, _ := json.Marshal(dir); string(b) != `{"name":"d","type":"dir","mode":"1777","uid":0,"gid":0,"mtime":"1970-01-01T00:00:00Z"}` {
		t.Fatalf("dir: %s", b)
	}
}
