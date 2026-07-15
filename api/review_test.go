package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
)

// URL builders for the tagset-addressed review endpoints (recording-tagsets P4).
func muMeta(base string, tid int64) string {
	return base + "/api/my/uploads/" + strconv.FormatInt(tid, 10) + "/metadata"
}
func muItem(base string, tid int64) string {
	return base + "/api/my/uploads/" + strconv.FormatInt(tid, 10)
}
func modAction(base string, tid int64, action string) string {
	return base + "/api/admin/moderation/" + strconv.FormatInt(tid, 10) + "/" + action
}

// mineRow / queueRow are the staging-list fields the tests assert on; the paged
// endpoints now return a {total, selectable_total, items} envelope.
type mineRow struct {
	Hash  string `json:"hash"`
	State string `json:"state"`
	Note  string `json:"note"`
}
type queueRow struct {
	Hash     string `json:"hash"`
	State    string `json:"state"`
	Uploader string `json:"uploader"`
}

// getStaged GETs a paged staging/trash list and returns its `items` decoded as
// []T (the envelope's total/selectable_total are ignored here).
func getStaged[T any](t *testing.T, c *http.Client, url string) []T {
	t.Helper()
	var env struct {
		Items []T `json:"items"`
	}
	if code := doJSON(t, c, http.MethodGet, url, nil, &env); code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, code)
	}
	return env.Items
}

// trashLiveAppearance trashes the live appearance offered from `hash` through
// the tagset-addressed bulk endpoint — the GC-model replacement for the
// removed DELETE /api/admin/files/{hash}. Pre-approval ids come from the
// owner's My-uploads listing, approved ones from the All Appearances lens.
func trashLiveAppearance(t *testing.T, admin *http.Client, base string, tagsetID int64) {
	t.Helper()
	if code := doJSON(t, admin, http.MethodPost, base+"/api/admin/appearances/bulk",
		map[string]any{"action": "trash", "tagset_ids": []int64{tagsetID}}, nil); code != http.StatusOK {
		t.Fatalf("trash appearance %d = %d, want 200", tagsetID, code)
	}
}

// liveAppearanceID resolves an approved blob's appearance id from the All
// Appearances lens (admin-visible, keyed tagset_id).
func liveAppearanceID(t *testing.T, admin *http.Client, base, hash string) int64 {
	t.Helper()
	var env struct {
		Items []struct {
			TagsetID int64  `json:"tagset_id"`
			Hash     string `json:"hash"`
		} `json:"items"`
	}
	if code := doJSON(t, admin, http.MethodGet, base+"/api/admin/appearances?limit=1000", nil, &env); code != http.StatusOK {
		t.Fatalf("appearances for id lookup = %d, want 200", code)
	}
	for _, it := range env.Items {
		if it.Hash == hash {
			return it.TagsetID
		}
	}
	t.Fatalf("no live appearance for hash %s", hash)
	return 0
}

// trashedAppearanceID finds the trashed appearance offered from `hash` in the
// Trash Appearances lens — the lens is where an appearance's id is published.
func trashedAppearanceID(t *testing.T, c *http.Client, base, hash string) int64 {
	t.Helper()
	type row struct {
		TagsetID int64  `json:"tagset_id"`
		Hash     string `json:"hash"`
	}
	for _, r := range getStaged[row](t, c, base+"/api/admin/trash") {
		if r.Hash == hash {
			return r.TagsetID
		}
	}
	t.Fatalf("no trashed appearance for hash %s", hash)
	return 0
}

