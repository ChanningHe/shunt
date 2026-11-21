package router

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const (
	maxBodyBytes   = 32 << 20 // Anthropic's own Messages API request limit
	messagesPath   = "/v1/messages"
	countTokenPath = "/v1/messages/count_tokens"
	targetAnthro   = "anthropic"
	targetRejected = "rejected"
	maxLoggedModel = 80
	// statusClientGone is logged (never sent) when the client disconnects.
	statusClientGone = 499
)

// hopHeaders are connection-scoped and never forwarded (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type proxy struct {
	anthropicBase string   // passthrough upstream, scheme://host
	anthropic     *url.URL // parsed anthropicBase; passthrough may only go here
	store         *config.Store
	client        *http.Client
	log           *slog.Logger
	rec           *admin.Recorder // nil: nothing recorded
}

// newProxy routes by the store's current snapshot on every request.
// anthropic is the passthrough upstream: anthropicAPI in production, a fake
// server in tests; it must parse as a URL.
func newProxy(anthropic string, store *config.Store, log *slog.Logger) *proxy {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Keep the client's Accept-Encoding and pass compressed bytes through
	// untouched instead of letting the transport add/strip gzip (I1).
	tr.DisableCompression = true
	client := &http.Client{
		Transport: tr,
		// Relay 3xx to the client; following would change the response (I1)
		// and carry credentials to whatever host the Location names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u, err := url.Parse(anthropic)
	if err != nil {
		panic("bad anthropic upstream: " + err.Error())
	}
	return &proxy{anthropicBase: anthropic, anthropic: u, store: store, client: client, log: log}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status, target, model, route := p.serve(w, r)
	ms := time.Since(start).Milliseconds()
	p.log.Info("request",
		"method", r.Method, "path", r.URL.Path, "model", logModel(model),
		"target", target, "status", status, "ms", ms)
	if target == targetRejected && model == "" {
		return // refused before it was read (foreign Host, browser, bad target): not the user's traffic
	}
	p.rec.Record(admin.Entry{Time: start, Path: r.URL.Path, Model: logModel(model), Route: route, Target: target, Status: status, Ms: ms})
}

func (p *proxy) serve(w http.ResponseWriter, r *http.Request) (status int, target, model, via string) {
	// Only local, non-browser clients: a foreign Host means DNS rebinding,
	// an Origin header means a web page (Claude Code sends neither).
	if !isLocalHost(r.Host) || r.Header.Get("Origin") != "" {
		return reject(w, http.StatusForbidden, "shunt: only local non-browser clients are served"), targetRejected, "", ""
	}
	if r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") {
		return reject(w, http.StatusBadRequest, "shunt: bad request target"), targetRejected, "", ""
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return reject(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable"), targetRejected, "", ""
	}
	var peek struct {
		Model  string          `json:"model"`
		System json.RawMessage `json:"system"`
	}
	if r.Method == http.MethodPost && len(body) > 0 {
		_ = json.Unmarshal(body, &peek) // not JSON => no model => passthrough
	}
	snap := p.store.Load()
	name, route, prov, owned, ok := snap.Config.Match(peek.Model)
	if !owned {
		return p.passthrough(w, r, body), targetAnthro, peek.Model, ""
	}
	if !ok {
		return reject(w, http.StatusBadRequest, noRouteMessage(name, snap.LoadErr != nil)), targetRejected, peek.Model, ""
	}

	switch r.URL.Path {
	case countTokenPath:
		return p.countTokens(w, prov, route, body), route.Provider, peek.Model, ""
	case messagesPath:
	default:
		return reject(w, http.StatusBadRequest, fmt.Sprintf(
			"shunt: model %q is routed to %s, which only serves %s", logModel(peek.Model), route.Provider, messagesPath)), targetRejected, peek.Model, ""
	}
	if !isSubagent(r.Header, peek.System) {
		return reject(w, http.StatusBadRequest, fmt.Sprintf(
			"shunt: model %q routes to %s only for Claude Code subagent requests; this looks like a main-thread request. "+
				"Switch the main thread to opus (/model opus) or to a full model id such as claude-sonnet-5-5, "+
				"or turn routing off with: shunt off.", logModel(peek.Model), route.Provider)), targetRejected, peek.Model, ""
	}
	return p.thirdParty(w, r, snap, prov, route, body), route.Provider, peek.Model, name
}

// noRouteMessage explains a shunt/<name> id without a route. Such ids are
// never passed through: the shunt/ namespace belongs to the router.
func noRouteMessage(name string, loadFailed bool) string {
	msg := fmt.Sprintf("shunt: no route named %q (model %q); add one with: shunt route set %s PROVIDER MODEL",
		logModel(name), logModel(config.WirePrefix+name), cmp.Or(logModel(name), "NAME"))
	if loadFailed {
		msg += " (the config failed to load; see the shunt log)"
	}
	return msg
}

// passthrough forwards the request to Anthropic with the original body bytes
// and the original headers minus hop-by-hop ones (invariant I1).
func (p *proxy) passthrough(w http.ResponseWriter, r *http.Request, body []byte) int {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.anthropicBase+r.URL.RequestURI(), bodyReader(body))
	if err != nil || out.URL.Scheme != p.anthropic.Scheme || out.URL.Host != p.anthropic.Host {
		return reject(w, http.StatusBadRequest, "shunt: bad request target")
	}
	out.Header = r.Header.Clone()
	removeHopHeaders(out.Header)
	if _, ok := r.Header["User-Agent"]; !ok {
		out.Header["User-Agent"] = []string{""} // stop Go from adding its own
	}
	out.ContentLength = int64(len(body))
	return p.relay(w, out)
}

