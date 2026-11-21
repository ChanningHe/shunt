package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"shunt/internal/admin"
	"shunt/internal/config"
)

const (
	envBaseURL     = "ANTHROPIC_BASE_URL"
	settingsIndent = "  "
)

// aliasVar is the Claude Code variable that resolves a model alias.
func aliasVar(alias string) string { return "ANTHROPIC_DEFAULT_" + strings.ToUpper(alias) + "_MODEL" }

func aliasVars() []string {
	out := make([]string, len(admin.Aliases))
	for i, a := range admin.Aliases {
		out[i] = aliasVar(a)
	}
	return out
}

// routingVars are every env key shunt manages.
func routingVars() []string { return append([]string{envBaseURL}, aliasVars()...) }

// wantAliases maps the alias variable of every alias-named route to its
// wire id. It is never nil.
func wantAliases(routes map[string]config.Route) map[string]string {
	out := map[string]string{}
	for _, a := range admin.Aliases {
		if _, ok := routes[a]; ok {
			out[aliasVar(a)] = config.WirePrefix + a
		}
	}
	return out
}

// member is one key of a JSON object; members keep the file's key order so
// editing settings.json (often tracked in a dotfiles repo) yields a minimal diff.
type member struct {
	Key   string
	Value json.RawMessage
}

type ordered []member