// uploadStaged uploads as the given client and returns the staged file's hash
// and blob path, asserting the response reports the pending (draft) state.
func uploadStaged(t *testing.T, client *http.Client, base, name string) (hash, path string) {
	t.Helper()
	resp := uploadViaClient(t, client, base, name)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Hash     string `json:"hash"`
		Filename string `json:"filename"`
		Pending  bool   `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if !body.Pending {
		t.Error("upload response pending = false, want true (authed uploads stage as drafts)")
	}
	return body.Hash, "/files/" + body.Hash + "/" + body.Filename
}

// stagedTagsetID resolves a staged blob's appearance id by reading the caller's
// My-uploads listing (which carries tagset_id). The upload/submit/approve flows
// address the appearance, not the blob (recording-tagsets P4). The lookup must
// use the owner's client (My-uploads is owner-scoped).
func stagedTagsetID(t *testing.T, c *http.Client, base, hash string) int64 {
	t.Helper()
	var env struct {
		Items []struct {
			TagsetID int64  `json:"tagset_id"`
			Hash     string `json:"hash"`
		} `json:"items"`
	}
	if code := doJSON(t, c, http.MethodGet, base+"/api/my/uploads?limit=1000", nil, &env); code != http.StatusOK {
		t.Fatalf("my uploads for tagset lookup = %d, want 200", code)
	}
	for _, it := range env.Items {
		if it.Hash == hash {
			return it.TagsetID
		}
	}
	t.Fatalf("no staged appearance for hash %s", hash)
	return 0
}

func TestReview_UploaderModeratorFlow(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)
	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")

	hash, path := uploadStaged(t, up, srv.URL, "draft.mp3")

	// Staged: invisible in the library, even for the admin.
	files := getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 0 {
		t.Errorf("admin /api/files = %d, want 0 (draft hidden)", len(files))
	}
	// The precheck reports it as pending.
	var check struct {
		Status string `json:"status"`
	}
	doJSON(t, up, http.MethodPost, srv.URL+"/api/files/check", map[string]any{"hash": hash}, &check)
	if check.Status != "pending" {
		t.Errorf("check status = %q, want pending", check.Status)
	}
	// Blob gate: listener (content.access only) 404s; uploader and moderator
	// permissions pass (deliberately not owner-scoped — see auth.md).
	for client, want := range map[*http.Client]int{
		lis: http.StatusNotFound, up: http.StatusOK, admin: http.StatusOK,
	} {
		if code := doJSON(t, client, http.MethodGet, srv.URL+path, nil, nil); code != want {
			t.Errorf("staged blob GET = %d, want %d", code, want)
		}
	}

	// My-uploads listing shows the draft; the owner can edit it.
	mine := getStaged[mineRow](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 1 || mine[0].State != "draft" {
		t.Fatalf("my uploads = %+v, want one draft", mine)
	}
	tid := stagedTagsetID(t, up, srv.URL, hash)
	if code := doJSON(t, up, http.MethodPatch, muMeta(srv.URL, tid),
		map[string]any{"title": "Fixed Title"}, nil); code != http.StatusOK {
		t.Errorf("owner edit of draft = %d, want 200", code)
	}
	// The owner can GET the full editable tag set (for the modal to prefill),
	// including the extended fields, and a rich PATCH round-trips.
	var draftMeta map[string]any
	if code := doJSON(t, up, http.MethodGet, muMeta(srv.URL, tid), nil, &draftMeta); code != http.StatusOK {
		t.Errorf("owner GET draft metadata = %d, want 200", code)
	}
	if _, ok := draftMeta["track_number"]; !ok {
		t.Errorf("owner GET metadata missing track_number: %+v", draftMeta)
	}
	if code := doJSON(t, up, http.MethodPatch, muMeta(srv.URL, tid),
		map[string]any{"track_number": "7", "genre": "Jazz"}, nil); code != http.StatusOK {
		t.Errorf("owner rich edit of draft = %d, want 200", code)
	}
	// The moderation queue is not for uploaders.
	if code := doJSON(t, up, http.MethodGet, srv.URL+"/api/admin/moderation", nil, nil); code != http.StatusForbidden {
		t.Errorf("uploader moderation list = %d, want 403", code)
	}

	// Submit: plain uploader lands in the queue, not the library, and the
	// file locks for him.
	var sub struct {
		Approved  bool `json:"approved"`
		Submitted int  `json:"submitted"`
	}
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, &sub)
	if sub.Approved || sub.Submitted != 1 {
		t.Fatalf("submit: approved=%v submitted=%d, want queued 1", sub.Approved, sub.Submitted)
	}
	if code := doJSON(t, up, http.MethodPatch, muMeta(srv.URL, tid),
		map[string]any{"title": "Too Late"}, nil); code != http.StatusNotFound {
		t.Errorf("owner edit after submit = %d, want 404 (locked)", code)
	}
	if code := doJSON(t, up, http.MethodGet, muMeta(srv.URL, tid), nil, nil); code != http.StatusNotFound {
		t.Errorf("owner GET metadata after submit = %d, want 404 (locked)", code)
	}

	// Moderator sees it, returns it with a note.
	queue := getStaged[queueRow](t, admin, srv.URL+"/api/admin/moderation")
	if len(queue) != 1 || queue[0].State != "submitted" || queue[0].Uploader != "up" {
		t.Fatalf("moderation queue = %+v, want up's submitted file", queue)
	}
	if code := doJSON(t, admin, http.MethodPost, modAction(srv.URL, tid, "return"),
		map[string]any{"note": "fix the artist tag"}, nil); code != http.StatusOK {
		t.Fatalf("return = %d, want 200", code)
	}
	mine = getStaged[mineRow](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 1 || mine[0].State != "returned" || mine[0].Note != "fix the artist tag" {
		t.Fatalf("my uploads after return = %+v, want returned with the note", mine)
	}
	// Returned is editable again; resubmit re-queues.
	if code := doJSON(t, up, http.MethodPatch, muMeta(srv.URL, tid),
		map[string]any{"artist": "Right Artist"}, nil); code != http.StatusOK {
		t.Errorf("owner edit of returned file = %d, want 200", code)
	}
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, &sub)
	if sub.Submitted != 1 {
		t.Fatalf("resubmit failed: %+v", sub)
	}

	// Approve publishes: library lists it, the listener can stream it, the
	// staging list is empty.
	if code := doJSON(t, admin, http.MethodPost, modAction(srv.URL, tid, "approve"), nil, nil); code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", code)
	}
	files = getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 1 {
		t.Errorf("admin /api/files after approve = %d, want 1", len(files))
	}
	if code := doJSON(t, lis, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusOK {
		t.Errorf("listener blob GET after approve = %d, want 200", code)
	}
	mine = getStaged[mineRow](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 0 {
		t.Errorf("my uploads after approve = %d, want 0", len(mine))
	}
	// Nothing left to moderate.
	queue = getStaged[queueRow](t, admin, srv.URL+"/api/admin/moderation")
	if len(queue) != 0 {
		t.Errorf("moderation queue after approve = %d, want 0", len(queue))
	}
}

func TestReview_ModeratorSelfApproves(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	// Migration 017 gives the built-in moderator role file.upload +
	// content.moderate: moderators are the trusted uploaders.
	makeUser(t, db, "mod", "moderator-pass-1", auth.RoleModerator)
	mod := clientFor(t, srv.URL, "mod", "moderator-pass-1")

	hash, _ := uploadStaged(t, mod, srv.URL, "trusted.mp3")
	tid := stagedTagsetID(t, mod, srv.URL, hash)

	var sub struct {
		Approved  bool `json:"approved"`
		Submitted int  `json:"submitted"`
	}
	doJSON(t, mod, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, &sub)
	if !sub.Approved || sub.Submitted != 1 {
		t.Fatalf("moderator submit: approved=%v submitted=%d, want self-approve", sub.Approved, sub.Submitted)
	}
	files := getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 1 {
		t.Errorf("library after self-approve = %d files, want 1", len(files))
	}
}

func TestReview_DiscardToTrashAndBack(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "discard.mp3")
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{stagedTagsetID(t, up, srv.URL, hash)}}, nil)

	// Discard = the existing soft delete (tagset-addressed); the file leaves
	// the queue.
	trashLiveAppearance(t, admin, srv.URL, stagedTagsetID(t, up, srv.URL, hash))
	queue := getStaged[map[string]any](t, admin, srv.URL+"/api/admin/moderation")
	if len(queue) != 0 {
		t.Errorf("queue after discard = %d, want 0", len(queue))
	}
	// Trash restore re-enters the queue (state survived), not the library. The
	// Trash Appearances lens is tagset-addressed (recording-tagsets P7c), so the
	// restore names the appearance, not the blob.
	tid := trashedAppearanceID(t, admin, srv.URL, hash)
	if code := doJSON(t, admin, http.MethodPost,
		srv.URL+"/api/admin/tagsets/"+strconv.FormatInt(tid, 10)+"/restore", nil, nil); code != http.StatusOK {
		t.Fatalf("trash restore = %d, want 200", code)
	}
	queue = getStaged[map[string]any](t, admin, srv.URL+"/api/admin/moderation")
	if len(queue) != 1 {
		t.Errorf("queue after restore = %d, want 1 (submitted state survives trash)", len(queue))
	}
	files := getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 0 {
		t.Errorf("library after restore = %d, want 0 (still awaiting review)", len(files))
	}
}

// approveViaQueue pushes an uploaded draft through submit + moderator approve.
func approveViaQueue(t *testing.T, up, admin *http.Client, base, hash string) {
	t.Helper()
	tid := stagedTagsetID(t, up, base, hash)
	doJSON(t, up, http.MethodPost, base+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil)
	if code := doJSON(t, admin, http.MethodPost, modAction(base, tid, "approve"), nil, nil); code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", code)
	}
}

// A re-upload of trashed bytes restores the file (reupload_restores policy) but
// must not silently republish an approved file: with moderation configured it
// re-enters the restorer's staging area as a draft. Anything else lets any
// file.upload holder bypass the review queue by re-sending trashed bytes.
func TestReview_ReuploadOfTrashedFileRestages(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	makeUser(t, db, "up2", "uploader-pass-2", auth.RoleUploader)
	up2 := clientFor(t, srv.URL, "up2", "uploader-pass-2")

	hash, _ := uploadStaged(t, up, srv.URL, "song.mp3")
	approveViaQueue(t, up, admin, srv.URL, hash)
	trashLiveAppearance(t, admin, srv.URL, liveAppearanceID(t, admin, srv.URL, hash))

	// Re-upload by another uploader: restored, but staged — not in the library.
	resp := uploadViaClient(t, up2, srv.URL, "song-again.mp3")
	defer resp.Body.Close()
	var body struct {
		Existed  bool `json:"existed"`
		Restored bool `json:"restored"`
		Pending  bool `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode re-upload response: %v", err)
	}
	if !body.Existed || !body.Restored || !body.Pending {
		t.Errorf("re-upload = %+v, want existed+restored+pending", body)
	}

	files := getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 0 {
		t.Errorf("library after re-upload restore = %d files, want 0 (re-staged, not republished)", len(files))
	}
	// The file lands in the *restorer's* staging area as a draft.
	mine := getStaged[mineRow](t, up2, srv.URL+"/api/my/uploads")
	if len(mine) != 1 || mine[0].Hash != hash || mine[0].State != "draft" {
		t.Errorf("restorer's my uploads = %+v, want the restored draft", mine)
	}
	mine = getStaged[mineRow](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 0 {
		t.Errorf("original uploader's my uploads = %d, want 0 (ownership moved to restorer)", len(mine))
	}
}

