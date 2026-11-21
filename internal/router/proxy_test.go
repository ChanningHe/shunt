package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"shunt/internal/config"
)

const fakeOAuth = "Bearer sk-ant-oat01-FAKEFAKEFAKE"

type captured struct {
	uri    string
	header http.Header
	body   []byte
}

// upstream is a fake HTTP server that records every request it receives.
type upstream struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []captured
	respond http.HandlerFunc
}

func newUpstream(t testing.TB, respond http.HandlerFunc) *upstream {
	u := &upstream{respond: respond}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, captured{r.RequestURI, r.Header.Clone(), body})
		u.mu.Unlock()
		if u.respond != nil {
			u.respond(w, r)
			return
		}
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

type rig struct {
	anthropic, deepseek *upstream
	store               *config.Store
	proxy               *proxy
	logs                *bytes.Buffer
}

// newRig serves a config directory with provider "deepseek" (the fake
// upstream) and route "deepseek-flash", i.e. wire model shunt/deepseek-flash.
// key is written to keys.toml unless empty.
func newRig(t testing.TB, key string) *rig {
	r := &rig{anthropic: newUpstream(t, nil), deepseek: newUpstream(t, nil), logs: &bytes.Buffer{}}
	dir := t.TempDir()
	cfg := fmt.Sprintf("[providers.deepseek]\nbase_url = %q\n[routes.deepseek-flash]\nprovider = \"deepseek\"\nmodel = \"deepseek-flash\"\n",
		r.deepseek.URL+"/anthropic")
	writeTestFile(t, filepath.Join(dir, "config.toml"), cfg)
	if key != "" {
		writeTestFile(t, filepath.Join(dir, "keys.toml"), fmt.Sprintf("deepseek = %q\n", key))
	}
	r.store = config.Open(dir)
	if err := r.store.Load().LoadErr; err != nil {
		t.Fatal(err)
	}
	r.proxy = newProxy(r.anthropic.URL, r.store, slog.New(slog.NewTextHandler(r.logs, nil)))
	return r
}

func writeTestFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ccHeaders mirrors the header set Claude Code 2.1.322 sent in the spike.
func ccHeaders(subagent bool) http.Header {
	h := http.Header{
		"Accept":          {"application/json"},
		"Accept-Encoding": {"gzip, deflate, br, zstd"},
		"Anthropic-Beta": {"oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27," +
			"claude-code-20250219,advisor-tool-2026-03-01,effort-2025-11-24"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"Anthropic-Version":                         {"2023-06-01"},
		"Authorization":                             {fakeOAuth},
		"Content-Type":                              {"application/json"},
		"User-Agent":                                {"claude-cli/2.1.322 (external, sdk-cli)"},
		"X-App":                                     {"cli"},
		"X-Claude-Code-Session-Id":                  {"6f1c-session"},
		"X-Stainless-Retry-Count":                   {"0"},
		"X-Stainless-Timeout":                       {"600"},
	}
	if subagent {
		h.Set("X-Claude-Code-Agent-Id", "a99f21961dda1ff97")
	}
	return h
}

func (r *rig) do(t testing.TB, method, uri string, h http.Header, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, uri, bytes.NewReader(body))
	req.Header = h.Clone()
	req.Host = "127.0.0.1:27486"
	rec := httptest.NewRecorder()
	r.proxy.ServeHTTP(rec, req)
	return rec
}

func fixture(t testing.TB, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// I1: an unrouted request reaches Anthropic byte-for-byte, with every
// end-to-end header intact (Authorization and the oauth beta included).
func TestI1PassthroughIsByteIdentical(t *testing.T) {
	cases := []struct {
		name     string
		subagent bool
		body     []byte
	}{
		{"main thread opus", false, fixture(t, "main_request.json")},
		{"subagent with full model id", true, bytes.Replace(fixture(t, "subagent_request.json"),
			[]byte(`"shunt/deepseek-flash"`), []byte(`"claude-sonnet-5-5"`), 1)},
		{"not json", false, []byte("\x00not json")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, "sk-deepseek-test")
			r.anthropic.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Request-Id", "req_1")
				_, _ = w.Write([]byte("\x1f\x8braw-gzip-bytes"))
			}
			// Send over real HTTP so hop-by-hop handling is exercised.
			srv := httptest.NewServer(r.proxy)
			defer srv.Close()
			sent := ccHeaders(c.subagent)
			sent.Set("Connection", "keep-alive, X-Hop")
			sent.Set("X-Hop", "drop-me")
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages?beta=true", bytes.NewReader(c.body))
			req.Header = sent.Clone()
			resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			reqs := r.anthropic.requests()
			if len(reqs) != 1 || len(r.deepseek.requests()) != 0 {
				t.Fatalf("anthropic got %d, deepseek got %d requests; want 1, 0", len(reqs), len(r.deepseek.requests()))
			}
			up := reqs[0]
			if !bytes.Equal(up.body, c.body) {
				t.Errorf("body changed in passthrough")
			}
			if up.uri != "/v1/messages?beta=true" {
				t.Errorf("uri = %q", up.uri)
			}
			want := sent.Clone()
			want.Del("Connection")
			want.Del("X-Hop")
			gotH := up.header.Clone()
			gotH.Del("Content-Length")
			if !reflect.DeepEqual(gotH, want) {
				t.Errorf("headers differ\n got: %v\nwant: %v", gotH, want)
			}
			if string(got) != "\x1f\x8braw-gzip-bytes" || resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Request-Id") != "req_1" {
				t.Errorf("response not relayed verbatim: %q %v", got, resp.Header)
			}
		})
	}
}

