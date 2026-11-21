// Package config is shunt's file-backed configuration: the data model, the
// toml/yaml/json codec, key resolution and the single-writer Store.
package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Config is the content of config.<ext>. The Anthropic upstream is a
// constructor argument of the proxy and deliberately not representable here.
type Config struct {
	Listen    string              `json:"listen,omitempty"`
	Providers map[string]Provider `json:"providers,omitempty"`
	Routes    map[string]Route    `json:"routes,omitempty"`
}

// Provider is an Anthropic-compatible third-party upstream. Empty lists mean
// "use the default" (see WithDefaults); defaults are never written to disk.
type Provider struct {
	BaseURL    string   `json:"base_url"`
	BetaAllow  []string `json:"beta_allow,omitempty"`  // anthropic-beta tokens forwarded; empty = drop the header
	TopKeys    []string `json:"top_keys,omitempty"`    // body top-level allowlist
	BlockTypes []string `json:"block_types,omitempty"` // allowed message content block types
}

// Route sends model WirePrefix+<route name> to Model at Provider.
type Route struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Keys is the content of keys.<ext>: provider name -> literal key or
// "file:///abs/path".
type Keys map[string]string

const (
	DefaultListen = "127.0.0.1:27486"
	WirePrefix    = "shunt/" // wire model id = WirePrefix + route name
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

	defaultTopKeys = []string{
		"model", "messages", "system", "max_tokens", "stream", "tools", "tool_choice",
		"thinking", "stop_sequences", "temperature", "top_p", "output_config",
	}
	defaultBlockTypes = []string{"text", "image", "document", "thinking", "tool_use", "tool_result"}
)

// ValidName reports whether s is a valid provider or route name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// DefaultDir is $SHUNT_CONFIG_DIR, else $XDG_CONFIG_HOME/shunt (absolute
// only, per the XDG spec), else ~/.config/shunt. It returns "" when no home
// directory is known.
func DefaultDir() string {
	if d := os.Getenv("SHUNT_CONFIG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "shunt")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "shunt")
}

// ListenAddr is Listen, or DefaultListen when unset.
func (c Config) ListenAddr() string {
	if c.Listen == "" {
		return DefaultListen
	}
	return c.Listen
}

// WithDefaults fills omitted TopKeys/BlockTypes with fresh copies of the
// defaults. BetaAllow has no default: empty drops the header.
func (p Provider) WithDefaults() Provider {
	if len(p.TopKeys) == 0 {
		p.TopKeys = slices.Clone(defaultTopKeys)
	}
	if len(p.BlockTypes) == 0 {
		p.BlockTypes = slices.Clone(defaultBlockTypes)
	}
	return p
}

// Match resolves a wire model id. owned reports whether the id carries
// WirePrefix (such ids must never be passed through); ok reports whether the
// route and its provider exist. p has WithDefaults applied.
func (c Config) Match(model string) (name string, r Route, p Provider, owned, ok bool) {
	name, owned = strings.CutPrefix(model, WirePrefix)
	if !owned {
		return "", Route{}, Provider{}, false, false
	}
	r, ok = c.Routes[name]
	if !ok {
		return name, Route{}, Provider{}, true, false
	}
	p, ok = c.Providers[r.Provider]
	if !ok {
		return name, Route{}, Provider{}, true, false
	}
	return name, r, p.WithDefaults(), true, true
}

// Validate checks everything that does not need the keys file.
func (c Config) Validate() error {
	if c.Listen != "" {
		host, _, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return fmt.Errorf("listen %q: %w", c.Listen, err)
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("listen %q: only loopback addresses are allowed", c.Listen)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("provider name %q: must match %s", name, nameRE)
		}
		if err := checkUpstream(c.Providers[name].BaseURL); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Routes)) {
		r := c.Routes[name]
		if !nameRE.MatchString(name) {
			return fmt.Errorf("route name %q: must match %s", name, nameRE)
		}
		if r.Model == "" {
			return fmt.Errorf("route %s: model is required", name)
		}
		if strings.ContainsFunc(r.Model, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
			return fmt.Errorf("route %s: model must not contain control characters", name)
		}
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("route %s: unknown provider %q", name, r.Provider)
		}
	}
	return nil
}

// checkUpstream requires https, except plain http to a loopback host (tests).
func checkUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" || u.RawQuery != "" || u.User != nil {
		return errors.New("base URL must be scheme://host[/path] without query or userinfo")
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("base URL %q must use https", raw)
}
