package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type sourceArchiver struct {
	root string
	once sync.Once
	data []byte
	err  error
}

func (s *sourceArchiver) get() ([]byte, error) {
	s.once.Do(func() {
		s.data, s.err = buildSourceArchive(s.root)
	})
	return s.data, s.err
}

// buildSourceArchive runs git ls-files in root to enumerate every tracked
// file (automatically respecting .gitignore and excluding .git/), then
// packages them into an in-memory tar.gz.
func buildSourceArchive(root string) ([]byte, error) {
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rel == "" {
			continue
		}
		full := filepath.Join(root, rel)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode()),
			Size:    int64(len(data)),
			ModTime: info.ModTime(),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (h *handler) sourceArchive(w http.ResponseWriter, r *http.Request) {
	if h.source == nil {
		http.NotFound(w, r)
		return
	}
	data, err := h.source.get()
	if err != nil {
		http.Error(w, "source archive unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="madshare-source.tar.gz"`)
	w.Write(data)
}
