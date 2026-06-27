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

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
)

// withSourceID returns req carrying the chi {id} URL param the new
// rescan/remove/preview handlers read.
func withSourceID(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

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

// End-to-end over the HTTP layer: add a source, preview its removal, refresh it
// (additive), then remove it — asserting the relation-aware counts and that the
// catalog row and source row are gone afterwards.
func TestSources_RescanPreviewAndRemove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, mgr := newSourcesHandler(t, root)

	w := postSources(t, h, map[string]any{"name": "NAS", "root": root})
	if w.Code != http.StatusCreated {
		t.Fatalf("Add = %d; body=%s", w.Code, w.Body.String())
	}
	var added struct {
		Source struct{ ID string } `json:"source"`
	}
	json.Unmarshal(w.Body.Bytes(), &added)
	mgr.Wait()
	id := added.Source.ID

	// Removal preview: one exclusive links record, nothing kept.
	pw := httptest.NewRecorder()
	h.adminSourcesRemovalPreview(pw, withSourceID(httptest.NewRequest(http.MethodGet, "/", nil), id))
	if pw.Code != http.StatusOK {
		t.Fatalf("preview = %d; body=%s", pw.Code, pw.Body.String())
	}
	var prev struct{ WillRemove, WillKeep int }
	json.Unmarshal(pw.Body.Bytes(), &struct {
		R *int `json:"will_remove"`
		K *int `json:"will_keep"`
	}{R: &prev.WillRemove, K: &prev.WillKeep})
	if prev.WillRemove != 1 || prev.WillKeep != 0 {
		t.Errorf("preview = %+v, want remove=1 keep=0", prev)
	}

	// Refresh (additive rescan): the file is already linked → skipped, re-attributed.
	rw := httptest.NewRecorder()
	h.adminSourcesRescan(rw, withSourceID(httptest.NewRequest(http.MethodPost, "/", nil), id))
	if rw.Code != http.StatusOK {
		t.Fatalf("rescan = %d; body=%s", rw.Code, rw.Body.String())
	}
	mgr.Wait()

	// Remove: the exclusive links record is deleted.
	dw := httptest.NewRecorder()
	h.adminSourcesDelete(dw, withSourceID(httptest.NewRequest(http.MethodDelete, "/", nil), id))
	if dw.Code != http.StatusOK {
		t.Fatalf("delete = %d; body=%s", dw.Code, dw.Body.String())
	}
	var del struct {
		Removed int `json:"removed"`
		Kept    int `json:"kept"`
	}
	json.Unmarshal(dw.Body.Bytes(), &del)
	if del.Removed != 1 || del.Kept != 0 {
		t.Errorf("delete result = %+v, want removed=1 kept=0", del)
	}

	if f, _ := h.repo.GetFileByHash(ctx, sha256OfSourcesTest("bytes")); f != nil {
		t.Error("catalog row should be deleted after source removal")
	}
}

func TestSources_RescanRemove_NotFound(t *testing.T) {
	h, _ := newSourcesHandler(t, t.TempDir())
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		meth string
	}{
		{"rescan", h.adminSourcesRescan, http.MethodPost},
		{"preview", h.adminSourcesRemovalPreview, http.MethodGet},
		{"delete", h.adminSourcesDelete, http.MethodDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.call(w, withSourceID(httptest.NewRequest(tc.meth, "/", nil), "missing"))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s(unknown) = %d, want 404", tc.name, w.Code)
			}
		})
	}
}
