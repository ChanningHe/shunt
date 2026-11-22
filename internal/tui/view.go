package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"shunt/internal/config"
)

const (
	labelWidth  = 10 // the column of form labels
	maxRecent   = 8  // request lines shown while the height is unknown
	messagesAPI = "/v1/messages"
)

// Only ANSI 16 colors, bold, faint and reverse: readable on dark and light
// terminals alike. Color repeats what a glyph and a word already say.
var (
	plain    = lipgloss.NewStyle()
	bold     = lipgloss.NewStyle().Bold(true)
	dim      = lipgloss.NewStyle().Faint(true)
	reverse  = lipgloss.NewStyle().Reverse(true)
	block    = reverse.Bold(true) // the wordmark and the focused heading
	okStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bad      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	errStyle = bad.Bold(true)
	onLamp   = block.Foreground(lipgloss.Color("2"))
	askStyle = block.Foreground(lipgloss.Color("3"))
)

// State glyphs; each has a single-cell width in every terminal.
const (
	okMark   = "✓ "
	warnMark = "! "
	badMark  = "✗ "
	ruleRune = "─"
)

// View only renders the model; it does no I/O.
func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *model) render() string {
	head, foot := wrap(m.header(), m.width), wrap(m.footer(), m.width)
	lines := append(head, "")
	avail := 0 // unlimited while the height is unknown
	if m.height > 0 {
		avail = max(1, m.height-len(head)-len(foot)-2) // the blank line and the rule
	}
	var body []string
	if m.mode == formMode {
		body = m.formLines()
		if avail > 0 && len(body) > avail {
			body = body[:avail]
		}
	} else {
		body = m.body(avail)
	}
	lines = append(lines, body...)
	// Fill the screen: the rule and the footer sit on its last lines.
	for n := len(body); n < avail; n++ {
		lines = append(lines, "")
	}
	lines = append(lines, dim.Render(strings.Repeat(ruleRune, max(0, m.width))))
	lines = append(lines, foot...)
	return clip(lines, m.width)
}

func (m *model) header() []string {
	state := dim.Render("routing off")
	if m.routing.on {
		state = onLamp.Render(" routing on ")
	}
	lines := []string{block.Render(" shunt ") + "  " + m.c.URL() + "  " + state}
	if m.st != nil && len(m.st.Files) > 0 {
		f := m.st.Files[0]
		const label = "config: "
		notes := fmt.Sprintf(" (%s)", f.Format)
		if !f.Writable {
			notes += " (read-only)"
		}
		if len(m.st.Files) > 1 && !m.st.Files[1].Writable {
			notes += " · keys file read-only"
		}
		path := cutLeft(m.short(f.Path), m.width-lipgloss.Width(label+notes))
		lines = append(lines, dim.Render(label+path+notes))
	}
	if m.st != nil && m.st.LoadError != nil {
		lines = append(lines, errStyle.Render(badMark+"config error: "+*m.st.LoadError+" (fix the files, then press r)"))
	}
	if m.stErr != nil {
		lines = append(lines, errStyle.Render(badMark+"daemon: "+m.stErr.Error()))
	}
	if m.routing.err != nil {
		lines = append(lines, warn.Render(warnMark+"settings.json: "+m.routing.err.Error()))
	}
	if m.routing.on && len(m.routing.drift) > 0 {
		lines = append(lines, warn.Render(warnMark+"settings.json does not match the alias routes ("+
			strings.Join(m.routing.drift, "; ")+"); run: shunt on"))
	}
	return lines
}

// cutLeft fits s into width cells by replacing its head with "…". Without
// room for more than the ellipsis, s is left for wrap to break.
func cutLeft(s string, width int) string {
	over := lipgloss.Width(s) - width
	if over <= 0 || width < 2 {
		return s
	}
	return ansi.TruncateLeft(s, over+1, "…")
}

// short shows a path under the home directory as ~/...
func (m *model) short(path string) string {
	if rest, ok := strings.CutPrefix(path, m.home+"/"); ok && m.home != "" {
		return "~/" + rest
	}
	return path
}

// cell is one table cell; st colors it unless its row is selected.
type cell struct {
	s  string
	st lipgloss.Style
}

