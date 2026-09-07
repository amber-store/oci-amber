package browse

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

// Options configure Run.
type Options struct {
	Store  *store.Store
	Blobs  *blob.Store // blob.NewReadOnly over Store is enough
	Images *image.Store
	Start  string // "", "repo", "repo:tag" or "repo@digest"
}

// Run resolves Start, then runs the browser in the alternate screen until
// q, ctrl-c or ctx is done. A start reference that does not exist is
// returned before anything is drawn. The caller owns signals: a SIGINT
// that cancels ctx ends the program and returns nil. A terminal failure
// is returned as a *tui.TerminalError.
func Run(ctx context.Context, opts Options) error {
	m, err := newModel(New(opts.Store, opts.Blobs, opts.Images), opts.Start)
	if err != nil {
		return err
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithoutSignalHandler())
	if _, err := p.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return &tui.TerminalError{Err: err}
	}
	return nil
}
