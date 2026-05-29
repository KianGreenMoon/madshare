package storage

import (
	"crypto/sha256"
	"encoding/hex"
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

// BlobPresent reports whether <baseDir>/<hash> exists and contains at least one
// regular file. An emptied hash directory (the blob deleted but the directory
// left behind) reads as not present.
func (s *Local) BlobPresent(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	entries, err := os.ReadDir(filepath.Join(s.baseDir, hash))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: read hash dir: %w", err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return true, nil
		}
	}
	return false, nil
}

// VerifyBlob re-reads the blob(s) under <baseDir>/<hash> and reports whether one
// hashes to hash. Returns false (no error) when the directory is missing/empty
// or the content has been corrupted (no file matches the expected digest).
func (s *Local) VerifyBlob(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: read hash dir: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		sum, err := sha256File(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, err
		}
		if sum == hash {
			return true, nil
		}
	}
	return false, nil
}

// sha256File streams a file through SHA-256 and returns the hex digest.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("storage: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("storage: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
