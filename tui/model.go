package tui

import (
	"errors"
	"io"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/draganm/oci-amber/importer"
)

// tickInterval is how often the view is refreshed from the tracker.
const tickInterval = 250 * time.Millisecond

type tickMsg time.Time

// doneMsg says the run returned; the program quits with an empty view.
type doneMsg struct{}

// TerminalError wraps the error tea.Program.Run returned: the terminal could
// not be set up, or the renderer failed mid-run. Err is never nil.
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return "terminal: " + e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// viewFunc renders one frame for width columns; bar draws a progress bar
// for a fraction and spinner is the current spinner frame.
type viewFunc func(width int, bar func(float64) string, spinner string) string

// model is the Bubble Tea model: it owns no progress state, it renders
// whatever view reads from its tracker.
type model struct {
	view   viewFunc
	cancel func()
	width  int
	bar    progress.Model
	spin   spinner.Model
	done   bool
}

func newModel(view viewFunc, cancel func()) model {
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	bar.Width = 30
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	return model{view: view, cancel: cancel, width: 80, bar: bar, spin: sp}
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(tick(), m.spin.Tick) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.bar.Width = max(10, min(40, msg.Width/3))
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cancel()
		}
	case tickMsg:
		return m, tick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case doneMsg:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() string {
	if m.done {
		return ""
	}
	return m.view(m.width, m.bar.ViewAs, m.spin.View())
}

// runProgram drives run under a Bubble Tea program: run starts on a
// goroutine, the program renders view until run returns, and run's error
// is returned. Keys are read from in and frames written to out; nil means
// the process's stdin and stdout. cancel is called on q or ctrl-c; it
// should cancel run's context. Signals are left to the caller (the program
// does not install a handler), so a SIGINT from outside cancels through the
// caller's context the same way.
func runProgram(view viewFunc, cancel func(), in io.Reader, out io.Writer, run func() error) error {
	opts := []tea.ProgramOption{tea.WithoutSignalHandler()}
	if in != nil {
		opts = append(opts, tea.WithInput(in))
	}
	if out != nil {
		opts = append(opts, tea.WithOutput(out))
	}
	p := tea.NewProgram(newModel(view, cancel), opts...)
	errs := make(chan error, 1)
	go func() {
		errs <- run()
		p.Send(doneMsg{})
	}()
	_, teaErr := p.Run()
	if teaErr != nil {
		// The terminal failed under us: cancel the run rather than leave
		// it going unattended with no way to report its progress or be
		// told to stop.
		cancel()
	}
	err := <-errs
	if teaErr != nil {
		return errors.Join(&TerminalError{teaErr}, err)
	}
	return err
}

// Run drives an import under the TUI on the process's terminal and returns
// run's result; see runProgram for cancellation.
func Run(tr *importer.Tracker, title string, cancel func(), run func() (*importer.Report, error)) (*importer.Report, error) {
	var rep *importer.Report
	err := runProgram(func(width int, bar func(float64) string, spinner string) string {
		return RenderView(tr.Snapshot(), title, width, bar, spinner)
	}, cancel, nil, nil, func() error {
		var err error
		rep, err = run()
		return err
	})
	return rep, err
}

// RunSave drives a save under the TUI, rendering on out with keys from in,
// and returns run's error; see runProgram for cancellation.
func RunSave(tr *SaveTracker, title string, cancel func(), in io.Reader, out io.Writer, run func() error) error {
	return runProgram(func(width int, bar func(float64) string, _ string) string {
		return RenderSaveView(tr.Snapshot(), title, width, bar)
	}, cancel, in, out, run)
}
