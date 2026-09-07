package browse

import (
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/store"
)

func TestModeString(t *testing.T) {
	for mode, want := range map[uint64]string{
		0o100644: "-rw-r--r--",
		0o040755: "drwxr-xr-x",
		0o120777: "lrwxrwxrwx",
		0o104755: "-rwsr-xr-x",
		0o102755: "-rwxr-sr-x",
		0o041777: "drwxrwxrwt",
		0o100000: "----------",
		0o020660: "crw-rw----",
		0o060660: "brw-rw----",
		0o010600: "prw-------",
		0o140755: "srwxr-xr-x",
	} {
		if got := modeString(mode); got != want {
			t.Errorf("modeString(%o) = %q, want %q", mode, got, want)
		}
	}
}

func TestBreadcrumb(t *testing.T) {
	crumbs := []string{"library/app", ":v1", "storage", "blobs", "sha256:4f7c9a1e"}
	if got := breadcrumb(crumbs, 80); got != "library/app › :v1 › storage › blobs › sha256:4f7c9a1e" {
		t.Errorf("wide: %q", got)
	}
	got := breadcrumb(crumbs, 30)
	if !strings.HasPrefix(got, "… › ") || !strings.HasSuffix(got, "sha256:4f7c9a1e") || len([]rune(got)) > 30 {
		t.Errorf("narrow: %q", got)
	}
	if got := breadcrumb([]string{"oci-amber"}, 5); got != "oci-a" {
		t.Errorf("single crumb wider than the terminal: %q", got)
	}
}

