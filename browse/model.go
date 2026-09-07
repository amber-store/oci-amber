package browse

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/amber-store/core/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/tui"
)

// stackKind names the two stacks an image has.
type stackKind int

const (
	stackStorage stackKind = iota
	stackFS
)

// viewMode is the viewer's representation of a file.
type viewMode int

const (
	modeText viewMode = iota
	modeHex
)

// inputKind says what the text input at the bottom collects.
type inputKind int

const (
	inputNone inputKind = iota
	inputFilter
	inputSearch
	inputGoto
)

// frame is one screen: a listing or a viewer with its state, so returning
// to it restores cursor, scroll and filter.
type frame struct {
	id        int
	node      Node
	loaded    bool // rows or the file are in
	loading   bool // a load command is in flight
	resolving bool // the symlink under the cursor is being followed
	// listing
	rows    []Row
	visible []int // indexes into rows that pass the filter
	cursor  int   // index into visible
	top     int   // first visible index on screen
	filter  string
	// viewer
	file *File
	view *viewer
}

// viewer is the state of an open file.
type viewer struct {
	class      Classification
	mode       viewMode
	text       *Text
	pretty     bool     // JSON shown indented
	lines      []string // lines of the current text representation
	top, left  int
	search     string
	hits       []int // line indexes matching search, ascending
	hexOff     int64 // offset of the first hex row on screen, a multiple of 16
	win        []byte
	winStart   int64
	winLoading bool
}

// imageGroup is the position inside one image: the storage stack, the
// filesystem stack and which one is active.
type imageGroup struct {
	crumb   string
	active  stackKind
	storage []*frame
	fs      []*frame
	fsRoot  key.Key // the image root the fs stack was built for
}

// lineInput is the one-line text input at the bottom of the screen: a
// prompt and the text typed so far. It stands in for bubbles/textinput,
// which would pull a clipboard module in for a paste feature the browser
// has no use for.
type lineInput struct {
	prompt string
	value  string
}

// Update applies one key: printable runes and space append, backspace
// deletes the last rune, ctrl+u clears.
func (in *lineInput) Update(k tea.KeyMsg) {
	switch k.Type {
	case tea.KeyRunes:
		in.value += string(k.Runes)
	case tea.KeySpace:
		in.value += " "
	case tea.KeyBackspace:
		if rs := []rune(in.value); len(rs) > 0 {
			in.value = string(rs[:len(rs)-1])
		}
	case tea.KeyCtrlU:
		in.value = ""
	}
}

// View is the prompt, the text and a cursor mark.
func (in *lineInput) View() string { return in.prompt + in.value + "▏" }

// model is the Bubble Tea model. base holds the repository listing and,
// below it, one repository; img is set while inside an image.
type model struct {
	b         *Browser
	base      []*frame
	img       *imageGroup
	width     int
	height    int
	nextID    int
	input     lineInput
	inputKind inputKind
	popup     []KV
	status    string
}

type listLoadedMsg struct {
	id   int
	rows []Row
	err  error
}

type fileLoadedMsg struct {
	id    int
	file  *File
	class Classification
	text  *Text  // text mode content, when read
	win   []byte // the first hex window, when opened in hex
	err   error
}

type windowLoadedMsg struct {
	id    int
	start int64
	data  []byte
	err   error
}

type resolvedMsg struct {
	id   int // the frame whose row was followed
	node Node
	err  error
}

// newModel builds the model at start: "" is the repository listing,
// "repo" a repository, "repo:tag" or "repo@digest" an image's storage
// root. A reference that does not exist is an error.
func newModel(b *Browser, start string) (*model, error) {
	m := &model{b: b, width: 80, height: 24}
	m.base = []*frame{m.newFrame(b.rootNode())}
	if start == "" {
		return m, nil
	}
	repo, reference := oci.SplitReference(start)
	rn, err := b.repoNode(repo)
	if err != nil {
		return nil, fmt.Errorf("browse: repository %s: %w", repo, err)
	}
	m.base = append(m.base, m.newFrame(rn))
	if reference == "" {
		return m, nil
	}
	in, err := b.imageNode(repo, reference)
	if err != nil {
		return nil, fmt.Errorf("browse: %s: %w", start, err)
	}
	m.img = &imageGroup{crumb: in.crumb, storage: []*frame{m.newFrame(in)}}
	return m, nil
}

