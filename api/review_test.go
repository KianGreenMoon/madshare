package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

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

func TestReview_UploaderModeratorFlow(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)
	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")

	hash, path := uploadStaged(t, up, srv.URL, "draft.mp3")

	// Staged: invisible in the library, even for the admin.
	var files []map[string]any
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/files", nil, &files)
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
	var mine []struct {
		Hash  string `json:"hash"`
		State string `json:"state"`
		Note  string `json:"note"`
	}
	doJSON(t, up, http.MethodGet, srv.URL+"/api/my/uploads", nil, &mine)
	if len(mine) != 1 || mine[0].State != "draft" {
		t.Fatalf("my uploads = %+v, want one draft", mine)
	}
	if code := doJSON(t, up, http.MethodPatch, srv.URL+"/api/my/uploads/"+hash+"/metadata",
		map[string]any{"title": "Fixed Title"}, nil); code != http.StatusOK {
		t.Errorf("owner edit of draft = %d, want 200", code)
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
		map[string]any{"hashes": []string{hash}}, &sub)
	if sub.Approved || sub.Submitted != 1 {
		t.Fatalf("submit: approved=%v submitted=%d, want queued 1", sub.Approved, sub.Submitted)
	}
	if code := doJSON(t, up, http.MethodPatch, srv.URL+"/api/my/uploads/"+hash+"/metadata",
		map[string]any{"title": "Too Late"}, nil); code != http.StatusNotFound {
		t.Errorf("owner edit after submit = %d, want 404 (locked)", code)
	}

	// Moderator sees it, returns it with a note.
	var queue []struct {
		Hash     string `json:"hash"`
		State    string `json:"state"`
		Uploader string `json:"uploader"`
	}
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/moderation", nil, &queue)
	if len(queue) != 1 || queue[0].State != "submitted" || queue[0].Uploader != "up" {
		t.Fatalf("moderation queue = %+v, want up's submitted file", queue)
	}
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/"+hash+"/return",
		map[string]any{"note": "fix the artist tag"}, nil); code != http.StatusOK {
		t.Fatalf("return = %d, want 200", code)
	}
	doJSON(t, up, http.MethodGet, srv.URL+"/api/my/uploads", nil, &mine)
	if len(mine) != 1 || mine[0].State != "returned" || mine[0].Note != "fix the artist tag" {
		t.Fatalf("my uploads after return = %+v, want returned with the note", mine)
	}
	// Returned is editable again; resubmit re-queues.
	if code := doJSON(t, up, http.MethodPatch, srv.URL+"/api/my/uploads/"+hash+"/metadata",
		map[string]any{"artist": "Right Artist"}, nil); code != http.StatusOK {
		t.Errorf("owner edit of returned file = %d, want 200", code)
	}
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"hashes": []string{hash}}, &sub)
	if sub.Submitted != 1 {
		t.Fatalf("resubmit failed: %+v", sub)
	}

	// Approve publishes: library lists it, the listener can stream it, the
	// staging list is empty.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/"+hash+"/approve", nil, nil); code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", code)
	}
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/files", nil, &files)
	if len(files) != 1 {
		t.Errorf("admin /api/files after approve = %d, want 1", len(files))
	}
	if code := doJSON(t, lis, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusOK {
		t.Errorf("listener blob GET after approve = %d, want 200", code)
	}
	doJSON(t, up, http.MethodGet, srv.URL+"/api/my/uploads", nil, &mine)
	if len(mine) != 0 {
		t.Errorf("my uploads after approve = %d, want 0", len(mine))
	}
	// Nothing left to moderate.
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/moderation", nil, &queue)
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

	var sub struct {
		Approved  bool `json:"approved"`
		Submitted int  `json:"submitted"`
	}
	doJSON(t, mod, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"hashes": []string{hash}}, &sub)
	if !sub.Approved || sub.Submitted != 1 {
		t.Fatalf("moderator submit: approved=%v submitted=%d, want self-approve", sub.Approved, sub.Submitted)
	}
	var files []map[string]any
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/files", nil, &files)
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
		map[string]any{"hashes": []string{hash}}, nil)

	// Discard = the existing soft delete; the file leaves the queue.
	if code := doJSON(t, admin, http.MethodDelete, srv.URL+"/api/admin/files/"+hash, nil, nil); code != http.StatusOK {
		t.Fatalf("discard (soft delete) = %d, want 200", code)
	}
	var queue []map[string]any
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/moderation", nil, &queue)
	if len(queue) != 0 {
		t.Errorf("queue after discard = %d, want 0", len(queue))
	}
	// Trash restore re-enters the queue (state survived), not the library.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/trash/"+hash+"/restore", nil, nil); code != http.StatusOK {
		t.Fatalf("trash restore = %d, want 200", code)
	}
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/moderation", nil, &queue)
	if len(queue) != 1 {
		t.Errorf("queue after restore = %d, want 1 (submitted state survives trash)", len(queue))
	}
	var files []map[string]any
	doJSON(t, admin, http.MethodGet, srv.URL+"/api/files", nil, &files)
	if len(files) != 0 {
		t.Errorf("library after restore = %d, want 0 (still awaiting review)", len(files))
	}
}
