package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// s7-3: control characters in a route's model would reach terminals
// verbatim (route list, status, TUI); every format rejects them.
func TestRouteModelControlCharsRejected(t *testing.T) {
	for _, bad := range []string{"m\x1b]0;title\x07", "m\n", "m\x00", "m\x7f", "\t"} {
		c := validConfig()
		c.Routes["sonnet"] = Route{Provider: "deepseek", Model: bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "control") {
			t.Errorf("model %q: err = %v", bad, err)
		}
	}
	for _, ext := range allExts {
		dir := t.TempDir()
		content := render(t, ext, tree{
			"providers": tree{"deepseek": tree{"base_url": "https://api.deepseek.com"}},
			"routes":    tree{"sonnet": tree{"provider": "deepseek", "model": "m\x1b]0;x\x07"}},
		})
		writeFile(t, filepath.Join(dir, "config."+ext), string(content), 0o644)
		if err := Open(dir).Load().LoadErr; err == nil {
			t.Errorf("%s: control characters in model accepted", ext)
		}
	}
}

// s7-5: a key is resolved from the snapshot the caller routed with, not
// from whatever snapshot is current by then.
func TestKeyUsesGivenSnapshot(t *testing.T) {
	s := keyStore(t, map[string]string{"a": "sk-old"})
	snap := s.Load()
	if err := s.Update(func(_ *Config, k Keys) error { k["a"] = "sk-new"; return nil }); err != nil {
		t.Fatal(err)
	}
	if key, state := s.Key(snap, "a"); key != "sk-old" || state != KeyLiteral {
		t.Errorf("Key(old snapshot) = %q, %q", key, state)
	}
	if key, _ := s.Key(s.Load(), "a"); key != "sk-new" {
		t.Errorf("Key(new snapshot) = %q", key)
	}
}

// s7-6: the daemon always leaves the keys file 0600, whatever it was; the
// config file keeps its mode.
func TestUpdateTightensKeysFileMode(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "providers:\n  a:\n    base_url: https://a.example\n", 0o664)
	keysPath := filepath.Join(dir, "keys.yaml")
	writeFile(t, keysPath, "a: sk-1\n", 0o644)
	for path, mode := range map[string]os.FileMode{cfgPath: 0o664, keysPath: 0o644} {
		if err := os.Chmod(path, mode); err != nil { // not subject to umask
			t.Fatal(err)
		}
	}
	s := Open(dir)
	err := s.Update(func(c *Config, k Keys) error {
		k["a"] = "sk-2"
		c.Providers["b"] = Provider{BaseURL: "https://b.example"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{keysPath: 0o600, cfgPath: 0o664} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v (%v)", filepath.Base(path), fi.Mode().Perm(), want, err)
		}
	}
}