func (m *model) newFrame(n Node) *frame {
	m.nextID++
	return &frame{id: m.nextID, node: n}
}

func (m *model) Init() tea.Cmd { return m.ensureLoaded() }

// bodyHeight is how many rows fit between the chrome lines.
func (m *model) bodyHeight() int { return max(m.height-chromeLines, 1) }

func (m *model) activeStack() []*frame {
	if m.img.active == stackFS {
		return m.img.fs
	}
	return m.img.storage
}

func (m *model) setActiveStack(s []*frame) {
	if m.img.active == stackFS {
		m.img.fs = s
	} else {
		m.img.storage = s
	}
}

// top is the frame on screen.
func (m *model) top() *frame {
	if m.img != nil {
		s := m.activeStack()
		return s[len(s)-1]
	}
	return m.base[len(m.base)-1]
}

// findFrame returns the frame with id, or nil once it was popped.
func (m *model) findFrame(id int) *frame {
	for _, f := range m.base {
		if f.id == id {
			return f
		}
	}
	if m.img != nil {
		for _, f := range m.img.storage {
			if f.id == id {
				return f
			}
		}
		for _, f := range m.img.fs {
			if f.id == id {
				return f
			}
		}
	}
	return nil
}

// popFrame removes f wherever it is. Emptying the fs stack returns to the
// storage stack; emptying the storage stack leaves the image; the
// repository listing is never removed.
func (m *model) popFrame(f *frame) {
	remove := func(s []*frame) []*frame {
		for i, g := range s {
			if g == f {
				return append(s[:i:i], s[i+1:]...)
			}
		}
		return s
	}
	if m.img != nil {
		m.img.storage = remove(m.img.storage)
		m.img.fs = remove(m.img.fs)
		if len(m.img.fs) == 0 && m.img.active == stackFS {
			m.img.active = stackStorage
			m.img.fsRoot = key.Key{}
		}
		if len(m.img.storage) == 0 {
			m.img = nil
		}
		return
	}
	if len(m.base) > 1 {
		m.base = remove(m.base)
	}
}

// hexWindowLen is how many bytes a hex window holds: the screen plus one
// screen before and after.
func (m *model) hexWindowLen() int64 { return 3 * int64(m.bodyHeight()) * hexRowBytes }

// ensureLoaded starts loading the top frame when it has nothing yet.
func (m *model) ensureLoaded() tea.Cmd {
	f := m.top()
	if f.loaded || f.loading {
		return nil
	}
	switch n := f.node.(type) {
	case Lister:
		f.loading = true
		return loadList(f.id, n)
	case Opener:
		f.loading = true
		return loadFile(f.id, n, m.hexWindowLen())
	}
	return nil
}

func loadList(id int, n Lister) tea.Cmd {
	return func() tea.Msg {
		rows, err := n.List()
		return listLoadedMsg{id: id, rows: rows, err: err}
	}
}

// loadFile opens n, classifies it from a probe, and reads either the
// whole text or the first hex window.
func loadFile(id int, n Opener, winLen int64) tea.Cmd {
	return func() tea.Msg {
		f, err := n.Open()
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		probe, err := readWindow(f, 0, probeSize)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		class := Classify(f.Name, f.Size, probe)
		msg := fileLoadedMsg{id: id, file: f, class: class}
		if class.Kind == KindText {
			data, err := readWindow(f, 0, f.Size)
			if err != nil {
				return fileLoadedMsg{id: id, err: err}
			}
			msg.text = LoadText(class.Label, data)
			return msg
		}
		win, err := readWindow(f, 0, winLen)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		msg.win = win
		return msg
	}
}

