package admin

import (
	"net/http"
	"strings"
	"testing"
)

// endpointsUnderTest is one valid request per API endpoint (valid for
// authCfg), so every cell of the auth matrix is checked everywhere.
const authCfg = baseCfg + "[providers.kimi]\nbase_url = \"https://k.example\"\n"

var endpointsUnderTest = []struct{ method, call, body string }{
	{http.MethodGet, "state", ""},
	{http.MethodPut, "providers/deepseek", `{"base_url":"https://api.deepseek.com/anthropic"}`},
	{http.MethodDelete, "providers/kimi", ""},
	{http.MethodPut, "providers/deepseek/key", `{"value":"sk-x"}`},
	{http.MethodDelete, "providers/deepseek/key", ""},
	{http.MethodPut, "routes/sonnet", `{"provider":"deepseek","model":"m"}`},
	{http.MethodDelete, "routes/sonnet", ""},
	{http.MethodPost, "reload", ""},
	{http.MethodPost, "web-code", ""},
}

func TestForeignHostRefusedEverywhere(t *testing.T) {
	g := newRig(t, authCfg, "")
	paths := []struct{ method, path string }{
		{http.MethodGet, Prefix + "/"},
		{http.MethodGet, Prefix + "/index.html"},
		{http.MethodGet, Prefix + "/missing"},
		{http.MethodGet, apiPrefix + "nope"},
		{http.MethodPost, apiPrefix + "web-token"},
	}
	for _, e := range endpointsUnderTest {
		paths = append(paths, struct{ method, path string }{e.method, apiPrefix + e.call})
	}
	for _, p := range paths {
		for _, host := range []string{"evil.example:27486", "evil.example", "10.0.0.1:27486", ""} {
			r := req(p.method, p.path, "")
			r.Host = host
			if rec := g.serve(r); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s Host %q: status %d, want 403", p.method, p.path, host, rec.Code)
			}
		}
	}
}

func TestOriginMustBeExactlySameOrigin(t *testing.T) {
	g := newRig(t, authCfg, "")
	for _, origin := range [][]string{
		{"null"}, {""}, {"http://evil.example"}, {"http://127.0.0.1:9999"}, {"https://127.0.0.1:27486"},
		{"http://localhost:27486"}, {"http://127.0.0.1:27486/"}, {"http://127.0.0.1:27486", "http://127.0.0.1:27486"},
	} {
		for _, path := range []string{Prefix + "/", apiPrefix + "state"} {
			r := req(http.MethodGet, path, "")
			r.Header["Origin"] = origin
			if rec := g.serve(r); rec.Code != http.StatusForbidden {
				t.Errorf("Origin %q on %s: status %d, want 403", origin, path, rec.Code)
			}
		}
	}
	r := req(http.MethodGet, apiPrefix+"state", "")
	r.Header.Set("Origin", "http://"+testHost)
	expect(t, g.serve(r), http.StatusOK)
}

func TestStateChangingRequestsNeedSameSiteAndJSON(t *testing.T) {
	for _, e := range endpointsUnderTest {
		if e.method == http.MethodGet {
			continue
		}
		for _, site := range []string{"cross-site", "same-site", "none"} {
			g := newRig(t, authCfg, "")
			r := req(e.method, apiPrefix+e.call, e.body)
			r.Header.Set("Sec-Fetch-Site", site)
			if rec := g.serve(r); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s Sec-Fetch-Site %s: status %d, want 403", e.method, e.call, site, rec.Code)
			}
		}
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx"} {
			g := newRig(t, authCfg, "")
			r := req(e.method, apiPrefix+e.call, e.body)
			r.Header.Set("Content-Type", ct)
			if rec := g.serve(r); rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s Content-Type %q: status %d, want 415", e.method, e.call, ct, rec.Code)
			}
		}
		g := newRig(t, authCfg, "")
		r := req(e.method, apiPrefix+e.call, e.body)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		if rec := g.serve(r); rec.Code != http.StatusOK {
			t.Errorf("%s %s same-origin JSON: status %d: %s", e.method, e.call, rec.Code, rec.Body)
		}
	}
}

func TestBearerRequiredExceptWebToken(t *testing.T) {
	g := newRig(t, authCfg, "")
	auths := map[string][]string{
		"missing":      nil,
		"wrong":        {"Bearer wrong"},
		"empty":        {"Bearer "},
		"prefix only":  {"Bearer " + testToken[:10]},
		"longer":       {"Bearer " + testToken + "x"},
		"lowercase":    {"bearer " + testToken},
		"basic":        {"Basic " + testToken},
		"bare token":   {testToken},
		"two headers":  {"Bearer wrong", "Bearer " + testToken},
		"unknown path": nil,
	}
	for name, auth := range auths {
		for _, e := range endpointsUnderTest {
			call := e.call
			if name == "unknown path" {
				call = "nope/x"
			}
			r := req(e.method, apiPrefix+call, e.body)
			r.Header.Del("Authorization")
			if auth != nil {
				r.Header["Authorization"] = auth
			}
			rec := g.serve(r)
			if rec.Code != http.StatusUnauthorized || errCode(t, rec) != codeUnauthorized {
				t.Errorf("%s: %s %s: status %d, want 401", name, e.method, call, rec.Code)
			}
		}
	}
	// web-token itself needs no bearer: a wrong code is 401 for the code.
	r := req(http.MethodPost, apiPrefix+"web-token", `{"code":"x"}`)
	r.Header.Del("Authorization")
	if rec := g.serve(r); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "code") {
		t.Errorf("web-token without bearer: %d %s", rec.Code, rec.Body)
	}
}

func TestBodyLimitsAndStrictJSON(t *testing.T) {
	cases := map[string]struct {
		body   string
		status int
	}{
		"too large":     {`{"base_url":"https://a.example/` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
		"unknown field": {`{"base_url":"https://a.example","beta_allow":["x"]}`, http.StatusBadRequest},
		"trailing data": {`{"base_url":"https://a.example"} {}`, http.StatusBadRequest},
		"not JSON":      {`base_url=https://a.example`, http.StatusBadRequest},
		"wrong type":    {`{"base_url":1}`, http.StatusBadRequest},
		"array":         {`[]`, http.StatusBadRequest},
		"empty":         {``, http.StatusBadRequest},
	}
	for name, c := range cases {
		g := newRig(t, authCfg, "")
		before := g.disk(t, "config.toml")
		rec := g.call(t, http.MethodPut, "providers/deepseek", c.body)
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, c.status, rec.Body)
			continue
		}
		errCode(t, rec)
		if g.disk(t, "config.toml") != before {
			t.Errorf("%s: config changed", name)
		}
	}
}

func TestIsLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:27486": true, "localhost:1": true, "[::1]:27486": true, "127.0.0.1": true, "127.9.9.9:1": true,
		"evil.example:27486": false, "0.0.0.0:27486": false, "10.0.0.1": false, "": false, "localhost.evil:1": false,
	} {
		if got := IsLocalHost(host); got != want {
			t.Errorf("IsLocalHost(%q) = %v", host, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"http://127.0.0.1:27486": true, "http://localhost:27486/": true, "http://[::1]:27486": true,
		"https://127.0.0.1:27486": false, "http://10.0.0.1:27486": false, "http://127.0.0.1:1/v1": false,
		"127.0.0.1:27486": false, "": false, "http://u:p@127.0.0.1:1": false, "http://127.0.0.1:1/#x": false,
		"http://127.0.0.1:1/?q": false,
	} {
		if err := CheckURL(u); (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v, want ok=%v", u, err, ok)
		}
	}
}
