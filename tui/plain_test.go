package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/oci-amber/dockerarchive"
	"github.com/amber-store/oci-amber/importer"
	"github.com/amber-store/oci-amber/oci"
)

var dockerarchivePlan = dockerarchive.Plan{Blobs: []dockerarchive.PlanBlob{{Digest: oci.DigestOfBytes([]byte("x")), Size: 10}}}

func TestRunPlainPrintsStatusAndReturnsResult(t *testing.T) {
	tr := importer.NewTracker(importer.TrackerOptions{Verify: true})
	var out bytes.Buffer
	want := &importer.Report{}
	rep, err := RunPlain(&out, tr, 10*time.Millisecond, func() (*importer.Report, error) {
		tr.Queue(&dockerarchivePlan)
		tr.StartBlobs()
		time.Sleep(50 * time.Millisecond)
		return want, nil
	})
	if err != nil || rep != want {
		t.Fatalf("rep, err = %v, %v", rep, err)
	}
	if !strings.Contains(out.String(), "blobs 0/1") {
		t.Fatalf("no status line printed:\n%s", out.String())
	}
	boom := errors.New("boom")
	if _, err := RunPlain(&out, tr, time.Hour, func() (*importer.Report, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
