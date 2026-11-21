package router

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"shunt/internal/admin"
)

// TestMain points every default location at a scratch directory, so no
// test can touch the real config, settings.json, admin token or a running
// daemon.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "shunt-test-")
	if err != nil {
		panic(err)
	}
	for env, sub := range map[string]string{
		"XDG_RUNTIME_DIR": "run", "SHUNT_CONFIG_DIR": "config", "XDG_CONFIG_HOME": "xdg",
		"CLAUDE_CONFIG_DIR": "claude", "HOME": "home",
	} {
		os.Setenv(env, filepath.Join(tmp, sub))
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// syncBuf is a log sink safe for concurrent writers and readers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or a deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const contentMark = "CONTENTMARK-sk-secret-value"

// A broken config directory must not take the passthrough down: serve
// starts on a real listener and relays main-thread traffic to Anthropic.
func TestServeDegradedConfigStillPassesThrough(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		loadErr bool
	}{
		{"missing dir", nil, false},
		{"empty dir", map[string]string{}, false},
		{"config syntax error", map[string]string{"config.toml": "listen = '" + contentMark + "\n"}, true},
		{"keys syntax error", map[string]string{"keys.yaml": "a: [" + contentMark + "\n"}, true},
		{"validation error", map[string]string{"config.toml": "[providers.d]\nbase_url = 'http://example.com'\n"}, true},
		{"two formats", map[string]string{"config.toml": "", "config.json": "{}"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shunt")
			if c.files != nil {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for name, content := range c.files {
				writeTestFile(t, filepath.Join(dir, name), content)
			}
			anthropic := newUpstream(t, nil)
			logs := &syncBuf{}
			log := NewLogger(logs)
			store := OpenStore(dir, log)

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- serve(ctx, ln, newProxy(anthropic.URL, store, log), log) }()

			post := func(h http.Header, body []byte) *http.Response {
				req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+messagesPath, bytes.NewReader(body))
				req.Header = h
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp
			}
			if resp := post(ccHeaders(false), fixture(t, "main_request.json")); resp.StatusCode != http.StatusOK || len(anthropic.requests()) != 1 {
				t.Fatalf("passthrough: status %d, anthropic requests %d", resp.StatusCode, len(anthropic.requests()))
			}
			if resp := post(ccHeaders(true), fixture(t, "subagent_request.json")); resp.StatusCode != http.StatusBadRequest || len(anthropic.requests()) != 1 {
				t.Fatalf("shunt/ id without routes: status %d, anthropic requests %d", resp.StatusCode, len(anthropic.requests()))
			}

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("serve: %v", err)
			}
			out := logs.String()
			if got := strings.Count(out, "level=ERROR"); (got == 1) != c.loadErr || got > 1 {
				t.Errorf("error log lines = %d, want load error %v:\n%s", got, c.loadErr, out)
			}
			if strings.Contains(out, "CONTENTMARK") || strings.Contains(out, "sk-secret") {
				t.Errorf("log leaks file content:\n%s", out)
			}
		})
	}
}

func TestReloadOnSignal(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeTestFile(t, cfgPath, "[providers.d]\nbase_url = 'https://d.example'\n")
	logs := &syncBuf{}
	log := NewLogger(logs)
	store := OpenStore(dir, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal)
	go ReloadOn(ctx, sig, store, log)

	writeTestFile(t, cfgPath, "[providers.d]\nbase_url = 'https://d.example'\n[routes.r]\nprovider = 'd'\nmodel = 'm'\n")
	sig <- syscall.SIGHUP
	waitFor(t, "reload", func() bool { return strings.Contains(logs.String(), "config reloaded") })
	if _, ok := store.Load().Config.Routes["r"]; !ok {
		t.Fatal("SIGHUP did not reload the config")
	}

	writeTestFile(t, cfgPath, "[routes.r\n")
	sig <- syscall.SIGHUP
	waitFor(t, "failed reload", func() bool { return strings.Contains(logs.String(), "level=ERROR") })
	if _, ok := store.Load().Config.Routes["r"]; !ok {
		t.Fatal("failed reload dropped the running config")
	}
}

