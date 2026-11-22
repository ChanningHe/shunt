package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"shunt/internal/admin"
)

// (a)
func TestAddProvider(t *testing.T) {
	r := newRig(t, "")
	r.wantView("no providers")
	r.press("a")
	r.typ("kimi")
	r.press("tab")
	r.typ("https://api.kimi.example/anthropic")
	r.press("enter")
	if got := r.disk("config.toml"); !strings.Contains(got, "[providers.kimi]") || !strings.Contains(got, "https://api.kimi.example/anthropic") {
		t.Fatalf("config.toml:\n%s", got)
	}
	r.wantView("provider kimi: https://api.kimi.example/anthropic", "missing")
}

func TestFormRequiresEveryField(t *testing.T) {
	r := newRig(t, "")
	r.press("a")
	r.typ("kimi")
	r.press("enter")
	r.wantView("base URL is required")
	if r.writes.Load() != 0 {
		t.Fatal("an incomplete form sent a request")
	}
	r.press("esc")
	r.wantView("no providers")
}

// (b)
func TestSetKeyIsNeverShown(t *testing.T) {
	r := newRig(t, deepseekOnly)
	const secret = "sk-SECRET-0123456789"
	r.press("K")
	r.wantView("deepseek", "hidden")
	for i, ch := range secret {
		r.typ(string(ch))
		if v := r.view(); i >= 3 && strings.Contains(v, secret[:i+1]) {
			t.Fatalf("key echoed after %d characters:\n%s", i+1, v)
		}
	}
	r.press("enter")
	if got := r.disk("keys.toml"); !strings.Contains(got, secret) {
		t.Fatalf("keys.toml:\n%s", got)
	}
	if v := r.view(); strings.Contains(v, "SECRET") {
		t.Fatalf("key shown after submit:\n%s", v)
	}
	r.wantView("key for deepseek set: literal")

	// A file:// value is a reference, stored as typed.
	ref := "file://" + filepath.Join(t.TempDir(), "deepseek.key")
	r.press("K")
	r.typ(ref)
	r.press("enter")
	if got := r.disk("keys.toml"); !strings.Contains(got, ref) || strings.Contains(got, secret) {
		t.Fatalf("keys.toml:\n%s", got)
	}
}

// (c)
func TestAddRoute(t *testing.T) {
	r := newRig(t, deepseekOnly)
	r.press("tab", "a")
	r.typ("coder")
	r.press("tab", "tab") // the provider field offers deepseek, the only one
	r.typ("deepseek-chat")
	r.press("enter")
	rt, ok := r.state().Config.Routes["coder"]
	if !ok || rt.Provider != "deepseek" || rt.Model != "deepseek-chat" {
		t.Fatalf("route = %+v (%v)\n%s", rt, ok, r.disk("config.toml"))
	}
	r.wantView("route coder: model shunt/coder -> deepseek-chat at deepseek", "shunt/coder")
	if r.fake.syncCalls != 0 {
		t.Error("a non-alias route synced settings.json")
	}
}

func TestAddRouteNeedsAProvider(t *testing.T) {
	r := newRig(t, "")
	r.press("tab", "a")
	r.wantView("add a provider first")
}

// (d) switching a route to another API
func TestEditRouteSwitchesProvider(t *testing.T) {
	r := newRig(t, twoProviders) // routes exist: the Routes list has the focus
	r.press("e")
	r.wantView("deepseek-flash")
	r.press("right") // provider: deepseek -> kimi
	r.press("tab", "ctrl+u")
	r.typ("kimi-k2")
	r.press("enter")
	if rt := r.state().Config.Routes["sonnet"]; rt.Provider != "kimi" || rt.Model != "kimi-k2" {
		t.Fatalf("route = %+v\n%s", rt, r.disk("config.toml"))
	}
	r.wantView("route sonnet: model shunt/sonnet -> kimi-k2 at kimi")
}

