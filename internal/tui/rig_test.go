package tui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const (
	twoProviders = `[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"

[providers.kimi]
base_url = "https://api.kimi.example/anthropic"

[routes.sonnet]
provider = "deepseek"
model = "deepseek-flash"
`
	deepseekOnly = `[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"
`
)

// fakeHooks records the local actions the TUI asks for.
type fakeHooks struct {
	on                   bool
	onCalls, offCalls    int
	syncCalls            int
	syncChanges, onDelta []string
	drift                []string
}

func (f *fakeHooks) hooks() Hooks {
	return Hooks{
		RoutingStatus: func() (bool, []string, error) { return f.on, f.drift, nil },
		On:            func() ([]string, error) { f.onCalls++; f.on = true; return f.onDelta, nil },
		Off:           func() (bool, error) { f.offCalls++; f.on = false; return true, nil },
		Sync:          func() ([]string, error) { f.syncCalls++; return f.syncChanges, nil },
	}
}

// rig drives a model against a real admin.Handler over a scratch config
// directory, running every command synchronously.
type rig struct {
	t      *testing.T
	dir    string
	c      *admin.Client
	h      *admin.Handler
	m      *model
	fake   *fakeHooks
	writes atomic.Int32 // non-GET API requests
	quit   bool
}

// startDaemon serves an admin.Handler over dir and dials it.
func startDaemon(t *testing.T, dir string, writes *atomic.Int32) (*admin.Client, *admin.Handler) {
	t.Helper()
	token, err := admin.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	h := admin.New(config.Open(dir), token, "127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && writes != nil {
			writes.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if _, err := admin.WriteToken(dir, u.Port(), token); err != nil {
		t.Fatal(err)
	}
	c, err := admin.Dial(dir, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c, h
}

func scratchDir(t *testing.T, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newRig(t *testing.T, cfg string) *rig {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	return newRigIn(t, scratchDir(t, cfg))
}

func newRigIn(t *testing.T, dir string) *rig {
	t.Helper()
	r := &rig{t: t, dir: dir, fake: &fakeHooks{}}
	r.c, r.h = startDaemon(t, dir, &r.writes)
	r.m = newModel(r.c, r.fake.hooks())
	r.run(r.m.Init())
	r.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	return r
}

func (r *rig) send(msg tea.Msg) {
	r.t.Helper()
	_, cmd := r.m.Update(msg)
	r.run(cmd)
}

func (r *rig) run(cmd tea.Cmd) {
	r.t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			r.run(c)
		}
	case tea.QuitMsg:
		r.quit = true
	default:
		r.send(msg)
	}
}

var specialKeys = map[string]tea.KeyPressMsg{
	"enter":  {Code: tea.KeyEnter},
	"tab":    {Code: tea.KeyTab},
	"esc":    {Code: tea.KeyEscape},
	"up":     {Code: tea.KeyUp},
	"down":   {Code: tea.KeyDown},
	"left":   {Code: tea.KeyLeft},
	"right":  {Code: tea.KeyRight},
	"ctrl+u": {Code: 'u', Mod: tea.ModCtrl},
	"ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
}

// press sends named keys ("enter", "tab", ...) or single characters.
func (r *rig) press(keys ...string) {
	r.t.Helper()
	for _, k := range keys {
		msg, ok := specialKeys[k]
		if !ok {
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		r.send(msg)
	}
}

// typ types text one key at a time.
func (r *rig) typ(s string) {
	r.t.Helper()
	for _, ch := range s {
		r.send(tea.KeyPressMsg{Code: ch, Text: string(ch)})
	}
}

func (r *rig) view() string { return r.m.View().Content }

func (r *rig) disk(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *rig) wantView(subs ...string) {
	r.t.Helper()
	v := r.view()
	for _, s := range subs {
		if !strings.Contains(v, s) {
			r.t.Fatalf("view lacks %q:\n%s", s, v)
		}
	}
}

func (r *rig) state() *admin.State {
	r.t.Helper()
	st, err := r.c.State()
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

// widest is the display width of the widest line of s.
func widest(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		w = max(w, lipgloss.Width(line))
	}
	return w
}
