// Package tui is shunt's terminal UI. It reads and changes the config only
// through the admin API client, the same path the CLI takes, and leaves
// Claude Code's settings.json to Hooks supplied by the caller.
package tui

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// Hooks are the local (non-daemon) actions; the root package supplies them
// from the same functions the CLI uses for on/off/sync.
type Hooks struct {
	RoutingStatus func() (on bool, drift []string, err error) // settings.json state
	On            func() (changed []string, err error)
	Off           func() (changed bool, err error)
	Sync          func() (changed []string, err error) // after a route change with needs_sync
}

// refreshEvery is how often the screen re-reads the daemon, so changes made
// elsewhere (CLI, web UI, traffic) show up on their own.
const refreshEvery = 2 * time.Second

// Run shows the TUI until the user quits.
func Run(c *admin.Client, h Hooks) error {
	m := newModel(c, h)
	m.every = refreshEvery
	_, err := tea.NewProgram(m).Run()
	return err
}

// section is one of the two lists on screen; tab moves between them.
type section int

const (
	routesSec section = iota
	providersSec
)

type mode int

const (
	listMode mode = iota
	formMode
	confirmMode
)

// routing is the settings.json side, as Hooks.RoutingStatus reports it.
type routing struct {
	on    bool
	drift []string
	err   error
}

type model struct {
	c       *admin.Client
	h       Hooks
	st      *admin.State    // last good state; nil until the first fetch
	stErr   error           // the last fetch failed
	act     *admin.Activity // last good activity; may be nil
	routing routing
	sec     section
	cursor  [2]int // per section
	width   int
	height  int
	every   time.Duration    // auto-refresh interval; 0 = never
	loading bool             // a refresh is in flight; ticks do not stack another
	now     func() time.Time // for request ages
	home    string           // shown as ~ in paths
	mode    mode
	form    *form
	pending func() tea.Cmd // the delete awaiting confirmation
	prompt  string         // its question
	result  string         // outcome of the last action
	failure error
}

// stateMsg carries a fresh GET /state, GET /activity and settings.json
// status.
type stateMsg struct {
	st      *admin.State
	err     error
	act     *admin.Activity
	routing routing
}

// tickMsg asks for the next automatic refresh.
type tickMsg struct{}

// doneMsg is the outcome of an action; text may accompany an error.
type doneMsg struct {
	text string
	err  error
}

func newModel(c *admin.Client, h Hooks) *model {
	home, _ := os.UserHomeDir()
	return &model{c: c, h: h, now: time.Now, home: home}
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.refresh(), m.tick()) }

func (m *model) tick() tea.Cmd {
	if m.every <= 0 {
		return nil
	}
	return tea.Tick(m.every, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) refresh() tea.Cmd {
	m.loading = true
	c, status := m.c, m.h.RoutingStatus
	return func() tea.Msg {
		st, err := c.State()
		msg := stateMsg{st: st, err: err}
		if err == nil {
			msg.act, _ = c.Activity() // optional: the lists work without it
		}
		if status != nil {
			msg.routing.on, msg.routing.drift, msg.routing.err = status()
		}
		return msg
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.form.resize(m.width)
		return m, nil
	case stateMsg:
		m.applyState(msg)
		return m, nil
	case tickMsg:
		if m.loading { // a slow daemon: wait for the answer already asked for
			return m, m.tick()
		}
		return m, tea.Batch(m.refresh(), m.tick())
	case doneMsg:
		m.result, m.failure = msg.text, msg.err
		return m, m.refresh()
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case formMode:
			return m, m.updateForm(msg)
		case confirmMode:
			return m, m.updateConfirm(msg)
		}
		return m, m.updateList(msg)
	}
	if m.mode == formMode { // e.g. a paste
		return m, m.form.update(msg)
	}
	return m, nil
}

func (m *model) applyState(msg stateMsg) {
	m.loading = false
	m.stErr = msg.err
	if msg.err == nil {
		if m.st == nil && len(msg.st.Config.Routes) == 0 {
			m.sec = providersSec // nothing to route yet: start where setup starts
		}
		m.st = msg.st
	}
	if msg.act != nil {
		m.act = msg.act
	}
	m.routing = msg.routing
	for s := range m.cursor {
		m.cursor[s] = max(0, min(m.cursor[s], len(m.names(section(s)))-1))
	}
}

// names are the sorted entries of a section.
func (m *model) names(s section) []string {
	if m.st == nil {
		return nil
	}
	if s == providersSec {
		return slices.Sorted(maps.Keys(m.st.Config.Providers))
	}
	return slices.Sorted(maps.Keys(m.st.Config.Routes))
}

func (m *model) selected() (string, bool) {
	names := m.names(m.sec)
	if len(names) == 0 {
		return "", false
	}
	return names[m.cursor[m.sec]], true
}

// selectedProvider is the selected provider, or the provider of the selected
// route: key actions work from either list.
func (m *model) selectedProvider() (string, error) {
	name, ok := m.selected()
	if !ok {
		return "", errors.New("nothing selected")
	}
	if m.sec == providersSec {
		return name, nil
	}
	p := m.st.Config.Routes[name].Provider
	if _, ok := m.st.Config.Providers[p]; !ok {
		return "", fmt.Errorf("route %s names provider %s, which does not exist", name, p)
	}
	return p, nil
}