// loadText reads a file already open in hex so it can be shown as text.
func loadText(id int, f *File) tea.Cmd {
	return func() tea.Msg {
		data, err := readWindow(f, 0, f.Size)
		if err != nil {
			return fileLoadedMsg{id: id, err: err}
		}
		return fileLoadedMsg{id: id, text: LoadText("binary", data)}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if f := m.top(); f.view != nil {
			return m, m.ensureWindow(f)
		}
	case listLoadedMsg:
		return m, m.onListLoaded(msg)
	case fileLoadedMsg:
		return m, m.onFileLoaded(msg)
	case windowLoadedMsg:
		return m, m.onWindowLoaded(msg)
	case resolvedMsg:
		return m, m.onResolved(msg)
	case tea.KeyMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *model) onListLoaded(msg listLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.loading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		m.popFrame(f)
		return m.ensureLoaded()
	}
	f.loaded = true
	f.rows = msg.rows
	f.applyFilter()
	return nil
}

func (m *model) onFileLoaded(msg fileLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.loading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		if f.view == nil {
			m.popFrame(f)
		}
		return m.ensureLoaded()
	}
	if f.view != nil { // text requested from hex mode
		v := f.view
		v.text = msg.text
		v.pretty = msg.text.Pretty != nil
		v.lines = Lines(v.currentBytes())
		v.mode = modeText
		v.top = min(lineAt(v.text.Raw, v.hexOff), max(len(v.lines)-1, 0))
		return nil
	}
	f.loaded = true
	f.file = msg.file
	v := &viewer{class: msg.class}
	if msg.text != nil {
		v.text = msg.text
		v.pretty = msg.text.Pretty != nil
		v.lines = Lines(v.currentBytes())
	} else {
		v.mode = modeHex
		v.win = msg.win
		if v.win == nil {
			v.win = []byte{}
		}
	}
	f.view = v
	return nil
}

func (m *model) onWindowLoaded(msg windowLoadedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil || f.view == nil {
		return nil
	}
	v := f.view
	v.winLoading = false
	if msg.err != nil {
		m.status = msg.err.Error()
		return nil
	}
	v.winStart = msg.start
	v.win = msg.data
	if v.win == nil {
		v.win = []byte{}
	}
	return m.ensureWindow(f) // the position may have moved during the load
}

func (m *model) onResolved(msg resolvedMsg) tea.Cmd {
	f := m.findFrame(msg.id)
	if f == nil {
		return nil
	}
	f.resolving = false
	if msg.err != nil {
		m.status = msg.err.Error()
		return nil
	}
	return m.push(msg.node, f)
}

// push enters n from frame from: a Lister or Opener becomes a new frame
// on the active stack, an image root opened outside an image starts the
// image group, a Resolver is followed first.
func (m *model) push(n Node, from *frame) tea.Cmd {
	m.status = ""
	switch n := n.(type) {
	case Resolver:
		from.resolving = true
		return func() tea.Msg {
			r, err := n.Resolve()
			return resolvedMsg{id: from.id, node: r, err: err}
		}
	case *imageRootNode:
		if m.img == nil {
			m.img = &imageGroup{crumb: n.crumb, storage: []*frame{m.newFrame(n)}}
			return m.ensureLoaded()
		}
	}
	fr := m.newFrame(n)
	if m.img != nil {
		m.setActiveStack(append(m.activeStack(), fr))
	} else {
		m.base = append(m.base, fr)
	}
	return m.ensureLoaded()
}

// open follows the row under the cursor.
func (m *model) open() tea.Cmd {
	f := m.top()
	if f.loading || f.resolving || f.view != nil {
		return nil
	}
	row := f.currentRow()
	if row == nil {
		return nil
	}
	if row.Child == nil {
		m.status = "nothing to open here"
		return nil
	}
	return m.push(row.Child, f)
}

// back pops the top frame; on the repository listing it does nothing.
func (m *model) back() tea.Cmd {
	if m.img == nil && len(m.base) == 1 {
		return nil
	}
	m.status = ""
	m.popFrame(m.top())
	return m.ensureLoaded()
}