// Nothing under /_shunt reaches Anthropic, whatever the path or method: a
// mistyped admin path would otherwise carry the admin token upstream.
func TestAdminNamespaceNeverReachesUpstream(t *testing.T) {
	d := startDaemon(t, "", "")
	token, err := os.ReadFile(admin.TokenPath(d.dir, d.port()))
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body string) int {
		req, _ := http.NewRequest(method, d.url+path, strings.NewReader(body))
		req.Header = ccHeaders(false)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/_shunt/nope", "/_shunt/api/v1/nope", "/_shunt/api/v1/state/x", "/_shunt/api/v2/state", "/_shunt/v1/messages", "/_shunt/../v1/messages"} {
		if got := send(http.MethodGet, path, ""); got != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, got)
		}
		send(http.MethodPost, path, string(fixture(t, "main_request.json")))
	}
	send(http.MethodGet, "/_shunt", "")
	if n := len(d.anthropic.requests()); n != 0 {
		t.Fatalf("admin namespace reached Anthropic %d times", n)
	}
	if got := send(http.MethodPost, messagesPath, string(fixture(t, "main_request.json"))); got != http.StatusOK || len(d.anthropic.requests()) != 1 {
		t.Fatalf("control passthrough: %d", got)
	}
}

func TestServeTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	logs := &syncBuf{}
	log := NewLogger(logs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	path := admin.TokenPath(dir, port)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, dir, newUpstream(t, nil).URL, OpenStore(dir, log), log) }()
	waitFor(t, "token file", func() bool { _, err := os.Stat(path); return err == nil })
	fi, _ := os.Stat(path)
	token, _ := os.ReadFile(path)
	if fi.Mode().Perm() != 0o600 || len(strings.TrimSpace(string(token))) != 64 {
		t.Errorf("token file mode %v, %d bytes", fi.Mode(), len(token))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token file left after exit: %v", err)
	}
	if strings.Contains(logs.String(), strings.TrimSpace(string(token))) {
		t.Error("token logged")
	}
}

// daemon is Serve on a real port over a scratch config directory.
type daemon struct {
	url, dir  string
	anthropic *upstream
	logs      *syncBuf
}

func (d *daemon) port() string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(d.url, "http://"))
	return port
}

func startDaemon(t *testing.T, cfg, keys string) *daemon {
	t.Helper()
	d := &daemon{dir: t.TempDir(), anthropic: newUpstream(t, nil), logs: &syncBuf{}}
	if cfg != "" {
		writeTestFile(t, filepath.Join(d.dir, "config.toml"), cfg)
	}
	if keys != "" {
		writeTestFile(t, filepath.Join(d.dir, "keys.toml"), keys)
	}
	log := NewLogger(d.logs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.url = "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, d.dir, d.anthropic.URL, OpenStore(d.dir, log), log) }()
	waitFor(t, "admin token", func() bool { _, err := os.Stat(admin.TokenPath(d.dir, d.port())); return err == nil })
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return d
}

// s7-1: paths that clean (and lower-case) into the admin namespace belong
// to it too; none of them may be passed through with the admin token.
func TestNearAdminPathsNeverReachUpstream(t *testing.T) {
	d := startDaemon(t, "", "")
	token, err := os.ReadFile(admin.TokenPath(d.dir, d.port()))
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, path string, body []byte) *http.Response {
		req, err := http.NewRequest(method, d.url+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = ccHeaders(false)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, path := range []string{"//_shunt/api/v1/state", "/_SHUNT/api/v1/state", "/%2F_shunt/api/v1/state",
		"/x/../_shunt/api/v1/state", "/_shunt", "/_Shunt", "//_shunt", "/_shunt/./api/v1/state"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			resp := send(method, path, fixture(t, "main_request.json"))
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently &&
				resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d", method, path, resp.StatusCode)
			}
		}
	}
	if n := len(d.anthropic.requests()); n != 0 {
		t.Fatalf("near-admin paths reached Anthropic %d times", n)
	}
	// Other paths, odd ones included, still pass through byte for byte.
	body := fixture(t, "main_request.json")
	for i, path := range []string{"/v1/messages", "//v1/messages", "/_shuntx/v1"} {
		if resp := send(http.MethodPost, path, body); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		reqs := d.anthropic.requests()
		if len(reqs) != i+1 || reqs[i].uri != path || !bytes.Equal(reqs[i].body, body) {
			t.Errorf("%s: not passed through unchanged (%d requests)", path, len(reqs))
		}
	}
}
