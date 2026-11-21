package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrConflict is returned by Update when the files on disk cannot serve as
// the base of a change (invalid content, several formats side by side). The
// files are left untouched; the user fixes them and reloads.
var ErrConflict = errors.New("config on disk is invalid; fix the files and reload")

// ErrInvalid wraps validation failures of the configuration an Update
// produced, as opposed to I/O failures.
var ErrInvalid = errors.New("invalid config")

// FileInfo describes one of the two config files.
type FileInfo struct {
	Path     string // config.<ext> / keys.<ext>; the default .toml name if absent
	Format   string // toml | yaml | json
	Exists   bool
	Writable bool // an Update could replace it (see writable)
}

// Snapshot is an immutable view of the configuration; callers must not
// modify it.
type Snapshot struct {
	Config Config
	Keys   Keys // raw values (literal or file://), not resolved keys
	// LoadErr is the last load error. After Open it means the config is
	// empty (pure passthrough); after a failed Reload, Config and Keys are
	// the last good ones.
	LoadErr error
	Files   [2]FileInfo // [0]=config, [1]=keys
}

// Store is the single writer of the config directory. Reads are lock-free.
type Store struct {
	dir  string
	mu   sync.Mutex // serializes Update and Reload
	snap atomic.Pointer[Snapshot]
	keys keyCache
}

var (
	fileBases = [2]string{"config", "keys"}
	// fileModes: a new config file is 0644 and an existing one keeps its
	// mode; the keys file is always (re)written 0600.
	fileModes = [2]struct {
		mode fs.FileMode
		keep bool
	}{{0o644, true}, {0o600, false}}
)

// disk is one consistent read of both files.
type disk struct {
	files [2]FileInfo
	cfg   Config
	keys  Keys
}

// Open loads dir. It never fails because of file content: problems end up in
// Snapshot.LoadErr with an empty Config.
func Open(dir string) *Store {
	s := &Store{dir: dir}
	d, err := s.read()
	snap := &Snapshot{Keys: Keys{}, LoadErr: err, Files: d.files}
	if err == nil {
		snap.Config, snap.Keys = d.cfg, d.keys
	}
	s.snap.Store(snap)
	return s
}

// Load returns the current snapshot.
func (s *Store) Load() *Snapshot { return s.snap.Load() }

// Reload re-reads the files. On error the running Config and Keys are kept
// and the error becomes the snapshot's LoadErr.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.read()
	if err != nil {
		old := s.snap.Load()
		s.snap.Store(&Snapshot{Config: old.Config, Keys: old.Keys, LoadErr: err, Files: d.files})
		return err
	}
	s.snap.Store(&Snapshot{Config: d.cfg, Keys: d.keys, Files: d.files})
	return nil
}

// Update applies fn to the configuration as it is on disk now (not to the
// snapshot, so hand edits made without a reload are kept), validates the
// result and rewrites only the files whose content changed. If fn, the
// validation or a write fails, the snapshot is unchanged.
func (s *Store) Update(fn func(*Config, Keys) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.read()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	before, err := d.encode()
	if err != nil {
		return err
	}
	if err := fn(&d.cfg, d.keys); err != nil {
		return err
	}
	if err := d.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	after, err := d.encode()
	if err != nil {
		return err
	}
	// Keys first: if the config write then fails, a removed key is already
	// gone instead of staying paired with a new base_url, and a new key for a
	// provider that did not make it to disk is an ignored orphan.
	for _, i := range []int{1, 0} {
		f := d.files[i]
		if bytes.Equal(before[i], after[i]) {
			continue // keeps hand formatting and comments of untouched files
		}
		if err := writeFileAtomic(f.Path, after[i], fileModes[i].mode, fileModes[i].keep); err != nil {
			return err
		}
		d.files[i].Exists, d.files[i].Writable = true, true
	}
	s.snap.Store(&Snapshot{Config: d.cfg, Keys: d.keys, Files: d.files})
	return nil
}

// read discovers, decodes and validates both files.
func (s *Store) read() (disk, error) {
	var d disk
	var errs []error
	for i, base := range fileBases {
		f, err := discover(s.dir, base)
		d.files[i] = f
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return d, err
	}
	cfgFile, keysFile := d.files[0], d.files[1]
	raw, err := readIfExists(cfgFile)
	if err != nil {
		return d, err
	}
	if err := decode(cfgFile.Format, raw, &d.cfg); err != nil {
		return d, fmt.Errorf("%s: %w", cfgFile.Path, err)
	}
	if raw, err = readIfExists(keysFile); err != nil {
		return d, err
	}
	// Parser errors can quote file content, which here is secret.
	if err := decode(keysFile.Format, raw, &d.keys); err != nil {
		return d, fmt.Errorf("%s: not a valid %s map of provider name to string (details withheld)", keysFile.Path, keysFile.Format)
	}
	if d.keys == nil {
		d.keys = Keys{}
	}
	return d, d.validate()
}

// validate checks the config only. Keys of unknown providers are ignored (see
// Store.Key), so a change that rewrote config but failed to rewrite keys
// still loads.
func (d *disk) validate() error {
	if err := d.cfg.Validate(); err != nil {
		return fmt.Errorf("%s: %w", d.files[0].Path, err)
	}
	return nil
}

func (d *disk) encode() ([2][]byte, error) {
	var out [2][]byte
	for i, v := range [2]any{d.cfg, d.keys} {
		b, err := encode(d.files[i].Format, v)
		if err != nil {
			return out, fmt.Errorf("%s: %w", d.files[i].Path, err)
		}
		out[i] = b
	}
	return out, nil
}

// discover finds <base>.<ext> in dir. More than one match is an error;
// none means the default <base>.toml, created on the first write.
func discover(dir, base string) (FileInfo, error) {
	var found []FileInfo
	for _, ext := range extensions {
		path := filepath.Join(dir, base+"."+ext)
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return FileInfo{}, err
		}
		found = append(found, FileInfo{Path: path, Format: formatOf(ext), Exists: true})
	}
	if len(found) == 0 {
		found = append(found, FileInfo{Path: filepath.Join(dir, base+"."+formatTOML), Format: formatTOML})
	}
	f := found[0]
	f.Writable = writable(f.Path)
	if len(found) > 1 {
		names := make([]string, len(found))
		for i, g := range found {
			names[i] = filepath.Base(g.Path)
		}
		return f, fmt.Errorf("%s: found %s; keep exactly one", dir, strings.Join(names, ", "))
	}
	return f, nil
}

func readIfExists(f FileInfo) ([]byte, error) {
	if !f.Exists {
		return nil, nil
	}
	return os.ReadFile(f.Path)
}
