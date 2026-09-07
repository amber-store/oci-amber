package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/draganm/oci-amber/dockerarchive"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
	"github.com/draganm/oci-amber/tui"
)

// runSaveApp runs the save command with args and returns the config the
// action received, or the error the app returned.
func runSaveApp(t *testing.T, args ...string) (saveConfig, error) {
	t.Helper()
	var got saveConfig
	called := false
	app := newApp(commands{Save: func(_ context.Context, cfg saveConfig) error {
		called = true
		got = cfg
		return nil
	}})
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.RunContext(context.Background(), append([]string{"oci-amber", "save"}, args...))
	if err == nil && !called {
		t.Fatal("save action was not called")
	}
	return got, err
}

func TestSaveFlags(t *testing.T) {
	clearEnv(t)
	cfg, err := runSaveApp(t, "--store", "/srv/amber", "library/app:v1")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if want := (saveConfig{Store: "/srv/amber", Refs: []string{"library/app:v1"}, Progress: "auto"}); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	if cfg, err := runSaveApp(t, "--store", "/srv/amber", "--progress", "plain", "app:v1"); err != nil || cfg.Progress != "plain" {
		t.Errorf("--progress plain: %+v, %v", cfg, err)
	}
	if _, err := runSaveApp(t, "--store", "/srv/amber", "--progress", "fancy", "app:v1"); err == nil || !strings.Contains(err.Error(), "--progress") {
		t.Errorf("--progress fancy must be rejected: %v", err)
	}
	cfg, err = runSaveApp(t, "--store", "/srv/amber", "-o", "out.tar", "app", "app@sha256:0000000000000000000000000000000000000000000000000000000000000000", "b/c:d")
	if err != nil {
		t.Fatalf("save -o: %v", err)
	}
	if cfg.Output != "out.tar" || len(cfg.Refs) != 3 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg, err := runSaveApp(t, "--store", "/srv/amber", "--output", "x.tar", "app:v1"); err != nil || cfg.Output != "x.tar" {
		t.Errorf("--output: %+v, %v", cfg, err)
	}
	if _, err := runSaveApp(t, "--store", "/srv/amber"); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Errorf("no reference: %v", err)
	}
	for _, bad := range []string{"Not/Valid:v1", "app:", "app@", "app:bad tag", "app@sha256:short", "app@md5:abc"} {
		if _, err := runSaveApp(t, "--store", "/srv/amber", bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if _, err := runSaveApp(t, "app:v1"); err == nil {
		t.Error("--store is required")
	}
	t.Setenv("OCI_AMBER_STORE", "/env/store")
	t.Setenv("OCI_AMBER_PROGRESS", "tui")
	if cfg, err := runSaveApp(t, "app:v1"); err != nil || cfg.Store != "/env/store" || cfg.Progress != "tui" {
		t.Errorf("store and progress from the environment: %+v, %v", cfg, err)
	}
}

// save runs the command over the fixture store into memory and opens the
// archive it wrote.
func save(t *testing.T, cfg saveConfig) ([]byte, *dockerarchive.Archive) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cfg.Stdout, cfg.Stderr = &stdout, &stderr
	if err := runSave(context.Background(), cfg); err != nil {
		t.Fatalf("runSave(%v): %v\n%s", cfg.Refs, err, stderr.String())
	}
	// Progress goes to stderr; a buffer is not a terminal, so plain mode,
	// whose status lines are seconds apart, leaves only the summary.
	if lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "Saved ") {
		t.Errorf("stderr must hold the summary line alone:\n%s", stderr.String())
	}
	path := filepath.Join(t.TempDir(), "saved.tar")
	if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatalf("opening the saved archive: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return stdout.Bytes(), a
}

func TestRunSaveImage(t *testing.T) {
	f := newFixture(t)
	_, a := save(t, saveConfig{Store: f.store, Refs: []string{"demo/app:v1"}})
	if len(a.Index.Manifests) != 1 {
		t.Fatalf("index.json has %d entries", len(a.Index.Manifests))
	}
	d := a.Index.Manifests[0]
	if d.Digest != f.app.Digest || d.MediaType != oci.MediaTypeOCIManifest || d.Size != f.app.Size {
		t.Errorf("index.json entry %+v, want the stored manifest %+v", d, f.app)
	}
	if d.Annotations[dockerarchive.AnnotationRefName] != "v1" || d.Annotations[dockerarchive.AnnotationImageName] != "docker.io/demo/app:v1" {
		t.Errorf("annotations %v", d.Annotations)
	}
	// The config and the recomposed layer are the bytes that were imported.
	for _, want := range [][]byte{f.appConfig, f.appLayer} {
		got, err := a.ReadBlob(oci.DigestOfBytes(want))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("blob %s: %v", oci.DigestOfBytes(want), err)
		}
	}
	if len(a.Legacy) != 1 || !reflect.DeepEqual(a.Legacy[0].RepoTags, []string{"demo/app:v1"}) || a.Legacy[0].Config != "blobs/sha256/"+oci.DigestOfBytes(f.appConfig).Hex() {
		t.Errorf("manifest.json = %+v", a.Legacy)
	}
	// Nothing but the image: the index's blobs are not in the archive.
	if a.Has(f.amd.Digest) || a.Has(f.index.Digest) {
		t.Error("the archive holds blobs of an image that was not saved")
	}
}

