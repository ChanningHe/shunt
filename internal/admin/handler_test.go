package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetState(t *testing.T) {
	g := newRig(t, baseCfg, `deepseek = "sk-literal-1"`)
	rec := g.call(t, http.MethodGet, "state", "")
	expect(t, rec, http.StatusOK)
	st := decode[State](t, rec)
	if st.Config.Routes["sonnet"].Model != "deepseek-flash" || st.Config.Providers["deepseek"].BaseURL == "" {
		t.Errorf("config: %+v", st.Config)
	}
	if st.Keys["deepseek"] != "literal" || st.LoadError != nil || st.Listen != testHost {
		t.Errorf("state: %+v", st)
	}
	if len(st.Files) != 2 || st.Files[0].Path != filepath.Join(g.dir, "config.toml") || st.Files[1].Format != "toml" ||
		!st.Files[0].Exists || !st.Files[0].Writable {
		t.Errorf("files: %+v", st.Files)
	}
	empty := decode[map[string]any](t, newRig(t, "", "").call(t, http.MethodGet, "state", ""))
	cfg := empty["config"].(map[string]any)
	if cfg["providers"] == nil || cfg["routes"] == nil || empty["load_error"] != nil {
		t.Errorf("empty state must have non-null maps and null load_error: %v", empty)
	}
	if rec := g.call(t, http.MethodPost, "state", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST state: %d", rec.Code)
	}
}

