package browse

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/tui"
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleSel   = lipgloss.NewStyle().Reverse(true)
	styleDir   = lipgloss.NewStyle().Bold(true)
	styleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// chromeLines is what a screen spends outside its body: the breadcrumb,
// two rules and the status line.
const chromeLines = 4

// sizeWidth fits FormatBytes' widest output, "1023.9 MiB".
const sizeWidth = 10

// listView is what RenderList needs from a frame: the rows on screen and
// the chrome around them.
type listView struct {
	Crumbs  []string
	Rows    []Row // the rows on screen, already filtered and scrolled
	Cursor  int   // index into Rows; -1 when no row is selected
	Count   int   // rows that pass the filter
	Total   int   // rows before filtering
	Loading bool
	Filter  string
	Status  string // transient message, an error mostly
	Input   string // when not "", the text input shown instead of the status
	Hints   string // key hints
	Popup   []KV   // when not nil, drawn instead of the rows
}

// RenderList renders one listing screen for a width×height terminal.
func RenderList(v listView, width, height int) string {
	body := max(height-chromeLines, 1)
	var lines []string
	switch {
	case v.Popup != nil:
		lines = popupLines(v.Popup, width)
	case v.Loading:
		lines = []string{styleDim.Render("  loading…")}
	case len(v.Rows) == 0:
		msg := "  (empty)"
		if v.Filter != "" {
			msg = fmt.Sprintf("  no rows match %q", v.Filter)
		}
		lines = []string{styleDim.Render(msg)}
	default:
		lines = renderRows(v.Rows, v.Cursor, body, width)
	}
	return screen(breadcrumb(v.Crumbs, width), lines, statusLine(v, width), width, height)
}

// viewerView is what RenderViewer needs: the rendered body and the chrome.
type viewerView struct {
	Crumbs  []string
	Body    string // RenderText or RenderHex output
	Status  string
	Input   string // when not "", shown instead of Status
	Loading bool
}

// RenderViewer renders one viewer screen.
func RenderViewer(v viewerView, width, height int) string {
	body := strings.Split(v.Body, "\n")
	if v.Loading {
		body = []string{styleDim.Render("  loading…")}
	}
	status := v.Status
	if v.Input != "" {
		status = v.Input
	}
	return screen(breadcrumb(v.Crumbs, width), body, status, width, height)
}