// toggleView switches between the storage and filesystem stacks. The
// filesystem shown is the one of the innermost image root on the storage
// stack; its stack is kept while that root does not change.
func (m *model) toggleView() tea.Cmd {
	if m.img == nil {
		m.status = "open an image first"
		return nil
	}
	m.status = ""
	if m.img.active == stackFS {
		m.img.active = stackStorage
		return m.ensureLoaded()
	}
	var root *imageRootNode
	for i := len(m.img.storage) - 1; i >= 0 && root == nil; i-- {
		root, _ = m.img.storage[i].node.(*imageRootNode)
	}
	if root == nil {
		return nil
	}
	if m.img.fs == nil || m.img.fsRoot != root.im.Root() {
		m.img.fs = []*frame{m.newFrame(fsRootFor(m.b, root.repo, root.im))}
		m.img.fsRoot = root.im.Root()
	}
	m.img.active = stackFS
	return m.ensureLoaded()
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	if m.popup != nil {
		m.popup = nil
		return nil
	}
	if m.inputKind != inputNone {
		return m.onInputKey(k)
	}
	s := k.String()
	switch s {
	case "q", "ctrl+c":
		return tea.Quit
	case "f":
		return m.toggleView()
	}
	f := m.top()
	if _, isFile := f.node.(Opener); isFile {
		return m.onViewerKey(f, s)
	}
	h := m.bodyHeight()
	switch s {
	case "up", "k":
		f.move(-1, h)
	case "down", "j":
		f.move(1, h)
	case "pgup":
		f.move(-h, h)
	case "pgdown":
		f.move(h, h)
	case "g", "home":
		f.moveTo(0, h)
	case "G", "end":
		f.moveTo(len(f.visible)-1, h)
	case "enter", "right", "l":
		return m.open()
	case "esc":
		if f.filter != "" {
			f.filter = ""
			f.applyFilter()
			return nil
		}
		return m.back()
	case "backspace", "left", "h":
		return m.back()
	case "/":
		m.startInput(inputFilter, f.filter)
	case "i":
		if r := f.currentRow(); r != nil {
			m.popup = r.Info
			if len(m.popup) == 0 {
				m.popup = []KV{{"name", r.Name}}
			}
		}
	}
	return nil
}

func (m *model) onViewerKey(f *frame, s string) tea.Cmd {
	v := f.view
	if v == nil {
		if s == "backspace" || s == "esc" {
			return m.back()
		}
		return nil
	}
	h := m.bodyHeight()
	switch s {
	case "backspace", "esc":
		return m.back()
	case "up", "k":
		return m.scrollViewer(f, -1)
	case "down", "j":
		return m.scrollViewer(f, 1)
	case "pgup":
		return m.scrollViewer(f, -h)
	case "pgdown":
		return m.scrollViewer(f, h)
	case "g", "home":
		return m.scrollViewerEnd(f, false)
	case "G", "end":
		return m.scrollViewerEnd(f, true)
	case "left":
		if v.mode == modeText {
			v.left = max(v.left-8, 0)
		}
	case "right":
		if v.mode == modeText {
			v.left += 8
		}
	case "h":
		return m.toggleHex(f)
	case "p":
		if v.text != nil && v.text.Pretty != nil {
			v.pretty = !v.pretty
			v.lines = Lines(v.currentBytes())
			v.top = min(v.top, max(len(v.lines)-1, 0))
			v.setSearch(v.search)
		}
	case "/":
		if v.mode == modeText {
			m.startInput(inputSearch, v.search)
		}
	case "n":
		v.nextHit(1)
	case "N":
		v.nextHit(-1)
	case ":":
		if v.mode == modeHex {
			m.startInput(inputGoto, "")
		}
	}
	return nil
}

// startInput opens the bottom-line text input for kind with initial text.
func (m *model) startInput(kind inputKind, initial string) {
	m.inputKind = kind
	switch kind {
	case inputFilter:
		m.input.prompt = "filter: "
	case inputSearch:
		m.input.prompt = "search: "
	case inputGoto:
		m.input.prompt = "offset (decimal or 0x…): "
	}
	m.input.value = initial
}

