package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testConfigTOML = `
[providers.a]
base_url = "https://a.example"
[providers.b]
base_url = "https://b.example"
[providers.c]
base_url = "https://c.example"
`

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// keyStore opens a store whose providers a, b, c map to the given key values.
func keyStore(t *testing.T, keys map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), testConfigTOML, 0o644)
	var b strings.Builder
	for k, v := range keys {
		b.WriteString(k + " = '" + v + "'\n")
	}
	writeFile(t, filepath.Join(dir, "keys.toml"), b.String(), 0o600)
	s := Open(dir)
	if err := s.Load().LoadErr; err != nil {
		t.Fatal(err)
	}
	return s
}

func assertKey(t *testing.T, s *Store, provider, wantKey, wantState string) {
	t.Helper()
	key, state := s.Key(s.Load(), provider)
	if key != wantKey || state != wantState {
		t.Fatalf("Key(%s) = (%q, %q), want (%q, %q)", provider, key, state, wantKey, wantState)
	}
}

func TestKeyLiteral(t *testing.T) {
	s := keyStore(t, map[string]string{"a": "sk-live", "b": "xx-SK-ANT-yy"})
	assertKey(t, s, "a", "sk-live", "literal")
	assertKey(t, s, "b", "", "invalid")
	assertKey(t, s, "c", "", "missing")
	assertKey(t, s, "nope", "", "missing")
}

func TestKeyFileRules(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writeFile(t, good, "sk-file\n", 0o600)
	crlf := filepath.Join(dir, "crlf")
	writeFile(t, crlf, "sk-crlf\r\n", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	multi := filepath.Join(dir, "multi")
	writeFile(t, multi, "sk-1\nsk-2\n", 0o600)
	empty := filepath.Join(dir, "empty")
	writeFile(t, empty, "\n", 0o600)
	big := filepath.Join(dir, "big")
	writeFile(t, big, strings.Repeat("k", maxKeyFileSize+1), 0o600)
	ant := filepath.Join(dir, "ant")
	writeFile(t, ant, "Sk-Ant-oat01-x\n", 0o600)

	cases := []struct {
		ref, key, state string
	}{
		{"file://" + good, "sk-file", "file"},
		{"file://" + crlf, "sk-crlf", "file"},
		{"file://" + link, "sk-file", "file"},
		{"file://" + multi, "", "invalid"},
		{"file://" + empty, "", "invalid"},
		{"file://" + big, "", "invalid"},
		{"file://" + dir, "", "invalid"},
		{"file://" + ant, "", "invalid"},
		{"file://relative/path", "", "invalid"},
		{"file://" + filepath.Join(dir, "absent"), "", "missing"},
	}
	for _, tc := range cases {
		s := keyStore(t, map[string]string{"a": tc.ref})
		assertKey(t, s, "a", tc.key, tc.state)
	}
}

func TestKeyFileAppearsAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	s := keyStore(t, map[string]string{"a": "file://" + path})
	assertKey(t, s, "a", "", "missing")

	writeFile(t, path, "sk-one\n", 0o600)
	assertKey(t, s, "a", "sk-one", "file")
	assertKey(t, s, "a", "sk-one", "file") // cached path

	writeFile(t, path, "sk-rotated\n", 0o600)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	assertKey(t, s, "a", "sk-rotated", "file")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertKey(t, s, "a", "", "missing")
}

func TestLoadErrorsDoNotLeakKeys(t *testing.T) {
	const secret = "sk-very-secret-value"
	bodies := map[string]string{
		"keys.toml": "a = '" + secret + "\n",            // unterminated string
		"keys.yaml": "a: [" + secret + "\n",             // unterminated flow
		"keys.json": "{\"a\": \"" + secret + "\" \"b\"", // missing comma
	}
	for name, body := range bodies {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.toml"), testConfigTOML, 0o644)
		writeFile(t, filepath.Join(dir, name), body, 0o600)
		err := Open(dir).Load().LoadErr
		if err == nil {
			t.Fatalf("%s: broken keys file accepted", name)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "very-secret") {
			t.Fatalf("%s: error leaks key: %v", name, err)
		}
	}
}
