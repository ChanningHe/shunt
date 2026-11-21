package admin

import (
	"crypto/subtle"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"shunt/internal/config"
)

// State is the GET /state response.
type State struct {
	Config    StateConfig       `json:"config"`
	Keys      map[string]string `json:"keys"` // provider -> config.Key* state
	Files     []File            `json:"files"`
	LoadError *string           `json:"load_error"`
	Listen    string            `json:"listen"` // the address actually served
}

// StateConfig is the configuration as loaded; maps are never null.
type StateConfig struct {
	Listen    string                     `json:"listen"`
	Providers map[string]config.Provider `json:"providers"`
	Routes    map[string]config.Route    `json:"routes"`
}

// File describes config.<ext> (first) or keys.<ext> (second).
type File struct {
	Path     string `json:"path"`
	Format   string `json:"format"`
	Exists   bool   `json:"exists"`
	Writable bool   `json:"writable"`
}

// RouteResult is the PUT/DELETE /routes/{name} response. NeedsSync means
// the set of alias routes changed, so settings.json needs SyncCommand.
type RouteResult struct {
	NeedsSync   bool   `json:"needs_sync"`
	SyncCommand string `json:"sync_command"`
}

// WebCode is the POST /web-code response.
type WebCode struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// TestResult is the POST /routes/{name}/test response: the outcome of one
// minimal request sent to the route's provider. Message is the provider's
// error text (or what stopped the request), safe to show: bounded, without
// control characters or the key.
type TestResult struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status"` // HTTP status of the provider; 0 if it never answered
	Ms      int64  `json:"ms"`
	Message string `json:"message,omitempty"`
}

// TestSummary is the one line every front end shows for a route test.
func TestSummary(route string, r TestResult) string {
	switch {
	case r.OK:
		return fmt.Sprintf("route %s: ok (HTTP %d, %d ms)", route, r.Status, r.Ms)
	case r.Status == 0:
		return fmt.Sprintf("route %s: failed: %s", route, r.Message)
	}
	return fmt.Sprintf("route %s: failed (HTTP %d, %d ms): %s", route, r.Status, r.Ms, r.Message)
}

type empty struct{}

func (h *Handler) getState(_ http.ResponseWriter, _ *http.Request, _ string) (any, error) {
	snap := h.store.Load()
	st := State{
		Config: StateConfig{
			Listen:    snap.Config.Listen,
			Providers: orEmpty(snap.Config.Providers),
			Routes:    orEmpty(snap.Config.Routes),
		},
		Keys:   map[string]string{},
		Listen: h.listen,
	}
	for name := range snap.Config.Providers {
		_, st.Keys[name] = h.store.Key(snap, name)
	}
	for _, f := range snap.Files {
		st.Files = append(st.Files, File(f))
	}
	if snap.LoadErr != nil {
		msg := snap.LoadErr.Error()
		st.LoadError = &msg
	}
	return st, nil
}

