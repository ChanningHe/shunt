package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shunt/internal/config"
)

const (
	testToken = "test-admin-token-0123456789abcdef"
	testHost  = "127.0.0.1:27486"
)

type rig struct {
	h    *Handler
	dir  string
	logs *bytes.Buffer
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newRig opens a store over config/keys TOML ("" = no file).
func newRig(t *testing.T, cfg, keys string) *rig {
	t.Helper()
	dir := t.TempDir()
	if cfg != "" {
		writeFile(t, filepath.Join(dir, "config.toml"), cfg)
	}
	if keys != "" {
		writeFile(t, filepath.Join(dir, "keys.toml"), keys)
	}
	logs := &bytes.Buffer{}
	h := New(config.Open(dir), testToken, testHost, slog.New(slog.NewTextHandler(logs, nil)))
	return &rig{h: h, dir: dir, logs: logs}
}

// req builds a request that passes every check: local Host, bearer token
// and, for non-GET methods, a JSON content type.
func req(method, path, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = testHost
	r.Header.Set("Authorization", "Bearer "+testToken)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func (g *rig) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, r)
	return rec
}

func (g *rig) call(t *testing.T, method, call, body string) *httptest.ResponseRecorder {
	t.Helper()
	return g.serve(req(method, apiPrefix+call, body))
}

// errCode decodes {"error":{"code":...}}.
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Code == "" || e.Error.Message == "" {
		t.Fatalf("not an error body (%v): %s", err, rec.Body)
	}
	return e.Error.Code
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func (g *rig) disk(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const baseCfg = `[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"
top_keys = ["model", "messages", "custom"]

[routes.sonnet]
provider = "deepseek"
model = "deepseek-flash"
`
