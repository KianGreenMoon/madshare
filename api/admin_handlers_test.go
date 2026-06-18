package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// ---- adminStorageStats -------------------------------------------------------

func TestAdminStorageStats(t *testing.T) {
	repo := &fakeRepo{libraryBytes: 4096}
	dir := t.TempDir()
	h := &handler{storage: storage.NewLocal(dir), repo: repo, cacheDir: t.TempDir(), maxUploadSize: testMaxUpload}

	rr := httptest.NewRecorder()
	h.adminStorageStats(rr, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Backend      string `json:"backend"`
		Location     string `json:"location"`
		LibraryBytes int64  `json:"library_bytes"`
		Volume       *struct {
			TotalBytes  uint64  `json:"total_bytes"`
			FreeBytes   uint64  `json:"free_bytes"`
			UsedBytes   uint64  `json:"used_bytes"`
			UsedPercent float64 `json:"used_percent"`
		} `json:"volume"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rr.Body.String())
	}
	if resp.Backend != "local" {
		t.Errorf("backend = %q, want local", resp.Backend)
	}
	if resp.LibraryBytes != 4096 {
		t.Errorf("library_bytes = %d, want 4096", resp.LibraryBytes)
	}
	if resp.Volume == nil {
		t.Skip("no volume reported on this platform")
	}
	if resp.Volume.TotalBytes == 0 {
		t.Error("volume.total_bytes = 0 on a real disk, want > 0")
	}
	if resp.Volume.UsedPercent < 0 || resp.Volume.UsedPercent > 100 {
		t.Errorf("used_percent = %v, want [0,100]", resp.Volume.UsedPercent)
	}
}

// TestAdminStorageStats_DBErrorReturns500 forces the library-size query to fail.
func TestAdminStorageStats_DBErrorReturns500(t *testing.T) {
	repo := &fakeRepo{libraryBytesErr: context.DeadlineExceeded}
	h := &handler{storage: storage.NewLocal(t.TempDir()), repo: repo, cacheDir: t.TempDir(), maxUploadSize: testMaxUpload}

	rr := httptest.NewRecorder()
	h.adminStorageStats(rr, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// ---- adminPrune --------------------------------------------------------------

// pruneReq builds a POST /api/admin/prune request with the given JSON body
// (pass nil for an empty body = scan).
func pruneReq(body []byte) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/prune", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/prune", bytes.NewReader(body))
	}
	return r
}

// startPrune fires adminPrune, asserts the start status code, then blocks until
// the detached job finishes — the prune is now async (202 + a background run), so
// tests start it and Wait rather than reading a synchronous result.
func startPrune(t *testing.T, h *handler, body []byte, wantStatus int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq(body))
	if rr.Code != wantStatus {
		t.Fatalf("start status = %d, want %d; body: %s", rr.Code, wantStatus, rr.Body.String())
	}
	h.pruneMgr.Wait()
}

// pruneStatus reads GET /api/admin/prune/status as a decoded map.
func pruneStatus(t *testing.T, h *handler) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	h.adminPruneStatus(rr, httptest.NewRequest(http.MethodGet, "/api/admin/prune/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return resp
}

func TestAdminPrune_ScanReportsDangling(t *testing.T) {
	h, _, base := newTestHandler(t)
	uploadAudio(t, h, "healthy.mp3", []byte("healthy"))
	dangling := uploadAudio(t, h, "gone.mp3", []byte("dangling"))

	// Delete the dangling blob on disk so its DB row is now dangling.
	if err := os.RemoveAll(filepath.Join(base, dangling)); err != nil {
		t.Fatal(err)
	}

	startPrune(t, h, []byte(`{"confirm":false}`), http.StatusAccepted)

	snap := pruneStatus(t, h)
	if snap["state"] != "idle" {
		t.Errorf("state = %v, want idle after scan finished", snap["state"])
	}
	res, _ := snap["last_result"].(map[string]any)
	if res == nil || res["kind"] != "scan" {
		t.Fatalf("last_result = %v, want a scan detail", snap["last_result"])
	}
	if got := res["scanned"]; got != float64(2) {
		t.Errorf("scanned = %v, want 2", got)
	}
	dl, _ := res["dangling"].([]any)
	if len(dl) != 1 {
		t.Fatalf("dangling = %v, want one entry", res["dangling"])
	}
	if first, _ := dl[0].(map[string]any); first["hash"] != dangling {
		t.Errorf("dangling hash = %v, want %s", first["hash"], dangling)
	}

	// Scan deletes nothing: both rows survive.
	refs, _ := h.repo.ListFileRefs(context.Background())
	if len(refs) != 2 {
		t.Errorf("file rows = %d after scan, want 2", len(refs))
	}
}

func TestAdminPrune_EmptyBodyIsScan(t *testing.T) {
	h, _, _ := newTestHandler(t)
	startPrune(t, h, nil, http.StatusAccepted)
	snap := pruneStatus(t, h)
	if snap["last_scan"] == nil {
		t.Errorf("last_scan = nil, want a scan summary after empty-body start")
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

// A confirm with no prior scan is refused (409) — prune deletes only a reviewed
// set, so a scan must run first.
func TestAdminPrune_ConfirmWithoutScanIs409(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminPrune(rr, pruneReq([]byte(`{"confirm":true}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 without a prior scan", rr.Code)
	}
}

func TestAdminPrune_ScanThenConfirmDeletesDangling(t *testing.T) {
	h, db, base := newTestHandler(t)
	healthy := uploadAudio(t, h, "healthy.mp3", []byte("healthy"))
	dangling := uploadAudio(t, h, "gone.mp3", []byte("dangling"))

	if err := os.RemoveAll(filepath.Join(base, dangling)); err != nil {
		t.Fatal(err)
	}

	// Scan (preview) first so the prune has a reviewed set to act on.
	startPrune(t, h, []byte(`{"confirm":false}`), http.StatusAccepted)
	// Then prune.
	startPrune(t, h, []byte(`{"confirm":true}`), http.StatusAccepted)

	snap := pruneStatus(t, h)
	res, _ := snap["last_result"].(map[string]any)
	if res == nil || res["kind"] != "prune" {
		t.Fatalf("last_result = %v, want a prune detail", snap["last_result"])
	}
	if got := res["pruned_count"]; got != float64(1) {
		t.Errorf("pruned_count = %v, want 1", got)
	}
	if failed, _ := res["failed"].([]any); len(failed) != 0 {
		t.Errorf("failed = %v, want empty", res["failed"])
	}

	// The dangling row is gone, healthy survives.
	if got, _ := db.GetFileByHash(context.Background(), dangling); got != nil {
		t.Error("dangling row still present after confirm prune")
	}
	if got, _ := db.GetFileByHash(context.Background(), healthy); got == nil {
		t.Error("healthy row removed by prune")
	}
}

// Cancel on an idle manager is a no-op reporting cancelled=false.
func TestAdminPruneCancel_IdleNoop(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.adminPruneCancel(rr, httptest.NewRequest(http.MethodPost, "/api/admin/prune/cancel", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["cancelled"] != false {
		t.Errorf("cancelled = %v, want false when idle", resp["cancelled"])
	}
}

// TestAdminRoutes_Wired exercises the endpoints through the full router so the
// route registration and hash param are covered. (CORS preflight, incl. the
// DELETE method advertisement, is covered by TestCORS_Preflight.)
func TestAdminRoutes_Wired(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewRouter(storage.NewLocal(filepath.Join(dir, storage.AudioSubdir)), db, t.TempDir(), dir, testMaxUpload))
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
