package storage

import (
	"io"
	"os"
	"path/filepath"
)

// Local stores files at baseDir/<sha256>/<filename>.
type Local struct {
	baseDir  string
	cacheDir string
}

// NewLocal creates a local filesystem storage backend.
func NewLocal(baseDir, cacheDir string) *Local {
	return &Local{baseDir: baseDir, cacheDir: cacheDir}
}

func (s *Local) Stat(hash string) (int64, error) {
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

func (s *Local) CacheDir() string { return s.cacheDir }
