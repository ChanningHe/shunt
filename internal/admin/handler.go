// Package admin is shunt's management API: the only writer of the config
// directory, served under Prefix on the router's own port, plus the Go
// client every front end (CLI, TUI) uses to reach it.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"shunt/internal/config"
)

const (
	// Prefix is the admin namespace. Every path in it is answered here and
	// never forwarded upstream.
	Prefix    = "/_shunt"
	apiPrefix = Prefix + "/api/v1/"

	maxBodyBytes = 64 << 10
	syncCommand  = "shunt on"
)

// Aliases are the Claude Code model aliases a route of the same name binds
// through ANTHROPIC_DEFAULT_<ALIAS>_MODEL.
var Aliases = []string{"sonnet", "opus", "haiku", "fable"}

// IsAlias reports whether name is one of Aliases.
func IsAlias(name string) bool { return slices.Contains(Aliases, name) }

// Handler serves Prefix. Construct it with New.
type Handler struct {
	store  *config.Store
	token  string
	listen string
	log    *slog.Logger
	now    func() time.Time
	codes  webCodes
	// Activity is what GET /activity reports; the router records into it.
	Activity *Recorder
	// Probe sends one minimal request through the named route; the router
	// sets it before serving. Nil answers 501.
	Probe func(ctx context.Context, route string) TestResult
	api   map[string]map[string]endpoint // pattern -> method -> endpoint
}

// endpoint handles one API call; name is the {name} path segment, if any.
type endpoint func(w http.ResponseWriter, r *http.Request, name string) (any, error)

// New returns the admin handler. token is the bearer secret (non-empty);
// listen is the address the router actually listens on, for GET /state.
func New(store *config.Store, token, listen string, log *slog.Logger) *Handler {
	h := &Handler{store: store, token: token, listen: listen, log: log, now: time.Now, Activity: NewRecorder()}
	h.api = map[string]map[string]endpoint{
		"state":           {http.MethodGet: h.getState},
		"providers/*":     {http.MethodPut: h.putProvider, http.MethodDelete: h.deleteProvider},
		"providers/*/key": {http.MethodPut: h.putKey, http.MethodDelete: h.deleteKey},
		"routes/*":        {http.MethodPut: h.putRoute, http.MethodDelete: h.deleteRoute},
		"routes/*/test":   {http.MethodPost: h.testRoute},
		"activity":        {http.MethodGet: h.getActivity},
		"reload":          {http.MethodPost: h.reload},
		"web-code":        {http.MethodPost: h.webCode},
		"web-token":       {http.MethodPost: h.webToken},
	}
	return h
}

// ServeHTTP logs method, path, status and duration only: never bodies,
// headers or tokens.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	h.serve(sw, r)
	h.log.Info("admin", "method", r.Method, "path", r.URL.Path, "status", sw.status,
		"ms", time.Since(start).Milliseconds())
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	if err := checkRequest(r); err != nil {
		writeError(w, err)
		return
	}
	// Route on the escaped path: an encoded "/" inside a name must not
	// become a path separator. The router also sends non-canonical
	// spellings of Prefix here (see withAdmin); they are answered 404.
	p := r.URL.EscapedPath()
	if p != Prefix && !strings.HasPrefix(p, Prefix+"/") {
		writeError(w, errorf(http.StatusNotFound, codeNotFound, "not found"))
		return
	}
	rest := strings.TrimPrefix(p, Prefix)
	call, isAPI := strings.CutPrefix(p, apiPrefix)
	switch {
	case isAPI:
		h.serveAPI(w, r, call)
	case rest == "/api" || strings.HasPrefix(rest, "/api/"):
		writeError(w, errorf(http.StatusNotFound, codeNotFound, "no such endpoint"))
	default:
		serveStatic(w, r, rest)
	}
}

func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, call string) {
	pattern, name, nameErr := splitCall(call)
	if pattern != "web-token" && !h.authorized(r) {
		writeError(w, errorf(http.StatusUnauthorized, codeUnauthorized, "missing or wrong admin token"))
		return
	}
	methods, ok := h.api[pattern]
	if !ok {
		writeError(w, errorf(http.StatusNotFound, codeNotFound, "no such endpoint"))
		return
	}
	ep, ok := methods[r.Method]
	if !ok {
		writeError(w, errorf(http.StatusMethodNotAllowed, codeMethod, "method %s not allowed here", r.Method))
		return
	}
	if nameErr != nil || (strings.Contains(pattern, "*") && !config.ValidName(name)) {
		writeError(w, errorf(http.StatusBadRequest, codeBadRequest, "invalid name; names match [a-z0-9][a-z0-9-]*"))
		return
	}
	out, err := ep(w, r, name)
	if err != nil {
		writeError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// splitCall maps the escaped call "providers/x/key" to ("providers/*/key",
// "x"), decoding each segment after splitting. Calls that match no pattern
// map to something absent from the table; err reports a bad escape.
func splitCall(call string) (pattern, name string, err error) {
	segs := strings.Split(call, "/")
	for i, s := range segs {
		if segs[i], err = url.PathUnescape(s); err != nil {
			return "", "", err
		}
	}
	if len(segs) >= 2 && (segs[0] == "providers" || segs[0] == "routes") && segs[1] != "" {
		name, segs[1] = segs[1], "*"
	}
	return strings.Join(segs, "/"), name, nil
}

// decodeBody strictly decodes a JSON object of at most maxBodyBytes into v.
// Error messages never quote the body, which may hold a key.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &tooBig):
		return errorf(http.StatusRequestEntityTooLarge, codeTooLarge, "request body over %d bytes", maxBodyBytes)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return errorf(http.StatusBadRequest, codeBadRequest, "%s", strings.TrimPrefix(err.Error(), "json: "))
	default:
		return errorf(http.StatusBadRequest, codeBadRequest, "request body must be one JSON object of the expected shape")
	}
}

// Error codes in {"error":{"code":...}}.
const (
	codeBadRequest       = "bad_request"
	codeInvalid          = "invalid"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeNotFound         = "not_found"
	codeMethod           = "method_not_allowed"
	codeConflict         = "conflict"
	codeTooLarge         = "too_large"
	codeUnsupportedMedia = "unsupported_media_type"
	codeInternal         = "write_failed"
	codeNotImplemented   = "not_implemented"
)

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{status, code, fmt.Sprintf(format, args...)}
}

// toAPIError maps Store and handler errors to HTTP. Store messages carry
// file paths and config values, never key material.
func toAPIError(err error) *apiError {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, config.ErrConflict):
		return errorf(http.StatusConflict, codeConflict, "%v", err)
	case errors.Is(err, config.ErrInvalid):
		return errorf(http.StatusBadRequest, codeInvalid, "%v", err)
	default:
		return errorf(http.StatusInternalServerError, codeInternal, "%v", err)
	}
}

func writeError(w http.ResponseWriter, e *apiError) {
	type body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, e.status, map[string]body{"error": {e.code, e.message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"write_failed","message":"encoding failed"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// webCodes holds the single live one-time code `shunt web` puts in a URL
// fragment; the page trades it for the token.
type webCodes struct {
	mu      sync.Mutex
	code    string
	expires time.Time
}

const webCodeTTL = 60 * time.Second
