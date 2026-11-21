package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func addProvider(name, url string) func(*Config, Keys) error {
	return func(c *Config, _ Keys) error {
		if c.Providers == nil {
			c.Providers = map[string]Provider{}
		}
		c.Providers[name] = Provider{BaseURL: url}
		return nil
	}
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

func TestOpenEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	snap := Open(dir).Load()
	if snap.LoadErr != nil || !reflect.DeepEqual(snap.Config, Config{}) || len(snap.Keys) != 0 {
		t.Fatalf("empty dir: %+v", snap)
	}
	want := [2]FileInfo{
		{Path: filepath.Join(dir, "config.toml"), Format: "toml", Writable: true},
		{Path: filepath.Join(dir, "keys.toml"), Format: "toml", Writable: true},
	}
	if snap.Files != want {
		t.Fatalf("files: %+v", snap.Files)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open must not create the directory")
	}
}

func TestOpenMultipleFormatsDegrades(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), "", 0o644)
	writeFile(t, filepath.Join(dir, "config.yml"), "", 0o644)
	s := Open(dir)
	snap := s.Load()
	if snap.LoadErr == nil || !reflect.DeepEqual(snap.Config, Config{}) {
		t.Fatalf("expected degraded snapshot: %+v", snap)
	}
	err := s.Update(addProvider("a", "https://a.example"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update on ambiguous dir: %v", err)
	}
}

func TestOpenInvalidContentDegrades(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), "listen = '0.0.0.0:1'\n", 0o644)
	snap := Open(dir).Load()
	if snap.LoadErr == nil || !reflect.DeepEqual(snap.Config, Config{}) {
		t.Fatalf("expected degraded snapshot: %+v", snap)
	}
}

func TestUpdateCreatesDefaultFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shunt")
	s := Open(dir)
	err := s.Update(func(c *Config, k Keys) error {
		c.Providers = map[string]Provider{"a": {BaseURL: "https://a.example"}}
		k["a"] = "sk-1"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		dir:                               0o700 | os.ModeDir,
		filepath.Join(dir, "config.toml"): 0o644,
		filepath.Join(dir, "keys.toml"):   0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != mode {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode(), mode)
		}
	}
	snap := s.Load()
	if !snap.Files[0].Exists || !snap.Files[1].Exists || snap.Keys["a"] != "sk-1" {
		t.Fatalf("snapshot after create: %+v", snap)
	}
	if got := Open(dir).Load(); !reflect.DeepEqual(got.Config, snap.Config) || !reflect.DeepEqual(got.Keys, snap.Keys) {
		t.Fatalf("reopen differs: %+v vs %+v", got, snap)
	}
}

func TestUpdateKeepsFormatAndMode(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	keysPath := filepath.Join(dir, "keys.json")
	writeFile(t, cfgPath, "providers:\n  a:\n    base_url: https://a.example\n", 0o640)
	writeFile(t, keysPath, `{"a": "sk-1"}`, 0o400)
	s := Open(dir)
	err := s.Update(func(c *Config, k Keys) error {
		c.Routes = map[string]Route{"r": {Provider: "a", Model: "m"}}
		k["a"] = "sk-2"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a toml file was created next to the yml one")
	}
	// The config file keeps its mode; the keys file is always 0600 (s8).
	for path, mode := range map[string]os.FileMode{cfgPath: 0o640, keysPath: 0o600} {
		if fi, _ := os.Stat(path); fi.Mode() != mode {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode(), mode)
		}
	}
	if !strings.Contains(readFile(t, cfgPath), "routes:") || !strings.Contains(readFile(t, keysPath), `"sk-2"`) {
		t.Fatalf("not rewritten in place:\n%s\n%s", readFile(t, cfgPath), readFile(t, keysPath))
	}
	if f := s.Load().Files; f[0].Format != "yaml" || f[1].Format != "json" {
		t.Fatalf("formats: %+v", f)
	}
}

// AC 7a: hand edits made without Reload survive an Update.
func TestUpdateRebasesOnDisk(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	s := Open(dir)
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n[providers.hand]\nbase_url = 'https://hand.example'\n", 0o644)
	if err := s.Update(addProvider("b", "https://b.example")); err != nil {
		t.Fatal(err)
	}
	got := Open(dir).Load().Config.Providers
	for _, name := range []string{"a", "hand", "b"} {
		if _, ok := got[name]; !ok {
			t.Errorf("provider %q lost: %v", name, got)
		}
	}
	if _, ok := s.Load().Config.Providers["hand"]; !ok {
		t.Error("snapshot misses the hand edit")
	}
}