// Like route set|rm in the CLI, any change to an alias-named route syncs
// settings.json (a no-op there while routing is off); other routes never do.
func TestAliasRouteEditSyncsLikeCLI(t *testing.T) {
	r := newRig(t, twoProviders+"[routes.coder]\nprovider = \"kimi\"\nmodel = \"k2\"\n")
	r.fake.on, r.fake.drift = true, []string{"ANTHROPIC_DEFAULT_SONNET_MODEL is \"\", routes want \"shunt/sonnet\""}
	r.fake.syncChanges = []string{"ANTHROPIC_DEFAULT_SONNET_MODEL=shunt/sonnet"}
	r.send(stateMsg{st: r.state(), routing: routing{on: true, drift: r.fake.drift}})
	r.wantView("does not match the alias routes")

	r.press("down", "e") // sonnet (coder sorts first)
	r.wantView("route sonnet")
	r.press("tab", "ctrl+u")
	r.typ("deepseek-pro")
	r.press("enter")
	if r.fake.syncCalls != 1 {
		t.Fatalf("editing alias route sonnet: %d syncs", r.fake.syncCalls)
	}
	r.wantView("ANTHROPIC_DEFAULT_SONNET_MODEL=shunt/sonnet")

	r.press("up", "e") // coder
	r.wantView("route coder")
	r.press("tab", "ctrl+u")
	r.typ("k3")
	r.press("enter")
	if rt := r.state().Config.Routes["coder"]; rt.Model != "k3" {
		t.Fatalf("coder = %+v", rt)
	}
	r.press("d", "y")
	if r.fake.syncCalls != 1 {
		t.Fatalf("non-alias route changes synced: %d syncs", r.fake.syncCalls)
	}
}

func TestEditProviderBaseURL(t *testing.T) {
	r := newRig(t, deepseekOnly)
	r.press("enter", "ctrl+u") // enter edits, like e
	r.typ("https://api.deepseek.example/v2")
	r.press("enter")
	if got := r.state().Config.Providers["deepseek"].BaseURL; got != "https://api.deepseek.example/v2" {
		t.Fatalf("base_url = %q", got)
	}
}

// (e)
func TestDeleteNeedsConfirmation(t *testing.T) {
	r := newRig(t, twoProviders)
	r.press("tab", "down") // kimi, not referenced by any route
	r.press("d")
	r.wantView("delete provider kimi and its key?")
	r.press("n")
	if _, ok := r.state().Config.Providers["kimi"]; !ok {
		t.Fatal("deleted without confirmation")
	}
	r.wantView("cancelled")
	r.press("d", "y")
	if _, ok := r.state().Config.Providers["kimi"]; ok {
		t.Fatal("not deleted after confirmation")
	}
	r.wantView("provider kimi removed")

	r.press("tab", "d", "y")
	if _, ok := r.state().Config.Routes["sonnet"]; ok {
		t.Fatal("route not deleted")
	}
	r.wantView("route sonnet removed")
	if r.fake.syncCalls != 1 {
		t.Errorf("removing an alias route: %d syncs", r.fake.syncCalls)
	}
}

// (f)
func TestDeleteReferencedProviderShowsConflict(t *testing.T) {
	r := newRig(t, twoProviders)
	before := r.disk("config.toml")
	r.press("tab", "d", "y")
	r.wantView("HTTP 409", "sonnet")
	if r.disk("config.toml") != before {
		t.Fatal("config changed")
	}
}

// (g)
func TestAliasRouteSyncsSettings(t *testing.T) {
	r := newRig(t, deepseekOnly)
	r.fake.syncChanges = []string{"ANTHROPIC_DEFAULT_OPUS_MODEL=shunt/opus"}
	r.press("tab", "a")
	r.typ("opus")
	r.press("tab", "tab")
	r.typ("deepseek-pro")
	r.press("enter")
	if r.fake.syncCalls != 1 {
		t.Fatalf("Sync called %d times", r.fake.syncCalls)
	}
	r.wantView("main thread using the opus alias gets 400 errors", "ANTHROPIC_DEFAULT_OPUS_MODEL=shunt/opus")
}

