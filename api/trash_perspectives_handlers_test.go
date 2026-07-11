package api

// Trash Recordings + Files perspective handler tests (soft-delete.md): DTO
// shaping, id pass-through to the repo, and the outcome→status mapping.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
)

func postJSON(url, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
}

// ── Recordings perspective ──────────────────────────────────────────────────

func TestAdminTrashRecordingsList(t *testing.T) {
	repo := &fakeRepo{
		countTrashedRec: 3,
		trashedRecRows: []database.RecordingRow{{
			ID: 9, Title: "Ghost Take", DisplayArtist: "The Vane",
			TrashedTagsets: 2, Dormant: true, CreatedAt: 1700000000,
		}},
	}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.adminTrashRecordingsList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/trash/recordings?limit=25", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Total int               `json:"total"`
		Items []recordingRowDTO `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 3 || len(out.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 3/1", out.Total, len(out.Items))
	}
	if row := out.Items[0]; row.ID != 9 || row.Artist != "The Vane" || !row.Dormant || row.TrashedAppearances != 2 {
		t.Errorf("row = %+v, mapped fields wrong", row)
	}
}

func TestRecordingsRestore(t *testing.T) {
	repo := &fakeRepo{restoreRecFound: true}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.recordingsRestore(rr, paramRequest(http.MethodPost, "/api/admin/recordings/9/restore", "recordingID", "9", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if repo.restoreRecID != 9 {
		t.Errorf("restore id = %d, want 9", repo.restoreRecID)
	}

	// Unknown recording → 404.
	h2 := &handler{repo: &fakeRepo{restoreRecFound: false}}
	rr = httptest.NewRecorder()
	h2.recordingsRestore(rr, paramRequest(http.MethodPost, "/api/admin/recordings/8/restore", "recordingID", "8", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: status = %d, want 404", rr.Code)
	}

	// Bad id → 400.
	rr = httptest.NewRecorder()
	h.recordingsRestore(rr, paramRequest(http.MethodPost, "/api/admin/recordings/x/restore", "recordingID", "x", ""))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", rr.Code)
	}
}

func TestTrashRecordingsBulk(t *testing.T) {
	// Restore selected.
	repo := &fakeRepo{bulkRestoreRecN: 2}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashRecordingsBulk(rr, postJSON("/api/admin/trash/recordings/bulk", `{"action":"restore","ids":[3,4]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRestoreRecIDs) != 2 || repo.bulkRestoreRecIDs[0] != 3 {
		t.Errorf("restore ids = %v, want [3 4]", repo.bulkRestoreRecIDs)
	}

	// Delete selected reclaims blobs.
	repo = &fakeRepo{bulkHardDelRecN: 2, hardDelOutcome: database.RecordingDeleteOutcome{
		Blobs: []database.DeletedBlob{{Hash: "h1"}, {Hash: "h2"}},
	}}
	h = &handler{repo: repo, storage: storage.NewLocal(t.TempDir())}
	rr = httptest.NewRecorder()
	h.trashRecordingsBulk(rr, postJSON("/api/admin/trash/recordings/bulk", `{"action":"delete","ids":[3,4]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rr.Code)
	}
	if len(repo.bulkHardDelRecIDs) != 2 {
		t.Errorf("delete ids = %v, want 2", repo.bulkHardDelRecIDs)
	}

	// Unknown action → 400; empty ids → 400.
	for _, body := range []string{`{"action":"nope","ids":[1]}`, `{"action":"restore","ids":[]}`} {
		rr = httptest.NewRecorder()
		h.trashRecordingsBulk(rr, postJSON("/api/admin/trash/recordings/bulk", body))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rr.Code)
		}
	}
}

// ── Files perspective ───────────────────────────────────────────────────────

func TestAdminTrashFilesList(t *testing.T) {
	repo := &fakeRepo{
		countRemoved: 2,
		pageRemoved: []*database.FileListEntry{{
			ID: 5, Hash: "abc", Filename: "old.mp3", Title: "Old", Artist: "Band",
			Album: "LP", ByteSize: 1234, ObjectKey: "abc/old.mp3",
			DeletedAt:      sql.NullInt64{Int64: 1700000000, Valid: true},
			StorageBackend: "local", RecordingID: 77,
			DurationSeconds: sql.NullFloat64{Float64: 210.5, Valid: true},
		}},
	}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.adminTrashFilesList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/trash/files?limit=25", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Total int              `json:"total"`
		Items []removedFileDTO `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 2 || len(out.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 2/1", out.Total, len(out.Items))
	}
	it := out.Items[0]
	if it.ID != 5 || it.URL != "/files/abc/old.mp3" || it.RemovedAt != 1700000000 ||
		it.StorageBackend != "local" || it.RecordingID != 77 || it.Duration == nil || *it.Duration != 210.5 {
		t.Errorf("item = %+v, mapped fields wrong", it)
	}
}

func TestRenditionHardDelete(t *testing.T) {
	repo := &fakeRepo{hardDelRemovedFound: true, hardDelRemovedBlobs: []database.DeletedBlob{{Hash: "abc"}}}
	h := &handler{repo: repo, storage: storage.NewLocal(t.TempDir())}
	rr := httptest.NewRecorder()
	h.renditionHardDelete(rr, paramRequest(http.MethodDelete, "/api/admin/renditions/5", "fileID", "5", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if repo.hardDelRemovedFileID != 5 {
		t.Errorf("delete id = %d, want 5", repo.hardDelRemovedFileID)
	}

	// A live/unknown file (found=false) → 404.
	h2 := &handler{repo: &fakeRepo{hardDelRemovedFound: false}, storage: storage.NewLocal(t.TempDir())}
	rr = httptest.NewRecorder()
	h2.renditionHardDelete(rr, paramRequest(http.MethodDelete, "/api/admin/renditions/6", "fileID", "6", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("not-removed: status = %d, want 404", rr.Code)
	}

	// Bad id → 400.
	rr = httptest.NewRecorder()
	h.renditionHardDelete(rr, paramRequest(http.MethodDelete, "/api/admin/renditions/x", "fileID", "x", ""))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", rr.Code)
	}
}

func TestTrashFilesBulk(t *testing.T) {
	// Restore selected.
	repo := &fakeRepo{bulkRestoreRemovedN: 2}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashFilesBulk(rr, postJSON("/api/admin/trash/files/bulk", `{"action":"restore","ids":[5,6]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRestoreRemovedIDs) != 2 || repo.bulkRestoreRemovedIDs[1] != 6 {
		t.Errorf("restore ids = %v, want [5 6]", repo.bulkRestoreRemovedIDs)
	}

	// Delete selected reclaims blobs.
	repo = &fakeRepo{bulkHardDelRemovedN: 2, hardDelRemovedBlobs: []database.DeletedBlob{{Hash: "h1"}}}
	h = &handler{repo: repo, storage: storage.NewLocal(t.TempDir())}
	rr = httptest.NewRecorder()
	h.trashFilesBulk(rr, postJSON("/api/admin/trash/files/bulk", `{"action":"delete","ids":[5,6]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rr.Code)
	}
	if len(repo.bulkHardDelRemovedIDs) != 2 {
		t.Errorf("delete ids = %v, want 2", repo.bulkHardDelRemovedIDs)
	}

	// all:true restores the whole bin ("Select all N") — ids resolved server-side.
	repo = &fakeRepo{removedFilterIDs: []int64{7, 8, 9}}
	h = &handler{repo: repo}
	rr = httptest.NewRecorder()
	h.trashFilesBulk(rr, postJSON("/api/admin/trash/files/bulk", `{"action":"restore","all":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("all restore status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRestoreRemovedIDs) != 3 {
		t.Errorf("all restore ids = %v, want the 3 removed ids", repo.bulkRestoreRemovedIDs)
	}
}

func TestTrashRecordingsBulkAll(t *testing.T) {
	repo := &fakeRepo{trashedRecordingIDs: []int64{4, 5}}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trashRecordingsBulk(rr, postJSON("/api/admin/trash/recordings/bulk", `{"action":"restore","all":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("all restore status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRestoreRecIDs) != 2 {
		t.Errorf("all restore ids = %v, want the 2 trashed recording ids", repo.bulkRestoreRecIDs)
	}
}
