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
	// prebuilt, when non-nil, is the source tar.gz embedded into the binary at
	// build time (make build -> git archive HEAD -> //go:embed). It is served
	// verbatim and makes /source work with no working tree, the AGPL-relevant
	// case. When nil, the archive is built on demand from git ls-files in root.
	prebuilt []byte
	root     string
	once     sync.Once
	data     []byte
	err      error

	// licensePrebuilt is the LICENSE.md embedded at build time (always present
	// in the real server). licenseOnce/licenseData cache the on-disk fallback
	// read from root when no embedded copy is supplied (tests via NewRouter).
	licensePrebuilt []byte
	licenseOnce     sync.Once
	licenseData     []byte
	licenseErr      error
}

// license returns the AGPL LICENSE text — the build-time-embedded bytes when
// present, otherwise read once from <root>/LICENSE.md and cached in memory.
func (s *sourceArchiver) license() ([]byte, error) {
	if s.licensePrebuilt != nil {
		return s.licensePrebuilt, nil
	}
	s.licenseOnce.Do(func() {
		if s.root == "" {
			s.licenseErr = fmt.Errorf("no embedded license and no source root")
			return
		}
		s.licenseData, s.licenseErr = os.ReadFile(filepath.Join(s.root, "LICENSE.md"))
	})
	return s.licenseData, s.licenseErr
}

func (s *sourceArchiver) get() ([]byte, error) {
	if s.prebuilt != nil {
		return s.prebuilt, nil
	}
	s.once.Do(func() {
		if s.root == "" {
			s.err = fmt.Errorf("no embedded source archive and no source root")
			return
		}
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

// licenseDoc serves the bundled LICENSE.md (the AGPL-3.0 text) as inline plain
// text. Shares the source-tree dependency with /source: nil source (no
// SourceRoot configured) falls through to 404.
func (h *handler) licenseDoc(w http.ResponseWriter, r *http.Request) {
	if h.source == nil {
		http.NotFound(w, r)
		return
	}
	data, err := h.source.license()
	if err != nil {
		http.Error(w, "license unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}