func (m *model) fail(err error) tea.Cmd {
	m.result, m.failure = "", err
	return nil
}

func (m *model) updateList(msg tea.KeyPressMsg) tea.Cmd {
	switch k := msg.String(); k {
	case "q":
		return tea.Quit
	case "tab", "shift+tab":
		m.sec = 1 - m.sec
	case "up", "k":
		m.cursor[m.sec] = max(0, m.cursor[m.sec]-1)
	case "down", "j":
		m.cursor[m.sec] = max(0, min(m.cursor[m.sec]+1, len(m.names(m.sec))-1))
	case "r":
		return reloadCmd(m.c)
	case "o":
		return toggleCmd(m.h, m.routing.on)
	case "t":
		return m.test()
	case "enter":
		k = "e"
		fallthrough
	case "a", "e", "d", "K", "X":
		if why := m.blocked(k == "K" || k == "X"); why != "" {
			return m.fail(errors.New(why))
		}
		return m.change(k)
	}
	return nil
}

// test sends one small request through the selected route. It changes no
// file, so it stays available while changes are blocked.
func (m *model) test() tea.Cmd {
	name, ok := m.selected()
	switch {
	case m.st == nil:
		return m.fail(errors.New("no state from the daemon yet; press r to retry"))
	case m.sec != routesSec:
		return m.fail(errors.New("t tests a route; switch to Routes with tab"))
	case !ok:
		return m.fail(errors.New("no route to test; press a to add one"))
	}
	m.result, m.failure = "testing route "+name+" ...", nil
	return testRouteCmd(m.c, name)
}

// blocked explains why a change cannot be sent now, or is "". keys selects
// the keys file instead of the config file.
func (m *model) blocked(keys bool) string {
	switch {
	case m.st == nil:
		return "no state from the daemon yet; press r to retry"
	case m.st.LoadError != nil:
		return "changes are disabled while the config has errors; fix the files, then press r to reload"
	}
	i := 0
	if keys {
		i = 1
	}
	if i < len(m.st.Files) && !m.st.Files[i].Writable {
		return fmt.Sprintf("%s is not writable; changes are disabled", m.st.Files[i].Path)
	}
	return ""
}

func (m *model) change(k string) tea.Cmd {
	if k == "a" {
		if m.sec == providersSec {
			return m.open(addProviderForm(m.c))
		}
		return m.openRouteForm("")
	}
	if k == "K" || k == "X" {
		p, err := m.selectedProvider()
		switch {
		case err != nil:
			return m.fail(err)
		case k == "K":
			return m.open(setKeyForm(m.c, p))
		case m.st.Keys[p] == config.KeyMissing:
			return m.fail(fmt.Errorf("provider %s has no key", p))
		}
		c := m.c
		m.ask("remove the key of "+p+"?", func() tea.Cmd { return deleteKeyCmd(c, p) })
		return nil
	}
	name, ok := m.selected()
	if !ok {
		return m.fail(errors.New("nothing selected"))
	}
	switch {
	case k == "e" && m.sec == providersSec:
		return m.open(editProviderForm(m.c, name, m.st.Config.Providers[name].BaseURL))
	case k == "e":
		return m.openRouteForm(name)
	}
	m.askDelete(name)
	return nil
}

// openRouteForm adds a route (name "") or edits the named one.
func (m *model) openRouteForm(name string) tea.Cmd {
	providers := m.names(providersSec)
	if len(providers) == 0 {
		return m.fail(errors.New("add a provider first (tab to Providers, then a)"))
	}
	return m.open(routeForm(m.c, m.h, name, m.st.Config.Routes[name], providers))
}

func (m *model) open(f *form) tea.Cmd {
	m.form, m.mode = f, formMode
	f.resize(m.width)
	return f.focusField(0)
}

func (m *model) closeForm() {
	m.form, m.mode = nil, listMode
}

func (m *model) updateForm(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.closeForm()
		m.result, m.failure = "cancelled", nil
		return nil
	case "enter":
		cmd, err := m.form.submit()
		if err != nil {
			m.form.err = err
			return nil
		}
		m.closeForm()
		m.result, m.failure = "saving...", nil
		return cmd
	}
	return m.form.update(msg)
}

func (m *model) askDelete(name string) {
	c, h := m.c, m.h
	if m.sec == providersSec {
		m.ask("delete provider "+name+" and its key?", func() tea.Cmd { return deleteProviderCmd(c, name) })
		return
	}
	m.ask("delete route "+name+"?", func() tea.Cmd { return deleteRouteCmd(c, h, name) })
}

// ask waits for y before running a destructive action.
func (m *model) ask(prompt string, run func() tea.Cmd) {
	m.prompt, m.pending, m.mode = prompt, run, confirmMode
}

func (m *model) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	run := m.pending
	m.mode, m.pending, m.prompt = listMode, nil, ""
	if msg.String() != "y" {
		m.result, m.failure = "cancelled", nil
		return nil
	}
	return run()
}
