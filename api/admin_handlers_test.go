package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// uploadAudio uploads one audio file through the handler and returns its hash.
func uploadAudio(t *testing.T, h *handler, filename string, body []byte) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", filename, "audio/mpeg", body))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("upload %s status = %d: %s", filename, rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	hash, _ := resp["hash"].(string)
	if hash == "" {
		t.Fatalf("upload %s returned empty hash", filename)
	}
	return hash
}

// deleteReq builds a DELETE /api/admin/files/{hash} request.
func deleteReq(hash string) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/files/"+hash, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// trashHardDeleteReq builds a DELETE /api/admin/trash/{hash} request.
func trashHardDeleteReq(hash string) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/trash/"+hash, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// trashRestoreReq builds a POST /api/admin/trash/{hash}/restore request.
func trashRestoreReq(hash string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/trash/"+hash+"/restore", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// ---- adminDeleteFile ---------------------------------------------------------

func TestAdminDeleteFile_InvalidHash(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq("not-a-valid-hash"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["error"] != "invalid hash" {
		t.Errorf("error = %v, want \"invalid hash\"", resp["error"])
	}
}

func TestAdminDeleteFile_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	unknown := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq(unknown))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["ok"] != false || resp["error"] != "not found" {
		t.Errorf("body = %v, want {ok:false, error:not found}", resp)
	}
}

