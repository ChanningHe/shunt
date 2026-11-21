package admin

import (
	"crypto/subtle"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// IsLocalHost reports whether a Host header (or URL host) names this machine.
func IsLocalHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckURL accepts only a plain http URL of this machine without path or
// query: the router (and so the admin token) must never leave the host.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("router URL %q must look like http://127.0.0.1:PORT", raw)
	}
	if !IsLocalHost(u.Host) {
		return fmt.Errorf("router URL %q must point at this machine", raw)
	}
	return nil
}

// checkRequest applies the browser defenses to every /_shunt request: a
// local Host (DNS rebinding), an Origin, if any, of exactly this server
// ("null" included in the refusal), and for state-changing methods a
// same-origin Sec-Fetch-Site when present plus a JSON content type, which
// a cross-site form or simple request cannot send.
func checkRequest(r *http.Request) *apiError {
	if !IsLocalHost(r.Host) {
		return errorf(http.StatusForbidden, codeForbidden, "only local clients are served")
	}
	if origin, ok := r.Header["Origin"]; ok && (len(origin) != 1 || origin[0] != "http://"+r.Host) {
		return errorf(http.StatusForbidden, codeForbidden, "cross-origin requests are refused")
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return errorf(http.StatusForbidden, codeForbidden, "cross-site requests are refused")
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errorf(http.StatusUnsupportedMediaType, codeUnsupportedMedia, "Content-Type must be application/json")
	}
	return nil
}

// authorized checks the bearer token in constant time.
func (h *Handler) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && h.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}
