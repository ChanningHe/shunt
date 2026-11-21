package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC 8a: a symlinked config file stays a symlink and its target is updated.
func TestUpdateThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real.toml")
	writeFile(t, target, "[providers.a]\nbase_url = 'https://a.example'\n", 0o640)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	if !s.Load().Files[0].Writable {
		t.Fatal("writable target reported read-only")
	}
	if err := s.Update(addProvider("b", "https://b.example")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	if !strings.Contains(readFile(t, target), "b.example") {
		t.Fatal("target not updated")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o640 {
		t.Fatalf("target mode %v", fi.Mode())
	}
}

// AC 8b: a symlink into a read-only directory gives a real write error.
func TestUpdateReadOnlySymlinkTarget(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real.toml")
	const orig = "[providers.a]\nbase_url = 'https://a.example'\n"
	writeFile(t, target, orig, 0o444)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(targetDir, 0o755) })

	s := Open(dir)
	before := s.Load()
	if before.Files[0].Writable {
		t.Fatal("read-only target reported writable")
	}
	if err := s.Update(addProvider("b", "https://b.example")); err == nil {
		t.Fatal("Update into read-only dir succeeded")
	}
	if s.Load() != before {
		t.Fatal("snapshot changed")
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if readFile(t, target) != orig {
		t.Fatal("target changed")
	}
}

func TestWritableReflectsDirectory(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if f := Open(dir).Load().Files; f[0].Writable || f[1].Writable {
		t.Fatalf("read-only dir reported writable: %+v", f)
	}
	if f := Open(filepath.Join(dir, "sub")).Load().Files; f[0].Writable {
		t.Fatalf("uncreatable dir reported writable: %+v", f)
	}
}

func TestUpdateRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere.toml"), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	s := Open(dir)
	if s.Load().LoadErr == nil {
		t.Fatal("dangling symlink accepted")
	}
	if err := s.Update(addProvider("a", "https://a.example")); err == nil {
		t.Fatal("Update replaced a dangling symlink")
	}
}

// When a change touches both files, keys are written first: if the config
// write then fails, a key is never left paired with a new base_url.
func TestUpdateWritesKeysBeforeConfig(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "config.toml")
	writeFile(t, target, "[providers.a]\nbase_url = 'https://a.example'\n", 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	keysPath := filepath.Join(dir, "keys.toml")
	writeFile(t, keysPath, "a = 'sk-1'\n", 0o600)
	if err := os.Chmod(targetDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(targetDir, 0o755) })

	s := Open(dir)
	err := s.Update(func(c *Config, k Keys) error {
		c.Providers["a"] = Provider{BaseURL: "https://evil.example"}
		delete(k, "a")
		return nil
	})
	if err == nil {
		t.Fatal("config write into read-only dir succeeded")
	}
	if got := readFile(t, keysPath); strings.Contains(got, "sk-1") {
		t.Fatalf("key still on disk after a failed config write: %q", got)
	}
}

func TestUpdateValidationErrorIsErrInvalid(t *testing.T) {
	s := Open(t.TempDir())
	err := s.Update(addProvider("b", "http://not-loopback.example"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
	boom := errors.New("boom")
	if err := s.Update(func(*Config, Keys) error { return boom }); errors.Is(err, ErrInvalid) {
		t.Fatal("fn error reported as ErrInvalid")
	}
}