// thirdParty sanitizes the request, builds headers from an allowlist and
// refuses to send anything containing an Anthropic credential (invariant I2).
func (p *proxy) thirdParty(w http.ResponseWriter, r *http.Request, snap *config.Snapshot, prov config.Provider, route config.Route, body []byte) int {
	key, state := p.store.Key(snap, route.Provider)
	if key == "" {
		return reject(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"shunt: key for provider %s is %s; provider unavailable", route.Provider, state))
	}
	strip := profile(prov)
	clean, err := sanitize(strip, route.Model, body)
	if err != nil {
		return reject(w, http.StatusBadRequest, "shunt: cannot sanitize request: "+err.Error())
	}
	h := outboundHeaders(r.Header, strip, key)
	if where := findCredential(h, clean); where != "" {
		p.log.Error("refused outbound request: credential pattern found", "provider", route.Provider, "where", where)
		return reject(w, http.StatusBadGateway, fmt.Sprintf(
			"shunt: refused to send request to %s: it contains an Anthropic credential pattern (%s) in %s", route.Provider, credentialToken, where))
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, prov.BaseURL+messagesPath, bytes.NewReader(clean))
	if err != nil {
		return reject(w, http.StatusInternalServerError, "shunt: cannot build upstream request")
	}
	out.Header = h
	return p.relay(w, out)
}

func outboundHeaders(in http.Header, s Sanitize, key string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	h.Set("User-Agent", "shunt")
	if v := in.Get("Anthropic-Version"); v != "" {
		h.Set("Anthropic-Version", v)
	}
	var betas []string
	for _, v := range in.Values("Anthropic-Beta") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" && slices.Contains(s.BetaAllow, tok) {
				betas = append(betas, tok)
			}
		}
	}
	if len(betas) > 0 {
		h.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	h.Set("X-Api-Key", key)
	return h
}

// countTokens estimates locally (~4 bytes per token on the sanitized body);
// nothing leaves the machine.
func (p *proxy) countTokens(w http.ResponseWriter, prov config.Provider, route config.Route, body []byte) int {
	clean, err := sanitize(profile(prov), route.Model, body)
	if err != nil {
		return reject(w, http.StatusBadRequest, "shunt: cannot sanitize request: "+err.Error())
	}
	return writeJSON(w, http.StatusOK, map[string]int{"input_tokens": (len(clean) + 3) / 4})
}

// relay sends out and streams the response back chunk by chunk.
func (p *proxy) relay(w http.ResponseWriter, out *http.Request) int {
	resp, err := p.client.Do(out)
	if err != nil {
		if out.Context().Err() != nil {
			return statusClientGone
		}
		p.log.Error("upstream request failed", "host", out.URL.Host, "err", err)
		return reject(w, http.StatusBadGateway, "shunt: upstream "+out.URL.Host+" unreachable")
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if k == "Set-Cookie" {
			continue
		}
		w.Header()[k] = vs
	}
	removeHopHeaders(w.Header())
	w.WriteHeader(resp.StatusCode)
	if err := stream(w, resp.Body); err != nil {
		p.log.Warn("stream interrupted", "host", out.URL.Host, "err", err)
	}
	return resp.StatusCode
}

func stream(w http.ResponseWriter, body io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if ferr := rc.Flush(); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// isLocalHost reports whether a Host header names this machine.
func isLocalHost(hostport string) bool { return admin.IsLocalHost(hostport) }

func bodyReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

// reject writes an Anthropic-shaped error so Claude Code shows the message.
func reject(w http.ResponseWriter, status int, msg string) int {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "api_error"
	}
	return writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) int {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return status
}

// logModel bounds an untrusted model string for logs and error messages.
func logModel(m string) string {
	if strings.Contains(strings.ToLower(m), credentialToken) {
		return "<redacted>"
	}
	if len(m) > maxLoggedModel {
		return m[:maxLoggedModel] + "..."
	}
	return m
}
