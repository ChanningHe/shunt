package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const accessWrite = 0x2 // W_OK for access(2)

// realPath resolves symlinks. ok is false when nothing exists at path yet;
// a dangling symlink is an error, because replacing it with a regular file
// would silently fork whatever copy it was meant to point at.
func realPath(path string) (real string, ok bool, err error) {
	real, err = filepath.EvalSymlinks(path)
	if err == nil {
		return real, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", false, fmt.Errorf("%s is a symlink to a missing file; create the target first", path)
	}
	return path, false, nil
}

// writeFileAtomic replaces the real file behind path (symlinks resolved) via
// a temp file in the same directory and rename. The file gets mode, except
// that with keepMode an existing file keeps its own; missing directories are
// created 0700.
func writeFileAtomic(path string, data []byte, mode fs.FileMode, keepMode bool) error {
	real, exists, err := realPath(path)
	if err != nil {
		return err
	}
	if exists && keepMode {
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(real)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(real)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
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
	return os.Rename(tmp.Name(), real)
}

// writable reports whether writeFileAtomic(path) can succeed as far as
// permissions go: the directory holding the real file (or, if that does not
// exist yet, its nearest existing ancestor) must be writable. The file's own
// mode bits do not matter for temp+rename.
func writable(path string) bool {
	real, _, err := realPath(path)
	if err != nil {
		return false
	}
	for dir := filepath.Dir(real); ; dir = filepath.Dir(dir) {
		_, err := os.Stat(dir)
		if err == nil {
			return syscall.Access(dir, accessWrite) == nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}
