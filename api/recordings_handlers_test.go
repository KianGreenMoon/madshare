package api

// /api/admin/recordings handler tests (recording-tagsets P5) — DTO shaping,
// option pass-through, and the outcome→status mapping over the fakeRepo.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/api/storage"
	"github.com/go-chi/chi/v5"
)

// paramRequest builds a request with one chi route param set and an optional
// JSON body.
func paramRequest(method, url, key, val, body string) *http.Request {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, rd)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestRecordingsList_OptionsAndDTO(t *testing.T) {
	repo := &fakeRepo{recordingRows: []database.RecordingRow{{
		ID: 42, Title: "Neon Rain", DisplayArtist: "Kessler Falls",
		LiveRenditions: 2, RemovedFiles: 1, Appearances: 2, TrashedTagsets: 1,
		BestFormat: "flac", Pinned: true, License: "CC-BY-4.0", GuestPlayable: true,
	}}}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.recordingsList(rr, httptest.NewRequest(http.MethodGet,
		"/api/admin/recordings?q=neon&filter=multi_rendition&limit=25&offset=50", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if o := repo.lastListOpts; o.Search != "neon" || o.Filter != "multi_rendition" || o.Limit != 25 || o.Offset != 50 {
		t.Errorf("options = %+v, want q/filter/limit/offset passed through", o)
	}
	var out struct {
		Total int               `json:"total"`
		Items []recordingRowDTO `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 1/1", out.Total, len(out.Items))
	}
	row := out.Items[0]
	if row.ID != 42 || row.Artist != "Kessler Falls" || row.RemovedFiles != 1 ||
		row.TrashedAppearances != 1 || !row.Pinned || !row.GuestPlayable || row.BestFormat != "flac" {
		t.Errorf("row = %+v, mapped fields wrong", row)
	}
}

func TestRecordingsDetail_RanksLiveOnly(t *testing.T) {
	repo := &fakeRepo{recordingDetail: &database.RecordingDetail{
		ID: 7, License: "CC0", GuestPlayable: true,
		Renditions: []database.RecordingFile{
			{FileID: 1, Hash: "h1", ObjectKey: "h1/a.flac", Codec: "flac", SampleRate: 44100, BitDepth: 16, ByteSize: 30e6},
			{FileID: 2, Hash: "h2", ObjectKey: "h2/a.mp3", Codec: "mp3", Bitrate: 320000, ByteSize: 9e6},
			{FileID: 3, Hash: "h3", ObjectKey: "h3/a.ogg", MimeType: "audio/ogg", Removed: true},
		},
		Appearances: []database.RecordingAppearance{
			{TagsetID: 11, Title: "A", IsPrimary: true, ReviewState: "approved",
				DiscNumber: sql.NullInt64{Int64: 1, Valid: true}},
			{TagsetID: 12, Title: "B", Trashed: true, ReviewState: "approved"},
		},
	}}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.recordingsDetail(rr, paramRequest(http.MethodGet, "/api/admin/recordings/7", "recordingID", "7", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var d recordingDetailDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Renditions) != 3 || len(d.Appearances) != 2 {
		t.Fatalf("arms = %d/%d, want 3/2", len(d.Renditions), len(d.Appearances))
	}
	byID := map[int64]recordingRenditionDTO{}
	for _, f := range d.Renditions {
		byID[f.FileID] = f
	}
	if !byID[1].Best || byID[1].Rank != 1 {
		t.Errorf("flac not best: %+v", byID[1])
	}
	if byID[3].Rank != 0 || byID[3].Best || !byID[3].Removed || byID[3].Format != "audio/ogg" {
		t.Errorf("removed blob should be unranked with MIME format: %+v", byID[3])
	}
	if byID[2].URL != "/files/h2/a.mp3" {
		t.Errorf("play url = %q", byID[2].URL)
	}
	if d.Appearances[0].Disc == nil || *d.Appearances[0].Disc != 1 || d.Appearances[1].Disc != nil {
		t.Errorf("nullable disc mapping wrong: %+v", d.Appearances)
	}
	if !d.Appearances[1].Trashed {
		t.Errorf("trashed appearance flag lost")
	}

	// Unknown recording → 404.
	h2 := &handler{repo: &fakeRepo{}}
	rr = httptest.NewRecorder()
	h2.recordingsDetail(rr, paramRequest(http.MethodGet, "/api/admin/recordings/9", "recordingID", "9", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: status = %d, want 404", rr.Code)
	}
}

func TestRecordingsMerge(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.recordingsMerge(rr, httptest.NewRequest(http.MethodPost, "/api/admin/recordings/merge",
		strings.NewReader(`{"target_id":10,"source_ids":[11,12]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body)
	}
	if repo.mergeTargetID != 10 || len(repo.mergeSourceIDs) != 2 {
		t.Errorf("merge args = %d %v", repo.mergeTargetID, repo.mergeSourceIDs)
	}

	// Stale selection → 404; bad body → 400.
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{mergeNotFound: true}}).recordingsMerge(rr,
		httptest.NewRequest(http.MethodPost, "/api/admin/recordings/merge",
			strings.NewReader(`{"target_id":10,"source_ids":[11]}`)))
	if rr.Code != http.StatusNotFound {
		t.Errorf("stale: status = %d, want 404", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.recordingsMerge(rr, httptest.NewRequest(http.MethodPost, "/api/admin/recordings/merge",
		strings.NewReader(`{"target_id":10}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty sources: status = %d, want 400", rr.Code)
	}
}

func TestTagsetMove_OutcomeStatuses(t *testing.T) {
	cases := map[string]struct {
		outcome database.MoveTagsetOutcome
		want    int
	}{
		"moved":     {database.MoveTagsetOutcome{Found: true, Moved: true}, http.StatusOK},
		"not found": {database.MoveTagsetOutcome{}, http.StatusNotFound},
		"same":      {database.MoveTagsetOutcome{Found: true, SameRecording: true}, http.StatusBadRequest},
		"last":      {database.MoveTagsetOutcome{Found: true, LastAppearance: true}, http.StatusConflict},
		"collides":  {database.MoveTagsetOutcome{Found: true, Collides: true}, http.StatusConflict},
	}
	for name, tc := range cases {
		repo := &fakeRepo{moveOutcome: tc.outcome}
		rr := httptest.NewRecorder()
		(&handler{repo: repo}).tagsetMove(rr, paramRequest(http.MethodPost,
			"/api/admin/tagsets/3/move", "tagsetID", "3", `{"target_recording_id":9}`))
		if rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", name, rr.Code, tc.want)
		}
		if repo.moveTagsetID != 3 || repo.moveTargetID != 9 {
			t.Errorf("%s: args = %d→%d, want 3→9", name, repo.moveTagsetID, repo.moveTargetID)
		}
	}
}

func TestRecordingsSetPrimary(t *testing.T) {
	repo := &fakeRepo{}
	rr := httptest.NewRecorder()
	(&handler{repo: repo}).recordingsSetPrimary(rr, paramRequest(http.MethodPost,
		"/api/admin/recordings/5/primary", "recordingID", "5", `{"tagset_id":33}`))
	if rr.Code != http.StatusOK || repo.primaryRecordingID != 5 || repo.primaryTagsetID != 33 {
		t.Errorf("status=%d args=%d/%d", rr.Code, repo.primaryRecordingID, repo.primaryTagsetID)
	}
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{primaryNotFound: true}}).recordingsSetPrimary(rr, paramRequest(
		http.MethodPost, "/api/admin/recordings/5/primary", "recordingID", "5", `{"tagset_id":33}`))
	if rr.Code != http.StatusNotFound {
		t.Errorf("foreign tagset: status = %d, want 404", rr.Code)
	}
}

func TestRecordingsAccess(t *testing.T) {
	repo := &fakeRepo{}
	rr := httptest.NewRecorder()
	(&handler{repo: repo}).recordingsAccess(rr, paramRequest(http.MethodPatch,
		"/api/admin/recordings/5/access", "recordingID", "5",
		`{"license":"CC-BY-4.0","guest_playable":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body)
	}
	if repo.accessRecordingID != 5 || repo.accessLicense == nil || *repo.accessLicense != "CC-BY-4.0" ||
		repo.accessGuest == nil || !*repo.accessGuest {
		t.Errorf("access args not passed through")
	}

	// Unknown license → 400; empty body → 400; guest-only leaves license nil.
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{}}).recordingsAccess(rr, paramRequest(http.MethodPatch,
		"/api/admin/recordings/5/access", "recordingID", "5", `{"license":"WTFPL"}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown license: status = %d, want 400", rr.Code)
	}
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{}}).recordingsAccess(rr, paramRequest(http.MethodPatch,
		"/api/admin/recordings/5/access", "recordingID", "5", `{}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty patch: status = %d, want 400", rr.Code)
	}
	repo2 := &fakeRepo{}
	rr = httptest.NewRecorder()
	(&handler{repo: repo2}).recordingsAccess(rr, paramRequest(http.MethodPatch,
		"/api/admin/recordings/5/access", "recordingID", "5", `{"guest_playable":false}`))
	if rr.Code != http.StatusOK || repo2.accessLicense != nil || repo2.accessGuest == nil {
		t.Errorf("guest-only patch: status=%d license=%v", rr.Code, repo2.accessLicense)
	}
}

func TestRecordingsTrashAndBulk(t *testing.T) {
	repo := &fakeRepo{}
	rr := httptest.NewRecorder()
	(&handler{repo: repo}).recordingsTrash(rr, paramRequest(http.MethodPost,
		"/api/admin/recordings/8/trash", "recordingID", "8", ""))
	if rr.Code != http.StatusOK || len(repo.trashRecordingIDs) != 1 || repo.trashRecordingIDs[0] != 8 {
		t.Errorf("trash: status=%d ids=%v", rr.Code, repo.trashRecordingIDs)
	}
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{trashRecNotFound: true}}).recordingsTrash(rr, paramRequest(
		http.MethodPost, "/api/admin/recordings/8/trash", "recordingID", "8", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: status = %d, want 404", rr.Code)
	}

	repo = &fakeRepo{}
	rr = httptest.NewRecorder()
	(&handler{repo: repo}).recordingsTrashBulk(rr, httptest.NewRequest(http.MethodPost,
		"/api/admin/recordings/trash", strings.NewReader(`{"recording_ids":[1,2,3]}`)))
	if rr.Code != http.StatusOK || len(repo.trashRecordingIDs) != 3 {
		t.Errorf("bulk: status=%d ids=%v", rr.Code, repo.trashRecordingIDs)
	}
}

func TestRecordingsHardDelete(t *testing.T) {
	repo := &fakeRepo{hardDelOutcome: database.RecordingDeleteOutcome{
		Found: true, Appearances: 2, Files: 3,
		Blobs: []database.DeletedBlob{{Hash: "h1"}, {Hash: "h2"}, {Hash: "h3"}},
	}}
	h := &handler{repo: repo, storage: storage.NewLocal(t.TempDir())}
	rr := httptest.NewRecorder()
	h.recordingsHardDelete(rr, paramRequest(http.MethodDelete,
		"/api/admin/recordings/4", "recordingID", "4", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body)
	}
	if repo.hardDelRecordingID != 4 {
		t.Errorf("recording id = %d, want 4", repo.hardDelRecordingID)
	}
	var out struct {
		Appearances int `json:"appearances"`
		Files       int `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Appearances != 2 || out.Files != 3 {
		t.Errorf("counts = %+v (err %v)", out, err)
	}

	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{}}).recordingsHardDelete(rr, paramRequest(http.MethodDelete,
		"/api/admin/recordings/4", "recordingID", "4", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: status = %d, want 404", rr.Code)
	}
}

func TestRenditionRemoveRestore(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.renditionRemove(rr, paramRequest(http.MethodPost,
		"/api/admin/renditions/6/remove", "fileID", "6", ""))
	if rr.Code != http.StatusOK || repo.removeRendID != 6 {
		t.Errorf("remove: status=%d id=%d", rr.Code, repo.removeRendID)
	}
	rr = httptest.NewRecorder()
	h.renditionRestore(rr, paramRequest(http.MethodPost,
		"/api/admin/renditions/6/restore", "fileID", "6", ""))
	if rr.Code != http.StatusOK || repo.restoreRendID != 6 {
		t.Errorf("restore: status=%d id=%d", rr.Code, repo.restoreRendID)
	}
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{rendNotFound: true}}).renditionRemove(rr, paramRequest(
		http.MethodPost, "/api/admin/renditions/6/remove", "fileID", "6", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown rendition: status = %d, want 404", rr.Code)
	}
}

func TestRenditionsBulk(t *testing.T) {
	// Remove selected — the Files lens's bulk soft-remove.
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.renditionsBulk(rr, postJSON("/api/admin/renditions/bulk", `{"action":"remove","ids":[5,6]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("remove status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRemoveRendIDs) != 2 || repo.bulkRemoveRendIDs[1] != 6 {
		t.Errorf("remove ids = %v, want [5 6]", repo.bulkRemoveRendIDs)
	}

	// Only "remove" is a known action here.
	rr = httptest.NewRecorder()
	h.renditionsBulk(rr, postJSON("/api/admin/renditions/bulk", `{"action":"restore","ids":[5]}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("restore action: status = %d, want 400", rr.Code)
	}

	// Empty id list → 400.
	rr = httptest.NewRecorder()
	h.renditionsBulk(rr, postJSON("/api/admin/renditions/bulk", `{"action":"remove","ids":[]}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty ids: status = %d, want 400", rr.Code)
	}

	// all:true resolves every live blob server-side ("Select all N").
	repo = &fakeRepo{liveFileIDs: []int64{1, 2, 3}}
	h = &handler{repo: repo}
	rr = httptest.NewRecorder()
	h.renditionsBulk(rr, postJSON("/api/admin/renditions/bulk", `{"action":"remove","all":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("all: status = %d, want 200", rr.Code)
	}
	if len(repo.bulkRemoveRendIDs) != 3 {
		t.Errorf("all: remove ids = %v, want the 3 live ids", repo.bulkRemoveRendIDs)
	}

	// ids and all are mutually exclusive → 400.
	rr = httptest.NewRecorder()
	h.renditionsBulk(rr, postJSON("/api/admin/renditions/bulk", `{"action":"remove","ids":[1],"all":true}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("ids+all: status = %d, want 400", rr.Code)
	}
}

func TestTagsetRestore(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.tagsetRestore(rr, paramRequest(http.MethodPost,
		"/api/admin/tagsets/7/restore", "tagsetID", "7", ""))
	if rr.Code != http.StatusOK || repo.restoreTagsetID != 7 {
		t.Errorf("restore: status=%d id=%d", rr.Code, repo.restoreTagsetID)
	}
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{restoreTagsetNotFound: true}}).tagsetRestore(rr, paramRequest(
		http.MethodPost, "/api/admin/tagsets/7/restore", "tagsetID", "7", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown tagset: status = %d, want 404", rr.Code)
	}
}

func TestTagsetHardDelete(t *testing.T) {
	// A trashed appearance whose delete freed the last blob routes through the
	// storage-aware reclaim (last-tagset cascade).
	repo := &fakeRepo{hardDelTagsetOutcome: database.HardDeleteTagsetOutcome{
		Found: true, Trashed: true,
		Blobs: []database.DeletedBlob{{Hash: "abc"}},
	}}
	h := &handler{repo: repo, storage: storage.NewLocal(t.TempDir())}
	rr := httptest.NewRecorder()
	h.tagsetHardDelete(rr, paramRequest(http.MethodDelete,
		"/api/admin/tagsets/7", "tagsetID", "7", ""))
	if rr.Code != http.StatusOK || repo.hardDelTagsetID != 7 {
		t.Fatalf("delete: status=%d id=%d body=%s", rr.Code, repo.hardDelTagsetID, rr.Body.String())
	}

	// A live appearance is refused (409): permanent delete is Trash-only.
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{hardDelTagsetOutcome: database.HardDeleteTagsetOutcome{Found: true}}}).
		tagsetHardDelete(rr, paramRequest(http.MethodDelete, "/api/admin/tagsets/7", "tagsetID", "7", ""))
	if rr.Code != http.StatusConflict {
		t.Errorf("live appearance: status = %d, want 409", rr.Code)
	}

	// Unknown id → 404.
	rr = httptest.NewRecorder()
	(&handler{repo: &fakeRepo{}}).tagsetHardDelete(rr, paramRequest(
		http.MethodDelete, "/api/admin/tagsets/7", "tagsetID", "7", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown tagset: status = %d, want 404", rr.Code)
	}
}

// TestRecordingsAddAppearance covers the "Add appearance" handler (P7d): the
// happy path passes the input through to CreateAppearance, and each DB refusal
// maps to its status.
func TestRecordingsAddAppearance(t *testing.T) {
	// Numeric fields arrive as strings (the shared track-edit.js form).
	body := `{"title":"Same Song","artist":"The Band","album":"Best Of","track_number":"4"}`

	t.Run("success", func(t *testing.T) {
		repo := &fakeRepo{createAppOutcome: database.CreateAppearanceOutcome{TagsetID: 42}}
		h := &handler{repo: repo}
		rr := httptest.NewRecorder()
		h.recordingsAddAppearance(rr, paramRequest(http.MethodPost,
			"/api/admin/recordings/5/appearances", "recordingID", "5", body))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if repo.createAppRecordingID != 5 {
			t.Errorf("recording id = %d, want 5", repo.createAppRecordingID)
		}
		if repo.createAppInput.Title != "Same Song" || repo.createAppInput.Album != "Best Of" ||
			repo.createAppInput.TrackNumber == nil || *repo.createAppInput.TrackNumber != 4 {
			t.Errorf("input not passed through: %+v", repo.createAppInput)
		}
		var resp map[string]any
		json.NewDecoder(rr.Body).Decode(&resp)
		if resp["tagset_id"].(float64) != 42 {
			t.Errorf("tagset_id = %v, want 42", resp["tagset_id"])
		}
	})

	for _, tc := range []struct {
		name    string
		outcome database.CreateAppearanceOutcome
		status  int
	}{
		{"unknown recording", database.CreateAppearanceOutcome{NotFound: true}, http.StatusNotFound},
		{"empty title", database.CreateAppearanceOutcome{EmptyTitle: true}, http.StatusBadRequest},
		{"nameless", database.CreateAppearanceOutcome{Nameless: true}, http.StatusUnprocessableEntity},
		{"collision", database.CreateAppearanceOutcome{Collides: true}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handler{repo: &fakeRepo{createAppOutcome: tc.outcome}}
			rr := httptest.NewRecorder()
			h.recordingsAddAppearance(rr, paramRequest(http.MethodPost,
				"/api/admin/recordings/5/appearances", "recordingID", "5", body))
			if rr.Code != tc.status {
				t.Errorf("status = %d, want %d; body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}

	t.Run("bad recording id", func(t *testing.T) {
		h := &handler{repo: &fakeRepo{}}
		rr := httptest.NewRecorder()
		h.recordingsAddAppearance(rr, paramRequest(http.MethodPost,
			"/api/admin/recordings/0/appearances", "recordingID", "0", body))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
	})
}

// TestRecordingsAddAppearance_RejectsNonNumeric: a non-numeric year/track is a
// clean 400, not a 500 from the DB layer.
func TestRecordingsAddAppearance_RejectsNonNumeric(t *testing.T) {
	repo := &fakeRepo{createAppOutcome: database.CreateAppearanceOutcome{TagsetID: 1}}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.recordingsAddAppearance(rr, paramRequest(http.MethodPost,
		"/api/admin/recordings/5/appearances", "recordingID", "5",
		`{"title":"T","artist":"A","year":"not-a-year"}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if repo.createAppRecordingID != 0 {
		t.Error("CreateAppearance was called despite an unparseable numeric field")
	}
}
