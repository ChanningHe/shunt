package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Listen: "127.0.0.1:27486",
		Providers: map[string]Provider{
			"deepseek": {BaseURL: "https://api.deepseek.com/anthropic"},
		},
		Routes: map[string]Route{
			"sonnet": {Provider: "deepseek", Model: "deepseek-flash"},
		},
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string // substring of the error; "" = valid
	}{
		{"valid", func(*Config) {}, ""},
		{"empty config", func(c *Config) { *c = Config{} }, ""},
		{"empty listen uses default", func(c *Config) { c.Listen = "" }, ""},
		{"ipv6 loopback listen", func(c *Config) { c.Listen = "[::1]:9000" }, ""},
		{"listen without port", func(c *Config) { c.Listen = "127.0.0.1" }, "listen"},
		{"non-loopback listen", func(c *Config) { c.Listen = "0.0.0.0:27486" }, "loopback"},
		{"hostname listen", func(c *Config) { c.Listen = "localhost:27486" }, "loopback"},
		{"loopback http upstream", func(c *Config) {
			c.Providers["deepseek"] = Provider{BaseURL: "http://127.0.0.1:9999/x"}
		}, ""},
		{"plain http upstream", func(c *Config) {
			c.Providers["deepseek"] = Provider{BaseURL: "http://api.deepseek.com"}
		}, "https"},
		{"upstream with query", func(c *Config) {
			c.Providers["deepseek"] = Provider{BaseURL: "https://api.deepseek.com/?a=b"}
		}, "query"},
		{"upstream with userinfo", func(c *Config) {
			c.Providers["deepseek"] = Provider{BaseURL: "https://u:p@api.deepseek.com"}
		}, "userinfo"},
		{"upstream without host", func(c *Config) {
			c.Providers["deepseek"] = Provider{BaseURL: ""}
		}, "base URL"},
		{"bad provider name", func(c *Config) {
			c.Providers["Deep_Seek"] = Provider{BaseURL: "https://x.example"}
		}, "provider name"},
		{"provider name leading dash", func(c *Config) {
			c.Providers["-x"] = Provider{BaseURL: "https://x.example"}
		}, "provider name"},
		{"bad route name", func(c *Config) {
			c.Routes["Sonnet"] = Route{Provider: "deepseek", Model: "m"}
		}, "route name"},
		{"route name with slash", func(c *Config) {
			c.Routes["a/b"] = Route{Provider: "deepseek", Model: "m"}
		}, "route name"},
		{"route unknown provider", func(c *Config) {
			c.Routes["sonnet"] = Route{Provider: "nope", Model: "m"}
		}, "unknown provider"},
		{"route empty model", func(c *Config) {
			c.Routes["sonnet"] = Route{Provider: "deepseek"}
		}, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.edit(&c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestListenAddr(t *testing.T) {
	if got := (Config{}).ListenAddr(); got != DefaultListen {
		t.Fatalf("empty: %q", got)
	}
	if got := (Config{Listen: "127.0.0.1:1"}).ListenAddr(); got != "127.0.0.1:1" {
		t.Fatalf("set: %q", got)
	}
}

func TestWithDefaults(t *testing.T) {
	p := Provider{BaseURL: "https://x.example"}.WithDefaults()
	if !reflect.DeepEqual(p.TopKeys, defaultTopKeys) || !reflect.DeepEqual(p.BlockTypes, defaultBlockTypes) {
		t.Fatalf("defaults not filled: %+v", p)
	}
	if p.BetaAllow != nil {
		t.Fatalf("beta_allow must stay empty: %v", p.BetaAllow)
	}
	p.TopKeys[0] = "mutated"
	if defaultTopKeys[0] == "mutated" {
		t.Fatal("WithDefaults shares the default slice")
	}
	own := Provider{TopKeys: []string{"model"}, BlockTypes: []string{"text"}}.WithDefaults()
	if !reflect.DeepEqual(own.TopKeys, []string{"model"}) || !reflect.DeepEqual(own.BlockTypes, []string{"text"}) {
		t.Fatalf("explicit values overridden: %+v", own)
	}
}

func TestMatch(t *testing.T) {
	c := validConfig()
	c.Routes["dangling"] = Route{Provider: "gone", Model: "m"}
	cases := []struct {
		model     string
		name      string
		owned, ok bool
	}{
		{"claude-opus-5-5", "", false, false},
		{"sonnet", "", false, false},
		{"shunt/sonnet", "sonnet", true, true},
		{"shunt/missing", "missing", true, false},
		{"shunt/dangling", "dangling", true, false},
		{"shunt/", "", true, false},
	}
	for _, tc := range cases {
		name, r, p, owned, ok := c.Match(tc.model)
		if name != tc.name || owned != tc.owned || ok != tc.ok {
			t.Errorf("%q: got (%q, owned=%v, ok=%v), want (%q, %v, %v)", tc.model, name, owned, ok, tc.name, tc.owned, tc.ok)
		}
		if tc.ok {
			if r.Model != "deepseek-flash" || p.BaseURL != "https://api.deepseek.com/anthropic" {
				t.Errorf("%q: wrong target %+v %+v", tc.model, r, p)
			}
			if len(p.TopKeys) == 0 || len(p.BlockTypes) == 0 {
				t.Errorf("%q: provider not WithDefaults: %+v", tc.model, p)
			}
		}
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("SHUNT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := DefaultDir(); got != filepath.Join("/home/u", ".config", "shunt") {
		t.Fatalf("home: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got := DefaultDir(); got != filepath.Join("/home/u", ".config", "shunt") {
		t.Fatalf("relative XDG must be ignored: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := DefaultDir(); got != "/xdg/shunt" {
		t.Fatalf("xdg: %q", got)
	}
	t.Setenv("SHUNT_CONFIG_DIR", "/explicit")
	if got := DefaultDir(); got != "/explicit" {
		t.Fatalf("explicit: %q", got)
	}
}
