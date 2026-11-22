package tui

import (
	"reflect"
	"testing"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// graphState is a State with one provider per key state and the given
// routes.
func graphState(keys map[string]string, routes map[string]config.Route) *admin.State {
	st := &admin.State{Keys: keys, Config: admin.StateConfig{Providers: map[string]config.Provider{}, Routes: routes}}
	for p := range keys {
		st.Config.Providers[p] = config.Provider{BaseURL: "https://" + p + ".example/anthropic"}
	}
	return st
}

func TestBuildGraph(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	ago := func(s int) time.Time { return now.Add(-time.Duration(s) * time.Second) }
	anth := target{provider: anthropic, state: stateUnchanged}
	rest := source{name: restName}
	stats := admin.RouteStats{Requests: 3, Errors: 1, Status: 200, Time: ago(30)}
	literal := map[string]string{"deepseek": "literal", "kimi": "literal"}

	cases := []struct {
		name string
		st   *admin.State
		act  *admin.Activity
		want graph
	}{
		{
			name: "empty config",
			st:   graphState(nil, nil),
			act:  &admin.Activity{},
			want: graph{sources: []source{rest}, targets: []target{anth}, edge: []int{0}},
		},
		{
			name: "routes sharing a target share one node, targets sorted by provider then model",
			st: graphState(literal, map[string]config.Route{
				"cheap": {Provider: "deepseek", Model: "flash"},
				"coder": {Provider: "deepseek", Model: "flash"},
				"alpha": {Provider: "kimi", Model: "k3"},
				"zed":   {Provider: "deepseek", Model: "chat"},
			}),
			want: graph{
				sources: []source{{name: "alpha"}, {name: "cheap"}, {name: "coder"}, {name: "zed"}, rest},
				targets: []target{
					{"deepseek", "chat", stateReady}, {"deepseek", "flash", stateReady}, {"kimi", "k3", stateReady}, anth,
				},
				edge: []int{2, 1, 1, 0, 3},
			},
		},
		{
			name: "key states and a missing provider",
			st: graphState(
				map[string]string{"deepseek": config.KeyMissing, "kimi": config.KeyInvalid, "local": "file"},
				map[string]config.Route{
					"a": {Provider: "deepseek", Model: "m"},
					"b": {Provider: "ghost", Model: "m"},
					"c": {Provider: "kimi", Model: "m"},
					"d": {Provider: "local", Model: "m"},
				}),
			want: graph{
				sources: []source{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}, rest},
				targets: []target{
					{"deepseek", "m", stateNoKey}, {"ghost", "m", stateNoProvider},
					{"kimi", "m", stateKeyInvalid}, {"local", "m", stateReady}, anth,
				},
				edge: []int{0, 1, 2, 3, 4},
			},
		},
		{
			name: "alias routes",
			st: graphState(literal, map[string]config.Route{
				"coder":  {Provider: "kimi", Model: "k3"},
				"sonnet": {Provider: "kimi", Model: "k3"},
			}),
			want: graph{
				sources: []source{{name: "coder"}, {name: "sonnet", alias: true}, rest},
				targets: []target{{"kimi", "k3", stateReady}, anth},
				edge:    []int{0, 0, 1},
			},
		},
		{
			name: "stats and live from when the latest request ended",
			st: graphState(literal, map[string]config.Route{
				"coder": {Provider: "deepseek", Model: "flash"},
				"cheap": {Provider: "deepseek", Model: "flash"},
				"idle":  {Provider: "deepseek", Model: "flash"},
			}),
			act: &admin.Activity{
				Routes: map[string]admin.RouteStats{"coder": stats, "cheap": stats},
				Recent: []admin.Entry{ // newest first
					{Time: ago(30), Route: "coder", Target: "deepseek", Status: 200, Ms: 25000},
					{Time: ago(30), Route: "cheap", Target: "deepseek", Status: 200, Ms: 1000},
					{Time: ago(11), Target: "anthropic", Status: 200, Ms: 2000},
					{Time: ago(3), Route: "cheap", Target: "deepseek", Status: 200, Ms: 1000}, // not the latest
					{Time: ago(3), Target: "anthropic", Status: 200, Ms: 1000},
				},
			},
			want: graph{
				sources: []source{
					{name: "cheap", stats: &stats}, {name: "coder", stats: &stats, live: true}, {name: "idle"},
					{name: restName, live: true},
				},
				targets: []target{{"deepseek", "flash", stateReady}, anth},
				edge:    []int{0, 0, 0, 1},
			},
		},
		{
			name: "a request that ended exactly ten seconds ago is no longer live",
			st:   graphState(nil, nil),
			act:  &admin.Activity{Recent: []admin.Entry{{Time: ago(12), Target: "anthropic", Status: 200, Ms: 2000}}},
			want: graph{sources: []source{rest}, targets: []target{anth}, edge: []int{0}},
		},
		{
			name: "no activity",
			st:   graphState(literal, map[string]config.Route{"coder": {Provider: "kimi", Model: "k3"}}),
			act:  nil,
			want: graph{
				sources: []source{{name: "coder"}, rest},
				targets: []target{{"kimi", "k3", stateReady}, anth},
				edge:    []int{0, 1},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildGraph(c.st, c.act, now); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGraph:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}