func TestRenderListStorageRows(t *testing.T) {
	v := listView{
		Crumbs: []string{"library/app", ":v1", "storage"},
		Rows: []Row{
			{Name: "blobs", Detail: "3 blobs", IsDir: true},
			{Name: "manifest", Detail: "application/vnd.oci.image.manifest.v1+json", Size: 1234, HasSize: true},
			{Name: "meta.json", Detail: "kind, digest, stats and rootfs status", Size: 512, HasSize: true},
		},
		Cursor: 1, Count: 3, Total: 3,
		Hints: "enter open · q quit",
	}
	out := RenderList(v, 80, 9)
	lines := strings.Split(out, "\n")
	if len(lines) != 9 {
		t.Fatalf("%d lines, want 9:\n%s", len(lines), out)
	}
	if lines[0] != "library/app › :v1 › storage" {
		t.Errorf("breadcrumb %q", lines[0])
	}
	if lines[1] != strings.Repeat("─", 80) || lines[7] != lines[1] {
		t.Errorf("rules: %q / %q", lines[1], lines[7])
	}
	if !strings.HasPrefix(lines[2], "  blobs/") || !strings.Contains(lines[2], "3 blobs") {
		t.Errorf("row 0 %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "▸ manifest ") || !strings.HasSuffix(lines[3], "  1.2 KiB") {
		t.Errorf("cursor row %q", lines[3])
	}
	if !strings.HasSuffix(lines[4], "512 B") {
		t.Errorf("row 2 %q", lines[4])
	}
	if lines[5] != "" || lines[6] != "" {
		t.Errorf("padding lines %q %q", lines[5], lines[6])
	}
	if lines[8] != "3 rows  ·  enter open · q quit" {
		t.Errorf("status %q", lines[8])
	}
	for _, l := range lines {
		if len([]rune(l)) > 80 {
			t.Errorf("line wider than 80: %q", l)
		}
	}
}

func TestRenderListMetaRows(t *testing.T) {
	mtime := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	v := listView{
		Crumbs: []string{"library/app", ":v1", "filesystem", "etc"},
		Rows: []Row{
			{Name: "os-release", Size: 258, HasSize: true, Meta: &RowMeta{Mode: store.TypeReg | 0o644, Mtime: mtime}},
			{Name: "link-to-os", Meta: &RowMeta{Mode: store.TypeLink | 0o777, UID: 1000, GID: 1000, Mtime: mtime, Target: "os-release"}},
			{Name: "rc.d", IsDir: true, Meta: &RowMeta{Mode: store.TypeDir | 0o755, Mtime: mtime}},
		},
		Cursor: 0, Count: 3, Total: 6, Filter: "o",
	}
	lines := strings.Split(RenderList(v, 100, 8), "\n")
	// Columns: mode, owner padded to the widest ("1000:1000", 9 cells), a
	// 10-cell right-aligned size, mtime, name; two spaces between columns.
	// The cursor row is padded to the full width, hence TrimRight.
	if want := "▸ -rw-r--r--  0:0" + strings.Repeat(" ", 6+2+5) + "258 B  2026-09-03 18:00  os-release"; strings.TrimRight(lines[2], " ") != want {
		t.Errorf("file row:\n%q\nwant\n%q", lines[2], want)
	}
	if want := "  lrwxrwxrwx  1000:1000" + strings.Repeat(" ", 2+10+2) + "2026-09-03 18:00  link-to-os -> os-release"; lines[3] != want {
		t.Errorf("symlink row:\n%q\nwant\n%q", lines[3], want)
	}
	if !strings.HasSuffix(lines[4], "  rc.d/") {
		t.Errorf("dir row %q", lines[4])
	}
	if lines[7] != `3 of 6 rows · filter "o"` {
		t.Errorf("status %q", lines[7])
	}
}

func TestRenderListStates(t *testing.T) {
	base := listView{Crumbs: []string{"oci-amber"}}
	if out := RenderList(base, 40, 6); !strings.Contains(out, "(empty)") || !strings.Contains(out, "0 rows") {
		t.Errorf("empty:\n%s", out)
	}
	loading := base
	loading.Loading = true
	if out := RenderList(loading, 40, 6); !strings.Contains(out, "loading…") {
		t.Errorf("loading:\n%s", out)
	}
	filtered := base
	filtered.Filter = "zzz"
	filtered.Total = 4
	if out := RenderList(filtered, 40, 6); !strings.Contains(out, `no rows match "zzz"`) {
		t.Errorf("filtered:\n%s", out)
	}
	status := base
	status.Status = "rootfs: no such path: etc/dangling"
	if out := RenderList(status, 60, 6); !strings.Contains(out, "no such path") {
		t.Errorf("status:\n%s", out)
	}
	input := base
	input.Input = "filter: os"
	if lines := strings.Split(RenderList(input, 40, 6), "\n"); lines[5] != "filter: os" {
		t.Errorf("input replaces the status line: %q", lines[5])
	}
	popup := base
	popup.Rows = []Row{{Name: "x"}}
	popup.Count, popup.Total = 1, 1
	popup.Popup = []KV{{"digest", "sha256:abc"}, {"kind", "manifest"}}
	out := RenderList(popup, 60, 10)
	if !strings.Contains(out, "digest  sha256:abc") || !strings.Contains(out, "kind    manifest") || !strings.Contains(out, "╭") {
		t.Errorf("popup:\n%s", out)
	}
	if strings.Contains(out, "▸ x") {
		t.Error("the popup replaces the rows")
	}
}

func TestRenderViewer(t *testing.T) {
	v := viewerView{
		Crumbs: []string{"library/app", ":v1", "filesystem", "etc", "os-release"},
		Body:   "1 ID=fixture\n2 VERSION_ID=1",
		Status: "text · 24 B · 2 lines · h hex · esc back",
	}
	lines := strings.Split(RenderViewer(v, 60, 7), "\n")
	if len(lines) != 7 || lines[2] != "1 ID=fixture" || lines[3] != "2 VERSION_ID=1" || lines[4] != "" {
		t.Errorf("viewer:\n%s", strings.Join(lines, "\n"))
	}
	if lines[6] != v.Status {
		t.Errorf("status %q", lines[6])
	}
	v.Input = "offset: 0x"
	if lines := strings.Split(RenderViewer(v, 60, 7), "\n"); lines[6] != "offset: 0x" {
		t.Errorf("input %q", lines[6])
	}
}
