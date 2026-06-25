package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/storages"
	"github.com/go-chi/chi/v5"
)

// fakeHash is a valid-shaped 64-char lowercase hex digest. The resolver keys on
// the hash shape and the filesystem, not on bytes actually hashing to it, so a
// synthetic value is fine for serving tests.
func fakeHash(seed byte) string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hexd[(int(seed)+i)%16]
	}
	return string(b)
}

// putBlob writes <root>/audio/<hash>/<name> with content (a local blob).
func putBlob(t *testing.T, root, hash, name, content string) {
	t.Helper()
	dir := filepath.Join(root, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkBlobInto writes <root>/audio/<hash>/<name> as a symlink to target (a links
// storage entry).
func linkBlobInto(t *testing.T, root, hash, name, target string) {
	t.Helper()
	dir := filepath.Join(root, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// newBlobServer wires SupportHEAD + RegisterAPI over an in-memory DB with the
// given registry. Auth is unconfigured, so the /files guard is a pass-through
// and serving is decided purely by the resolver.
func newBlobServer(t *testing.T, filesDir string, reg *storages.Registry) *httptest.Server {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := chi.NewRouter()
	r.Use(SupportHEAD)
	RegisterAPI(r, Deps{
		Store:         storage.NewLocal(filepath.Join(filesDir, storage.AudioSubdir)),
		Repo:          db,
		CacheDir:      t.TempDir(),
		FilesDir:      filesDir,
		Storages:      reg,
		MaxUploadSize: testMaxUpload,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestBlobServe_LocalGET serves an owned blob and checks body + headers.
func TestBlobServe_LocalGET(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := fakeHash(1)
	putBlob(t, files, h, "song.mp3", "the-audio-bytes")
	srv := newBlobServer(t, files, storages.New(files, links))

	resp, err := http.Get(srv.URL + "/files/" + h + "/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if got := string(bodyBytes(t, resp)); got != "the-audio-bytes" {
		t.Errorf("body = %q, want the blob content", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff = %q, want nosniff", got)
	}
}

// TestBlobServe_HEAD confirms http.ServeFile sizes the blob for HEAD (200,
// Content-Length set, empty body) through the resolver + SupportHEAD chain.
func TestBlobServe_HEAD(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := fakeHash(2)
	content := "0123456789"
	putBlob(t, files, h, "clip.mp3", content)
	srv := newBlobServer(t, files, storages.New(files, links))

	resp := headReq(t, srv, "/files/"+h+"/clip.mp3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if got := resp.ContentLength; got != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d", got, len(content))
	}
	if b := bodyBytes(t, resp); len(b) != 0 {
		t.Errorf("HEAD body = %q, want empty", b)
	}
}

// TestBlobServe_Range confirms native byte-range support: a Range request gets
// 206 with the requested slice (http.ServeFile/ServeContent provide this).
func TestBlobServe_Range(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := fakeHash(3)
	content := "abcdefghij" // bytes 2-5 = "cdef"
	putBlob(t, files, h, "track.mp3", content)
	srv := newBlobServer(t, files, storages.New(files, links))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/files/"+h+"/track.mp3", nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", resp.StatusCode)
	}
	if got := string(bodyBytes(t, resp)); got != "cdef" {
		t.Errorf("range body = %q, want %q", got, "cdef")
	}
}

// TestBlobServe_LinksGET serves a blob that exists only as a symlink in the
// links storage; ServeFile follows it to the external original.
func TestBlobServe_LinksGET(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "original.mp3")
	if err := os.WriteFile(external, []byte("external-original"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := fakeHash(4)
	linkBlobInto(t, links, h, "original.mp3", external)
	srv := newBlobServer(t, files, storages.New(files, links))

	resp, err := http.Get(srv.URL + "/files/" + h + "/original.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("links GET status = %d, want 200", resp.StatusCode)
	}
	if got := string(bodyBytes(t, resp)); got != "external-original" {
		t.Errorf("body = %q, want the external original via the symlink", got)
	}
}

// TestBlobServe_DanglingLinkFallsThroughToLocal: a broken link is treated as
// absent, so a local duplicate of the same hash transparently covers it.
func TestBlobServe_DanglingLinkFallsThroughToLocal(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := fakeHash(5)
	linkBlobInto(t, links, h, "gone.mp3", filepath.Join(t.TempDir(), "missing.mp3"))
	putBlob(t, files, h, "song.mp3", "local-copy")
	srv := newBlobServer(t, files, storages.New(files, links))

	resp, err := http.Get(srv.URL + "/files/" + h + "/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local covers the dangling link)", resp.StatusCode)
	}
	if got := string(bodyBytes(t, resp)); got != "local-copy" {
		t.Errorf("body = %q, want the local duplicate", got)
	}
}

// TestBlobServe_DanglingLinkOnly404: only a broken link exists → 404.
func TestBlobServe_DanglingLinkOnly404(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := fakeHash(6)
	linkBlobInto(t, links, h, "gone.mp3", filepath.Join(t.TempDir(), "missing.mp3"))
	srv := newBlobServer(t, files, storages.New(files, links))

	resp, err := http.Get(srv.URL + "/files/" + h + "/gone.mp3")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (a dangling link is not present)", resp.StatusCode)
	}
}

// TestBlobServe_LocalBeatsLinks: when both storages have the hash, the local
// copy is served (fixed precedence).
func TestBlobServe_LocalBeatsLinks(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "ext.mp3")
	if err := os.WriteFile(external, []byte("from-links"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := fakeHash(7)
	linkBlobInto(t, links, h, "ext.mp3", external)
	putBlob(t, files, h, "song.mp3", "from-local")
	srv := newBlobServer(t, files, storages.New(files, links))

	resp, err := http.Get(srv.URL + "/files/" + h + "/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(bodyBytes(t, resp)); got != "from-local" {
		t.Errorf("body = %q, want the local copy (local outranks links)", got)
	}
}

// TestBlobServe_InvalidHash404: a non-hash first segment (e.g. the images dir
// name) never resolves, so /files can't reach anything but audio hash dirs.
func TestBlobServe_InvalidHash404(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	srv := newBlobServer(t, files, storages.New(files, links))
	for _, p := range []string{"/files/images/cover.png", "/files/", "/files/not-a-hash/x.mp3"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestBlobServe_FallbackRegistry: with no registry wired, the blob server falls
// back to a local-only registry rooted at FilesDir (preserving old behaviour).
func TestBlobServe_FallbackRegistry(t *testing.T) {
	files := t.TempDir()
	h := fakeHash(8)
	putBlob(t, files, h, "song.mp3", "fallback-local")
	srv := newBlobServer(t, files, nil) // nil registry → fallback

	resp, err := http.Get(srv.URL + "/files/" + h + "/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback registry", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(bodyBytes(t, resp))); got != "fallback-local" {
		t.Errorf("body = %q, want fallback-local", got)
	}
}
