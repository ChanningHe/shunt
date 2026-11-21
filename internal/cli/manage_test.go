package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const deepseekCfg = `[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"
`

// withStdin feeds the next command's stdin.
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := stdin
	stdin = f
	t.Cleanup(func() { stdin = old; f.Close() })
}

func mustRun(t *testing.T, d *daemon, args ...string) string {
	t.Helper()
	out, err := d.run(t, args...)
	if err != nil {
		t.Fatalf("shunt %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func readDisk(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProviderCommands(t *testing.T) {
	d := startDaemon(t, "", "")
	mustRun(t, d, "provider", "add", "deepseek", "-b", "https://api.deepseek.com/anthropic")
	mustRun(t, d, "provider", "add", "kimi", "--base-url", "https://api.kimi.example/anthropic", "--key-file", "rel/kimi.key")

	withStdin(t, "sk-STDINSECRET\n")
	out := mustRun(t, d, "provider", "set-key", "deepseek")
	if strings.Contains(out, "STDINSECRET") {
		t.Errorf("set-key echoed the key: %s", out)
	}
	keys := readDisk(t, filepath.Join(d.dir, "keys.toml"))
	wd, _ := os.Getwd()
	if !strings.Contains(keys, "sk-STDINSECRET") || !strings.Contains(keys, "file://"+filepath.Join(wd, "rel/kimi.key")) {
		t.Errorf("keys.toml:\n%s", keys)
	}

	out = mustRun(t, d, "provider", "list")
	lines := matchLines(t, out,
		`^NAME\s+KEY\s+BASE_URL$`,
		`^deepseek\s+literal\s+https://api.deepseek.com/anthropic$`,
		`^kimi\s+missing\s+https://api.kimi.example/anthropic$`)
	if strings.Index(lines[1], "literal") != strings.Index(lines[0], "KEY") {
		t.Errorf("columns not aligned:\n%s", out)
	}
	if strings.Contains(out, "STDINSECRET") || strings.Contains(out, "kimi.key") {
		t.Errorf("list shows key material:\n%s", out)
	}

	out = mustRun(t, d, "provider", "add", "deepseek", "-b", "https://evil.example")
	if !strings.Contains(out, "set-key deepseek") {
		t.Errorf("host change did not say the key was cleared: %s", out)
	}
	mustRun(t, d, "provider", "rm", "kimi")
	if cfg := readDisk(t, filepath.Join(d.dir, "config.toml")); strings.Contains(cfg, "kimi") {
		t.Errorf("rm left the provider:\n%s", cfg)
	}

	for _, args := range [][]string{
		{"provider", "set-key", "deepseek", "--key", "sk-x"},
		{"provider", "set-key", "deepseek", "sk-x"},
		{"provider", "add", "x"},
		{"provider", "rm"},
		{"provider", "nope"},
	} {
		if _, err := d.run(t, args...); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	withStdin(t, "\n")
	if _, err := d.run(t, "provider", "set-key", "deepseek"); err == nil {
		t.Error("empty stdin key accepted")
	}
	withStdin(t, "sk-ant-api03-SECRET\n")
	if out, err := d.run(t, "provider", "set-key", "deepseek"); err == nil || strings.Contains(out+err.Error(), "SECRET") {
		t.Errorf("Anthropic key: err=%v out=%s", err, out)
	}
}

func TestRouteCommands(t *testing.T) {
	d := startDaemon(t, deepseekCfg, "")
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeTestFile(t, settings, "{}\n")

	out := mustRun(t, d, "route", "set", "kimi-k2", "deepseek", "deepseek-chat", "-s", settings)
	if strings.Contains(out, "400") {
		t.Errorf("non-alias route warned: %s", out)
	}
	out = mustRun(t, d, "route", "set", "sonnet", "deepseek", "deepseek-flash", "-s", settings)
	if !strings.Contains(out, "sonnet alias") || !strings.Contains(out, "400") {
		t.Errorf("alias route did not warn about the main thread: %s", out)
	}
	if got := readDisk(t, settings); got != "{}\n" {
		t.Errorf("routing off, but settings changed: %s", got)
	}

	matchLines(t, mustRun(t, d, "route", "list"),
		`^NAME\s+PROVIDER\s+MODEL\s+MODEL_ID$`,
		`^kimi-k2\s+deepseek\s+deepseek-chat\s+shunt/kimi-k2$`,
		`^sonnet\s+deepseek\s+deepseek-flash\s+shunt/sonnet$`)
	mustRun(t, d, "route", "rm", "kimi-k2", "-s", settings)
	if strings.Contains(mustRun(t, d, "route", "list"), "kimi-k2") {
		t.Error("route rm did not remove the route")
	}
	for _, args := range [][]string{
		{"route", "set", "x", "nope", "m"},
		{"route", "set", "x", "deepseek"},
		{"route", "rm", "nope"},
	} {
		if _, err := d.run(t, args...); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

// With routing on, route set|rm of an alias keeps settings.json in step in
// the same command.
func TestRouteSyncsSettingsWhenOn(t *testing.T) {
	d := startDaemon(t, deepseekCfg, "")
	link, real := writeLinkedSettings(t, sampleSettings)
	mustRun(t, d, "on", "-s", link)
	before := readDisk(t, real)

	out := mustRun(t, d, "route", "set", "haiku", "deepseek", "m", "-s", link)
	sf, _ := loadSettings(link)
	if v, _ := sf.envString(aliasVar("haiku")); v != "shunt/haiku" || !strings.Contains(out, aliasVar("haiku")) || !strings.Contains(out, "restart") {
		t.Errorf("haiku not synced (%q): %s", v, out)
	}
	mustRun(t, d, "route", "set", "haiku", "deepseek", "m2", "-s", link) // no alias change: no rewrite
	mustRun(t, d, "route", "rm", "haiku", "-s", link)
	if got := readDisk(t, real); got != before {
		t.Errorf("after set+rm, settings differ:\n%s\nwant\n%s", got, before)
	}

	// A value the user pinned is not overwritten.
	sf, _ = loadSettings(link)
	_ = sf.setEnv(aliasVar("opus"), "claude-opus-5-5")
	_ = sf.save()
	if _, err := d.run(t, "route", "set", "opus", "deepseek", "m", "-s", link); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("foreign opus value: err = %v", err)
	}
	if v, _ := mustLoad(t, link).envString(aliasVar("opus")); v != "claude-opus-5-5" {
		t.Errorf("foreign value replaced: %q", v)
	}
}

func mustLoad(t *testing.T, path string) *settingsFile {
	t.Helper()
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	return sf
}

func TestReloadAndWeb(t *testing.T) {
	d := startDaemon(t, deepseekCfg, "")
	writeTestFile(t, filepath.Join(d.dir, "config.toml"), deepseekCfg+"[routes.opus]\nprovider = \"deepseek\"\nmodel = \"m\"\n")
	mustRun(t, d, "reload")
	if !strings.Contains(mustRun(t, d, "route", "list"), "shunt/opus") {
		t.Error("reload did not pick up the file")
	}
	writeTestFile(t, filepath.Join(d.dir, "config.toml"), "[routes\n")
	if _, err := d.run(t, "reload"); err == nil {
		t.Error("reload of a broken file succeeded")
	}

	out := mustRun(t, d, "web", "--no-open")
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(d.url) + `/_shunt/#code=[0-9a-f]{64}$`).MatchString(strings.Split(out, "\n")[0]) {
		t.Errorf("web output: %s", out)
	}
}

func TestConfigCommandsWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"provider", "list"}, {"route", "list"}, {"reload"}, {"web", "--no-open"}} {
		_, err := runCmd(t, append(args, "--config-dir", dir, "-u", "http://127.0.0.1:1")...)
		if err == nil || !strings.Contains(err.Error(), "systemctl --user start shunt") || !strings.Contains(err.Error(), filepath.Join(dir, "config.toml")) {
			t.Errorf("%q: err = %v", args, err)
		}
	}
	if _, err := runCmd(t, "route", "list", "--config-dir", dir, "-u", "http://10.0.0.1:27486"); err == nil {
		t.Error("non-local URL accepted")
	}
	for _, args := range [][]string{{"provider", "--help"}, {"route", "set", "-h"}, {"web", "--help"}, {"provider", "set-key", "-h"}} {
		out, err := runCmd(t, args...)
		if err != nil || !strings.Contains(out, "usage: shunt") {
			t.Errorf("%q: %v %s", args, err, out)
		}
		if strings.Contains(out, "set-key") && strings.Contains(out, "--key ") {
			t.Errorf("a --key option exists:\n%s", out)
		}
	}
}

