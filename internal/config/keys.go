package config

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Key states reported by Store.Key.
const (
	KeyLiteral = "literal"
	KeyFile    = "file"
	KeyMissing = "missing"
	KeyInvalid = "invalid"
)

// KeyFilePrefix marks a keys-file value naming a file that holds the key.
const KeyFilePrefix = "file://"

const (
	maxKeyFileSize = 4096
	anthropicKey   = "sk-ant-" // never forwarded to a third party
)

// keyCache remembers file-backed keys by (path, mtime, size), so a routed
// request costs one stat while the file is unchanged. Only valid keys are
// cached; missing or invalid files are looked at again on every call.
type keyCache struct {
	mu sync.Mutex
	m  map[string]cachedKey
}

type cachedKey struct {
	mtime time.Time
	size  int64
	key   string
}

// Key resolves the key of a provider in snap, the snapshot the caller took
// its routing decision from (so a key never pairs with another snapshot's
// base_url). key is non-empty only when state is KeyLiteral or KeyFile; a
// provider that is not configured is KeyMissing even if the keys file still
// names it. Nothing here returns or logs key material on failure.
func (s *Store) Key(snap *Snapshot, provider string) (key, state string) {
	if _, ok := snap.Config.Providers[provider]; !ok {
		return "", KeyMissing
	}
	ref, ok := snap.Keys[provider]
	if !ok {
		return "", KeyMissing
	}
	path, isFile := strings.CutPrefix(ref, KeyFilePrefix)
	if !isFile {
		return checkKey(ref, KeyLiteral)
	}
	if !filepath.IsAbs(path) {
		return "", KeyInvalid
	}
	return s.keys.resolve(path)
}

// checkKey rejects empty keys and Anthropic keys, in any letter case.
func checkKey(key, state string) (string, string) {
	if key == "" || strings.Contains(strings.ToLower(key), anthropicKey) {
		return "", KeyInvalid
	}
	return key, state
}

func (c *keyCache) resolve(path string) (string, string) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.store(path, nil)
		return "", KeyMissing
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxKeyFileSize {
		return "", KeyInvalid
	}
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.key, KeyFile
	}
	e, ok = readKeyFile(path)
	if !ok {
		c.store(path, nil)
		return "", KeyInvalid
	}
	if key, state := checkKey(e.key, KeyFile); state == KeyFile {
		c.store(path, &e)
		return key, state
	}
	c.store(path, nil)
	return "", KeyInvalid
}

func (c *keyCache) store(path string, e *cachedKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e == nil {
		delete(c.m, path)
		return
	}
	if c.m == nil {
		c.m = map[string]cachedKey{}
	}
	c.m[path] = *e
}

// readKeyFile reads a single-line key from a regular file of at most
// maxKeyFileSize bytes. Trailing line breaks are dropped.
func readKeyFile(path string) (cachedKey, bool) {
	// O_NONBLOCK: the path may have been swapped for a FIFO since the stat.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return cachedKey{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxKeyFileSize {
		return cachedKey{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil || len(data) > maxKeyFileSize {
		return cachedKey{}, false
	}
	key := strings.TrimRight(string(data), "\r\n")
	if strings.ContainsAny(key, "\r\n") {
		return cachedKey{}, false
	}
	return cachedKey{mtime: fi.ModTime(), size: fi.Size(), key: key}, true
}