func orEmpty[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

// update refuses changes while the running config failed to load: the user
// fixes the files and reloads first, so a write never papers over them.
func (h *Handler) update(fn func(*config.Config, config.Keys) error) error {
	if err := h.store.Load().LoadErr; err != nil {
		return errorf(http.StatusConflict, codeConflict, "the config failed to load; fix it and reload first: %v", err)
	}
	return h.store.Update(fn)
}

func (h *Handler) putProvider(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	var in struct {
		BaseURL string `json:"base_url"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return nil, err
	}
	if in.BaseURL == "" {
		return nil, errorf(http.StatusBadRequest, codeBadRequest, "base_url is required")
	}
	var out struct {
		KeyCleared bool `json:"key_cleared"`
	}
	err := h.update(func(c *config.Config, k config.Keys) error {
		if c.Providers == nil {
			c.Providers = map[string]config.Provider{}
		}
		p := c.Providers[name] // merge: hand-written lists are kept
		// A literal key must not follow the provider to another host. A key
		// left over without a provider has no host to compare: drop it too.
		if ref, ok := k[name]; ok && !strings.HasPrefix(ref, config.KeyFilePrefix) && !sameOrigin(p.BaseURL, in.BaseURL) {
			delete(k, name)
			out.KeyCleared = true
		}
		p.BaseURL = in.BaseURL
		c.Providers[name] = p
		return nil
	})
	return out, err
}

// sameOrigin compares scheme and host:port, ignoring case. Anything that
// does not parse is a different origin.
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host != "" &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func (h *Handler) deleteProvider(_ http.ResponseWriter, _ *http.Request, name string) (any, error) {
	return empty{}, h.update(func(c *config.Config, k config.Keys) error {
		if _, ok := c.Providers[name]; !ok {
			return errorf(http.StatusNotFound, codeNotFound, "no provider %q", name)
		}
		for _, rn := range slices.Sorted(maps.Keys(c.Routes)) {
			if c.Routes[rn].Provider == name {
				return errorf(http.StatusConflict, codeConflict, "provider %q is used by route %q; remove or move the route first", name, rn)
			}
		}
		delete(c.Providers, name)
		delete(k, name)
		return nil
	})
}

const anthropicKeyMark = "sk-ant-"

// checkKeyValue validates a keys-file value without ever quoting it.
func checkKeyValue(v string) *apiError {
	bad := func(msg string) *apiError { return errorf(http.StatusBadRequest, codeBadRequest, "%s", msg) }
	switch path, isFile := strings.CutPrefix(v, config.KeyFilePrefix); {
	case v == "":
		return bad("value is required")
	case strings.Contains(strings.ToLower(v), anthropicKeyMark):
		return bad("Anthropic keys are never sent to third-party providers")
	case strings.ContainsFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }):
		return bad("value must not contain whitespace or control characters")
	case isFile && !filepath.IsAbs(path):
		return bad("file:// values need an absolute path, as in file:///run/secrets/key")
	}
	return nil
}

func (h *Handler) putKey(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	var in struct {
		Value string `json:"value"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return nil, err
	}
	if err := checkKeyValue(in.Value); err != nil {
		return nil, err
	}
	err := h.update(func(c *config.Config, k config.Keys) error {
		if _, ok := c.Providers[name]; !ok {
			return errorf(http.StatusNotFound, codeNotFound, "no provider %q; add it first", name)
		}
		k[name] = in.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, state := h.store.Key(h.store.Load(), name)
	return struct {
		State string `json:"state"`
	}{state}, nil
}

func (h *Handler) deleteKey(_ http.ResponseWriter, _ *http.Request, name string) (any, error) {
	return empty{}, h.update(func(c *config.Config, k config.Keys) error {
		if _, ok := c.Providers[name]; !ok {
			return errorf(http.StatusNotFound, codeNotFound, "no provider %q", name)
		}
		delete(k, name)
		return nil
	})
}

func (h *Handler) putRoute(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	var in config.Route
	if err := decodeBody(w, r, &in); err != nil {
		return nil, err
	}
	var created bool
	err := h.update(func(c *config.Config, _ config.Keys) error {
		if c.Routes == nil {
			c.Routes = map[string]config.Route{}
		}
		_, exists := c.Routes[name]
		created = !exists
		c.Routes[name] = in
		return nil
	})
	if err == nil {
		h.Activity.forget(func(route string) bool { return route != name })
	}
	return RouteResult{NeedsSync: created && IsAlias(name), SyncCommand: syncCommand}, err
}

func (h *Handler) deleteRoute(_ http.ResponseWriter, _ *http.Request, name string) (any, error) {
	err := h.update(func(c *config.Config, _ config.Keys) error {
		if _, ok := c.Routes[name]; !ok {
			return errorf(http.StatusNotFound, codeNotFound, "no route %q", name)
		}
		delete(c.Routes, name)
		return nil
	})
	if err == nil {
		h.Activity.forget(func(route string) bool { return route != name })
	}
	return RouteResult{NeedsSync: IsAlias(name), SyncCommand: syncCommand}, err
}

func (h *Handler) getActivity(_ http.ResponseWriter, _ *http.Request, _ string) (any, error) {
	routes := h.store.Load().Config.Routes
	h.Activity.forget(func(name string) bool { _, ok := routes[name]; return ok })
	return h.Activity.Snapshot(), nil
}

// testRoute asks the router to send one minimal request through the route.
// It costs a few tokens at the provider, so only a user action triggers it.
func (h *Handler) testRoute(_ http.ResponseWriter, r *http.Request, name string) (any, error) {
	if _, ok := h.store.Load().Config.Routes[name]; !ok {
		return nil, errorf(http.StatusNotFound, codeNotFound, "no route %q", name)
	}
	if h.Probe == nil {
		return nil, errorf(http.StatusNotImplemented, codeNotImplemented, "this router cannot test routes")
	}
	return h.Probe(r.Context(), name), nil
}

func (h *Handler) reload(_ http.ResponseWriter, _ *http.Request, _ string) (any, error) {
	if err := h.store.Reload(); err != nil {
		return nil, errorf(http.StatusConflict, codeConflict, "reload failed, running config kept: %v", err)
	}
	return empty{}, nil
}

func (h *Handler) webCode(_ http.ResponseWriter, _ *http.Request, _ string) (any, error) {
	code, err := NewToken()
	if err != nil {
		return nil, err
	}
	h.codes.mu.Lock()
	defer h.codes.mu.Unlock()
	h.codes.code, h.codes.expires = code, h.now().Add(webCodeTTL)
	return WebCode{Code: code, ExpiresIn: int(webCodeTTL / time.Second)}, nil
}

// webToken trades the live one-time code for the token. Any attempt that
// presents the right code consumes it.
func (h *Handler) webToken(w http.ResponseWriter, r *http.Request, _ string) (any, error) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return nil, err
	}
	h.codes.mu.Lock()
	defer h.codes.mu.Unlock()
	c := &h.codes
	if c.code == "" || !h.now().Before(c.expires) || subtle.ConstantTimeCompare([]byte(in.Code), []byte(c.code)) != 1 {
		return nil, errorf(http.StatusUnauthorized, codeUnauthorized, "code is wrong, used or expired; run shunt web again")
	}
	c.code = ""
	return struct {
		Token string `json:"token"`
	}{h.token}, nil
}