func text(s string) cell { return cell{s, plain} }

// table is a section's content: a header row and one row per entry.
type table struct {
	head []string
	rows [][]cell
	tail []cell // an unselectable last row; nil: none
}

// widths are the column widths, the tail included, so rows line up whether
// it is shown or not.
func (t table) widths() []int {
	widths := make([]int, len(t.head))
	for i, h := range t.head {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range slices.Concat(t.rows, [][]cell{t.tail}) {
		for i, c := range row {
			widths[i] = max(widths[i], lipgloss.Width(c.s))
		}
	}
	return widths
}

// line renders one row in columns of widths.
func line(row []cell, widths []int, selected bool) string {
	var b strings.Builder
	for i, c := range row {
		if selected || c.s == "" {
			b.WriteString(c.s)
		} else {
			b.WriteString(c.st.Render(c.s))
		}
		if i < len(widths)-1 { // a gap follows every column but the last
			b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(c.s)+2))
		}
	}
	s := strings.TrimRight(b.String(), " ")
	if selected {
		return block.Render("> " + s)
	}
	return "  " + s
}

// lines renders rows first..last-1 under the header, then the tail if asked;
// row cur is marked when the section has the focus.
func (t table) lines(first, last, cur int, focused, tail bool) []string {
	widths := t.widths()
	head := make([]cell, len(t.head))
	for i, h := range t.head {
		head[i] = text(h)
	}
	out := []string{dim.Render(line(head, widths, false))}
	for r := first; r < last; r++ {
		out = append(out, line(t.rows[r], widths, focused && r == cur))
	}
	if tail && t.tail != nil {
		out = append(out, line(t.tail, widths, false))
	}
	return out
}

const (
	arrow     = "──▶ "
	liveArrow = "━━▶ "
)

func (m *model) routesTable() table {
	g := buildGraph(m.st, m.act, m.now())
	t := table{head: []string{"ROUTE", "TARGET", "STATE", "REQS", "LAST"}}
	rest := len(g.sources) - 1
	for i, s := range g.sources[:rest] {
		to := g.targets[g.edge[i]]
		state := targetCell(to.state)
		if s.alias {
			state.s += " (alias)"
		}
		reqs, last := text("-"), text("-")
		if s.stats != nil {
			reqs.s = fmt.Sprint(s.stats.Requests)
			if s.stats.Errors > 0 {
				reqs = cell{fmt.Sprintf("%d (%d failed)", s.stats.Requests, s.stats.Errors), warn}
			}
			last = cell{fmt.Sprintf("%d %s ago", s.stats.Status, age(m.now().Sub(s.stats.Time))), statusStyle(s.stats.Status)}
		}
		dest := destCell(s.live, to.provider+" · "+to.model)
		if to.state == stateNoProvider {
			dest.st = bad
		}
		t.rows = append(t.rows, []cell{{config.WirePrefix + s.name, bold}, dest, state, reqs, last})
	}
	dest := destCell(g.sources[rest].live, g.targets[g.edge[rest]].provider)
	if !g.sources[rest].live {
		dest.st = dim
	}
	t.tail = []cell{{g.sources[rest].name, dim}, dest, {g.targets[g.edge[rest]].state, dim}}
	return t
}

// destCell is a row's arrow and target. A route that just carried a request
// gets the heavy arrow, so the mark does not rest on color alone.
func destCell(live bool, to string) cell {
	if live {
		return cell{liveArrow + to, okStyle}
	}
	return text(arrow + to)
}

func targetCell(state string) cell {
	switch state {
	case stateReady:
		return cell{okMark + stateReady, okStyle}
	case stateNoKey:
		return cell{warnMark + stateNoKey + ": press K", warn}
	}
	return cell{badMark + state, bad}
}