// (h)
func TestToggleRouting(t *testing.T) {
	r := newRig(t, "")
	r.fake.onDelta = []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:1"}
	r.wantView("routing off")
	r.press("o")
	if r.fake.onCalls != 1 || r.fake.offCalls != 0 {
		t.Fatalf("on %d off %d", r.fake.onCalls, r.fake.offCalls)
	}
	r.wantView("routing on", "ANTHROPIC_BASE_URL=http://127.0.0.1:1")
	r.press("o")
	if r.fake.offCalls != 1 {
		t.Fatalf("off %d", r.fake.offCalls)
	}
	r.wantView("routing off")
	if r.writes.Load() != 0 {
		t.Error("routing toggled through the daemon")
	}
}

var changeKeys = []string{"a", "e", "enter", "d", "K", "X"}

// (i)
func TestLoadErrorBlocksChanges(t *testing.T) {
	r := newRig(t, "[providers.deepseek\n")
	r.wantView("config error")
	for _, tab := range []string{"", "tab"} {
		if tab != "" {
			r.press(tab)
		}
		for _, k := range changeKeys {
			r.press(k)
			if r.m.mode != listMode {
				t.Fatalf("%q opened %v", k, r.m.mode)
			}
			r.wantView("fix the files, then press r")
		}
	}
	if n := r.writes.Load(); n != 0 {
		t.Fatalf("%d change requests sent", n)
	}
	r.press("r") // reload stays allowed: it is the way out
	if r.writes.Load() != 1 {
		t.Fatal("reload not sent")
	}
}

func TestReadOnlyConfigBlocksChanges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	target := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(target, []byte(deepseekOnly), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(target), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(target), 0o755) })
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	r := newRigIn(t, dir)
	r.wantView("read-only")
	r.press("a")
	r.wantView("is not writable")
	r.press("K") // the keys file is writable
	if r.m.mode != formMode {
		t.Fatal("set key blocked by a read-only config file")
	}
	if r.writes.Load() != 0 {
		t.Fatal("request sent")
	}
}

