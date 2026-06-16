package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// newHeadTestServer builds an httptest server with SupportHEAD wired in front of
// the real API routes (mirroring buildHandler), over an in-memory DB. Auth is not
// configured, so the /files guard is a pass-through (a missing blob still 404s).
func newHeadTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := chi.NewRouter()
	r.Use(SupportHEAD)
	RegisterAPI(r, Deps{
		Store:         storage.NewLocal(dir),
		Repo:          db,
		CacheDir:      t.TempDir(),
		FilesDir:      dir,
		MaxUploadSize: testMaxUpload,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func headReq(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	return resp
}

func bodyBytes(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// TestSupportHEAD_HealthzEmptyBody is the canonical case from the issue: a HEAD on
// a GET-only route (/healthz) returns 200 with no body, where without the
// middleware chi answers 405.
func TestSupportHEAD_HealthzEmptyBody(t *testing.T) {
	srv := newHeadTestServer(t)

	resp := headReq(t, srv, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /healthz status = %d, want 200", resp.StatusCode)
	}
	if b := bodyBytes(t, resp); len(b) != 0 {
		t.Errorf("HEAD /healthz body = %q, want empty", b)
	}

	// Contrast: the same route under GET still returns its body.
	g, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if b := bodyBytes(t, g); string(b) != "ok" {
		t.Errorf("GET /healthz body = %q, want \"ok\"", b)
	}
}

// TestSupportHEAD_ApiRoutesAnswerHead checks that the JSON API routes answer HEAD
// too (every GET route, not just the file servers).
func TestSupportHEAD_ApiRoutesAnswerHead(t *testing.T) {
	srv := newHeadTestServer(t)
	for _, path := range []string{"/api/artists", "/api/files"} {
		resp := headReq(t, srv, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s status = %d, want 200", path, resp.StatusCode)
		}
		if b := bodyBytes(t, resp); len(b) != 0 {
			t.Errorf("HEAD %s body = %q, want empty", path, b)
		}
	}
}

// TestSupportHEAD_MissingFileStill404 confirms a HEAD goes through the file
// server's normal not-found path (status preserved, body discarded).
func TestSupportHEAD_MissingFileStill404(t *testing.T) {
	srv := newHeadTestServer(t)
	resp := headReq(t, srv, "/files/deadbeefdeadbeef/missing.mp3")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD missing /files status = %d, want 404", resp.StatusCode)
	}
	if b := bodyBytes(t, resp); len(b) != 0 {
		t.Errorf("HEAD 404 body = %q, want empty", b)
	}
}

// TestSupportHEAD_GuardRunsOnHead is the security property: the rewrite happens
// before routing, so a guard wrapping a GET route runs on HEAD too — access
// control can't be bypassed by switching method. A deny guard returns 403; the
// HEAD must see it, not a 200.
func TestSupportHEAD_GuardRunsOnHead(t *testing.T) {
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		})
	}
	r := chi.NewRouter()
	r.Use(SupportHEAD)
	r.With(deny).Get("/secret", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("leaked"))
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := headReq(t, srv, "/secret")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HEAD guarded route status = %d, want 403", resp.StatusCode)
	}
	if b := bodyBytes(t, resp); len(b) != 0 {
		t.Errorf("HEAD guarded body = %q, want empty (no leak)", b)
	}
}

// TestSupportHEAD_ForwardsHeaders confirms headers a GET would set are present on
// the HEAD response (only the body is dropped).
func TestSupportHEAD_ForwardsHeaders(t *testing.T) {
	r := chi.NewRouter()
	r.Use(SupportHEAD)
	r.Get("/thing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test", "yes")
		w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := headReq(t, srv, "/thing")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Test"); got != "yes" {
		t.Errorf("X-Test header = %q, want \"yes\"", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if b := bodyBytes(t, resp); len(b) != 0 {
		t.Errorf("HEAD body = %q, want empty", b)
	}
}