func TestUpdateConflictLeavesDiskAlone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	s := Open(dir)
	before := s.Load()
	const broken = "[providers.a\n"
	writeFile(t, cfgPath, broken, 0o644)
	err := s.Update(addProvider("b", "https://b.example"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if readFile(t, cfgPath) != broken {
		t.Fatal("broken file was rewritten")
	}
	if s.Load() != before {
		t.Fatal("snapshot changed")
	}
}

// AC 7b: an Update that only touches routes does not rewrite the keys file.
func TestUpdateWritesOnlyChangedFiles(t *testing.T) {
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.toml")
	writeFile(t, filepath.Join(dir, "config.toml"), "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	writeFile(t, keysPath, "# hand comment\na = 'sk-1'\n", 0o600)
	before, err := os.Stat(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	err = s.Update(func(c *Config, _ Keys) error {
		c.Routes = map[string]Route{"r": {Provider: "a", Model: "m"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("keys file was rewritten")
	}
	if readFile(t, keysPath) != "# hand comment\na = 'sk-1'\n" {
		t.Fatal("keys file content changed")
	}
}

// AC 7c: a failing fn or Validate changes neither disk nor snapshot.
func TestUpdateFailureIsAtomic(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	const orig = "[providers.a]\nbase_url = 'https://a.example'\n"
	writeFile(t, cfgPath, orig, 0o644)
	s := Open(dir)
	before := s.Load()
	boom := errors.New("boom")
	fails := map[string]func(*Config, Keys) error{
		"fn error": func(c *Config, _ Keys) error {
			c.Listen = "127.0.0.1:1"
			return boom
		},
		"invalid config": addProvider("b", "http://not-loopback.example"),
	}
	for name, fn := range fails {
		if err := s.Update(fn); err == nil {
			t.Fatalf("%s: Update succeeded", name)
		}
		if readFile(t, cfgPath) != orig {
			t.Fatalf("%s: disk changed", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "keys.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: keys file created", name)
		}
		if s.Load() != before {
			t.Fatalf("%s: snapshot changed", name)
		}
	}
	if err := s.Update(func(*Config, Keys) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn error not returned: %v", err)
	}
}

// AC 7d: concurrent Updates never lose a write.
func TestUpdateConcurrent(t *testing.T) {
	const n = 100
	s := Open(t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			errs <- s.Update(addProvider(fmt.Sprintf("p%d", i), "https://p.example"))
		})
		wg.Go(func() { _ = s.Load().Config.Providers }) // concurrent readers
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Load().Config.Providers); got != n {
		t.Fatalf("snapshot has %d providers, want %d", got, n)
	}
	if got := len(Open(s.dir).Load().Config.Providers); got != n {
		t.Fatalf("disk has %d providers, want %d", got, n)
	}
}

// AC 9: defaults are never written back.
func TestUpdateDoesNotMaterializeDefaults(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.Update(addProvider("a", "https://a.example")); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "config.toml"))
	for _, field := range []string{"listen", "top_keys", "block_types", "beta_allow"} {
		if strings.Contains(got, field) {
			t.Errorf("%q materialized:\n%s", field, got)
		}
	}
}

// AC 11: a broken file on Reload keeps the running config.
func TestReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	s := Open(dir)

	writeFile(t, cfgPath, "[providers.b]\nbase_url = 'https://b.example'\n", 0o644)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load().Config.Providers["b"]; !ok {
		t.Fatal("reload did not pick up change")
	}

	writeFile(t, cfgPath, "[providers.b\n", 0o644)
	err := s.Reload()
	snap := s.Load()
	if err == nil || snap.LoadErr == nil || err.Error() != snap.LoadErr.Error() {
		t.Fatalf("reload error %v, LoadErr %v", err, snap.LoadErr)
	}
	if _, ok := snap.Config.Providers["b"]; !ok {
		t.Fatal("broken reload dropped the running config")
	}

	writeFile(t, cfgPath, "[providers.c]\nbase_url = 'https://c.example'\n", 0o644)
	if err := s.Reload(); err != nil || s.Load().LoadErr != nil {
		t.Fatalf("recovery: %v", err)
	}
}

// A key whose provider is gone (e.g. config written, keys write failed) is
// ignored: the directory still loads, Key reports missing, Update proceeds.
func TestOrphanKeyIsIgnored(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n[providers.ghost]\nbase_url = 'https://g.example'\n", 0o644)
	writeFile(t, filepath.Join(dir, "keys.toml"), "a = 'sk-a'\nghost = 'sk-ghost'\n", 0o600)
	s := Open(dir)
	assertKey(t, s, "ghost", "sk-ghost", "literal")

	// Half-applied "remove provider ghost": config rewritten, keys not.
	writeFile(t, cfgPath, "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	if err := s.Reload(); err != nil {
		t.Fatalf("orphan key blocks reload: %v", err)
	}
	if err := Open(dir).Load().LoadErr; err != nil {
		t.Fatalf("orphan key blocks open: %v", err)
	}
	assertKey(t, s, "ghost", "", "missing")
	assertKey(t, s, "a", "sk-a", "literal")

	if err := s.Update(addProvider("b", "https://b.example")); err != nil {
		t.Fatalf("orphan key blocks update: %v", err)
	}
	if _, ok := s.Load().Config.Providers["b"]; !ok {
		t.Fatal("update not applied")
	}
	if err := s.Update(func(_ *Config, k Keys) error { delete(k, "ghost"); return nil }); err != nil {
		t.Fatalf("cleaning up the orphan: %v", err)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "keys.toml")), "ghost") {
		t.Fatal("orphan not removed")
	}
}
