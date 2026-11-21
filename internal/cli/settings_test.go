package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shunt/internal/config"
)

// A settings.json in jq's canonical layout, as in a dotfiles repo.
const sampleSettings = `{
  "attribution": {
    "commit": "",
    "pr": ""
  },
  "env": {
    "CLAUDE_CODE_SUBAGENT_MODEL": "sonnet"
  },
  "permissions": {
    "allow": [
      "Bash(grep:*)",
      "Bash(go:*)"
    ],
    "deny": [],
    "defaultMode": "auto"
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "tool == \"Bash\" && tool_input.command matches \"git commit\"",
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/x.sh <a> & é",
            "timeout": 85
          }
        ]
      }
    ]
  },
  "zeta": 1e3
}
`

// writeLinkedSettings creates dotfiles/settings.json and a symlink to it,
// mirroring ~/.claude/settings.json -> dotfiles.
func writeLinkedSettings(t *testing.T, content string) (link, real string) {
	t.Helper()
	dir := t.TempDir()
	real = filepath.Join(dir, "dotfiles", "settings.json")
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	if err := os.WriteFile(real, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return link, real
}

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(&out, &errOut, args)
	return out.String() + errOut.String(), err
}

const routesCfg = `[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"
[routes.sonnet]
provider = "deepseek"
model = "deepseek-flash"
[routes.haiku]
provider = "deepseek"
model = "deepseek-flash"
[routes.kimi-k2]
provider = "deepseek"
model = "k2"
`