// Headers the client did not send must not appear upstream either: Go's
// transport would otherwise add its own User-Agent and Accept-Encoding.
func TestI1NoHeadersAdded(t *testing.T) {
	r := newRig(t, "")
	h := ccHeaders(false)
	h.Del("User-Agent")
	h.Del("Accept-Encoding")
	r.do(t, http.MethodPost, "/v1/messages", h, fixture(t, "main_request.json"))
	got := r.anthropic.requests()[0].header
	for _, k := range []string{"User-Agent", "Accept-Encoding"} {
		if v, ok := got[k]; ok {
			t.Errorf("proxy added %s %v", k, v)
		}
	}
}

func TestI1OtherMethodsPassThrough(t *testing.T) {
	r := newRig(t, "")
	rec := r.do(t, http.MethodHead, "/api/hello", http.Header{}, nil)
	if rec.Code != http.StatusOK || len(r.anthropic.requests()) != 1 {
		t.Fatalf("HEAD /api/hello: code %d, anthropic requests %d", rec.Code, len(r.anthropic.requests()))
	}
}

func TestRoutedSubagentGoesToDeepSeek(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	rec := r.do(t, http.MethodPost, "/v1/messages?beta=true", ccHeaders(true), fixture(t, "subagent_request.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if len(r.anthropic.requests()) != 0 {
		t.Fatal("routed request leaked to anthropic")
	}
	up := r.deepseek.requests()[0]
	if up.uri != "/anthropic/v1/messages" {
		t.Errorf("uri = %q", up.uri)
	}
	wantH := map[string]string{
		"X-Api-Key": "sk-deepseek-test", "Anthropic-Version": "2023-06-01",
		"Content-Type": "application/json", "User-Agent": "shunt",
	}
	for k, v := range wantH {
		if up.header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, up.header.Get(k), v)
		}
	}
	for k := range up.header {
		if k == "Authorization" || k == "Anthropic-Beta" || strings.HasPrefix(k, "X-Claude-Code") || strings.HasPrefix(k, "X-Stainless") {
			t.Errorf("header %s must not reach the third party", k)
		}
	}
	var body object
	_ = json.Unmarshal(up.body, &body)
	if body["model"] != "deepseek-flash" {
		t.Errorf("model = %v", body["model"])
	}
	if strings.Contains(string(up.body), billingPrefix) {
		t.Error("billing block reached the third party")
	}
}

func TestGuardRejectsMainThread(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	body := bytes.Replace(fixture(t, "subagent_request.json"), []byte(" cc_is_subagent=true;"), nil, 1)
	for name, h := range map[string]http.Header{
		"no marker, no agent id": ccHeaders(false),
		"no marker, agent id":    ccHeaders(true),
	} {
		rec := r.do(t, http.MethodPost, "/v1/messages?beta=true", h, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "main-thread") || !strings.Contains(rec.Body.String(), "shunt off") {
			t.Errorf("%s: code %d body %s", name, rec.Code, rec.Body)
		}
	}
	marked := fixture(t, "subagent_request.json")
	if rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(false), marked); rec.Code != http.StatusBadRequest {
		t.Errorf("marker without agent-id header: code %d, want 400", rec.Code)
	}
	if rec := r.do(t, http.MethodPost, "/v1/messages/batches", ccHeaders(true), marked); rec.Code != http.StatusBadRequest {
		t.Errorf("routed model on other path: code %d, want 400", rec.Code)
	}
	if n := len(r.anthropic.requests()) + len(r.deepseek.requests()); n != 0 {
		t.Errorf("rejected requests reached an upstream %d times", n)
	}
}

func TestCountTokensIsLocal(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	body := []byte(`{"model":"shunt/deepseek-flash","messages":[{"role":"user","content":"hello world"}]}`)
	rec := r.do(t, http.MethodPost, "/v1/messages/count_tokens?beta=true", ccHeaders(false), body)
	var got struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.InputTokens <= 0 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
	if len(r.anthropic.requests())+len(r.deepseek.requests()) != 0 {
		t.Error("count_tokens left the machine")
	}
}