func TestPutProvider(t *testing.T) {
	g := newRig(t, baseCfg, "")
	rec := g.call(t, http.MethodPut, "providers/kimi", `{"base_url":"https://api.kimi.example/anthropic"}`)
	expect(t, rec, http.StatusOK)
	if decode[map[string]bool](t, rec)["key_cleared"] {
		t.Error("new provider reported key_cleared")
	}
	if !strings.Contains(g.disk(t, "config.toml"), "api.kimi.example") {
		t.Error("provider not written")
	}
	for body, code := range map[string]string{
		`{"base_url":"http://not-local.example"}`: codeInvalid,
		`{"base_url":"https://a.example?x=1"}`:    codeInvalid,
		`{}`:                                      codeBadRequest,
	} {
		if rec := g.call(t, http.MethodPut, "providers/kimi", body); rec.Code != http.StatusBadRequest || errCode(t, rec) != code {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := g.call(t, http.MethodPut, "providers/Bad_Name", `{"base_url":"https://a.example"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", rec.Code)
	}
}

// (a) a literal key never follows its provider to another host.
func TestPutProviderKeyClearing(t *testing.T) {
	cases := []struct {
		name, keys, url string
		cleared         bool
	}{
		{"host change clears literal", `deepseek = "sk-1"`, "https://evil.example/anthropic", true},
		{"port change clears literal", `deepseek = "sk-1"`, "https://api.deepseek.com:8443/anthropic", true},
		{"scheme change clears literal", `deepseek = "sk-1"`, "http://127.0.0.1:9/anthropic", true},
		{"same host new path keeps", `deepseek = "sk-1"`, "https://API.deepseek.com/v2", false},
		{"file key kept on host change", `deepseek = "file:///run/k"`, "https://evil.example", false},
		{"no key", ``, "https://evil.example", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, baseCfg, c.keys)
			rec := g.call(t, http.MethodPut, "providers/deepseek", `{"base_url":"`+c.url+`"}`)
			expect(t, rec, http.StatusOK)
			if got := decode[map[string]bool](t, rec)["key_cleared"]; got != c.cleared {
				t.Errorf("key_cleared = %v, want %v", got, c.cleared)
			}
			if c.keys == "" {
				return
			}
			hasKey := strings.Contains(g.disk(t, "keys.toml"), "deepseek")
			if hasKey == c.cleared {
				t.Errorf("key on disk = %v, cleared = %v", hasKey, c.cleared)
			}
		})
	}
	// A leftover literal key without a provider is not adopted by a new one.
	g := newRig(t, "", `ghost = "sk-old"`)
	rec := g.call(t, http.MethodPut, "providers/ghost", `{"base_url":"https://new.example"}`)
	if expect(t, rec, http.StatusOK); !decode[map[string]bool](t, rec)["key_cleared"] || strings.Contains(g.disk(t, "keys.toml"), "sk-old") {
		t.Error("orphan literal key adopted by a new provider")
	}
}

// (b) PUT merges: hand-written fields survive.
func TestPutProviderKeepsHandWrittenFields(t *testing.T) {
	g := newRig(t, baseCfg, "")
	expect(t, g.call(t, http.MethodPut, "providers/deepseek", `{"base_url":"https://api.deepseek.com/v2"}`), http.StatusOK)
	got := g.disk(t, "config.toml")
	if !strings.Contains(got, "custom") || !strings.Contains(got, "/v2") {
		t.Errorf("top_keys lost or base_url unchanged:\n%s", got)
	}
}

func TestDeleteProvider(t *testing.T) {
	g := newRig(t, baseCfg+"[providers.kimi]\nbase_url = \"https://k.example\"\n", "kimi = \"sk-k\"\ndeepseek = \"sk-d\"\n")
	if rec := g.call(t, http.MethodDelete, "providers/deepseek", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "sonnet") {
		t.Errorf("referenced provider: %d %s", rec.Code, rec.Body)
	}
	if rec := g.call(t, http.MethodDelete, "providers/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing provider: %d", rec.Code)
	}
	expect(t, g.call(t, http.MethodDelete, "providers/kimi", ""), http.StatusOK)
	if strings.Contains(g.disk(t, "config.toml"), "kimi") || strings.Contains(g.disk(t, "keys.toml"), "kimi") {
		t.Error("provider or its key left on disk")
	}
}

func TestPutKey(t *testing.T) {
	g := newRig(t, baseCfg, "")
	rec := g.call(t, http.MethodPut, "providers/deepseek/key", `{"value":"sk-good"}`)
	expect(t, rec, http.StatusOK)
	if decode[map[string]string](t, rec)["state"] != "literal" {
		t.Errorf("state: %s", rec.Body)
	}
	keyFile := filepath.Join(t.TempDir(), "k")
	writeFile(t, keyFile, "sk-from-file\n")
	rec = g.call(t, http.MethodPut, "providers/deepseek/key", `{"value":"file://`+keyFile+`"}`)
	if expect(t, rec, http.StatusOK); decode[map[string]string](t, rec)["state"] != "file" {
		t.Errorf("file state: %s", rec.Body)
	}
	if rec := g.call(t, http.MethodPut, "providers/nope/key", `{"value":"sk-x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: %d", rec.Code)
	}
	for _, v := range []string{"", "sk-ant-api03-x", "xSK-ANT-y", "file://relative/k", "sk with space", "sk-x\\n", "sk-\\u0007"} {
		if rec := g.call(t, http.MethodPut, "providers/deepseek/key", `{"value":"`+v+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("value %q: %d", v, rec.Code)
		}
	}
	if !strings.Contains(g.disk(t, "keys.toml"), keyFile) {
		t.Error("a rejected value replaced the key")
	}
}

func TestDeleteKey(t *testing.T) {
	g := newRig(t, baseCfg, `deepseek = "sk-1"`)
	expect(t, g.call(t, http.MethodDelete, "providers/deepseek/key", ""), http.StatusOK)
	if strings.Contains(g.disk(t, "keys.toml"), "sk-1") {
		t.Error("key not deleted")
	}
	if rec := g.call(t, http.MethodDelete, "providers/nope/key", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: %d", rec.Code)
	}
}

func TestRoutes(t *testing.T) {
	g := newRig(t, baseCfg, "")
	cases := []struct {
		method, call, body string
		needsSync          bool
	}{
		{http.MethodPut, "routes/haiku", `{"provider":"deepseek","model":"m1"}`, true},  // new alias
		{http.MethodPut, "routes/haiku", `{"provider":"deepseek","model":"m2"}`, false}, // changed alias
		{http.MethodPut, "routes/kimi-k2", `{"provider":"deepseek","model":"m"}`, false},
		{http.MethodDelete, "routes/haiku", "", true},
		{http.MethodDelete, "routes/kimi-k2", "", false},
	}
	for _, c := range cases {
		rec := g.call(t, c.method, c.call, c.body)
		expect(t, rec, http.StatusOK)
		if got := decode[RouteResult](t, rec); got.NeedsSync != c.needsSync || got.SyncCommand != "shunt on" {
			t.Errorf("%s %s: %+v, want needs_sync %v", c.method, c.call, got, c.needsSync)
		}
	}
	for body, status := range map[string]int{
		`{"provider":"nope","model":"m"}`:      http.StatusBadRequest,
		`{"provider":"deepseek","model":""}`:   http.StatusBadRequest,
		`{"provider":"deepseek","model":"m"`:   http.StatusBadRequest,
		`{"provider":"deepseek","models":"m"}`: http.StatusBadRequest,
	} {
		if rec := g.call(t, http.MethodPut, "routes/x", body); rec.Code != status {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := g.call(t, http.MethodDelete, "routes/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing route: %d", rec.Code)
	}
}

func TestReload(t *testing.T) {
	g := newRig(t, baseCfg, "")
	writeFile(t, filepath.Join(g.dir, "config.toml"), baseCfg+"[routes.opus]\nprovider = \"deepseek\"\nmodel = \"x\"\n")
	expect(t, g.call(t, http.MethodPost, "reload", ""), http.StatusOK)
	if _, ok := g.h.store.Load().Config.Routes["opus"]; !ok {
		t.Error("reload did not pick up the file")
	}
	writeFile(t, filepath.Join(g.dir, "config.toml"), "[routes.opus\n")
	if rec := g.call(t, http.MethodPost, "reload", ""); rec.Code != http.StatusConflict || errCode(t, rec) != codeConflict {
		t.Errorf("bad file: %d %s", rec.Code, rec.Body)
	}
}

// (d) while the running config failed to load, every change is refused.
func TestLoadErrorBlocksChanges(t *testing.T) {
	g := newRig(t, "[providers.deepseek\n", "")
	st := decode[State](t, g.call(t, http.MethodGet, "state", ""))
	if st.LoadError == nil {
		t.Fatal("no load_error")
	}
	for _, e := range endpointsUnderTest {
		if e.method == http.MethodGet || e.call == "reload" || e.call == "web-code" {
			continue
		}
		if rec := g.call(t, e.method, e.call, e.body); rec.Code != http.StatusConflict {
			t.Errorf("%s %s: %d, want 409", e.method, e.call, rec.Code)
		}
	}
	// Fixed on disk but not reloaded: still refused until reload.
	writeFile(t, filepath.Join(g.dir, "config.toml"), baseCfg)
	if rec := g.call(t, http.MethodPut, "routes/x", `{"provider":"deepseek","model":"m"}`); rec.Code != http.StatusConflict {
		t.Errorf("before reload: %d", rec.Code)
	}
	expect(t, g.call(t, http.MethodPost, "reload", ""), http.StatusOK)
	expect(t, g.call(t, http.MethodPut, "routes/x", `{"provider":"deepseek","model":"m"}`), http.StatusOK)
}

// (e) web codes: one live code, single use, 60 s.
func TestWebCodes(t *testing.T) {
	g := newRig(t, baseCfg, "")
	now := time.Unix(1_000_000, 0)
	g.h.now = func() time.Time { return now }
	issue := func() string {
		rec := g.call(t, http.MethodPost, "web-code", "")
		expect(t, rec, http.StatusOK)
		wc := decode[WebCode](t, rec)
		if wc.ExpiresIn != 60 || len(wc.Code) < 32 {
			t.Fatalf("web-code: %+v", wc)
		}
		return wc.Code
	}
	redeem := func(code string) *http.Response {
		r := req(http.MethodPost, apiPrefix+"web-token", `{"code":"`+code+`"}`)
		r.Header.Del("Authorization")
		return g.serve(r).Result()
	}

	code := issue()
	if resp := redeem(code); resp.StatusCode != http.StatusOK {
		t.Fatalf("first redeem: %d", resp.StatusCode)
	}
	if resp := redeem(code); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("reused code: %d", resp.StatusCode)
	}
	r := req(http.MethodPost, apiPrefix+"web-token", `{"code":"`+issue()+`"}`)
	r.Header.Del("Authorization")
	if tok := decode[map[string]string](t, g.serve(r))["token"]; tok != testToken {
		t.Errorf("token = %q", tok)
	}

	old := issue()
	fresh := issue()
	if resp := redeem(old); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("superseded code: %d", resp.StatusCode)
	}
	if resp := redeem("wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	if resp := redeem(fresh); resp.StatusCode != http.StatusOK {
		t.Errorf("fresh code after a wrong attempt: %d", resp.StatusCode)
	}

	expiring := issue()
	now = now.Add(60 * time.Second)
	if resp := redeem(expiring); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired code: %d", resp.StatusCode)
	}
	if resp := redeem(""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty code with none live: %d", resp.StatusCode)
	}
}