func (m *model) providersTable() table {
	used := map[string]int{}
	for _, r := range m.st.Config.Routes {
		used[r.Provider]++
	}
	t := table{head: []string{"NAME", "KEY", "ROUTES", "BASE URL"}}
	for _, n := range m.names(providersSec) {
		key := cell{okMark + m.st.Keys[n], okStyle}
		switch m.st.Keys[n] {
		case config.KeyMissing:
			key = cell{warnMark + config.KeyMissing, warn}
		case config.KeyInvalid:
			key = cell{badMark + config.KeyInvalid, bad}
		}
		t.rows = append(t.rows, []cell{{n, bold}, key, text(fmt.Sprint(used[n])), {m.st.Config.Providers[n].BaseURL, dim}})
	}
	return t
}

func statusStyle(status int) lipgloss.Style {
	if status >= 400 {
		return bad
	}
	return okStyle
}

// age is a short "how long ago".
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// part is a section ready to lay out.
type part struct {
	sec   section
	title string
	empty string // shown instead of the table when there are no rows
	table table
}

func (p part) heading(focused bool, width int) string {
	st := bold
	if focused {
		st = block
	}
	return heading(fmt.Sprintf("%s (%d)", p.title, len(p.table.rows)), st, width)
}

// heading is a title followed by a faint rule to the right edge.
func heading(title string, st lipgloss.Style, width int) string {
	s := st.Render(" " + title + " ")
	n := width - lipgloss.Width(title) - 3
	if n < 1 {
		return s
	}
	return s + " " + dim.Render(strings.Repeat(ruleRune, n))
}

// partFixed is the number of lines of a part besides its rows: the heading
// and the column header (or the hint of an empty part).
const partFixed = 2

// show renders p with at most rows rows, scrolled to keep cur in view. The
// tail shows only when every other row fits, so it gives way first.
func (p part) show(rows, cur int, focused bool, width int) []string {
	out := []string{p.heading(focused, width)}
	n := len(p.table.rows)
	tail := rows > n
	if n == 0 {
		out = append(out, dim.Render("  "+p.empty))
		if tail && p.table.tail != nil {
			out = append(out, line(p.table.tail, p.table.widths(), false))
		}
		return out
	}
	first, last := 0, n
	if rows < n {
		first = min(max(0, cur-rows+1), n-rows)
		last = first + rows
	}
	return append(out, p.table.lines(first, last, cur, focused, tail)...)
}

// body lays out both sections and, when room is left, the recent requests,
// within avail lines (0: unlimited).
func (m *model) body(avail int) []string {
	if m.st == nil {
		return []string{dim.Render("  loading...")}
	}
	parts := []part{
		{routesSec, "Routes", "no routes; press a to add one", m.routesTable()},
		{providersSec, "Providers", "no providers; press a to add one", m.providersTable()},
	}
	if len(parts[providersSec].table.rows) == 0 {
		parts[routesSec].empty = "no routes; add a provider first"
	}
	want := [2]int{len(parts[0].table.rows), len(parts[1].table.rows)}
	const fixed = 2*partFixed + 1 // a blank line between the parts

	focus, other := m.sec, 1-m.sec
	free := avail - fixed
	switch {
	case avail <= 0 || want[0]+want[1] <= free:
	case free >= 2: // both lists, the focused one gets most of the room
		want[other] = min(want[other], max(1, free/3))
		want[focus] = min(want[focus], free-want[other])
	default: // only the focused list fits
		return parts[focus].show(max(1, avail-partFixed), m.cursor[focus], true, m.width)
	}
	// The routes' tail takes only room left once both lists have their
	// share, so it never costs either list a row.
	if want[routesSec] == len(parts[routesSec].table.rows) && (avail <= 0 || want[0]+want[1] < free) {
		want[routesSec]++
	}
	var out []string
	for i, p := range parts {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, p.show(want[i], m.cursor[i], m.sec == p.sec, m.width)...)
	}
	left := maxRecent
	if avail > 0 { // the requests take whatever room is left
		left = avail - len(out) - recentFixed
	}
	return append(out, m.recent(left)...)
}

// recentFixed is the number of lines of the recent requests besides its
// rows: a blank line, the heading and the column header.
const recentFixed = 3

