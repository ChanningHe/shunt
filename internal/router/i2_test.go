package router

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shunt/internal/config"
)

const leak = "sk-ant-oat01-LEAKED"

// subagentBody builds a routed subagent request with s placed at one of
// several positions. kind selects the position; s is JSON-escaped.
func subagentBody(kind int, s string) []byte {
	q, _ := json.Marshal(s)
	v := string(q)
	pos := map[int]string{}
	pos[kind%9] = v
	at := func(i int, def string) string {
		if p, ok := pos[i]; ok {
			return p
		}
		return def
	}
	return []byte(`{"model":` + at(0, `"shunt/deepseek-flash"`) +
		`,"metadata":{"user_id":` + at(1, `"u"`) + `}` +
		`,"safeguards":[{"x":` + at(2, `"y"`) + `}]` +
		`,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322; cc_is_subagent=true;"},{"type":"text","text":` + at(3, `"sys"`) + `}]` +
		`,"tools":[{"name":"Read","description":` + at(4, `"d"`) + `,"input_schema":{}},{"type":"web_search_20250305","name":"web_search","x":` + at(5, `"y"`) + `}]` +
		`,"messages":[{"role":"user","content":` + at(6, `"hi"`) + `}` +
		`,{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"T","description":` + at(7, `"t"`) + `,"input_schema":{}}}}]}` +
		`,{"role":"assistant","content":[{"type":"redacted_thinking","data":` + at(8, `"r"`) + `}]}]}`)
}

// assertClean checks the property I2 guards: nothing the third party
// received contains an Anthropic credential pattern.
func assertClean(t *testing.T, r *rig) {
	t.Helper()
	for _, c := range r.deepseek.requests() {
		if where := findCredential(c.header, c.body); where != "" {
			t.Fatalf("third party received sk-ant- in %s", where)
		}
	}
	if strings.Contains(strings.ToLower(r.logs.String()), credentialToken) {
		t.Fatalf("log contains a credential pattern:\n%s", r.logs)
	}
}

func TestI2InboundCredentialHeadersAreDropped(t *testing.T) {
	for _, name := range []string{
		"Authorization", "X-Api-Key", "Anthropic-Beta", "X-Claude-Code-Session-Id",
		"X-Claude-Code-Future-Header", "X-Stainless-Foo", "Cookie", "Proxy-Authorization", "X-Random",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "sk-deepseek-test")
			h := ccHeaders(true)
			h.Add(name, "Bearer "+leak)
			rec := r.do(t, http.MethodPost, "/v1/messages", h, subagentBody(-1, ""))
			if rec.Code != http.StatusOK || len(r.deepseek.requests()) != 1 {
				t.Fatalf("code %d (%s), deepseek requests %d; header must be dropped, not refused", rec.Code, rec.Body, len(r.deepseek.requests()))
			}
			assertClean(t, r)
		})
	}
}

func TestI2BodyPositions(t *testing.T) {
	// Positions sanitize drops anyway (forwarded fine) vs. positions that
	// carry content to the third party (must be refused).
	dropped := map[int]string{0: "model", 1: "metadata", 2: "safeguards", 5: "server tool", 8: "redacted_thinking"}
	kept := map[int]string{3: "system text", 4: "tool description", 6: "message text", 7: "tool_addition"}
	for kind, name := range dropped {
		t.Run("dropped/"+name, func(t *testing.T) {
			r := newRig(t, "sk-deepseek-test")
			body := subagentBody(kind, leak)
			if kind == 0 {
				// Routes match by exact name, so the credential has to be
				// the route name (names are lowercase) to stay routed; the
				// model is rewritten.
				name := strings.ToLower(leak)
				if err := r.store.Update(func(c *config.Config, _ config.Keys) error {
					c.Routes[name] = config.Route{Provider: "deepseek", Model: "deepseek-flash"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				body = subagentBody(kind, "shunt/"+name)
			}
			if rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), body); rec.Code != http.StatusOK {
				t.Fatalf("code %d (%s)", rec.Code, rec.Body)
			}
			assertClean(t, r)
		})
	}
	for kind, name := range kept {
		for _, variant := range []string{leak, strings.ToUpper(leak), "prefix " + leak + " suffix"} {
			t.Run("refused/"+name, func(t *testing.T) {
				r := newRig(t, "sk-deepseek-test")
				rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), subagentBody(kind, variant))
				if rec.Code != http.StatusBadGateway || len(r.deepseek.requests()) != 0 {
					t.Fatalf("code %d, deepseek requests %d; want 502 and no upstream call", rec.Code, len(r.deepseek.requests()))
				}
				assertClean(t, r)
			})
		}
	}
}

func TestI2UnicodeEscapedCredentialIsCaught(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	body := strings.Replace(string(subagentBody(6, "MARK")), "MARK", `sk-ant-oat01-x`, 1)
	rec := r.do(t, http.MethodPost, "/v1/messages", ccHeaders(true), []byte(body))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code %d, want 502", rec.Code)
	}
	assertClean(t, r)
}

func TestI2AnthropicVersionHeaderInjection(t *testing.T) {
	r := newRig(t, "sk-deepseek-test")
	h := ccHeaders(true)
	h.Set("Anthropic-Version", leak)
	r.do(t, http.MethodPost, "/v1/messages", h, subagentBody(-1, ""))
	assertClean(t, r)
}

// FuzzI2 places arbitrary strings in every inbound header we know of and at
// every body position; the third party must never see sk-ant-.
func FuzzI2(f *testing.F) {
	for kind := 0; kind < 9; kind++ {
		f.Add(uint8(kind), leak, "Bearer "+leak, "oauth-2025-04-20,"+leak)
	}
	f.Add(uint8(3), "SK-ANT-x", "x", "y")
	f.Add(uint8(6), "sk-an"+"t-", "sk-ant", "-")
	f.Fuzz(func(t *testing.T, kind uint8, text, auth, beta string) {
		r := newRig(t, "sk-deepseek-test")
		h := ccHeaders(true)
		h.Set("Authorization", auth)
		h.Set("X-Api-Key", auth)
		h.Set("Anthropic-Beta", beta)
		h.Set("X-Claude-Code-Session-Id", text)
		h.Set("Anthropic-Version", beta)
		r.do(t, http.MethodPost, "/v1/messages", h, subagentBody(int(kind), text))
		assertClean(t, r)
	})
}