func TestAdminDeleteFile_Success(t *testing.T) {
	h, db, base := newTestHandler(t)
	hash := uploadAudio(t, h, "song.mp3", []byte("delete this audio"))

	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq(hash))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK        bool     `json:"ok"`
		Hash      string   `json:"hash"`
		Filenames []string `json:"filenames"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Hash != hash {
		t.Errorf("resp = %+v, want ok=true hash=%s", resp, hash)
	}
	if len(resp.Filenames) != 1 || resp.Filenames[0] != "song.mp3" {
		t.Errorf("filenames = %v, want [song.mp3]", resp.Filenames)
	}

	// DB row still present with deleted_at set (soft delete).
	got, _ := db.GetFileByHash(context.Background(), hash)
	if got == nil {
		t.Fatal("files row should still exist after soft delete")
	}
	if !got.DeletedAt.Valid {
		t.Error("files row should have deleted_at set after soft delete")
	}
	// Blob still on disk.
	if _, err := os.Stat(filepath.Join(base, hash)); os.IsNotExist(err) {
		t.Error("blob dir should still be present after soft delete")
	}
}

// TestAdminDeleteFile_BlobAlreadyMissing verifies that soft-deleting a file
// whose blob is already missing still returns 200 — the DB row is marked as
// trashed regardless of blob presence.
func TestAdminDeleteFile_BlobAlreadyMissing(t *testing.T) {
	h, db, base := newTestHandler(t)
	hash := uploadAudio(t, h, "song.mp3", []byte("blob will vanish"))

	// Remove the blob before soft-deleting.
	if err := os.RemoveAll(filepath.Join(base, hash)); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq(hash))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}

	// Row still present with deleted_at set.
	got, _ := db.GetFileByHash(context.Background(), hash)
	if got == nil || !got.DeletedAt.Valid {
		t.Error("files row should exist and have deleted_at set after soft delete")
	}
}

// TestAdminDeleteFile_DBErrorReturns500 uses a fakeRepo to force a delete error.
func TestAdminDeleteFile_DBErrorReturns500(t *testing.T) {
	repo := &fakeRepo{deleteErr: context.DeadlineExceeded}
	h := &handler{storage: storage.NewLocal(t.TempDir()), repo: repo, cacheDir: t.TempDir(), maxUploadSize: testMaxUpload}
	hash := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq(hash))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// ---- adminPrune --------------------------------------------------------------

// pruneReq builds a POST /api/admin/prune request with the given JSON body
// (pass nil for an empty body = dry run).
func pruneReq(body []byte) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/prune", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/prune", bytes.NewReader(body))
	}
	return r
}

func TestAdminPrune_DryRunReportsDangling(t *testing.T) {
	h, _, base := newTestHandler(t)
	healthy := uploadAudio(t, h, "healthy.mp3", []byte("healthy"))
	dangling := uploadAudio(t, h, "gone.mp3", []byte("dangling"))

	// Delete the dangling blob on disk so its DB row is now dangling.
	if err := os.RemoveAll(filepath.Join(base, dangling)); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq([]byte(`{"confirm":false}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK            bool `json:"ok"`
		DryRun        bool `json:"dry_run"`
		Scanned       int  `json:"scanned"`
		DanglingCount int  `json:"dangling_count"`
		Dangling      []struct {
			Hash      string   `json:"hash"`
			Filenames []string `json:"filenames"`
		} `json:"dangling"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.DryRun {
		t.Errorf("resp ok=%v dry_run=%v, want both true", resp.OK, resp.DryRun)
	}
	if resp.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", resp.Scanned)
	}
	if resp.DanglingCount != 1 || len(resp.Dangling) != 1 || resp.Dangling[0].Hash != dangling {
		t.Errorf("dangling = %+v, want one entry for %s", resp.Dangling, dangling)
	}

	// Dry run deletes nothing: the healthy file is still listed.
	refs, _ := h.repo.ListFileRefs(context.Background())
	if len(refs) != 2 {
		t.Errorf("file rows = %d after dry run, want 2", len(refs))
	}
	_ = healthy
}

func TestAdminPrune_EmptyBodyIsDryRun(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq(nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["dry_run"] != true {
		t.Errorf("dry_run = %v, want true for empty body", resp["dry_run"])
	}
}

func TestAdminPrune_InvalidJSON(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq([]byte(`{not json`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAdminPrune_ConfirmDeletesDangling(t *testing.T) {
	h, db, base := newTestHandler(t)
	healthy := uploadAudio(t, h, "healthy.mp3", []byte("healthy"))
	dangling := uploadAudio(t, h, "gone.mp3", []byte("dangling"))

	if err := os.RemoveAll(filepath.Join(base, dangling)); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq([]byte(`{"confirm":true}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK          bool `json:"ok"`
		DryRun      bool `json:"dry_run"`
		Scanned     int  `json:"scanned"`
		PrunedCount int  `json:"pruned_count"`
		Pruned      []struct {
			Hash string `json:"hash"`
		} `json:"pruned"`
		Failed []any `json:"failed"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DryRun {
		t.Error("dry_run = true, want false on confirm")
	}
	if resp.PrunedCount != 1 || len(resp.Pruned) != 1 || resp.Pruned[0].Hash != dangling {
		t.Errorf("pruned = %+v, want one entry for %s", resp.Pruned, dangling)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed = %v, want empty", resp.Failed)
	}

	// The dangling row is gone, healthy survives.
	if got, _ := db.GetFileByHash(context.Background(), dangling); got != nil {
		t.Error("dangling row still present after confirm prune")
	}
	if got, _ := db.GetFileByHash(context.Background(), healthy); got == nil {
		t.Error("healthy row removed by prune")
	}
}

// TestAdminRoutes_Wired exercises the endpoints through the full router so the
// route registration, hash param, and CORS DELETE method are all covered.
func TestAdminRoutes_Wired(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewRouter(storage.NewLocal(dir), db, t.TempDir(), dir, testMaxUpload))
	t.Cleanup(srv.Close)

	// Upload through the router so a real blob + row exist.
	body := buildUploadBody(t, "file", "song.mp3", "audio/mpeg", []byte("route delete"))
	up, err := http.Post(srv.URL+"/files/upload", body.contentType, body.reader)
	if err != nil {
		t.Fatal(err)
	}
	var upResp map[string]any
	json.NewDecoder(up.Body).Decode(&upResp)
	up.Body.Close()
	hash, _ := upResp["hash"].(string)

	// DELETE via the router.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/files/"+hash, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", resp.StatusCode)
	}

	// CORS preflight for DELETE must advertise the method.
	pre, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/admin/files/"+hash, nil)
	preResp, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	defer preResp.Body.Close()
	if methods := preResp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "DELETE") {
		t.Errorf("Allow-Methods = %q, want it to include DELETE", methods)
	}
}

// ---- adminTrashList ---------------------------------------------------------

func TestAdminTrashList_Empty(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminTrashList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/trash", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var items []any
	if err := json.NewDecoder(rr.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("len = %d, want 0 for empty trash", len(items))
	}
}

func TestAdminTrashList_ReturnsTrashedFiles(t *testing.T) {
	h, _, _ := newTestHandler(t)
	hash := uploadAudio(t, h, "trash-me.mp3", []byte("trash content"))

	// Soft-delete the file.
	rr := httptest.NewRecorder()
	h.adminDeleteFile(rr, deleteReq(hash))
	if rr.Code != http.StatusOK {
		t.Fatalf("soft-delete status = %d", rr.Code)
	}

	// Trash list must contain the file.
	rr2 := httptest.NewRecorder()
	h.adminTrashList(rr2, httptest.NewRequest(http.MethodGet, "/api/admin/trash", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("trash list status = %d", rr2.Code)
	}
	var items []map[string]any
	if err := json.NewDecoder(rr2.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	if items[0]["hash"] != hash {
		t.Errorf("hash = %v, want %s", items[0]["hash"], hash)
	}
	if items[0]["deleted_at"] == nil || items[0]["deleted_at"].(float64) == 0 {
		t.Error("deleted_at not set in trash list response")
	}
}

// ---- adminTrashHardDelete ---------------------------------------------------

func TestAdminTrashHardDelete_InvalidHash(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminTrashHardDelete(rr, trashHardDeleteReq("not-valid"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAdminTrashHardDelete_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	unknown := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	rr := httptest.NewRecorder()
	h.adminTrashHardDelete(rr, trashHardDeleteReq(unknown))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestAdminTrashHardDelete_LiveFileRejected is the key invariant: calling the
// permanent-delete endpoint on a live (non-trashed) file must fail with 404
// so that only the two-step soft-delete → hard-delete path is possible.
func TestAdminTrashHardDelete_LiveFileRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)
	hash := uploadAudio(t, h, "live.mp3", []byte("live audio"))

	rr := httptest.NewRecorder()
	h.adminTrashHardDelete(rr, trashHardDeleteReq(hash))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for live (non-trashed) file", rr.Code)
	}
}

func TestAdminTrashHardDelete_Success(t *testing.T) {
	h, db, base := newTestHandler(t)
	hash := uploadAudio(t, h, "delete-forever.mp3", []byte("gone for good"))

	// Soft-delete first.
	h.adminDeleteFile(httptest.NewRecorder(), deleteReq(hash))

	rr := httptest.NewRecorder()
	h.adminTrashHardDelete(rr, trashHardDeleteReq(hash))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}

	// DB row gone.
	if got, _ := db.GetFileByHash(context.Background(), hash); got != nil {
		t.Error("files row still present after hard delete from trash")
	}
	// Blob dir gone.
	if _, err := os.Stat(filepath.Join(base, hash)); !os.IsNotExist(err) {
		t.Errorf("blob dir still present after hard delete: %v", err)
	}
}

// ---- adminTrashRestore ------------------------------------------------------

func TestAdminTrashRestore_InvalidHash(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminTrashRestore(rr, trashRestoreReq("not-valid"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAdminTrashRestore_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	unknown := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	rr := httptest.NewRecorder()
	h.adminTrashRestore(rr, trashRestoreReq(unknown))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAdminTrashRestore_Success(t *testing.T) {
	h, db, _ := newTestHandler(t)
	hash := uploadAudio(t, h, "restore-me.mp3", []byte("restore content"))

	// Soft-delete then restore.
	h.adminDeleteFile(httptest.NewRecorder(), deleteReq(hash))

	rr := httptest.NewRecorder()
	h.adminTrashRestore(rr, trashRestoreReq(hash))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["hash"] != hash {
		t.Errorf("hash = %v, want %s", resp["hash"], hash)
	}

	// Row must be live again (no deleted_at).
	got, _ := db.GetFileByHash(context.Background(), hash)
	if got == nil {
		t.Fatal("files row missing after restore")
	}
	if got.DeletedAt.Valid {
		t.Errorf("DeletedAt still set after restore: %d", got.DeletedAt.Int64)
	}

	// File must appear in ListFiles.
	entries, _ := db.ListFiles(context.Background())
	var found bool
	for _, e := range entries {
		if e.Hash == hash {
			found = true
		}
	}
	if !found {
		t.Error("restored file not present in ListFiles")
	}
}
