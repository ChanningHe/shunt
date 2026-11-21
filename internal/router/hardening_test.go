package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// M1: an opaque request-target such as "http:@evil/x" must not move the
// passthrough to another host.
func TestPassthroughRejectsOpaqueTarget(t *testing.T) {
	r := newRig(t, "")
	evil := newUpstream(t, nil)
	srv := httptest.NewServer(r.proxy)
	defer srv.Close()
	evilHost := strings.TrimPrefix(evil.URL, "http://")
	for _, target := range []string{"http:@" + evilHost + "/v1/x", "http:." + evilHost + "/v1/x"} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: %s\r\nConnection: close\r\n\r\n", target, fakeOAuth)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", target, resp.StatusCode)
		}
	}
	if n := len(evil.requests()) + len(r.anthropic.requests()); n != 0 {
		t.Errorf("opaque target reached an upstream %d times", n)
	}
}

// M2: redirects are relayed to the client, never followed.
func TestRedirectsAreNotFollowed(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	evil := newUpstream(t, nil)
	redirect := func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, evil.URL+"/landing", http.StatusTemporaryRedirect)
	}
	r.anthropic.respond = redirect
	r.deepseek.respond = redirect
	if rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(false), fixture(t, "main_request.json")); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("passthrough: status %d, want 307 relayed", rec.Code)
	}
	if rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), fixture(t, "subagent_request.json")); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("third party: status %d, want 307 relayed", rec.Code)
	}
	if n := len(evil.requests()); n != 0 {
		t.Errorf("redirect target received %d requests", n)
	}
}

// M3: DNS-rebinding / browser requests (foreign Host, any Origin) are refused.
func TestForeignHostAndOriginRefused(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	cases := map[string]func(*http.Request){
		"foreign host": func(q *http.Request) { q.Host = "attacker.example:27486" },
		"origin":       func(q *http.Request) { q.Header.Set("Origin", "http://127.0.0.1:27486") },
	}
	for name, mutate := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(fixture(t, "subagent_request.json"))))
		req.Header = ccHeaders(true)
		req.Host = "127.0.0.1:27486"
		mutate(req)
		rec := httptest.NewRecorder()
		r.proxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:27486", "localhost:27486", "[::1]:27486", "127.0.0.1"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(fixture(t, "main_request.json"))))
		req.Header = ccHeaders(false)
		req.Host = host
		rec := httptest.NewRecorder()
		r.proxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %s: status %d, want 200", host, rec.Code)
		}
	}
	if n := len(r.deepseek.requests()); n != 0 {
		t.Errorf("refused requests reached deepseek %d times", n)
	}
}

// L1: a tool_result whose nested blocks are all filtered must not become
// "content": null.
func TestSanitizeEmptyToolResultContent(t *testing.T) {
	in := `{"model":"shunt/x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"x"}]}]}]}`
	out, err := sanitize(deepseekProfile, "m", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var obj object
	_ = json.Unmarshal(out, &obj)
	blk := obj["messages"].([]any)[0].(object)["content"].([]any)[0].(object)
	if c, ok := blk["content"]; ok && c == nil {
		t.Errorf("tool_result content encoded as null: %s", out)
	}
}