// The TUI writes exactly what the client writes for the same steps.
func TestSameBytesAsClient(t *testing.T) {
	r := newRig(t, "")
	r.press("a")
	r.typ("kimi")
	r.press("tab")
	r.typ("https://api.kimi.example/anthropic")
	r.press("enter", "K")
	r.typ("sk-same-0001")
	r.press("enter", "tab", "a") // still on Providers: the first state had no routes
	r.typ("coder")
	r.press("tab", "tab")
	r.typ("kimi-k2")
	r.press("enter")

	dir := scratchDir(t, "")
	c, _ := startDaemon(t, dir, nil)
	if _, err := c.PutProvider("kimi", "https://api.kimi.example/anthropic"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetKey("kimi", "sk-same-0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutRoute("coder", "kimi", "kimi-k2"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.toml", "keys.toml"} {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := r.disk(name); got != string(want) {
			t.Errorf("%s via TUI:\n%s\nvia client:\n%s", name, got, want)
		}
	}
}

func TestNarrowTerminals(t *testing.T) {
	r := newRig(t, twoProviders)
	for _, w := range []int{59, 30, 8, 1} {
		r.send(tea.WindowSizeMsg{Width: w, Height: 10})
		for _, keys := range [][]string{nil, {"tab"}, {"a"}, {"esc", "d"}, {"n", "tab"}} {
			r.press(keys...)
			if got := widest(r.view()); got > w {
				t.Fatalf("width %d: a line is %d wide:\n%s", w, got, r.view())
			}
		}
	}
}

func TestQuit(t *testing.T) {
	r := newRig(t, "")
	r.press("q")
	if !r.quit {
		t.Fatal("q did not quit")
	}
	r = newRig(t, deepseekOnly)
	r.press("a", "q") // typed into the form
	if r.quit {
		t.Fatal("q in a form quit")
	}
	r.press("ctrl+c")
	if !r.quit {
		t.Fatal("ctrl+c did not quit")
	}
}

// A long list scrolls so the header and the result lines stay on screen.
func TestShortTerminalKeepsCursorAndFooter(t *testing.T) {
	cfg := deepseekOnly
	for i := range 30 {
		cfg += fmt.Sprintf("[routes.r%02d]\nprovider = \"deepseek\"\nmodel = \"m\"\n", i)
	}
	r := newRig(t, cfg)
	r.send(tea.WindowSizeMsg{Width: 100, Height: 15})
	for range 20 {
		r.press("down")
	}
	v := r.view()
	if n := strings.Count(v, "\n") + 1; n > 15 {
		t.Fatalf("%d lines in a 15-line terminal:\n%s", n, v)
	}
	r.wantView("r20", "quit", "routing off", "config: ")
}

func TestAddProviderWithKey(t *testing.T) {
	r := newRig(t, "")
	const secret = "sk-SECRET-0123456789"
	r.press("a")
	r.typ("kimi")
	r.press("tab")
	r.typ("https://api.kimi.example/anthropic")
	r.press("tab")
	r.typ(secret)
	if strings.Contains(r.view(), "SECRET") {
		t.Fatalf("key echoed:\n%s", r.view())
	}
	r.press("enter")
	if got := r.disk("keys.toml"); !strings.Contains(got, secret) {
		t.Fatalf("keys.toml:\n%s", got)
	}
	r.wantView("provider kimi: https://api.kimi.example/anthropic", "key for kimi set: literal")
	if strings.Contains(r.view(), "SECRET") {
		t.Fatalf("key shown after submit:\n%s", r.view())
	}
}

func TestClearKeyNeedsConfirmation(t *testing.T) {
	r := newRig(t, twoProviders)
	r.press("X") // on route sonnet: its provider deepseek has no key
	r.wantView("provider deepseek has no key")
	r.press("K") // sets the key of the route's provider
	r.wantView("key for deepseek")
	r.typ("sk-clear-0001")
	r.press("enter")
	r.press("X")
	r.wantView("remove the key of deepseek?")
	r.press("n")
	if r.state().Keys["deepseek"] != "literal" {
		t.Fatal("key removed without confirmation")
	}
	r.press("tab", "X", "y") // the same from the Providers list
	if got := r.state().Keys["deepseek"]; got != "missing" {
		t.Fatalf("key state = %q", got)
	}
	r.wantView("key for deepseek removed")
}

func TestRouteTest(t *testing.T) {
	r := newRig(t, twoProviders)
	var asked []string
	result := admin.TestResult{OK: true, Status: 200, Ms: 42}
	r.h.Probe = func(_ context.Context, route string) admin.TestResult {
		asked = append(asked, route)
		return result
	}
	r.press("t")
	r.wantView("route sonnet: ok (HTTP 200, 42 ms)")

	result = admin.TestResult{Status: 401, Ms: 7, Message: "invalid api key"}
	r.press("t")
	r.wantView("error: route sonnet: failed (HTTP 401, 7 ms): invalid api key")

	r.press("tab", "t")
	r.wantView("t tests a route")
	if len(asked) != 2 || asked[0] != "sonnet" {
		t.Errorf("probed %v", asked)
	}
	if n := r.writes.Load(); n != 2 { // the two POST .../test calls, nothing else
		t.Errorf("%d non-GET requests", n)
	}
}

// One screen shows what is routed where, which keys are missing and what the
// router did last.
func TestOverviewShowsRoutesProvidersAndActivity(t *testing.T) {
	r := newRig(t, twoProviders+"[routes.ghost]\nprovider = \"deepseek\"\nmodel = \"m\"\n")
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	r.m.now = func() time.Time { return now }
	if _, err := r.c.SetKey("kimi", "sk-kimi-0001"); err != nil {
		t.Fatal(err)
	}
	for i, e := range []admin.Entry{
		{Path: "/v1/messages", Model: "claude-fable-5-2", Target: "anthropic", Status: 200, Ms: 900},
		{Path: "/v1/messages", Model: "shunt/sonnet", Route: "sonnet", Target: "deepseek", Status: 529, Ms: 31},
		{Path: "/v1/messages", Model: "shunt/sonnet", Route: "sonnet", Target: "deepseek", Status: 200, Ms: 1200},
		{Path: "/v1/messages", Model: "shunt/sonnet", Target: "rejected", Status: 400},
	} {
		e.Time = now.Add(time.Duration(i-10) * time.Second)
		r.h.Activity.Record(e)
	}
	r.press("r")
	r.send(tea.WindowSizeMsg{Width: 120, Height: 30}) // the widest route row fits
	r.wantView(
		"Routes (2)", "Providers (2)", "Recent requests",
		"shunt/sonnet", "━━▶ deepseek · deepseek-flash", "2 (1 failed)", "200 8s ago",
		"no key (alias)", // deepseek has no key
		"everything else", "━━▶ anthropic", "unchanged",
		"kimi", "literal", "https://api.kimi.example/anthropic",
		"claude-fable-5-2", "anthropic", "rejected", "529",
	)
}

// Whatever the height, the view fits and the selected row stays visible.
func TestLayoutFitsEveryHeight(t *testing.T) {
	cfg := twoProviders
	for i := range 12 {
		cfg += fmt.Sprintf("[routes.r%02d]\nprovider = \"deepseek\"\nmodel = \"m\"\n", i)
		cfg += fmt.Sprintf("[providers.p%02d]\nbase_url = \"https://p%02d.example/anthropic\"\n", i, i)
	}
	r := newRig(t, cfg)
	for i := range 30 {
		r.h.Activity.Record(admin.Entry{Time: time.Now(), Model: fmt.Sprint("model-", i), Target: "anthropic", Status: 200})
	}
	r.press("r")
	for range 2 {
		for range 8 {
			r.press("down")
		}
		sec, _ := r.m.selected()
		if r.m.sec == routesSec {
			sec = "shunt/" + sec
		}
		for h := 11; h <= 60; h++ { // 11: header, footer with a result line, one list
			r.send(tea.WindowSizeMsg{Width: 100, Height: h})
			v := r.view()
			if n := strings.Count(v, "\n") + 1; n > h {
				t.Fatalf("height %d: %d lines:\n%s", h, n, v)
			}
			if !strings.Contains(v, "> "+sec) {
				t.Fatalf("height %d: selected row %s not shown:\n%s", h, sec, v)
			}
			if h >= 16 && !(strings.Contains(v, "Routes (13)") && strings.Contains(v, "Providers (14)")) {
				t.Fatalf("height %d: a list is missing:\n%s", h, v)
			}
			if strings.Contains(v, "everything else") && strings.Count(v, "shunt/") != 13 {
				t.Fatalf("height %d: everything else shown before every route:\n%s", h, v)
			}
		}
		r.press("tab")
	}
}

// Every route row tells where the route goes without selecting it.
func TestRouteRowsShowTheirTarget(t *testing.T) {
	r := newRig(t, twoProviders+`[routes.coder]
provider = "deepseek"
model = "deepseek-flash"

[routes.cheap]
provider = "kimi"
model = "kimi-k3"
`)
	provider := map[string]string{"cheap": "kimi", "coder": "deepseek", "sonnet": "deepseek"}
	for _, size := range [][2]int{{80, 24}, {50, 12}} {
		r.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		lines := strings.Split(ansi.Strip(r.view()), "\n")
		if len(lines) != size[1] || !strings.Contains(strings.Join(lines[size[1]-3:], "\n"), "quit") {
			t.Fatalf("%v: footer not at the bottom:\n%s", size, r.view())
		}
		rows, selected := 0, false
		for _, l := range lines {
			row, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimPrefix(l, "> "), "  "), "shunt/")
			if !ok {
				continue
			}
			rows++
			selected = selected || strings.HasPrefix(l, "> ")
			name, _, _ := strings.Cut(row, " ")
			if !strings.Contains(row, "──▶ "+provider[name]+" · ") {
				t.Fatalf("%v: route row %q lacks its target", size, l)
			}
		}
		if rows == 0 || !selected {
			t.Fatalf("%v: %d route rows, selected shown %v:\n%s", size, rows, selected, r.view())
		}
	}
	r.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	r.wantView("Routes (3)", "everything else", "──▶ anthropic", "unchanged", "> shunt/cheap", "REQS")
}