// onInputKey feeds the text input; filter and search apply as they are
// typed, a goto offset applies on enter, esc clears what was typed.
func (m *model) onInputKey(k tea.KeyMsg) tea.Cmd {
	f := m.top()
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc":
		switch m.inputKind {
		case inputFilter:
			f.filter = ""
			f.applyFilter()
		case inputSearch:
			if f.view != nil {
				f.view.setSearch("")
			}
		}
		m.inputKind = inputNone
		return nil
	case "enter":
		kind := m.inputKind
		m.inputKind = inputNone
		if f.view == nil {
			return nil
		}
		switch kind {
		case inputGoto:
			return m.gotoOffset(f, m.input.value)
		case inputSearch:
			f.view.nextHit(0)
		}
		return nil
	}
	m.input.Update(k)
	switch m.inputKind {
	case inputFilter:
		f.filter = m.input.value
		f.applyFilter()
	case inputSearch:
		if f.view != nil {
			f.view.setSearch(m.input.value)
		}
	}
	return nil
}

// scrollViewer moves the viewer by delta rows.
func (m *model) scrollViewer(f *frame, delta int) tea.Cmd {
	v := f.view
	if v.mode == modeHex {
		return m.scrollHex(f, int64(delta))
	}
	v.top = min(max(v.top+delta, 0), max(len(v.lines)-m.bodyHeight(), 0))
	return nil
}

// scrollViewerEnd jumps to the start or the end.
func (m *model) scrollViewerEnd(f *frame, toEnd bool) tea.Cmd {
	v := f.view
	if v.mode == modeHex {
		if toEnd {
			return m.scrollHex(f, 1<<40)
		}
		return m.scrollHex(f, -(1 << 40))
	}
	v.top = 0
	if toEnd {
		v.top = max(len(v.lines)-m.bodyHeight(), 0)
	}
	return nil
}

// scrollHex moves the hex view by rows, keeping the last page full, and
// loads a new window when the screen leaves the loaded one.
func (m *model) scrollHex(f *frame, deltaRows int64) tea.Cmd {
	v := f.view
	h := int64(m.bodyHeight())
	rows := (f.file.Size + hexRowBytes - 1) / hexRowBytes
	maxTop := max(rows-h, 0) * hexRowBytes
	row := v.hexOff/hexRowBytes + deltaRows
	v.hexOff = min(max(row, 0)*hexRowBytes, maxTop)
	return m.ensureWindow(f)
}

// ensureWindow loads the bytes around the hex position when the loaded
// window does not cover the screen.
func (m *model) ensureWindow(f *frame) tea.Cmd {
	v := f.view
	if v == nil || v.mode != modeHex || v.winLoading {
		return nil
	}
	h := int64(m.bodyHeight())
	need := min(h*hexRowBytes, f.file.Size-v.hexOff)
	if v.win != nil && v.hexOff >= v.winStart && v.hexOff+need <= v.winStart+int64(len(v.win)) {
		return nil
	}
	start := max(v.hexOff-h*hexRowBytes, 0)
	length := m.hexWindowLen()
	v.winLoading = true
	file, id := f.file, f.id
	return func() tea.Msg {
		data, err := readWindow(file, start, length)
		return windowLoadedMsg{id: id, start: start, data: data, err: err}
	}
}

// gotoOffset jumps the hex view to a typed offset.
func (m *model) gotoOffset(f *frame, s string) tea.Cmd {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	off, err := strconv.ParseInt(s, 0, 64)
	if err != nil || off < 0 {
		m.status = fmt.Sprintf("not an offset: %q", s)
		return nil
	}
	f.view.hexOff = off / hexRowBytes * hexRowBytes
	return m.scrollHex(f, 0)
}

// toggleHex switches the viewer between text and hex at the same place.
func (m *model) toggleHex(f *frame) tea.Cmd {
	v := f.view
	switch v.mode {
	case modeText:
		v.mode = modeHex
		v.hexOff = lineOffset(v.text.Raw, v.top) / hexRowBytes * hexRowBytes
		return m.scrollHex(f, 0)
	default:
		if v.class.Kind == KindTooLarge {
			m.status = fmt.Sprintf("too large for text (over %s)", tui.FormatBytes(MaxTextSize))
			return nil
		}
		if v.text == nil {
			f.loading = true
			return loadText(f.id, f.file)
		}
		v.mode = modeText
		v.top = min(lineAt(v.text.Raw, v.hexOff), max(len(v.lines)-1, 0))
	}
	return nil
}

