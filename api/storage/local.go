package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// validHash matches a lowercase SHA-256 hex digest (64 chars).
var validHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Local stores files at baseDir/<sha256>/<filename>.
type Local struct {
	baseDir string
}

// NewLocal creates a local filesystem storage backend.
func NewLocal(baseDir string) *Local {
	return &Local{baseDir: baseDir}
}

// Exists reports whether the blob at <hash>/<filename> already exists on disk.
func (s *Local) Exists(hash, filename string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	path := filepath.Join(s.baseDir, hash, filepath.Base(filename))
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DeleteAll removes the entire <baseDir>/<hash> directory and everything under
// it. It is idempotent: a missing directory yields (false, nil).
func (s *Local) DeleteAll(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: stat %s: %w", dir, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("storage: remove %s: %w", dir, err)
	}
	return true, nil
}

// HashDirExists reports whether the <baseDir>/<hash> directory exists.
func (s *Local) HashDirExists(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	info, err := os.Stat(filepath.Join(s.baseDir, hash))
	if err == nil {
		return info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("storage: stat hash dir: %w", err)
}

// Put writes r to <baseDir>/<hash>/<filename>, creating the hash directory if
// needed. Path traversal in filename is neutralised by filepath.Base.
func (s *Local) Put(hash, filename string, r io.Reader) error {
	if !validHash.MatchString(hash) {
		return fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.Base(filename))
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}