func TestOnOffRoundTripIsByteIdentical(t *testing.T) {
	d := startDaemon(t, routesCfg, "")
	link, real := writeLinkedSettings(t, sampleSettings)
	if out, err := d.run(t, "on", "--settings", link); err != nil {
		t.Fatalf("on: %v %s", err, out)
	}
	sf := mustLoad(t, link)
	want := map[string]string{
		envBaseURL: d.url, aliasVar("sonnet"): "shunt/sonnet", aliasVar("haiku"): "shunt/haiku",
		aliasVar("opus"): "", aliasVar("fable"): "", "ANTHROPIC_DEFAULT_KIMI-K2_MODEL": "",
	}
	for k, v := range want {
		if got, _ := sf.envString(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	var keys []string
	for _, m := range sf.root {
		keys = append(keys, m.Key)
	}
	if strings.Join(keys, ",") != "attribution,env,permissions,hooks,zeta" {
		t.Errorf("key order changed: %v", keys)
	}
	if out, err := runCmd(t, "off", "-s", link); err != nil {
		t.Fatalf("off: %v %s", err, out)
	}
	got, _ := os.ReadFile(real)
	if string(got) != sampleSettings {
		t.Errorf("on+off is not byte-identical:\n%s", got)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(real))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// on follows the routes: stale shunt/ alias values go, values the user
// set themselves stay unless they would be replaced (then --force).
func TestOnSyncsAliasesWithRoutes(t *testing.T) {
	d := startDaemon(t, routesCfg, "")
	link, _ := writeLinkedSettings(t, `{"env":{"`+aliasVar("opus")+`":"shunt/opus","`+aliasVar("fable")+`":"claude-fable-1"}}`+"\n")
	out, err := d.run(t, "on", "-s", link)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	sf := mustLoad(t, link)
	if _, ok := sf.env.get(aliasVar("opus")); ok {
		t.Error("stale shunt/opus kept")
	}
	if v, _ := sf.envString(aliasVar("fable")); v != "claude-fable-1" {
		t.Errorf("foreign fable value touched: %q", v)
	}
	if !strings.Contains(out, "removed "+aliasVar("opus")) || !strings.Contains(out, aliasVar("haiku")+"=shunt/haiku") {
		t.Errorf("on did not report its changes: %s", out)
	}

	link, real := writeLinkedSettings(t, `{"env":{"`+aliasVar("haiku")+`":"claude-haiku-9"}}`+"\n")
	if _, err := d.run(t, "on", "-s", link); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("pinned haiku replaced without --force: %v", err)
	}
	if got, _ := os.ReadFile(real); !strings.Contains(string(got), "claude-haiku-9") {
		t.Error("file changed on refusal")
	}
	if _, err := d.run(t, "on", "-s", link, "--force"); err != nil {
		t.Fatal(err)
	}
	if v, _ := mustLoad(t, link).envString(aliasVar("haiku")); v != "shunt/haiku" {
		t.Errorf("--force: haiku = %q", v)
	}
}

func TestOnRefusesForeignBaseURL(t *testing.T) {
	link, real := writeLinkedSettings(t, `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example"}}`+"\n")
	before, _ := os.ReadFile(real)
	sf, _ := loadSettings(link)
	if _, err := enableRouting(sf, routerURL, nil, false); err == nil {
		t.Fatal("foreign base URL replaced without --force")
	}
	if after, _ := os.ReadFile(real); !bytes.Equal(before, after) {
		t.Error("file changed on refusal")
	}
	if _, err := enableRouting(sf, routerURL, nil, true); err != nil {
		t.Errorf("--force: %v", err)
	}
}

const routerURL = "http://127.0.0.1:27486"

func TestOnRefusesWhenRouterDown(t *testing.T) {
	link, real := writeLinkedSettings(t, "{}\n")
	// Port 1 on loopback: nothing listens there.
	if _, err := runCmd(t, "on", "-s", link, "-u", "http://127.0.0.1:1"); err == nil {
		t.Fatal("on succeeded with the router down")
	}
	if got, _ := os.ReadFile(real); string(got) != "{}\n" {
		t.Errorf("file changed: %s", got)
	}
}

func TestOnCreatesMissingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude", "settings.json")
	if out, err := runCmd(t, "on", "-s", path, "-f"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var got map[string]map[string]string
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil || got["env"][envBaseURL] != "http://"+config.DefaultListen {
		t.Errorf("created file = %s (%v)", data, err)
	}
}

func TestOffWhenAlreadyOffDoesNotWrite(t *testing.T) {
	link, real := writeLinkedSettings(t, sampleSettings)
	info, _ := os.Stat(real)
	out, err := runCmd(t, "off", "-s", link)
	if err != nil || !strings.Contains(out, "already off") {
		t.Fatalf("%v %s", err, out)
	}
	if after, _ := os.Stat(real); !after.ModTime().Equal(info.ModTime()) {
		t.Error("file rewritten although nothing changed")
	}
}

func TestStatusReportsDeadRouterWhenOn(t *testing.T) {
	link, _ := writeLinkedSettings(t, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1","ANTHROPIC_DEFAULT_SONNET_MODEL":"shunt/x"}}`)
	out, err := runCmd(t, "status", "-s", link)
	if err == nil || !strings.Contains(out, "settings: on") || !strings.Contains(out, "router: unreachable") {
		t.Errorf("status = %q, %v; want on + unreachable + error", out, err)
	}
	link, _ = writeLinkedSettings(t, sampleSettings)
	if out, err := runCmd(t, "status", "-s", link, "-u", "http://127.0.0.1:1"); err != nil || !strings.Contains(out, "settings: off") {
		t.Errorf("status (off) = %q, %v", out, err)
	}
}

func TestSettingsRejectsMalformed(t *testing.T) {
	for _, content := range []string{"", "[]", `{"env":[]}`, `{"a":1} {}`, `{"a":`} {
		link, _ := writeLinkedSettings(t, content)
		if _, err := loadSettings(link); err == nil {
			t.Errorf("loadSettings(%q): want error", content)
		}
	}
}

// Review 2 M-1: off/on must not destroy values that were never ours.
func TestOffKeepsForeignValues(t *testing.T) {
	const foreign = `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.corp","ANTHROPIC_DEFAULT_SONNET_MODEL":"claude-sonnet-5-5"}}` + "\n"
	link, real := writeLinkedSettings(t, foreign)
	if _, err := runCmd(t, "off", "-s", link); err == nil {
		t.Error("off removed/accepted foreign values without --force")
	}
	if got, _ := os.ReadFile(real); string(got) != foreign {
		t.Errorf("file changed: %s", got)
	}
	if out, err := runCmd(t, "status", "-s", link); err != nil || !strings.Contains(out, "settings: off") {
		t.Errorf("status with a foreign gateway = %q, %v; want off, no error", out, err)
	}
	if _, err := runCmd(t, "off", "-s", link, "--force"); err != nil {
		t.Errorf("off --force: %v", err)
	}
}

func TestOnRefusesForeignSonnetModel(t *testing.T) {
	link, _ := writeLinkedSettings(t, `{"env":{"ANTHROPIC_DEFAULT_SONNET_MODEL":"claude-sonnet-5-5"}}`+"\n")
	sf, _ := loadSettings(link)
	want := map[string]string{aliasVar("sonnet"): "shunt/sonnet"}
	if _, err := enableRouting(sf, routerURL, want, false); err == nil {
		t.Error("pinned sonnet model overwritten without --force")
	}
	if _, err := enableRouting(sf, routerURL, map[string]string{}, false); err != nil {
		t.Errorf("pinned sonnet model without a sonnet route: %v", err)
	}
}

// off is the escape hatch: it never needs the daemon, removes shunt's
// values and leaves models the user pinned.
func TestOffWithoutDaemon(t *testing.T) {
	content := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1","` + aliasVar("sonnet") + `":"shunt/sonnet","` +
		aliasVar("fable") + `":"shunt/fable","` + aliasVar("opus") + `":"claude-opus-5-5"}}` + "\n"
	link, _ := writeLinkedSettings(t, content)
	if out, err := runCmd(t, "off", "-s", link); err != nil {
		t.Fatalf("off: %v %s", err, out)
	}
	sf := mustLoad(t, link)
	for _, k := range []string{envBaseURL, aliasVar("sonnet"), aliasVar("fable")} {
		if _, ok := sf.env.get(k); ok {
			t.Errorf("%s kept", k)
		}
	}
	if v, _ := sf.envString(aliasVar("opus")); v != "claude-opus-5-5" {
		t.Errorf("pinned opus removed: %q", v)
	}
}

// Review 2 M-2: a dangling symlink must not be replaced by a regular file.
func TestDanglingSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	_ = os.Symlink(filepath.Join(dir, "dotfiles", "missing.json"), link)
	if _, err := runCmd(t, "on", "-s", link, "-f"); err == nil {
		t.Error("on through a dangling symlink succeeded")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("dangling symlink was replaced")
	}
}

func TestSettingsStrictParsing(t *testing.T) {
	for _, content := range []string{
		`{"env":{"A":"1"},"env":{"B":"2"}}`, // duplicate key: JS keeps the last one, we would edit the first
		`{"env":{"A":"1","A":"2"}}`,
		"{\"a\":1}\n}\n,{\"b\":2}", // invalid trailing data
	} {
		link, _ := writeLinkedSettings(t, content)
		if _, err := loadSettings(link); err == nil {
			t.Errorf("loadSettings(%q): want error", content)
		}
	}
	link, _ := writeLinkedSettings(t, `{"env":null}`)
	if out, err := runCmd(t, "on", "-s", link, "-f"); err != nil {
		t.Errorf("env: null should be treated as empty: %v %s", err, out)
	}
}

// Values shunt writes: a loopback router URL, or any shunt/ model id. The
// pre-config-dir ds/ prefix is no longer ours.
func TestOurs(t *testing.T) {
	cases := []struct {
		key, val string
		want     bool
	}{
		{envBaseURL, "http://127.0.0.1:27486", true},
		{envBaseURL, "https://gateway.corp", false},
		{aliasVar("sonnet"), "shunt/sonnet", true},
		{aliasVar("sonnet"), "shunt/anything-else", true},
		{aliasVar("sonnet"), "shunt/", true},
		{aliasVar("opus"), "shunt/opus", true},
		{aliasVar("haiku"), "shunt/haiku", true},
		{aliasVar("fable"), "shunt/fable", true},
		{aliasVar("sonnet"), "ds/deepseek-flash", false},
		{aliasVar("sonnet"), "claude-sonnet-5-5", false},
		{aliasVar("sonnet"), "SHUNT/sonnet", false},
		{"OTHER", "shunt/sonnet", false},
		{"ANTHROPIC_DEFAULT_KIMI_MODEL", "shunt/kimi", false},
	}
	for _, c := range cases {
		if got := ours(c.key, c.val); got != c.want {
			t.Errorf("ours(%s, %q) = %v, want %v", c.key, c.val, got, c.want)
		}
	}
}

func TestStatusShowsDaemonState(t *testing.T) {
	d := startDaemon(t, routesCfg+"[providers.kimi]\nbase_url = \"https://k.example\"\n", `deepseek = "sk-STATUSSECRET"`)
	link, _ := writeLinkedSettings(t, "{}\n")
	if out, err := d.run(t, "on", "-s", link); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	out := mustRun(t, d, "status", "-s", link)
	for _, want := range []string{"settings: on", "router: reachable", "shunt/kimi-k2", "deepseek-flash", "literal", "missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "STATUSSECRET") || strings.Contains(out, "shunt on") {
		t.Errorf("status leaks a key or reports drift without any:\n%s", out)
	}

	// Drift: a route added behind settings.json's back (e.g. in the web UI).
	writeTestFile(t, filepath.Join(d.dir, "config.toml"), routesCfg+"[routes.opus]\nprovider = \"deepseek\"\nmodel = \"m\"\n")
	mustRun(t, d, "reload")
	if out := mustRun(t, d, "status", "-s", link); !strings.Contains(out, aliasVar("opus")) || !strings.Contains(out, "shunt on") {
		t.Errorf("drift not reported:\n%s", out)
	}

	writeTestFile(t, filepath.Join(d.dir, "config.toml"), "[routes\n")
	_, _ = d.run(t, "reload")
	if out := mustRun(t, d, "status", "-s", link); !strings.Contains(out, "config error") {
		t.Errorf("load error not reported:\n%s", out)
	}
}