// currentBytes is what the text mode shows: pretty JSON or the raw bytes.
func (v *viewer) currentBytes() []byte {
	if v.pretty && v.text.Pretty != nil {
		return v.text.Pretty
	}
	return v.text.Raw
}

// setSearch recomputes the matching lines, case-insensitively.
func (v *viewer) setSearch(s string) {
	v.search = s
	v.hits = v.hits[:0]
	if s == "" {
		return
	}
	needle := strings.ToLower(s)
	for i, l := range v.lines {
		if strings.Contains(strings.ToLower(l), needle) {
			v.hits = append(v.hits, i)
		}
	}
}

// nextHit scrolls to the next hit after the top line (dir > 0), the
// previous one before it (dir < 0) or the first at or after it (dir 0),
// wrapping around.
func (v *viewer) nextHit(dir int) {
	if len(v.hits) == 0 {
		return
	}
	switch {
	case dir > 0:
		for _, h := range v.hits {
			if h > v.top {
				v.top = h
				return
			}
		}
		v.top = v.hits[0]
	case dir < 0:
		for i := len(v.hits) - 1; i >= 0; i-- {
			if v.hits[i] < v.top {
				v.top = v.hits[i]
				return
			}
		}
		v.top = v.hits[len(v.hits)-1]
	default:
		for _, h := range v.hits {
			if h >= v.top {
				v.top = h
				return
			}
		}
		v.top = v.hits[0]
	}
}

// move shifts the cursor by delta rows and keeps it on screen.
func (f *frame) move(delta, height int) { f.moveTo(f.cursor+delta, height) }

// moveTo puts the cursor on visible row i, clamped, and scrolls so it is
// on screen.
func (f *frame) moveTo(i, height int) {
	if len(f.visible) == 0 {
		f.cursor, f.top = 0, 0
		return
	}
	f.cursor = min(max(i, 0), len(f.visible)-1)
	height = max(height, 1)
	if f.cursor < f.top {
		f.top = f.cursor
	}
	if f.cursor >= f.top+height {
		f.top = f.cursor - height + 1
	}
}

// currentRow is the row under the cursor, nil when there is none.
func (f *frame) currentRow() *Row {
	if f.cursor < 0 || f.cursor >= len(f.visible) {
		return nil
	}
	return &f.rows[f.visible[f.cursor]]
}

// applyFilter recomputes visible from filter, keeping the cursor on its
// row when that row survives and moving it to the first row otherwise.
func (f *frame) applyFilter() {
	prev := -1
	if r := f.currentRow(); r != nil {
		prev = f.visible[f.cursor]
	}
	f.visible = f.visible[:0]
	needle := strings.ToLower(f.filter)
	for i, r := range f.rows {
		if needle == "" || strings.Contains(strings.ToLower(r.Name), needle) || strings.Contains(strings.ToLower(r.Detail), needle) {
			f.visible = append(f.visible, i)
		}
	}
	f.cursor, f.top = 0, 0
	for j, i := range f.visible {
		if i == prev {
			f.cursor = j
			break
		}
	}
}

// crumbs is the breadcrumb: the repository, then inside an image its
// crumb, the active view's name and the crumbs of the frames above the
// view's root.
func (m *model) crumbs() []string {
	if m.img == nil {
		if len(m.base) == 1 {
			return []string{m.base[0].node.Crumb()}
		}
		c := make([]string, 0, len(m.base)-1)
		for _, f := range m.base[1:] {
			c = append(c, f.node.Crumb())
		}
		return c
	}
	c := []string{m.base[len(m.base)-1].node.Crumb(), m.img.crumb, "storage"}
	if m.img.active == stackFS {
		c[2] = "filesystem"
	}
	for _, f := range m.activeStack()[1:] {
		if s := f.node.Crumb(); s != "" {
			c = append(c, s)
		}
	}
	return c
}

