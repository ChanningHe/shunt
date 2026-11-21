package admin

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenPathAndWrite(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	if got := TokenPath(cfgDir, "27486"); got != filepath.Join(rt, "shunt", "token-27486") {
		t.Errorf("runtime path = %s", got)
	}
	if err := os.Mkdir(filepath.Join(rt, "shunt"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A planted symlink is replaced, not followed.
	victim := filepath.Join(t.TempDir(), "victim")
	writeFile(t, victim, "keep")
	if err := os.Symlink(victim, filepath.Join(rt, "shunt", "token-27486")); err != nil {
		t.Fatal(err)
	}
	path, err := WriteToken(cfgDir, "27486", "tok")
	if err != nil || path != TokenPath(cfgDir, "27486") {
		t.Fatalf("WriteToken = %s, %v", path, err)
	}
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Dir(path), 0o700)
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Error("symlink target overwritten")
	}
	if tok, err := readToken(path); tok != "tok" || err != nil {
		t.Errorf("readToken = %q, %v", tok, err)
	}

	for _, rt := range []string{"", "relative/dir"} {
		t.Setenv("XDG_RUNTIME_DIR", rt)
		want := filepath.Join(cfgDir, ".admin-token-9")
		if got := TokenPath(cfgDir, "9"); got != want {
			t.Errorf("XDG_RUNTIME_DIR=%q: path = %s", rt, got)
		}
		if path, err := WriteToken(cfgDir, "9", "tok"); err != nil || path != want {
			t.Fatalf("fallback WriteToken = %s, %v", path, err)
		}
		assertMode(t, want, 0o600)
		assertMode(t, cfgDir, 0o700)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("%s mode %v, want %v", path, fi.Mode(), want)
	}
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	b, _ := NewToken()
	if err != nil || len(a) != 64 || a == b {
		t.Errorf("tokens %q %q %v", a, b, err)
	}
}

// startDaemon serves g.h on a real port and writes its token where Dial
// looks for it.
func startDaemon(t *testing.T, g *rig) *httptest.Server {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	if _, err := WriteToken(g.dir, u.Port(), testToken); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestClientRoundTrip(t *testing.T) {
	g := newRig(t, baseCfg, "")
	srv := startDaemon(t, g)
	c, err := Dial(g.dir, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL() != srv.URL {
		t.Errorf("URL = %s", c.URL())
	}
	if cleared, err := c.PutProvider("kimi", "https://k.example"); err != nil || cleared {
		t.Fatalf("PutProvider: %v %v", cleared, err)
	}
	if state, err := c.SetKey("kimi", "sk-1"); err != nil || state != "literal" {
		t.Fatalf("SetKey: %q %v", state, err)
	}
	if cleared, err := c.PutProvider("kimi", "https://k2.example"); err != nil || !cleared {
		t.Fatalf("PutProvider host change: %v %v", cleared, err)
	}
	if res, err := c.PutRoute("opus", "kimi", "k2"); err != nil || !res.NeedsSync || res.SyncCommand != "shunt on" {
		t.Fatalf("PutRoute: %+v %v", res, err)
	}
	st, err := c.State()
	if err != nil || st.Config.Routes["opus"].Provider != "kimi" || st.Keys["kimi"] != "missing" || st.Listen != testHost {
		t.Fatalf("State: %+v %v", st, err)
	}
	if res, err := c.DeleteRoute("opus"); err != nil || !res.NeedsSync {
		t.Fatalf("DeleteRoute: %+v %v", res, err)
	}
	if _, err := c.SetKey("kimi", "sk-2"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteKey("kimi"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteProvider("kimi"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	if wc, err := c.WebCode(); err != nil || wc.Code == "" || wc.ExpiresIn != 60 {
		t.Fatalf("WebCode: %+v %v", wc, err)
	}

	var apiErr *Error
	err = c.DeleteProvider("deepseek")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != codeConflict || !strings.Contains(apiErr.Message, "sonnet") {
		t.Errorf("conflict error = %#v", err)
	}
	if _, err := c.SetKey("nope", "sk-1"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("not found error = %#v", err)
	}
	if _, err := c.SetKey("deepseek", "sk-ant-x"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("bad request error = %#v", err)
	}
	c.token = "wrong"
	if _, err := c.State(); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("unauthorized error = %#v", err)
	}
}

func TestDialLocatesFromConfig(t *testing.T) {
	g := newRig(t, "", "")
	srv := startDaemon(t, g)
	addr := strings.TrimPrefix(srv.URL, "http://")
	writeFile(t, filepath.Join(g.dir, "config.toml"), "listen = '"+addr+"'\n")
	before, _ := os.ReadDir(g.dir)
	c, err := Dial(g.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.State(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadDir(g.dir); len(after) != len(before) {
		t.Errorf("Dial wrote to the config dir: %v", after)
	}
}

func TestDialErrors(t *testing.T) {
	g := newRig(t, "", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfgPath := filepath.Join(g.dir, "config.toml")

	// No token: the daemon is not running on that port.
	_, err := Dial(g.dir, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "systemctl --user start shunt") || !strings.Contains(err.Error(), cfgPath) {
		t.Errorf("no token: %v", err)
	}

	// Token left behind by a dead daemon: the call fails with the same hint.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(addr)
	if _, err := WriteToken(g.dir, port, testToken); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(g.dir, "http://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.State()
	if err == nil || !strings.Contains(err.Error(), "not reachable") || !strings.Contains(err.Error(), cfgPath) ||
		strings.Contains(err.Error(), testToken) {
		t.Errorf("dead daemon: %v", err)
	}

	for _, bad := range []string{"http://10.0.0.1:27486", "https://127.0.0.1:1", "http://127.0.0.1:1/x"} {
		if _, err := Dial(g.dir, bad); err == nil {
			t.Errorf("Dial(%s) accepted", bad)
		}
	}
	if _, err := Dial("", "http://127.0.0.1:1"); err == nil {
		t.Error("Dial without a config dir accepted")
	}
}
