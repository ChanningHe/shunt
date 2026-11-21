package admin

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// s7-2: names are path segments decoded one by one; "a%2Fkey" is the name
// "a/key", which is invalid, not provider a's key.
func TestEncodedSlashInNameIsRejected(t *testing.T) {
	g := newRig(t, authCfg+"[providers.a]\nbase_url = \"https://a.example\"\n", "a = \"sk-a\"\n")
	for _, c := range []struct{ method, call, body string }{
		{http.MethodDelete, "providers/a%2Fkey", ""},
		{http.MethodPut, "providers/a%2Fkey", `{"base_url":"https://x.example"}`},
		{http.MethodDelete, "routes/sonnet%2Fx", ""},
		{http.MethodPut, "providers/a%2Fkey/key", `{"value":"sk-x"}`},
		{http.MethodDelete, "providers/A", ""},
	} {
		rec := g.call(t, c.method, c.call, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status %d, want 400: %s", c.method, c.call, rec.Code, rec.Body)
		}
	}
	if !strings.Contains(g.disk(t, "keys.toml"), "sk-a") {
		t.Error("provider a's key was deleted")
	}
	// Plain names still work.
	expect(t, g.call(t, http.MethodDelete, "providers/a/key", ""), http.StatusOK)
}

// s6-2: after a failed reload the daemon still serves the previous config,
// and /state shows that running config together with the error.
func TestStateAfterFailedReloadShowsRunningConfig(t *testing.T) {
	g := newRig(t, baseCfg, "")
	writeFile(t, filepath.Join(g.dir, "config.toml"), "[routes\n")
	if rec := g.call(t, http.MethodPost, "reload", ""); rec.Code != http.StatusConflict {
		t.Fatalf("reload of a broken file: %d", rec.Code)
	}
	st := decode[State](t, g.call(t, http.MethodGet, "state", ""))
	if st.LoadError == nil || st.Config.Routes["sonnet"].Model != "deepseek-flash" || st.Config.Providers["deepseek"].BaseURL == "" {
		t.Errorf("state after failed reload: %+v", st)
	}
	// Broken from the start: nothing is running, so the config is empty.
	st = decode[State](t, newRig(t, "[routes\n", "").call(t, http.MethodGet, "state", ""))
	if st.LoadError == nil || len(st.Config.Routes) != 0 {
		t.Errorf("state when broken at start: %+v", st)
	}
}
