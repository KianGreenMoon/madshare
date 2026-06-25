package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
)

// sha256OfSourcesTest is the content hash the scan engine computes for a file
// holding exactly these bytes.
func sha256OfSourcesTest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newSourcesHandler builds a handler wired with a real in-memory DB (which
// satisfies both api.Repository and sources.Store) and a sources.Manager whose
// allow-list is the given root. Returns the handler and the manager (for Wait).
func newSourcesHandler(t *testing.T, root string) (*handler, *sources.Manager) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	linker := storages.NewLinker(t.TempDir())
	var roots []string
	if root != "" {
		roots = []string{root}
	}
	mgr := sources.New(db, linker, nil, roots, AcceptedAudioTypes())
	return &handler{repo: db, sourcesMgr: mgr}, mgr
}

func postSources(t *testing.T, h *handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/sources", bytes.NewReader(buf))
	w := httptest.NewRecorder()
	h.adminSourcesAdd(w, req)
	return w
}

func TestSources_Unavailable_WhenNoManager(t *testing.T) {
	h := &handler{}
	for _, call := range []func(http.ResponseWriter, *http.Request){h.adminSourcesList, h.adminSourcesAdd} {
		w := httptest.NewRecorder()
		call(w, httptest.NewRequest(http.MethodGet, "/api/admin/sources", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("no-manager call = %d, want 503", w.Code)
		}
	}
}

func TestSources_Add_AndList(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, mgr := newSourcesHandler(t, root)

	w := postSources(t, h, map[string]any{"kind": "symlink", "name": "NAS", "root": root})
	if w.Code != http.StatusCreated {
		t.Fatalf("Add = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var added struct {
		OK     bool `json:"ok"`
		Source struct {
			ID, Status, Root string
		} `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if !added.OK || added.Source.Status != database.SourceStatusScanning {
		t.Errorf("added source = %+v, want ok + scanning", added.Source)
	}

	mgr.Wait() // let the background scan finish before listing

	lw := httptest.NewRecorder()
	h.adminSourcesList(lw, httptest.NewRequest(http.MethodGet, "/api/admin/sources", nil))
	if lw.Code != http.StatusOK {
		t.Fatalf("List = %d, want 200", lw.Code)
	}
	var listed struct {
		OK      bool             `json:"ok"`
		Enabled bool             `json:"enabled"`
		Sources []sources.Source `json:"sources"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if !listed.Enabled || len(listed.Sources) != 1 {
		t.Fatalf("List = %+v, want one enabled source", listed)
	}
	s := listed.Sources[0]
	if s.Status != database.SourceStatusActive {
		t.Errorf("scanned source status = %q, want active", s.Status)
	}
	if s.Summary == nil || s.Summary.Linked != 1 {
		t.Errorf("summary = %+v, want linked=1", s.Summary)
	}

	// The scanned file is now a links-backed catalog row.
	f, err := h.repo.GetFileByHash(context.Background(), sha256OfSourcesTest("bytes"))
	if err != nil || f == nil {
		t.Fatalf("expected a catalog row for the linked file: %v", err)
	}
	if f.StorageBackend != database.StorageBackendLinks || !f.LinkTarget.Valid {
		t.Errorf("linked file = %+v, want links backend + link_target", f)
	}
}

func TestSources_Add_RootNotAllowed(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	h, _ := newSourcesHandler(t, allowed)

	w := postSources(t, h, map[string]any{"name": "x", "root": outside})
	if w.Code != http.StatusForbidden {
		t.Errorf("Add(outside) = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestSources_Add_BadRequests(t *testing.T) {
	root := t.TempDir()
	h, _ := newSourcesHandler(t, root)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing name", map[string]any{"root": root}, http.StatusBadRequest},
		{"unsupported kind", map[string]any{"kind": "s3", "name": "x", "root": root}, http.StatusBadRequest},
		{"relative root", map[string]any{"name": "x", "root": "rel/path"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postSources(t, h, tc.body).Code; got != tc.want {
				t.Errorf("Add = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSources_Disabled_WhenNoRoots(t *testing.T) {
	h, _ := newSourcesHandler(t, "") // no allow-list → kind disabled
	w := postSources(t, h, map[string]any{"name": "x", "root": "/srv/music"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Add with no roots = %d, want 503", w.Code)
	}
}