// The explicit uploader-restore endpoint (uploader_restore policy) follows the
// same rule: restoring an approved file re-stages it as the restorer's draft.
func TestReview_UploaderRestoreRestages(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "song.mp3")
	approveViaQueue(t, up, admin, srv.URL, hash)
	trashLiveAppearance(t, admin, srv.URL, liveAppearanceID(t, admin, srv.URL, hash))
	if err := db.SetTrashRestorePolicy(t.Context(), database.TrashUploaderRestore); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	var res struct {
		OK     bool `json:"ok"`
		Staged bool `json:"staged"`
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/files/"+hash+"/restore", nil, &res); code != http.StatusOK {
		t.Fatalf("uploader restore = %d, want 200", code)
	}
	if !res.OK || !res.Staged {
		t.Errorf("uploader restore = %+v, want ok+staged", res)
	}
	files := getFileItems(t, admin, srv.URL+"/api/files")
	if len(files) != 0 {
		t.Errorf("library after uploader restore = %d files, want 0 (re-staged)", len(files))
	}
	mine := getStaged[mineRow](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 1 || mine[0].State != "draft" {
		t.Errorf("my uploads after uploader restore = %+v, want one draft", mine)
	}
}

// The owner may remove his own draft/returned files from the staging area
// (DELETE /api/my/uploads/{hash} → Trash) — but not submitted ones (no
// withdraw) and never another user's.
func TestReview_OwnerRemovesStagedFile(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	makeUser(t, db, "up2", "uploader-pass-2", auth.RoleUploader)
	up2 := clientFor(t, srv.URL, "up2", "uploader-pass-2")

	hash, _ := uploadStaged(t, up, srv.URL, "song.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)

	// Another uploader cannot remove it.
	if code := doJSON(t, up2, http.MethodDelete, muItem(srv.URL, tid), nil, nil); code != http.StatusNotFound {
		t.Errorf("foreign remove = %d, want 404", code)
	}
	// The owner removes the draft → it leaves staging and lands in Trash.
	if code := doJSON(t, up, http.MethodDelete, muItem(srv.URL, tid), nil, nil); code != http.StatusOK {
		t.Fatalf("owner remove = %d, want 200", code)
	}
	mine := getStaged[map[string]any](t, up, srv.URL+"/api/my/uploads")
	if len(mine) != 0 {
		t.Errorf("my uploads after remove = %d, want 0", len(mine))
	}
	trash := getStaged[map[string]any](t, admin, srv.URL+"/api/admin/trash")
	if len(trash) != 1 {
		t.Errorf("trash after owner remove = %d, want 1", len(trash))
	}

	// A submitted file cannot be removed (no withdraw). Distinct bytes — the
	// shared helper's fixed content would dedupe against the file just trashed.
	req := buildUploadRequest(t, "file", "song2.mp3", "audio/mpeg", []byte("different bytes for the second staged file"))
	resp, err := up.Post(srv.URL+"/files/upload", req.Header.Get("Content-Type"), req.Body)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("second upload: err=%v code=%v", err, resp.StatusCode)
	}
	var body2 struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body2); err != nil {
		t.Fatalf("decode second upload: %v", err)
	}
	resp.Body.Close()
	tid2 := stagedTagsetID(t, up, srv.URL, body2.Hash)
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid2}}, nil)
	if code := doJSON(t, up, http.MethodDelete, muItem(srv.URL, tid2), nil, nil); code != http.StatusNotFound {
		t.Errorf("remove of submitted file = %d, want 404 (no withdraw)", code)
	}
}

// A file trashed while *pending* keeps its state and owner across a
// restore-via-reupload — it re-enters the queue where it was, no re-staging.
func TestReview_ReuploadOfTrashedPendingFileKeepsState(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "song.mp3")
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{stagedTagsetID(t, up, srv.URL, hash)}}, nil)
	// Discard the submission, then re-upload the bytes.
	trashLiveAppearance(t, admin, srv.URL, stagedTagsetID(t, up, srv.URL, hash))
	uploadViaClient(t, up, srv.URL, "song.mp3").Body.Close()

	queue := getStaged[queueRow](t, admin, srv.URL+"/api/admin/moderation")
	if len(queue) != 1 || queue[0].State != "submitted" || queue[0].Uploader != "up" {
		t.Errorf("queue after pending-file re-upload = %+v, want up's submitted file back", queue)
	}
}