func TestUnknownAndMethods(t *testing.T) {
	g := newRig(t, baseCfg, "")
	for _, call := range []string{"", "nope", "providers", "providers/", "providers/a/b", "providers/a/key/x", "routes", "state/x", "../state"} {
		if rec := g.call(t, http.MethodGet, call, ""); rec.Code != http.StatusNotFound || errCode(t, rec) != codeNotFound {
			t.Errorf("GET %q: %d", call, rec.Code)
		}
	}
	for _, path := range []string{Prefix + "/api", Prefix + "/api/", Prefix + "/api/v2/state"} {
		if rec := g.serve(req(http.MethodGet, path, "")); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
	if rec := g.call(t, http.MethodGet, "reload", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reload: %d", rec.Code)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	g := newRig(t, baseCfg, "")
	for _, path := range []string{Prefix + "/", Prefix + "/index.html"} {
		r := req(http.MethodGet, path, "")
		r.Header.Del("Authorization") // static files need no token
		rec := g.serve(r)
		expect(t, rec, http.StatusOK)
		h := rec.Header()
		if h.Get("Content-Security-Policy") != "default-src 'self'; frame-ancestors 'none'" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(h.Get("Content-Type"), "text/html") {
			t.Errorf("%s headers: %v", path, h)
		}
	}
	for path, status := range map[string]int{
		Prefix:                     http.StatusMovedPermanently,
		Prefix + "/missing.js":     http.StatusNotFound,
		Prefix + "/../index.html":  http.StatusNotFound,
		Prefix + "/web/index.html": http.StatusNotFound,
	} {
		if rec := g.serve(req(http.MethodGet, path, "")); rec.Code != status {
			t.Errorf("GET %s: %d, want %d", path, rec.Code, status)
		}
	}
	if rec := g.serve(req(http.MethodPost, Prefix+"/", "{}")); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST static: %d", rec.Code)
	}
	if rec := g.call(t, http.MethodGet, "state", ""); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("API response is cacheable")
	}
}

