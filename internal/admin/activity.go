package admin

import (
	"strings"
	"sync"
	"time"
)

const (
	maxRecent     = 200 // requests kept in memory
	maxEntryField = 120 // runes of an Entry's Path or Model
)

// Printable makes an untrusted string safe to show in a terminal or a log
// line: invalid UTF-8 and control characters (C0, DEL, C1) are dropped,
// whitespace runs collapse to one space, and at most limit runes are kept.
func Printable(s string, limit int) string {
	s = strings.Map(func(c rune) rune {
		if c < ' ' || c == 0x7f || (c >= 0x80 && c < 0xa0) {
			return ' '
		}
		return c
	}, strings.ToValidUTF8(s, "?"))
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "..."
	}
	return s
}

// Entry is one request the router answered: metadata only, never bodies or
// headers. Path and Model come from the client; Record makes them Printable.
type Entry struct {
	Time   time.Time `json:"time"`
	Path   string    `json:"path"`
	Model  string    `json:"model"`
	Route  string    `json:"route,omitempty"` // route name, when sent to its provider
	Target string    `json:"target"`          // "anthropic", "rejected" or a provider name
	Status int       `json:"status"`
	Ms     int64     `json:"ms"`
}

// RouteStats counts the requests a route sent to its provider.
type RouteStats struct {
	Requests int       `json:"requests"`
	Errors   int       `json:"errors"` // status >= 400
	Status   int       `json:"status"` // of the last request
	Time     time.Time `json:"time"`   // of the last request
}

// Activity is the GET /activity response.
type Activity struct {
	Since  time.Time             `json:"since"`  // when the router started counting
	Routes map[string]RouteStats `json:"routes"` // by route name
	Recent []Entry               `json:"recent"` // newest first
}

// Recorder keeps the last maxRecent requests and per-route counters, in
// memory only. A nil Recorder records nothing.
type Recorder struct {
	mu     sync.Mutex
	since  time.Time
	ring   []Entry // oldest first once full, starting at next
	next   int
	routes map[string]RouteStats
}

func NewRecorder() *Recorder {
	return &Recorder{since: time.Now(), routes: map[string]RouteStats{}}
}

func (r *Recorder) Record(e Entry) {
	if r == nil {
		return
	}
	e.Path, e.Model = Printable(e.Path, maxEntryField), Printable(e.Model, maxEntryField)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ring) < maxRecent {
		r.ring = append(r.ring, e)
	} else {
		r.ring[r.next] = e
		r.next = (r.next + 1) % maxRecent
	}
	if e.Route == "" {
		return
	}
	s := r.routes[e.Route]
	s.Requests++
	if e.Status >= 400 {
		s.Errors++
	}
	s.Status, s.Time = e.Status, e.Time
	r.routes[e.Route] = s
}

// forget drops the counters of routes that no longer exist or changed their
// target, so the map stays bounded by the config and never credits a new
// target with old requests.
func (r *Recorder) forget(exists func(route string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.routes {
		if !exists(name) {
			delete(r.routes, name)
		}
	}
}

// Snapshot copies the current activity, newest request first.
func (r *Recorder) Snapshot() Activity {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := Activity{Since: r.since, Routes: make(map[string]RouteStats, len(r.routes)), Recent: make([]Entry, 0, len(r.ring))}
	for name, s := range r.routes {
		a.Routes[name] = s
	}
	for i := len(r.ring) - 1; i >= 0; i-- {
		a.Recent = append(a.Recent, r.ring[(r.next+i)%len(r.ring)])
	}
	return a
}
