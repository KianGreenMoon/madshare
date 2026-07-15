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

// ---- adminStorageStats -------------------------------------------------------

// writeFileN writes a file of exactly n bytes at path, creating parents.
func writeFileN(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdminStorageStats(t *testing.T) {
	filesDir := t.TempDir()
	imagesDir := filepath.Join(filesDir, "images")
	// Hybrid sizing: the files-table categories come from the DB breakdown
	// (audio=3000, review=700, trash=300); images are walked on disk
	// (1000 + 500 = 1500 across two variant files).
	writeFileN(t, filepath.Join(imagesDir, "key", "small_crop.jpg"), 1000)
	writeFileN(t, filepath.Join(imagesDir, "key", "small_fit.jpg"), 500)

	repo := &fakeRepo{breakdown: database.StorageByteBreakdown{Library: 3000, Review: 700, Trash: 300}}
	h := &handler{
		storage:   storage.NewLocal(filepath.Join(filesDir, storage.AudioSubdir)),
		repo:      repo,
		filesDir:  filesDir,
		imagesDir: imagesDir,
		spoolDir:  t.TempDir(), maxUploadSize: testMaxUpload,
	}

	rr := httptest.NewRecorder()
	h.adminStorageStats(rr, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Backend      string `json:"backend"`
		Location     string `json:"location"`
		LibraryBytes uint64 `json:"library_bytes"`
		Categories   []struct {
			Name  string `json:"name"`
			Bytes uint64 `json:"bytes"`
		} `json:"categories"`
		Volume *struct {
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
	if resp.Location != filesDir {
		t.Errorf("location = %q, want %q", resp.Location, filesDir)
	}
	want := map[string]uint64{"audio": 3000, "review": 700, "trash": 300, "images": 1500}
	got := map[string]uint64{}
	for _, c := range resp.Categories {
		got[c.Name] = c.Bytes
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("category %q = %d bytes, want %d", name, got[name], n)
		}
	}
	if resp.LibraryBytes != 5500 {
		t.Errorf("library_bytes = %d, want 5500 (sum of categories)", resp.LibraryBytes)
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

// TestAdminStorageStats_WalkErrorReturns500 forces the images walk to fail by
// pointing imagesDir below a regular file (ENOTDIR).
func TestAdminStorageStats_WalkErrorReturns500(t *testing.T) {
	filesDir := t.TempDir()
	regular := filepath.Join(filesDir, "not-a-dir")
	writeFileN(t, regular, 1)

	h := &handler{
		storage:   storage.NewLocal(filepath.Join(filesDir, storage.AudioSubdir)),
		repo:      &fakeRepo{},
		filesDir:  filesDir,
		imagesDir: filepath.Join(regular, "sub"), // below a regular file → walk errors
		spoolDir:  t.TempDir(), maxUploadSize: testMaxUpload,
	}

	rr := httptest.NewRecorder()
	h.adminStorageStats(rr, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// TestAdminStorageStats_DBErrorReturns500 forces the DB breakdown query to fail.
func TestAdminStorageStats_DBErrorReturns500(t *testing.T) {
	filesDir := t.TempDir()
	h := &handler{
		storage:   storage.NewLocal(filepath.Join(filesDir, storage.AudioSubdir)),
		repo:      &fakeRepo{breakdownErr: context.DeadlineExceeded},
		filesDir:  filesDir,
		imagesDir: filepath.Join(filesDir, "images"),
		spoolDir:  t.TempDir(), maxUploadSize: testMaxUpload,
	}

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
// admin route registration is covered. (CORS preflight, incl. the DELETE
// method advertisement, is covered by TestCORS_Preflight.)
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

	if hash == "" {
		t.Fatal("upload returned no hash")
	}

	// Trash the appearance via the router (the tagset-addressed bulk endpoint).
	body2, _ := json.Marshal(map[string]any{
		"action": "trash", "filter": map[string]any{}, "all": true,
	})
	resp, err := http.Post(srv.URL+"/api/admin/appearances/bulk", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK       bool `json:"ok"`
		Affected int  `json:"affected"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || !out.OK || out.Affected != 1 {
		t.Fatalf("bulk trash via router = %d ok=%v affected=%d, want 200/true/1", resp.StatusCode, out.OK, out.Affected)
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
	var env struct {
		Total int   `json:"total"`
		Items []any `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Total != 0 || len(env.Items) != 0 {
		t.Errorf("total=%d len=%d, want 0/0 for empty trash", env.Total, len(env.Items))
	}
}

func TestAdminTrashList_ReturnsTrashedFiles(t *testing.T) {
	h, db, _ := newTestHandler(t)
	hash := uploadAudio(t, h, "trash-me.mp3", []byte("trash content"))

	// Trash the appearance (production does this by tagset id).
	trashAppearancesOf(t, db, hash)

	// Trash list must contain the file.
	rr2 := httptest.NewRecorder()
	h.adminTrashList(rr2, httptest.NewRequest(http.MethodGet, "/api/admin/trash", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("trash list status = %d", rr2.Code)
	}
	var env struct {
		Total int              `json:"total"`
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(rr2.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Total != 1 || len(env.Items) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", env.Total, len(env.Items))
	}
	if env.Items[0]["hash"] != hash {
		t.Errorf("hash = %v, want %s", env.Items[0]["hash"], hash)
	}
	// The row identity is the appearance, not the blob (recording-tagsets P7c).
	if id, ok := env.Items[0]["tagset_id"].(float64); !ok || id == 0 {
		t.Errorf("tagset_id = %v, want a non-zero appearance id", env.Items[0]["tagset_id"])
	}
	if env.Items[0]["deleted_at"] == nil || env.Items[0]["deleted_at"].(float64) == 0 {
		t.Error("deleted_at not set in trash list response")
	}
}

// ---- trashBulk: the unit is the appearance (recording-tagsets P7c) ----------

// jsonReq builds a request carrying a JSON body.
func jsonReq(method, url, body string) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestTrashBulk_RestoreAddressedByTagsetIDs: the explicit-id form names
// appearances. A blob can host several trashed appearances, so the old
// hash-addressed body could not name the row the UI was showing.
func TestTrashBulk_RestoreAddressedByTagsetIDs(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashBulk(rr, jsonReq(http.MethodPost, "/api/admin/trash/bulk",
		`{"action":"restore","tagset_ids":[7,9]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(repo.bulkRestoredTagsets) != 2 ||
		repo.bulkRestoredTagsets[0] != 7 || repo.bulkRestoredTagsets[1] != 9 {
		t.Errorf("restored %v, want [7 9]", repo.bulkRestoredTagsets)
	}
}

// A body carrying the retired `hashes` key names no target set, so it is a 400
// rather than a silent no-op over the whole Trash.
func TestTrashBulk_RejectsRetiredHashesBody(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashBulk(rr, jsonReq(http.MethodPost, "/api/admin/trash/bulk",
		`{"action":"restore","hashes":["`+strings.Repeat("a", 64)+`"]}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a hash-addressed body", rr.Code)
	}
	if len(repo.bulkRestoredTagsets) != 0 {
		t.Errorf("restored %v, want nothing", repo.bulkRestoredTagsets)
	}
}

// Exactly one of {tagset_ids, filter} — supplying both is ambiguous.
func TestTrashBulk_RejectsBothIDsAndFilter(t *testing.T) {
	h := &handler{repo: &fakeRepo{}}
	rr := httptest.NewRecorder()
	h.trashBulk(rr, jsonReq(http.MethodPost, "/api/admin/trash/bulk",
		`{"action":"delete","tagset_ids":[1],"filter":{"q":""}}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Access (license / guest) is a recording property; the Trash lens must refuse
// to edit it on a trashed appearance rather than half-apply the patch.
func TestTrashBulk_EditRejectsAccessPatch(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashBulk(rr, jsonReq(http.MethodPost, "/api/admin/trash/bulk",
		`{"action":"edit","tagset_ids":[3],"patch":{"artist":"X","guest":true}}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(repo.bulkMetaTagsets) != 0 {
		t.Errorf("patched %v, want nothing", repo.bulkMetaTagsets)
	}
}