// The everything-else row is never selected and never costs a row of either
// list, the selected row or the footer.
func TestRestRowGivesWayFirst(t *testing.T) {
	cfg := twoProviders
	providers := []string{"deepseek", "kimi"}
	for i := range 6 {
		cfg += fmt.Sprintf("[providers.p%02d]\nbase_url = \"https://p%02d.example/anthropic\"\n", i, i)
		providers = append(providers, fmt.Sprintf("p%02d", i))
	}
	r := newRig(t, cfg)
	r.wantView("everything else")
	r.press("down", "down")
	r.wantView("> shunt/sonnet")
	unfocused := "Providers (8)"
	for _, selected := range []string{"shunt/sonnet", "p03"} {
		if selected == "p03" {
			r.press("tab", "down", "down", "down", "down", "down")
			unfocused = "Routes (1)"
		}
		for h := 9; h <= 30; h++ {
			r.send(tea.WindowSizeMsg{Width: 100, Height: h})
			v := r.view()
			rows := map[string]bool{}
			for _, l := range strings.Split(ansi.Strip(v), "\n") {
				name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(l, "> "), "  "), " ")
				rows[name] = true
			}
			if !strings.Contains(v, "> "+selected) || !strings.Contains(v, "quit") {
				t.Fatalf("height %d: selected row or footer missing:\n%s", h, v)
			}
			// 12: header 2, blank, both lists with one row each, rule, footer.
			if strings.Contains(v, unfocused) != (h >= 12) {
				t.Fatalf("height %d: %s shown %v:\n%s", h, unfocused, !(h >= 12), v)
			}
			if !strings.Contains(v, "everything else") {
				continue
			}
			// The unfocused list is capped to a third of the room either way.
			want := []string{"shunt/sonnet"}
			if selected == "p03" {
				want = append(want, providers...)
			}
			for _, name := range want {
				if !rows[name] {
					t.Fatalf("height %d: everything else shown but row %s is not:\n%s", h, name, v)
				}
			}
		}
	}
}