func TestRunSaveRepositoryAndDigest(t *testing.T) {
	f := newFixture(t)
	// A bare repository saves every tag, in tag order.
	_, a := save(t, saveConfig{Store: f.store, Refs: []string{"demo/app"}})
	var refs []string
	for _, d := range a.Index.Manifests {
		refs = append(refs, d.Annotations[dockerarchive.AnnotationRefName])
	}
	if !reflect.DeepEqual(refs, []string{"latest", "v1"}) {
		t.Errorf("saved tags %v, want [latest v1]", refs)
	}
	if len(a.Legacy) != 1 || !reflect.DeepEqual(a.Legacy[0].RepoTags, []string{"demo/app:latest", "demo/app:v1"}) {
		t.Errorf("manifest.json = %+v", a.Legacy)
	}
	// A digest reference is saved without names and imports with --name.
	_, a = save(t, saveConfig{Store: f.store, Refs: []string{"demo/app@" + f.app.Digest.String()}})
	if d := a.Index.Manifests[0]; d.Digest != f.app.Digest || d.Annotations != nil {
		t.Errorf("index.json entry %+v", d)
	}
	if len(a.Legacy) != 1 || a.Legacy[0].RepoTags != nil {
		t.Errorf("manifest.json = %+v", a.Legacy)
	}
	if _, err := a.Plan(dockerarchive.PlanOptions{}); err == nil || !strings.Contains(err.Error(), "no RepoTags") {
		t.Errorf("import without --name: %v", err)
	}
}

