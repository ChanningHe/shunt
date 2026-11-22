package tui

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

// The words and rules here mirror buildGraph in the web UI's graph.js; a
// change to one is a change to both.
const (
	stateReady      = "ready"
	stateNoKey      = "no key"
	stateKeyInvalid = "key invalid"
	stateNoProvider = "no such provider"
	stateUnchanged  = "unchanged" // the anthropic target: requests pass as sent

	restName   = "everything else"
	anthropic  = "anthropic"
	liveWindow = 10 * time.Second
)

// graph is where requests go: sources[i] is sent to targets[edge[i]]. The
// last source is always restName and goes to the last target, anthropic.
type graph struct {
	sources []source
	targets []target
	edge    []int
}

type source struct {
	name  string // route name, or restName
	alias bool
	stats *admin.RouteStats // nil before the route's first request
	live  bool
}

type target struct {
	provider, model string
	state           string
}

// buildGraph derives the graph from a state and its activity (nil: none).
func buildGraph(st *admin.State, act *admin.Activity, now time.Time) graph {
	routes := st.Config.Routes
	names := slices.Sorted(maps.Keys(routes))
	key := func(r config.Route) target { return target{provider: r.Provider, model: r.Model} }

	var g graph
	for _, n := range names {
		if k := key(routes[n]); !slices.Contains(g.targets, k) {
			g.targets = append(g.targets, k)
		}
	}
	slices.SortFunc(g.targets, func(a, b target) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.model, b.model))
	})
	for _, n := range names {
		g.edge = append(g.edge, slices.Index(g.targets, key(routes[n])))
		s := source{name: n, alias: admin.IsAlias(n)}
		if act != nil {
			if rs, ok := act.Routes[n]; ok {
				s.stats = &rs
			}
		}
		s.live = live(act, now, func(e admin.Entry) bool { return e.Route == n })
		g.sources = append(g.sources, s)
	}
	for i := range g.targets {
		g.targets[i].state = targetState(st, g.targets[i].provider)
	}

	g.targets = append(g.targets, target{provider: anthropic, state: stateUnchanged})
	g.sources = append(g.sources, source{name: restName, live: live(act, now, func(e admin.Entry) bool { return e.Target == anthropic })})
	g.edge = append(g.edge, len(g.targets)-1)
	return g
}

func targetState(st *admin.State, provider string) string {
	if _, ok := st.Config.Providers[provider]; !ok {
		return stateNoProvider
	}
	switch st.Keys[provider] {
	case config.KeyMissing:
		return stateNoKey
	case config.KeyInvalid:
		return stateKeyInvalid
	}
	return stateReady
}

// live reports whether the latest matching request ended within liveWindow.
// Entry.Time is when a request started, so a long one counts from its end.
func live(act *admin.Activity, now time.Time, match func(admin.Entry) bool) bool {
	if act == nil {
		return false
	}
	for _, e := range act.Recent { // newest first
		if match(e) {
			end := e.Time.Add(time.Duration(e.Ms) * time.Millisecond)
			return now.Sub(end) < liveWindow
		}
	}
	return false
}