// screen stacks the breadcrumb, a rule, the body padded or cut to fit, a
// rule and the status line, every line cut to width so nothing wraps.
func screen(crumb string, body []string, status string, width, height int) string {
	n := max(height-chromeLines, 1)
	for len(body) < n {
		body = append(body, "")
	}
	body = body[:n]
	lines := make([]string, 0, n+chromeLines)
	lines = append(lines, crumb, rule(width))
	lines = append(lines, body...)
	lines = append(lines, rule(width), status)
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

func rule(width int) string { return styleDim.Render(strings.Repeat("─", max(width, 0))) }

// breadcrumb joins crumbs with " › "; when that is wider than the
// terminal, leading segments are dropped behind a "…".
func breadcrumb(crumbs []string, width int) string {
	dropped := false
	for {
		s := strings.Join(crumbs, " › ")
		if dropped {
			s = "… › " + s
		}
		if lipgloss.Width(s) <= width || len(crumbs) <= 1 {
			return styleTitle.Render(truncate(s, width))
		}
		crumbs = crumbs[1:]
		dropped = true
	}
}

// renderRows lays the rows out in columns: ls -l columns when the rows
// carry Meta, otherwise name, detail and size. The cursor row is drawn
// reversed without inner styling so the highlight is one span.
func renderRows(rows []Row, cursor, height, width int) []string {
	rows = rows[:min(len(rows), height)]
	lsl := false
	nameW, ownerW := 0, 0
	for _, r := range rows {
		w := lipgloss.Width(r.Name)
		if r.IsDir {
			w++
		}
		nameW = max(nameW, w)
		if r.Meta != nil {
			lsl = true
			ownerW = max(ownerW, len(fmt.Sprintf("%d:%d", r.Meta.UID, r.Meta.GID)))
		}
	}
	nameW = min(nameW, max(8, width/3))
	out := make([]string, 0, len(rows))
	for i, r := range rows {
		selected := i == cursor
		var line string
		if lsl {
			line = lslLine(r, ownerW, width-2, selected)
		} else {
			line = storageLine(r, nameW, width-2, selected)
		}
		if selected {
			out = append(out, "▸ "+styleSel.Render(padRight(line, width-2)))
		} else {
			out = append(out, "  "+line)
		}
	}
	return out
}

// storageLine is "name  detail  size" in width cells.
func storageLine(r Row, nameW, width int, plain bool) string {
	name := r.Name
	if r.IsDir {
		name += "/"
	}
	name = padRight(truncate(name, nameW), nameW)
	size := strings.Repeat(" ", sizeWidth)
	if r.HasSize {
		size = fmt.Sprintf("%*s", sizeWidth, tui.FormatBytes(r.Size))
	}
	detailW := max(width-nameW-2-sizeWidth-2, 0)
	detail := padRight(truncate(r.Detail, detailW), detailW)
	if !plain {
		if r.IsDir {
			name = styleDir.Render(name)
		}
		detail = styleDim.Render(detail)
	}
	return name + "  " + detail + "  " + size
}

// lslLine is "mode owner size mtime name [-> target]" in width cells.
func lslLine(r Row, ownerW, width int, plain bool) string {
	m := r.Meta
	size := strings.Repeat(" ", sizeWidth)
	if r.HasSize {
		size = fmt.Sprintf("%*s", sizeWidth, tui.FormatBytes(r.Size))
	}
	prefix := fmt.Sprintf("%s  %-*s  %s  %s  ", modeString(m.Mode), ownerW, fmt.Sprintf("%d:%d", m.UID, m.GID), size, m.Mtime.Format("2006-01-02 15:04"))
	name := r.Name
	if r.IsDir {
		name += "/"
	}
	if m.Target != "" {
		name += " -> " + m.Target
	}
	name = truncate(name, max(width-lipgloss.Width(prefix), 0))
	if !plain {
		prefix = styleDim.Render(prefix)
		if r.IsDir {
			name = styleDir.Render(name)
		}
	}
	return prefix + name
}

// modeString renders mode like ls -l: a type letter and nine permission
// bits with setuid, setgid and sticky folded in.
func modeString(mode uint64) string {
	var b [10]byte
	switch mode & store.TypeMask {
	case store.TypeDir:
		b[0] = 'd'
	case store.TypeLink:
		b[0] = 'l'
	case store.TypeChar:
		b[0] = 'c'
	case store.TypeBlock:
		b[0] = 'b'
	case store.TypeFIFO:
		b[0] = 'p'
	case store.TypeSocket:
		b[0] = 's'
	default:
		b[0] = '-'
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if mode&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		} else {
			b[i+1] = '-'
		}
	}
	fold := func(i int, bit uint64, x, X byte) {
		if mode&bit == 0 {
			return
		}
		if b[i] == 'x' {
			b[i] = x
		} else {
			b[i] = X
		}
	}
	fold(3, 0o4000, 's', 'S')
	fold(6, 0o2000, 's', 'S')
	fold(9, 0o1000, 't', 'T')
	return string(b[:])
}

// statusLine is the input when one is open, else the status message, the
// row count and the key hints.
func statusLine(v listView, width int) string {
	if v.Input != "" {
		return truncate(v.Input, width)
	}
	var parts []string
	if v.Status != "" {
		parts = append(parts, styleErr.Render(v.Status))
	}
	count := plural(v.Count, "row")
	if v.Filter != "" {
		count = fmt.Sprintf("%d of %d rows · filter %q", v.Count, v.Total, v.Filter)
	}
	parts = append(parts, count)
	if v.Hints != "" {
		parts = append(parts, styleDim.Render(v.Hints))
	}
	return truncate(strings.Join(parts, "  ·  "), width)
}

// popupLines draws the info pairs in a rounded box centred in width.
func popupLines(info []KV, width int) []string {
	keyW := 0
	for _, kv := range info {
		keyW = max(keyW, lipgloss.Width(kv.Key))
	}
	inner := make([]string, 0, len(info))
	innerW := 0
	for _, kv := range info {
		l := fmt.Sprintf("%-*s  %s", keyW, kv.Key, kv.Value)
		inner = append(inner, l)
		innerW = max(innerW, lipgloss.Width(l))
	}
	innerW = min(innerW, max(width-6, 10))
	for i, l := range inner {
		inner[i] = padRight(truncate(l, innerW), innerW)
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Render(strings.Join(inner, "\n"))
	lines := strings.Split(box, "\n")
	pad := strings.Repeat(" ", max((width-lipgloss.Width(lines[0]))/2, 0))
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}