func TestAccessLogHasNoBodies(t *testing.T) {
	g := newRig(t, baseCfg, "")
	expect(t, g.call(t, http.MethodPut, "providers/deepseek/key", `{"value":"sk-LOGMARK"}`), http.StatusOK)
	out := g.logs.String()
	for _, want := range []string{"method=PUT", "path=/_shunt/api/v1/providers/deepseek/key", "status=200", "ms="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, "LOGMARK") || strings.Contains(out, testToken) {
		t.Errorf("log leaks body or token: %s", out)
	}
}

// (c) no response ever carries key material: literal keys, file:// file
// contents, or values echoed back from rejected requests.
func TestResponsesNeverLeakKeys(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k")
	writeFile(t, keyFile, "sk-FILESECRET\n")
	bad := filepath.Join(t.TempDir(), "multi")
	writeFile(t, bad, "line1-BADSECRET\nline2\n")
	cfg := baseCfg + "[providers.kimi]\nbase_url = \"https://k.example\"\n[providers.bad]\nbase_url = \"https://b.example\"\n"
	keys := "deepseek = \"sk-LITSECRET\"\nkimi = \"file://" + keyFile + "\"\nbad = \"file://" + bad + "\"\n"
	secrets := []string{"LITSECRET", "FILESECRET", "BADSECRET", "ANTSECRET", "NEWSECRET"}

	g := newRig(t, cfg, keys)
	calls := []struct{ method, call, body string }{
		{http.MethodGet, "state", ""},
		{http.MethodPut, "providers/deepseek/key", `{"value":"sk-ant-ANTSECRET"}`},
		{http.MethodPut, "providers/deepseek/key", `{"value":"sk NEWSECRET"}`},
		{http.MethodPut, "providers/deepseek/key", `{"value":"sk-NEWSECRET","extra":1}`},
		{http.MethodPut, "providers/nope/key", `{"value":"sk-NEWSECRET"}`},
		{http.MethodPut, "providers/kimi/key", `{"value":"sk-NEWSECRET"}`},
		{http.MethodPut, "providers/kimi", `{"base_url":"https://other.example"}`},
		{http.MethodPut, "routes/sonnet", `{"provider":"kimi","model":"m"}`},
		{http.MethodDelete, "routes/sonnet", ""},
		{http.MethodDelete, "providers/kimi/key", ""},
		{http.MethodDelete, "providers/bad", ""},
		{http.MethodPost, "reload", ""},
		{http.MethodPost, "web-code", ""},
		{http.MethodGet, "state", ""},
	}
	var all strings.Builder
	for _, c := range calls {
		all.WriteString(g.call(t, c.method, c.call, c.body).Body.String())
	}
	writeFile(t, filepath.Join(g.dir, "keys.toml"), "deepseek = \"sk-LITSECRET\n") // syntax error
	all.WriteString(g.call(t, http.MethodPost, "reload", "").Body.String())
	all.WriteString(g.call(t, http.MethodGet, "state", "").Body.String())
	all.WriteString(g.call(t, http.MethodPut, "providers/deepseek/key", `{"value":"sk-NEWSECRET"}`).Body.String())
	for _, s := range secrets {
		if strings.Contains(all.String(), s) {
			t.Errorf("a response contains %s:\n%s", s, all.String())
		}
	}
	if strings.Contains(g.logs.String(), "SECRET") {
		t.Errorf("log contains key material:\n%s", g.logs)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatal(err)
	}
}