// matchLines checks out line by line against patterns and returns the lines.
func matchLines(t *testing.T, out string, patterns ...string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(patterns) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(patterns), out)
	}
	for i, p := range patterns {
		if !regexp.MustCompile(p).MatchString(lines[i]) {
			t.Errorf("line %d %q does not match %s", i, lines[i], p)
		}
	}
	return lines
}

func TestRouteTestAndActivityCommands(t *testing.T) {
	provider := newUpstream(t)
	d := startDaemon(t, "", "")
	mustRun(t, d, "provider", "add", "local", "-b", provider.URL)
	mustRun(t, d, "route", "set", "coder", "local", "some-model")

	if out := mustRun(t, d, "activity"); !strings.HasPrefix(out, "no requests since the router started") {
		t.Errorf("activity before any request: %s", out)
	}
	out, err := d.run(t, "route", "test", "coder")
	if err == nil || !strings.Contains(err.Error(), "route coder: failed: key for provider local is missing") {
		t.Fatalf("test without a key: %v\n%s", err, out)
	}

	withStdin(t, "sk-STDINSECRET\n")
	mustRun(t, d, "provider", "set-key", "local")
	if out := mustRun(t, d, "route", "test", "coder"); !regexp.MustCompile(`^route coder: ok \(HTTP 200, \d+ ms\)\n$`).MatchString(out) {
		t.Errorf("test output: %q", out)
	}
	if got := provider.requests(); len(got) != 1 || got[0].header.Get("X-Api-Key") != "sk-STDINSECRET" {
		t.Errorf("provider saw %d requests", len(got))
	}
	if _, err := d.run(t, "route", "test", "nope"); err == nil || !strings.Contains(err.Error(), `no route "nope"`) {
		t.Errorf("unknown route: %v", err)
	}

	if out := mustRun(t, d, "provider", "rm-key", "local"); out != "key for local removed\n" {
		t.Errorf("rm-key output: %q", out)
	}
	if out := mustRun(t, d, "provider", "list"); !regexp.MustCompile(`(?m)^local\s+missing\s`).MatchString(out) {
		t.Errorf("key still set:\n%s", out)
	}
	// A routed subagent request and a main-thread passthrough, as Claude
	// Code would send them.
	send := func(model string, subagent bool) {
		quoted, _ := json.Marshal(model)
		body := `{"model":` + string(quoted) + `,"max_tokens":8,"messages":[{"role":"user","content":"hi"}],` +
			`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322; cc_is_subagent=` + strconv.FormatBool(subagent) + `;"}]}`
		req, _ := http.NewRequest(http.MethodPost, d.url+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if subagent {
			req.Header.Set("X-Claude-Code-Agent-Id", "a1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	send("shunt/coder", true)
	send("claude-opus-5-5\x1b[31m", false)
	out = mustRun(t, d, "activity")
	matchLines(t, out,
		`^TIME\s+TARGET\s+STATUS\s+MS\s+MODEL\s+PATH$`,
		`^\d\d:\d\d:\d\d\s+local\s+\d+\s+\d+\s+shunt/coder\s+/v1/messages$`,
		`^\d\d:\d\d:\d\d\s+anthropic\s+200\s+\d+\s+claude-opus-5-5 \[31m\s+/v1/messages$`)
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence printed: %q", out)
	}
	if out := mustRun(t, d, "activity", "-n", "1"); strings.Contains(out, "shunt/coder") || !strings.Contains(out, "claude-opus") {
		t.Errorf("-n 1 should show only the newest request:\n%s", out)
	}
	if _, err := d.run(t, "activity", "-n", "x"); err == nil {
		t.Error("activity -n x succeeded")
	}
}
