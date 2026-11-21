package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"shunt/internal/admin"
	"shunt/internal/config"
)

func TestProbeSendsAMinimalRoutedRequest(t *testing.T) {
	r := newRig(t, "sk-third-party-key")
	res := r.proxy.probe(context.Background(), "deepseek-flash")
	if !res.OK || res.Status != http.StatusOK || res.Message != "" {
		t.Fatalf("result = %+v", res)
	}
	reqs := r.deepseek.requests()
	if len(reqs) != 1 || len(r.anthropic.requests()) != 0 {
		t.Fatalf("deepseek got %d requests, anthropic %d", len(reqs), len(r.anthropic.requests()))
	}
	got := reqs[0]
	if got.uri != "/anthropic/v1/messages" || got.header.Get("X-Api-Key") != "sk-third-party-key" ||
		got.header.Get("Authorization") != "" || got.header.Get("Anthropic-Version") == "" {
		t.Errorf("request %s, headers %v", got.uri, got.header)
	}
	if body := string(got.body); !strings.Contains(body, `"model":"deepseek-flash"`) || !strings.Contains(body, `"max_tokens":16`) {
		t.Errorf("body = %s", body)
	}
}

func TestProbeReportsProviderErrorsSafely(t *testing.T) {
	const key = "sk-third-party-key"
	r := newRig(t, key)
	r.deepseek.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key `+key+
			` \u001b[31mred\u0007\n`+strings.Repeat("x", 2000)+`"}}`)
	}
	res := r.proxy.probe(context.Background(), "deepseek-flash")
	if res.OK || res.Status != http.StatusUnauthorized {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(res.Message, "bad key <key> [31mred ") || strings.Contains(res.Message, key) {
		t.Errorf("message = %q", res.Message)
	}
	if n := len([]rune(res.Message)); n > maxProbeMessage+3 {
		t.Errorf("message is %d runes long", n)
	}
	if strings.ContainsFunc(res.Message, func(c rune) bool { return c < ' ' || c == 0x7f }) {
		t.Errorf("control characters in %q", res.Message)
	}

	r.deepseek.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>upstream down</html>")
	}
	if res := r.proxy.probe(context.Background(), "deepseek-flash"); res.Message != "<html>upstream down</html>" {
		t.Errorf("non-JSON body: %+v", res)
	}
}

func TestProbeWithoutKeyOrProviderSendsNothing(t *testing.T) {
	r := newRig(t, "")
	if res := r.proxy.probe(context.Background(), "deepseek-flash"); res.OK || res.Message != "key for provider deepseek is missing" {
		t.Errorf("no key: %+v", res)
	}
	if res := r.proxy.probe(context.Background(), "nope"); res.OK || res.Message == "" {
		t.Errorf("no route: %+v", res)
	}
	r2 := newRig(t, "sk-third-party-key")
	r2.deepseek.Close()
	if res := r2.proxy.probe(context.Background(), "deepseek-flash"); res.OK || res.Status != 0 || !strings.HasSuffix(res.Message, "unreachable") {
		t.Errorf("provider down: %+v", res)
	}
	if n := len(r.deepseek.requests()) + len(r.anthropic.requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

func TestProxyRecordsActivity(t *testing.T) {
	r := newRig(t, "sk-third-party-key")
	r.proxy.rec = admin.NewRecorder()
	r.do(t, http.MethodPost, messagesPath, ccHeaders(false), fixture(t, "main_request.json"))
	r.do(t, http.MethodPost, messagesPath, ccHeaders(true), fixture(t, "subagent_request.json"))
	r.do(t, http.MethodPost, messagesPath, ccHeaders(false), fixture(t, "subagent_request.json")) // main thread on a route

	a := r.proxy.rec.Snapshot()
	if len(a.Recent) != 3 {
		t.Fatalf("recent = %+v", a.Recent)
	}
	rejected, routed, main := a.Recent[0], a.Recent[1], a.Recent[2]
	if main.Target != targetAnthro || main.Route != "" || main.Status != http.StatusOK || main.Path != messagesPath {
		t.Errorf("main = %+v", main)
	}
	if routed.Target != "deepseek" || routed.Route != "deepseek-flash" || routed.Model != "shunt/deepseek-flash" {
		t.Errorf("routed = %+v", routed)
	}
	if rejected.Target != targetRejected || rejected.Route != "" || rejected.Status != http.StatusBadRequest {
		t.Errorf("rejected = %+v", rejected)
	}
	if s := a.Routes["deepseek-flash"]; len(a.Routes) != 1 || s.Requests != 1 || s.Errors != 0 {
		t.Errorf("route stats = %+v", a.Routes)
	}
}

// What a client puts in the path or the model ends up on a terminal
// (shunt activity, the TUI): no escape sequence may survive.
func TestActivityFieldsAreSafeToPrint(t *testing.T) {
	r := newRig(t, "sk-third-party-key")
	r.proxy.rec = admin.NewRecorder()
	evil := []byte(`{"model":"m\u001b]52;c;QUJD\u0007\n\u009b31m"}`)
	r.do(t, http.MethodPost, "/v1/%1b%5d52;c;QUJD%07/x%0a", ccHeaders(false), evil)

	// Refused before the body is read: a web page or a foreign Host cannot
	// write into the list at all.
	browser := ccHeaders(false)
	browser.Set("Origin", "https://evil.example")
	r.do(t, http.MethodPost, messagesPath, browser, evil)

	a := r.proxy.rec.Snapshot()
	if len(a.Recent) != 1 {
		t.Fatalf("recent = %+v", a.Recent)
	}
	e := a.Recent[0]
	for _, s := range []string{e.Path, e.Model} {
		if s == "" || strings.ContainsFunc(s, func(c rune) bool { return c < ' ' || c == 0x7f || (c >= 0x80 && c < 0xa0) }) {
			t.Errorf("unsafe or empty field %q", s)
		}
	}
}

// A provider may be called like one of the fixed targets; its traffic is
// still counted as the route's.
func TestActivityCountsRouteWhateverTheProviderIsCalled(t *testing.T) {
	up := newUpstream(t, nil)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "config.toml"), fmt.Sprintf(
		"[providers.anthropic]\nbase_url = %q\n[routes.coder]\nprovider = \"anthropic\"\nmodel = \"m\"\n", up.URL))
	writeTestFile(t, filepath.Join(dir, "keys.toml"), "anthropic = \"sk-third-party-key\"\n")
	p := newProxy(newUpstream(t, nil).URL, config.Open(dir), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.rec = admin.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, messagesPath, bytes.NewReader(withModel(t, "shunt/coder")))
	req.Header = ccHeaders(true)
	req.Host = "127.0.0.1:1"
	p.ServeHTTP(httptest.NewRecorder(), req)
	a := p.rec.Snapshot()
	if len(up.requests()) != 1 || a.Routes["coder"].Requests != 1 || a.Recent[0].Route != "coder" {
		t.Errorf("provider got %d requests; activity %+v", len(up.requests()), a)
	}
}
