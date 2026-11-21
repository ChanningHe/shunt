package admin

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRecorderKeepsNewestAndCounts(t *testing.T) {
	r := NewRecorder()
	t0 := time.Unix(1000, 0)
	for i := range maxRecent + 5 {
		e := Entry{Time: t0.Add(time.Duration(i) * time.Second), Model: fmt.Sprint(i), Target: "anthropic", Status: 200}
		if i%2 == 0 {
			e.Route, e.Target, e.Status = "coder", "deepseek", 200+(i%4)*150 // 200, 500, 200, ...
		}
		r.Record(e)
	}
	a := r.Snapshot()
	if len(a.Recent) != maxRecent {
		t.Fatalf("recent = %d entries, want %d", len(a.Recent), maxRecent)
	}
	if a.Recent[0].Model != fmt.Sprint(maxRecent+4) || a.Recent[maxRecent-1].Model != "5" {
		t.Errorf("order: first %q last %q", a.Recent[0].Model, a.Recent[maxRecent-1].Model)
	}
	s := a.Routes["coder"]
	if want := (maxRecent + 5 + 1) / 2; s.Requests != want {
		t.Errorf("requests = %d, want %d", s.Requests, want)
	}
	if s.Errors == 0 || s.Errors >= s.Requests || s.Status != 200 || !s.Time.Equal(t0.Add((maxRecent+4)*time.Second)) {
		t.Errorf("stats = %+v", s)
	}
	if len(a.Routes) != 1 {
		t.Errorf("unrouted requests were counted as routes: %v", a.Routes)
	}
	var none *Recorder
	none.Record(Entry{}) // a proxy without a recorder must not panic
}

func TestActivityEndpoint(t *testing.T) {
	g := newRig(t, baseCfg, "")
	g.h.Activity.Record(Entry{Time: time.Now(), Path: "/v1/messages", Model: "shunt/sonnet", Route: "sonnet", Target: "deepseek", Status: 200, Ms: 12})
	g.h.Activity.Record(Entry{Time: time.Now(), Model: "shunt/gone", Route: "gone", Target: "x", Status: 200})

	rec := g.call(t, http.MethodGet, "activity", "")
	expect(t, rec, http.StatusOK)
	a := decode[Activity](t, rec)
	if len(a.Recent) != 2 || a.Recent[1].Route != "sonnet" || a.Routes["sonnet"].Requests != 1 {
		t.Errorf("activity = %+v", a)
	}
	if _, ok := a.Routes["gone"]; ok {
		t.Error("counters of a route that is not in the config were kept")
	}

	// Changing or removing a route starts its counters afresh.
	expect(t, g.call(t, http.MethodPut, "routes/sonnet", `{"provider":"deepseek","model":"other"}`), http.StatusOK)
	if a := decode[Activity](t, g.call(t, http.MethodGet, "activity", "")); len(a.Routes) != 0 || len(a.Recent) != 2 {
		t.Errorf("after a route change: %+v", a)
	}

	noToken := req(http.MethodGet, apiPrefix+"activity", "")
	noToken.Header.Del("Authorization")
	expect(t, g.serve(noToken), http.StatusUnauthorized)
}

func TestRouteTestEndpoint(t *testing.T) {
	g := newRig(t, baseCfg, "")
	expect(t, g.call(t, http.MethodPost, "routes/sonnet/test", ""), http.StatusNotImplemented)

	var asked string
	g.h.Probe = func(_ context.Context, route string) TestResult {
		asked = route
		return TestResult{Status: 401, Ms: 7, Message: "bad key"}
	}
	rec := g.call(t, http.MethodPost, "routes/sonnet/test", "")
	expect(t, rec, http.StatusOK)
	if got := decode[TestResult](t, rec); asked != "sonnet" || got != (TestResult{Status: 401, Ms: 7, Message: "bad key"}) {
		t.Errorf("asked %q, got %+v", asked, got)
	}
	asked = ""
	expect(t, g.call(t, http.MethodPost, "routes/nope/test", ""), http.StatusNotFound)
	expect(t, g.call(t, http.MethodGet, "routes/sonnet/test", ""), http.StatusMethodNotAllowed)
	if asked != "" {
		t.Errorf("probe ran for %q", asked)
	}
}

func TestTestSummary(t *testing.T) {
	for want, r := range map[string]TestResult{
		"route a: ok (HTTP 200, 5 ms)":                   {OK: true, Status: 200, Ms: 5},
		"route a: failed (HTTP 401, 9 ms): bad key":      {Status: 401, Ms: 9, Message: "bad key"},
		"route a: failed: key for provider p is missing": {Message: "key for provider p is missing"},
	} {
		if got := TestSummary("a", r); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