// A request that ended moments ago marks its route live, by glyph as well
// as color.
func TestLiveRouteIsMarked(t *testing.T) {
	r := newRig(t, twoProviders)
	now := time.Now()
	r.m.now = func() time.Time { return now }
	r.h.Activity.Record(admin.Entry{Time: now.Add(-30 * time.Second), Route: "sonnet", Target: "deepseek", Status: 200, Ms: 25000})
	r.press("tab", "r") // the route row is not selected, so its styles show
	r.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	for _, l := range strings.Split(r.view(), "\n") {
		if strings.Contains(l, "shunt/sonnet") {
			if !strings.Contains(ansi.Strip(l), "200 30s ago") || !strings.Contains(l, okStyle.Render("━━▶ deepseek · deepseek-flash")) {
				t.Fatalf("live route row not marked: %q", l)
			}
			return
		}
	}
	t.Fatalf("no route row:\n%s", r.view())
}

// A tick while the daemon has not answered the last refresh asks nothing
// new, so a slow daemon is not buried under stacked requests.
func TestTickDoesNotStackRefreshes(t *testing.T) {
	r := newRig(t, deepseekOnly)
	if _, cmd := r.m.Update(tickMsg{}); cmd == nil || !r.m.loading {
		t.Fatal("an idle tick did not refresh")
	}
	if _, cmd := r.m.Update(tickMsg{}); cmd != nil {
		t.Fatal("a tick during a refresh started another one")
	}
	r.send(stateMsg{st: r.state()})
	if r.m.loading {
		t.Fatal("still loading after the answer")
	}
}

