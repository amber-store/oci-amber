package tui

import (
	"fmt"
	"io"
	"time"

	"github.com/draganm/oci-amber/importer"
)

// runPlain drives run without a screen: status is written to w as a line
// every interval until run returns.
func runPlain(w io.Writer, interval time.Duration, status func() string, run func() error) error {
	errs := make(chan error, 1)
	go func() { errs <- run() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-errs:
			return err
		case <-ticker.C:
			fmt.Fprintln(w, status())
		}
	}
}

// RunPlain drives an import without a screen: a status line is written to
// w every interval until run returns. It is what a non-terminal stdout
// gets.
func RunPlain(w io.Writer, tr *importer.Tracker, interval time.Duration, run func() (*importer.Report, error)) (*importer.Report, error) {
	var rep *importer.Report
	err := runPlain(w, interval, func() string { return StatusLine(tr.Snapshot()) }, func() error {
		var err error
		rep, err = run()
		return err
	})
	return rep, err
}

// RunSavePlain drives a save without a screen: a status line is written
// to w every interval until run returns.
func RunSavePlain(w io.Writer, tr *SaveTracker, interval time.Duration, run func() error) error {
	return runPlain(w, interval, func() string { return SaveStatusLine(tr.Snapshot()) }, run)
}
