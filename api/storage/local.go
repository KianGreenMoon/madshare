package storage

import (
	"fmt"
	"io"
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

func (s *Local) Stat(hash string) (int64, error) {
	if !validHash.MatchString(hash) {
		return -1, fmt.Errorf("storage: invalid hash %q", hash)
	}
	entries, err := os.ReadDir(filepath.Join(s.baseDir, hash))
	if os.IsNotExist(err) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return -1, err
			}
			return info.Size(), nil
		}
	}
	return -1, nil
}

func (s *Local) Put(hash, filename string, r io.Reader, size int64) error {
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
