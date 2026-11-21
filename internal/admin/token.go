package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const tokenBytes = 32

// TokenPath is where the daemon listening on port keeps its admin token:
// $XDG_RUNTIME_DIR/shunt/token-<port>, or <configDir>/.admin-token-<port>
// without an (absolute) XDG_RUNTIME_DIR. The token is a runtime secret, so
// it stays out of the config directory whenever possible.
func TokenPath(configDir, port string) string {
	dir, name, _ := tokenLocation(configDir, port)
	return filepath.Join(dir, name)
}

func tokenLocation(configDir, port string) (dir, name string, runtime bool) {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(rt) {
		return filepath.Join(rt, "shunt"), "token-" + port, true
	}
	return configDir, ".admin-token-" + port, false
}

// NewToken returns 32 random bytes, hex encoded.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteToken stores token at TokenPath(configDir, port), mode 0600, via a
// temp file and rename so a planted symlink is replaced, not followed. The
// runtime directory is forced to 0700; a config directory is only created
// (0700), never re-moded. It returns the path written.
func WriteToken(configDir, port, token string) (string, error) {
	dir, name, runtime := tokenLocation(configDir, port)
	if dir == "" {
		return "", errors.New("no directory for the admin token")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if runtime {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".token-*") // created 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// readToken reads a token file. A missing file is fs.ErrNotExist.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty: %w", path, fs.ErrNotExist)
	}
	return token, nil
}