func TestMissingKeyIs503(t *testing.T) {
	r := newRig(t, "")
	rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), fixture(t, "subagent_request.json"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "deepseek") || !strings.Contains(rec.Body.String(), config.KeyMissing) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
}

// An Anthropic-looking key disables the provider; the message names the
// provider and state but never the key.
func TestInvalidKeyIs503(t *testing.T) {
	const bad = "sk-ant-api03-NOTFORTHIRDPARTIES"
	r := newRig(t, bad)
	rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), fixture(t, "subagent_request.json"))
	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(body, "deepseek") || !strings.Contains(body, config.KeyInvalid) {
		t.Fatalf("code %d body %s", rec.Code, body)
	}
	if strings.Contains(body, "NOTFORTHIRDPARTIES") || strings.Contains(r.logs.String(), "NOTFORTHIRDPARTIES") {
		t.Fatalf("key leaked: %s\n%s", body, r.logs)
	}
	if n := len(r.deepseek.requests()) + len(r.anthropic.requests()); n != 0 {
		t.Fatalf("request reached an upstream %d times", n)
	}
}

// withModel is the routed subagent fixture with its model replaced.
func withModel(t testing.TB, model string) []byte {
	q, _ := json.Marshal(model)
	return bytes.Replace(fixture(t, "subagent_request.json"), []byte(`"shunt/deepseek-flash"`), q, 1)
}

// The shunt/ namespace belongs to shunt: an id without a route is refused
// with a fix-it hint on every path, never passed through.
func TestUnroutedShuntModelIsRejected(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	for _, name := range []string{"nope", "", "DEEPSEEK-FLASH", "deepseek-flash/x", "deepseek-flash "} {
		for _, path := range []string{messagesPath, countTokenPath, "/v1/messages/batches"} {
			for _, sub := range []bool{true, false} {
				rec := r.do(t, http.MethodPost, path, ccHeaders(sub), withModel(t, "shunt/"+name))
				body := rec.Body.String()
				q, _ := json.Marshal(fmt.Sprintf("%q", name))
				if rec.Code != http.StatusBadRequest || !strings.Contains(body, "shunt route set") || !strings.Contains(body, string(q[1:len(q)-1])) {
					t.Errorf("model shunt/%s %s subagent=%v: code %d body %s", name, path, sub, rec.Code, body)
				}
			}
		}
	}
	if n := len(r.anthropic.requests()) + len(r.deepseek.requests()); n != 0 {
		t.Errorf("unrouted shunt/ ids reached an upstream %d times", n)
	}
}

// Store changes apply to the next request of the same proxy, no restart.
func TestRoutesHotUpdate(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	body := withModel(t, "shunt/fresh")
	if rec := r.do(t, http.MethodPost, messagesPath, ccHeaders(true), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("before route exists: code %d", rec.Code)
	}
	for _, model := range []string{"model-one", "model-two"} {
		err := r.store.Update(func(c *config.Config, _ config.Keys) error {
			c.Routes["fresh"] = config.Route{Provider: "deepseek", Model: model}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec := r.do(t, http.MethodPost, messagesPath, ccHeaders(true), body); rec.Code != http.StatusOK {
			t.Fatalf("route -> %s: code %d body %s", model, rec.Code, rec.Body)
		}
		reqs := r.deepseek.requests()
		var got object
		_ = json.Unmarshal(reqs[len(reqs)-1].body, &got)
		if got["model"] != model {
			t.Fatalf("upstream model = %v, want %s", got["model"], model)
		}
	}
	if n := len(r.anthropic.requests()); n != 0 {
		t.Fatalf("anthropic got %d requests", n)
	}
}

// The first SSE event must reach the client before the upstream finishes.
func TestStreamingIsNotBuffered(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	release := make(chan struct{})
	r.deepseek.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}
	srv := httptest.NewServer(r.proxy)
	defer srv.Close()
	defer close(release) // runs before srv.Close, which waits for the handler
	line := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(fixture(t, "subagent_request.json")))
		req.Header = ccHeaders(true)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			line <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "event: message_start\n" {
			t.Errorf("first line = %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Error("first SSE event was buffered")
	}
}

func TestLogsHoldNoSecretsOrBodies(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	r.do(t, http.MethodPost, "/v1/messages", ccHeaders(false), fixture(t, "main_request.json"))
	r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), fixture(t, "subagent_request.json"))
	logs := r.logs.String()
	for _, s := range []string{"sk-ant-", "Bearer", "oat01", "sk-deepseek-test", "What is 2+2", "You are"} {
		if strings.Contains(logs, s) {
			t.Errorf("log contains %q:\n%s", s, logs)
		}
	}
	if !strings.Contains(logs, "target=deepseek") || !strings.Contains(logs, "target=anthropic") {
		t.Errorf("log lacks routing info:\n%s", logs)
	}
}
