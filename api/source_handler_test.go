package api

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSourceArchive(t *testing.T) {
	root := t.TempDir()

	mustRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	mustRun("git", "init")
	mustRun("git", "config", "user.email", "test@test.com")
	mustRun("git", "config", "user.name", "test")

	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.toml\n"), 0644)
	os.WriteFile(filepath.Join(root, "secret.toml"), []byte("[secret]\n"), 0644)

	mustRun("git", "add", "go.mod", "main.go", ".gitignore")
	mustRun("git", "commit", "-m", "initial")

	h := &handler{source: &sourceArchiver{root: root}}
	rec := httptest.NewRecorder()
	h.sourceArchive(rec, httptest.NewRequest(http.MethodGet, "/source", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}

	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)

	want := map[string]bool{"go.mod": false, "main.go": false, ".gitignore": false}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		delete(want, hdr.Name)
		if hdr.Name == "secret.toml" {
			t.Errorf("archive contains gitignored file %q", hdr.Name)
		}
	}
	for f := range want {
		t.Errorf("archive missing expected file %q", f)
	}
}

func TestSourceArchivePrebuilt(t *testing.T) {
	// A build-time-embedded archive is served verbatim, with no git invocation
	// and no working tree (root left empty).
	want := []byte("not really a tarball, but served as-is")
	h := &handler{source: &sourceArchiver{prebuilt: want}}
	rec := httptest.NewRecorder()
	h.sourceArchive(rec, httptest.NewRequest(http.MethodGet, "/source", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if got := rec.Body.Bytes(); string(got) != string(want) {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestLicensePrebuilt(t *testing.T) {
	// Embedded LICENSE bytes are served even with no SourceRoot on disk.
	want := "GNU AFFERO GENERAL PUBLIC LICENSE (embedded)\n"
	h := &handler{source: &sourceArchiver{licensePrebuilt: []byte(want)}}
	rec := httptest.NewRecorder()
	h.licenseDoc(rec, httptest.NewRequest(http.MethodGet, "/license", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestSourceArchiveNotConfigured(t *testing.T) {
	h := &handler{}
	rec := httptest.NewRecorder()
	h.sourceArchive(rec, httptest.NewRequest(http.MethodGet, "/source", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestLicenseDoc(t *testing.T) {
	root := t.TempDir()
	body := "GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3\n"
	if err := os.WriteFile(filepath.Join(root, "LICENSE.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	h := &handler{source: &sourceArchiver{root: root}}
	rec := httptest.NewRecorder()
	h.licenseDoc(rec, httptest.NewRequest(http.MethodGet, "/license", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

func TestLicenseDocNotConfigured(t *testing.T) {
	h := &handler{}
	rec := httptest.NewRecorder()
	h.licenseDoc(rec, httptest.NewRequest(http.MethodGet, "/license", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestLicenseDocMissingFile(t *testing.T) {
	// SourceRoot configured but no LICENSE.md on disk → 503, not a panic.
	h := &handler{source: &sourceArchiver{root: t.TempDir()}}
	rec := httptest.NewRecorder()
	h.licenseDoc(rec, httptest.NewRequest(http.MethodGet, "/license", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