func (m *model) View() string {
	f := m.top()
	crumbs := m.crumbs()
	if _, isFile := f.node.(Opener); isFile {
		return RenderViewer(m.viewerView(f, crumbs), m.width, m.height)
	}
	return RenderList(m.listView(f, crumbs), m.width, m.height)
}

func (m *model) listHints() string {
	hints := "enter open · backspace back · / filter · i info"
	if m.img != nil {
		if m.img.active == stackFS {
			hints += " · f storage"
		} else {
			hints += " · f filesystem"
		}
	}
	return hints + " · q quit"
}

// listView gathers the rows on screen and the chrome of a listing frame.
func (m *model) listView(f *frame, crumbs []string) listView {
	h := m.bodyHeight()
	f.moveTo(f.cursor, h)
	end := min(f.top+h, len(f.visible))
	rows := make([]Row, 0, max(end-f.top, 0))
	for _, idx := range f.visible[f.top:end] {
		rows = append(rows, f.rows[idx])
	}
	v := listView{
		Crumbs:  crumbs,
		Rows:    rows,
		Cursor:  f.cursor - f.top,
		Count:   len(f.visible),
		Total:   len(f.rows),
		Loading: !f.loaded,
		Filter:  f.filter,
		Status:  m.status,
		Hints:   m.listHints(),
		Popup:   m.popup,
	}
	if len(rows) == 0 {
		v.Cursor = -1
	}
	if m.inputKind != inputNone {
		v.Input = m.input.View()
	}
	return v
}

// viewerView renders a viewer frame's body and status.
func (m *model) viewerView(f *frame, crumbs []string) viewerView {
	v := viewerView{Crumbs: crumbs, Loading: f.view == nil}
	if m.inputKind != inputNone {
		v.Input = m.input.View()
	}
	if f.view == nil {
		return v
	}
	vw := f.view
	var parts []string
	if m.status != "" {
		parts = append(parts, styleErr.Render(m.status))
	}
	switch vw.mode {
	case modeText:
		vw.top = min(max(vw.top, 0), max(len(vw.lines)-1, 0))
		hits := make(map[int]bool, len(vw.hits))
		for _, i := range vw.hits {
			hits[i] = true
		}
		v.Body = RenderText(vw.lines, vw.top, vw.left, m.bodyHeight(), m.width, hits)
		parts = append(parts, vw.text.Label, tui.FormatBytes(f.file.Size), plural(len(vw.lines), "line"))
		if vw.text.Pretty != nil {
			if vw.pretty {
				parts = append(parts, "pretty")
			} else {
				parts = append(parts, "raw")
			}
		}
		if vw.search != "" {
			parts = append(parts, fmt.Sprintf("%s for %q", plural(len(vw.hits), "hit"), vw.search))
		}
	default:
		v.Body = m.hexBody(f)
		pct := 0.0
		if f.file.Size > 0 {
			pct = 100 * float64(vw.hexOff) / float64(f.file.Size)
		}
		parts = append(parts, "hex", tui.FormatBytes(f.file.Size), fmt.Sprintf("offset %#x · %.1f%%", vw.hexOff, pct))
	}
	for _, kv := range f.file.Labels {
		parts = append(parts, kv.Key+" "+kv.Value)
	}
	hints := ": goto · h text · g/G ends · esc back"
	if vw.mode == modeText {
		hints = "h hex · / search · n/N hits · ←/→ scroll · esc back"
		if vw.text.Pretty != nil {
			hints = "p raw/pretty · " + hints
		}
	}
	parts = append(parts, styleDim.Render(hints))
	v.Status = strings.Join(parts, " · ")
	return v
}

// hexBody renders the hex rows on screen from the loaded window.
func (m *model) hexBody(f *frame) string {
	v := f.view
	if f.file.Size == 0 {
		return styleDim.Render("  (empty file)")
	}
	off := v.hexOff - v.winStart
	if v.win == nil || off < 0 || off > int64(len(v.win)) {
		return styleDim.Render("  loading…")
	}
	end := min(off+int64(m.bodyHeight())*hexRowBytes, int64(len(v.win)))
	return RenderHex(v.hexOff, v.win[off:end], m.width)
}