// recent lists the latest n requests, newest first.
func (m *model) recent(n int) []string {
	if n < 1 || m.act == nil {
		return nil
	}
	if len(m.act.Recent) == 0 {
		return []string{"", heading("Recent requests", bold, m.width),
			dim.Render("  no requests since the router started; with routing on, every Claude Code request shows here")}
	}
	t := table{head: []string{"TIME", "MODEL", "TARGET", "STATUS", "MS", ""}}
	for _, e := range m.act.Recent[:min(n, len(m.act.Recent))] {
		target := cell{e.Target, plain}
		if e.Route == "" {
			target.st = dim
		}
		if e.Target == "rejected" {
			target.st = warn
		}
		path := ""
		if e.Path != messagesAPI {
			path = e.Path
		}
		model := e.Model
		if model == "" {
			model = "-"
		}
		status := okMark
		if e.Status >= 400 {
			status = badMark
		}
		t.rows = append(t.rows, []cell{
			{e.Time.Local().Format("15:04:05"), dim}, text(model), target,
			{status + fmt.Sprint(e.Status), statusStyle(e.Status)}, {fmt.Sprint(e.Ms), dim}, {path, dim},
		})
	}
	out := []string{"", heading("Recent requests", bold, m.width)}
	return append(out, t.lines(0, len(t.rows), -1, false, false)...)
}

func (m *model) formLines() []string {
	f := m.form
	lines := []string{heading(f.title, block, m.width), ""}
	for i := range f.fields {
		fl := &f.fields[i]
		mark, label := "  ", dim
		if i == f.focus {
			mark, label = "> ", bold
		}
		val := fl.input.View()
		if fl.choices != nil {
			val = fmt.Sprintf("< %s >  %s", fl.choices[fl.choice],
				dim.Render(fmt.Sprintf("%d/%d", fl.choice+1, len(fl.choices))))
		}
		lines = append(lines, mark+label.Render(fmt.Sprintf("%-*s", labelWidth, fl.label))+val)
	}
	notes := []string{""}
	if f.hint != "" {
		notes = append(notes, dim.Render(f.hint))
	}
	if f.err != nil {
		notes = append(notes, errStyle.Render(badMark+f.err.Error()))
	}
	return append(lines, wrap(notes, m.width)...)
}

func (m *model) footer() []string {
	var lines []string
	switch {
	case m.mode == formMode:
		lines = hints(m.width, "enter save", "esc cancel", "tab/↑↓ next field")
	case m.mode == confirmMode:
		lines = []string{askStyle.Render(" "+m.prompt+" ") + " y to confirm, any other key cancels"}
	case m.sec == routesSec:
		lines = hints(m.width, "tab providers", "a add", "e edit", "d delete", "t test", "K set key", "r reload", "o routing", "q quit")
	default:
		lines = hints(m.width, "tab routes", "a add", "e edit", "d delete", "K set key", "X clear key", "r reload", "o routing", "q quit")
	}
	if m.result != "" {
		lines = append(lines, strings.Split(m.result, "\n")...)
	}
	if m.failure != nil {
		lines = append(lines, errStyle.Render(badMark+"error: "+m.failure.Error()))
	}
	return lines
}

// hints joins key hints with faint separators into lines of at most width
// (0: one line), never splitting a hint from its key.
func hints(width int, h ...string) []string {
	const sep = " · "
	sepW := lipgloss.Width(sep)
	var lines []string
	line, w := "", 0
	for _, s := range h {
		sw := lipgloss.Width(s)
		switch {
		case w == 0:
			line, w = s, sw
		case width > 0 && w+sepW+sw > width:
			lines = append(lines, line)
			line, w = s, sw
		default:
			line += dim.Render(sep) + s
			w += sepW + sw
		}
	}
	return append(lines, line)
}

// wrap breaks lines longer than width (0: unknown, left as they are).
func wrap(lines []string, width int) []string {
	if width <= 0 {
		return lines
	}
	var out []string
	for _, l := range lines {
		out = append(out, strings.Split(lipgloss.Wrap(l, width, " "), "\n")...)
	}
	return out
}

// clip cuts every line to width and joins them; the tables and forms stay
// one line per row however narrow the terminal is.
func clip(lines []string, width int) string {
	if width > 0 {
		cut := lipgloss.NewStyle().MaxWidth(width)
		for i, l := range lines {
			lines[i] = cut.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