// The view fills the terminal: the key hints and the last result stay on
// its bottom lines, and the requests take the room in between.
func TestViewFillsTheScreen(t *testing.T) {
	r := newRig(t, twoProviders)
	r.wantView("Recent requests", "no requests since the router started")
	for i := range 40 {
		r.h.Activity.Record(admin.Entry{Time: time.Now(), Model: fmt.Sprint("model-", i), Target: "anthropic", Status: 200})
	}
	r.press("r")
	for _, h := range []int{14, 24, 40} {
		r.send(tea.WindowSizeMsg{Width: 100, Height: h})
		for _, keys := range [][]string{nil, {"a"}, {"esc", "d"}, {"n"}} {
			r.press(keys...)
			lines := strings.Split(r.view(), "\n")
			if len(lines) != h {
				t.Fatalf("height %d after %v: %d lines:\n%s", h, keys, len(lines), r.view())
			}
			if last := strings.Join(lines[h-3:], "\n"); !strings.Contains(last, "cancel") && !strings.Contains(last, "quit") {
				t.Fatalf("height %d after %v: footer is not at the bottom:\n%s", h, keys, r.view())
			}
		}
	}
	// 40 lines: header 2, blank, lists 4+1+4, rule, footer 2 leave 22 request rows.
	if v := r.view(); !strings.Contains(v, "model-39") || !strings.Contains(v, "model-18 ") || strings.Contains(v, "model-17 ") {
		t.Fatalf("requests do not fill the room left:\n%s", v)
	}
}

// A config path wider than the terminal keeps the header line to one row:
// its tail, the file name, is what tells the user which config this is.
func TestLongConfigPathStaysOnOneLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), strings.Repeat("long-dir-name-", 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(twoProviders), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRigIn(t, dir)
	lines := strings.Split(ansi.Strip(r.view()), "\n")
	if !strings.HasPrefix(lines[1], "config: …") || !strings.HasSuffix(lines[1], "/config.toml (toml)") || lines[2] != "" {
		t.Fatalf("config path is not cut to one line:\n%s", r.view())
	}
}

// A form taller than the room left is cut, so the footer with its save and
// cancel keys stays on screen.
func TestFormFitsASmallScreen(t *testing.T) {
	r := newRig(t, twoProviders)
	const h = 10
	r.send(tea.WindowSizeMsg{Width: 40, Height: h})
	r.press("a")
	lines := strings.Split(r.view(), "\n")
	if len(lines) != h || !strings.Contains(strings.Join(lines[h-3:], "\n"), "cancel") {
		t.Fatalf("form does not fit %d lines (%d):\n%s", h, len(lines), r.view())
	}
}

func TestFooterKeysAreKeycaps(t *testing.T) {
	r := newRig(t, twoProviders)
	r.wantView(keycap.Render(" q ")+" quit", keycap.Render(" tab ")+" providers")
}

// The bar of the selected row covers every column, however short its text.
func TestSelectionBarSpansTheTable(t *testing.T) {
	r := newRig(t, twoProviders)
	r.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	var head, bar int
	for _, l := range strings.Split(r.view(), "\n") {
		switch s := ansi.Strip(l); {
		case strings.HasPrefix(s, "  ROUTE"):
			head = lipgloss.Width(s)
		case strings.HasPrefix(s, "> "):
			bar = lipgloss.Width(s)
		}
	}
	if head == 0 || bar < head {
		t.Fatalf("selection bar is %d cells, the header %d:\n%s", bar, head, r.view())
	}
}