func decodeOrdered(data []byte) (ordered, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out ordered
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		// JS JSON.parse keeps the last duplicate; editing the first would
		// report a state Claude Code does not see.
		if seen[key] {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		out = append(out, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return out, nil
}

func (o ordered) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// set replaces key in place, or appends it.
func (o ordered) set(key string, val json.RawMessage) ordered {
	for i, m := range o {
		if m.Key == key {
			o[i].Value = val
			return o
		}
	}
	return append(o, member{key, val})
}

func (o ordered) del(key string) ordered {
	out := o[:0]
	for _, m := range o {
		if m.Key != key {
			out = append(out, m)
		}
	}
	return out
}

// encode writes the object with 2-space indentation (jq's layout), values
// kept verbatim apart from whitespace.
func (o ordered) encode() ([]byte, error) {
	if len(o) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, m := range o {
		key, err := marshalNoEscape(m.Key)
		if err != nil {
			return nil, err
		}
		buf.WriteString(settingsIndent)
		buf.Write(key)
		buf.WriteString(": ")
		if err := json.Indent(&buf, m.Value, settingsIndent, settingsIndent); err != nil {
			return nil, err
		}
		if i < len(o)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// settingsFile is settings.json resolved through symlinks.
type settingsFile struct {
	path string // the real file (symlinks resolved)
	mode fs.FileMode
	root ordered
	env  ordered
}

func loadSettings(path string) (*settingsFile, error) {
	sf := &settingsFile{path: path, mode: 0o644}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Replacing a dangling symlink with a file would silently fork the
		// dotfiles copy it points to.
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink to a missing file; create the target first", path)
		}
		return sf, nil // created on save
	}
	if err != nil {
		return nil, err
	}
	sf.path = real
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	sf.mode = info.Mode().Perm()
	data, err := os.ReadFile(real)
	if err != nil {
		return nil, err
	}
	if sf.root, err = decodeOrdered(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if raw, ok := sf.root.get("env"); ok && string(bytes.TrimSpace(raw)) != "null" {
		if sf.env, err = decodeOrdered(raw); err != nil {
			return nil, fmt.Errorf("%s: env: %w", path, err)
		}
	}
	return sf, nil
}

// envString returns env[key] if it is a JSON string.
func (sf *settingsFile) envString(key string) (string, bool) {
	raw, ok := sf.env.get(key)
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func (sf *settingsFile) setEnv(key, val string) error {
	raw, err := marshalNoEscape(val)
	if err != nil {
		return err
	}
	sf.env = sf.env.set(key, raw)
	return nil
}

// save writes atomically next to the real file (temp + rename), so a
// symlink pointing at it stays a symlink and the mode is kept.
func (sf *settingsFile) save() error {
	if len(sf.env) > 0 || hasKey(sf.root, "env") {
		env, err := sf.env.encode()
		if err != nil {
			return err
		}
		sf.root = sf.root.set("env", env)
	}
	data, err := sf.root.encode()
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(sf.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(sf.mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), sf.path)
}

func hasKey(o ordered, key string) bool {
	_, ok := o.get(key)
	return ok
}

// ours reports whether an env value was written by this tool: a loopback
// router URL, or an alias model in shunt's shunt/ namespace. Anything else
// (a company gateway, a pinned model id) belongs to the user.
func ours(key, val string) bool {
	switch {
	case key == envBaseURL:
		return admin.CheckURL(val) == nil
	case slices.Contains(aliasVars(), key):
		return strings.HasPrefix(val, config.WirePrefix)
	}
	return false
}

// foreignKeys lists those of keys whose current values are not ours.
func foreignKeys(sf *settingsFile, keys []string) []string {
	var out []string
	for _, key := range keys {
		if raw, ok := sf.env.get(key); ok {
			if val, isStr := sf.envString(key); !isStr || !ours(key, val) {
				out = append(out, fmt.Sprintf("%s=%s", key, raw))
			}
		}
	}
	return out
}

// enableRouting points the base URL at the router and, unless want is nil
// (routes unknown), syncs the alias variables to want. Values the user set
// themselves are only replaced with force. It returns what changed.
func enableRouting(sf *settingsFile, url string, want map[string]string, force bool) ([]string, error) {
	check := append([]string{envBaseURL}, slices.Sorted(maps.Keys(want))...)
	if f := foreignKeys(sf, check); len(f) > 0 && !force {
		return nil, fmt.Errorf("%s already sets %v; use --force to replace", sf.path, f)
	}
	var changes []string
	if cur, ok := sf.envString(envBaseURL); !ok || cur != url {
		if err := sf.setEnv(envBaseURL, url); err != nil {
			return nil, err
		}
		changes = append(changes, envBaseURL+"="+url)
	}
	if want == nil {
		return changes, nil
	}
	more, err := syncAliases(sf, want)
	return append(changes, more...), err
}

// syncAliases sets every alias variable in want and removes shunt's values
// of the others; the caller has checked for foreign values in want. Values
// the user set for aliases without a route are left alone.
func syncAliases(sf *settingsFile, want map[string]string) ([]string, error) {
	var changes []string
	for _, key := range aliasVars() {
		cur, isStr := sf.envString(key)
		target, wanted := want[key]
		switch {
		case wanted && (!isStr || cur != target):
			if err := sf.setEnv(key, target); err != nil {
				return nil, err
			}
			changes = append(changes, key+"="+target)
		case !wanted && isStr && ours(key, cur):
			sf.env = sf.env.del(key)
			changes = append(changes, "removed "+key)
		}
	}
	return changes, nil
}

// aliasDrift lists alias variables that do not match want.
func aliasDrift(sf *settingsFile, want map[string]string) []string {
	var out []string
	for _, key := range aliasVars() {
		cur, _ := sf.envString(key)
		target, wanted := want[key]
		if (wanted && cur != target) || (!wanted && ours(key, cur)) {
			out = append(out, fmt.Sprintf("%s is %q, routes want %q", key, cur, target))
		}
	}
	return out
}

// disableRouting removes shunt's routing values and reports whether
// anything changed. A base URL the user set is an error without force
// (routing may be on through it); force removes every routing key. Models
// the user pinned for an alias are not routing and stay.
func disableRouting(sf *settingsFile, force bool) (bool, error) {
	if f := foreignKeys(sf, []string{envBaseURL}); len(f) > 0 && !force {
		return false, fmt.Errorf("%s sets %v, which shunt did not write; use --force to remove", sf.path, f)
	}
	before := len(sf.env)
	for _, key := range routingVars() {
		if val, isStr := sf.envString(key); force || (isStr && ours(key, val)) {
			sf.env = sf.env.del(key)
		}
	}
	return len(sf.env) != before, nil
}