func TestRunSaveIndex(t *testing.T) {
	f := newFixture(t)
	archive, a := save(t, saveConfig{Store: f.store, Refs: []string{"multi:latest"}, Platform: &oci.Platform{OS: "linux", Architecture: "arm64"}})
	// The whole index: every child, the attestation, every blob, byte for byte.
	for _, d := range []oci.Descriptor{f.index, f.amd, f.arm, f.att} {
		if !a.Has(d.Digest) {
			t.Errorf("manifest %s missing", d.Digest)
		}
	}
	for d, want := range f.blobs {
		if d == oci.DigestOfBytes(f.appConfig) || d == oci.DigestOfBytes(f.appLayer) {
			continue
		}
		if got, err := a.ReadBlob(d); err != nil || !bytes.Equal(got, want) {
			t.Errorf("blob %s: %v", d, err)
		}
	}
	p, err := a.Plan(dockerarchive.PlanOptions{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if e := p.Entries[0]; e.Digest != f.index.Digest || e.Platforms != 2 || e.Attestations != 1 || len(e.Manifests) != 4 {
		t.Errorf("plan entry %+v", e)
	}
	// manifest.json describes the preferred platform's child.
	if len(a.Legacy) != 1 || !strings.HasSuffix(a.Legacy[0].Config, digestOfConfig(t, f, f.arm).Hex()) {
		t.Errorf("manifest.json = %+v, want the arm64 child's config", a.Legacy)
	}
	// Two saves are the same bytes.
	again, _ := save(t, saveConfig{Store: f.store, Refs: []string{"multi:latest"}, Platform: &oci.Platform{OS: "linux", Architecture: "arm64"}})
	if !bytes.Equal(archive, again) {
		t.Error("saving the same image twice gave different archives")
	}
}

// digestOfConfig returns the config digest of the manifest desc in the
// fixture's archive.
func digestOfConfig(t *testing.T, f *fixture, desc oci.Descriptor) oci.Digest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.tar")
	if err := os.WriteFile(path, f.archive, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := dockerarchive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	body, err := a.ReadBlob(desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := oci.ParseManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	return m.Config.Digest
}

// TestRunSaveRoundTrip imports what save wrote into a fresh store: the
// digests are the ones that were saved.
func TestRunSaveRoundTrip(t *testing.T) {
	f := newFixture(t)
	archive, _ := save(t, saveConfig{Store: f.store, Refs: []string{"demo/app:v1", "multi:latest"}})
	tmp := t.TempDir()
	path := filepath.Join(tmp, "saved.tar")
	if err := os.WriteFile(path, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cfg := importConfig{
		Store: filepath.Join(tmp, "store2"), MaxInMemory: 8 << 20, AnalyzeParallelism: 1, AnalyzeTimeout: time.Minute,
		MaxConcurrentFinalize: 1, VerifyRoundTrip: true, LogLevel: slog.LevelWarn,
		Archive: path, Progress: "plain", Stdout: io.Discard, Stderr: &stderr,
	}
	if err := runImport(context.Background(), cfg); err != nil {
		t.Fatalf("importing the saved archive: %v\n%s", err, stderr.String())
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(cfg.Store, store.Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	images := image.New(st, nil, quiet)
	for ref, want := range map[[2]string]oci.Digest{{"demo/app", "v1"}: f.app.Digest, {"multi", "latest"}: f.index.Digest} {
		im, err := images.Open(ref[0], ref[1])
		if err != nil {
			t.Errorf("%s:%s after the round trip: %v", ref[0], ref[1], err)
			continue
		}
		if im.Meta.Digest != want {
			t.Errorf("%s:%s is %s after the round trip, want %s", ref[0], ref[1], im.Meta.Digest, want)
		}
	}
	if _, err := images.Open("demo/app", "latest"); err == nil {
		t.Error("demo/app:latest was not saved and must not come back")
	}
}

func TestRunSaveToFile(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "app.tar")
	var stdout, stderr bytes.Buffer
	if err := runSave(context.Background(), saveConfig{Store: f.store, Refs: []string{"demo/app:v1"}, Output: out, Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatalf("runSave -o: %v", err)
	}
	if stdout.Len() != 0 {
		t.Error("with -o nothing goes to stdout")
	}
	if !strings.Contains(stderr.String(), "Saved demo/app:v1 → "+out+": ") {
		t.Errorf("summary must name the output file:\n%s", stderr.String())
	}
	inMemory, _ := save(t, saveConfig{Store: f.store, Refs: []string{"demo/app:v1"}})
	if onDisk, err := os.ReadFile(out); err != nil || !bytes.Equal(onDisk, inMemory) {
		t.Errorf("file differs from the stdout archive: %v", err)
	}
}

func TestRunSaveUnknownReference(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "nope.tar")
	for _, tc := range []struct {
		refs []string
		want string
	}{
		{[]string{"demo/app:nope"}, "demo/app:nope: not found"},
		{[]string{"nobody"}, "nobody: not found"},
		{[]string{"demo/app:v1", "nobody:x"}, "nobody:x: not found"},
		{[]string{"demo/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"}, "demo/app@sha256:0000000000000000000000000000000000000000000000000000000000000000: not found"},
	} {
		refs := tc.refs
		var stdout bytes.Buffer
		err := runSave(context.Background(), saveConfig{Store: f.store, Refs: refs, Output: out, Stdout: &stdout, Stderr: io.Discard})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%v: err = %v, want %q", refs, err, tc.want)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("%v: the output file was created", refs)
		}
	}
}

func TestRunSaveRefusesMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := runSave(context.Background(), saveConfig{Store: dir, Refs: []string{"a:b"}, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "no oci-amber store at "+dir) {
		t.Fatalf("missing store: %v", err)
	}
}

// TestRunSaveRemovesOutputOnFailure: a failure while writing leaves no
// partial file behind. A cancelled context fails the first blob.
func TestRunSaveRemovesOutputOnFailure(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "partial.tar")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runSave(ctx, saveConfig{Store: f.store, Refs: []string{"demo/app:v1"}, Output: out, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || err.Error() != "save cancelled" {
		t.Fatalf("cancelled save: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("partial output left behind: %v", statErr)
	}
}

// TestRunSaveDuplicateReference: the same image named twice is saved once.
func TestRunSaveDuplicateReference(t *testing.T) {
	f := newFixture(t)
	_, a := save(t, saveConfig{Store: f.store, Refs: []string{"demo/app:v1", "demo/app", "demo/app:v1"}})
	var refs []string
	for _, d := range a.Index.Manifests {
		refs = append(refs, d.Annotations[dockerarchive.AnnotationRefName])
	}
	if !reflect.DeepEqual(refs, []string{"v1", "latest"}) {
		t.Errorf("index.json entries %v, want [v1 latest]", refs)
	}
}

// TestHostPlatform: containers run on linux unless the host is windows,
// so a darwin client still prefers the linux child of its architecture.
func TestHostPlatform(t *testing.T) {
	p := hostPlatform()
	wantOS := "linux"
	if runtime.GOOS == "windows" {
		wantOS = "windows"
	}
	if p.OS != wantOS || p.Architecture != runtime.GOARCH || p.Variant != "" {
		t.Errorf("hostPlatform() = %+v", p)
	}
}

// TestRunSaveSummaryLine: the archive's blob bytes and the time it took,
// on stderr, in every progress mode.
func TestRunSaveSummaryLine(t *testing.T) {
	f := newFixture(t)
	var stdout, stderr bytes.Buffer
	if err := runSave(context.Background(), saveConfig{Store: f.store, Refs: []string{"demo/app:v1"}, Progress: "plain", Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatalf("runSave: %v", err)
	}
	total := f.app.Size + int64(len(f.appConfig)) + int64(len(f.appLayer))
	want := "Saved demo/app:v1 → stdout: " + tui.FormatBytes(total) + " in "
	if got := stderr.String(); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "s\n") {
		t.Errorf("stderr = %q, want %q…", got, want)
	}
}
