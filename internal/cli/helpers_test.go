package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shunt/internal/admin"
	"shunt/internal/router"
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

func writeTestFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type captured struct {
	uri    string
	header http.Header
	body   []byte
}

// upstream is a fake HTTP server (Anthropic or a provider) that records
// every request and answers 200 with an empty message.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []captured
}

func newUpstream(t testing.TB) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, captured{r.RequestURI, r.Header.Clone(), body})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) requests() []captured {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]captured(nil), u.reqs...)
}

// daemon is router.Serve on a real port over a scratch config directory.
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
	d := &daemon{dir: t.TempDir(), anthropic: newUpstream(t), logs: &syncBuf{}}
	if cfg != "" {
		writeTestFile(t, filepath.Join(d.dir, "config.toml"), cfg)
	}
	if keys != "" {
		writeTestFile(t, filepath.Join(d.dir, "keys.toml"), keys)
	}
	log := router.NewLogger(d.logs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.url = "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Serve(ctx, ln, d.dir, d.anthropic.URL, router.OpenStore(d.dir, log), log) }()
	waitFor(t, "admin token", func() bool { _, err := os.Stat(admin.TokenPath(d.dir, d.port())); return err == nil })
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return d
}

// run runs a shunt command against d.
func (d *daemon) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCmd(t, append(args, "--config-dir", d.dir, "-u", d.url)...)
}
