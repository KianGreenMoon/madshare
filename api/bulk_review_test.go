package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

// stageBytes uploads distinct bytes as the given client and returns the staged
// hash. Distinct content avoids the content-address dedupe that uploadViaClient's
// fixed bytes would cause, so a test can stage several files at once.
func stageBytes(t *testing.T, c *http.Client, base, name, content string) string {
	t.Helper()
	req := buildUploadRequest(t, "file", name, "audio/mpeg", []byte(content))
	resp, err := c.Post(base+"/files/upload", req.Header.Get("Content-Type"), req.Body)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s = %d, want 201", name, resp.StatusCode)
	}
	var body struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode upload %s: %v", name, err)
	}
	return body.Hash
}

// listEnvelope is the {total, selectable_total, items} shape the paged staging /
// trash endpoints return.
type listEnvelope struct {
	Total      int              `json:"total"`
	Selectable int              `json:"selectable_total"`
	Items      []map[string]any `json:"items"`
}

func getEnvelope(t *testing.T, c *http.Client, url string) listEnvelope {
	t.Helper()
	var env listEnvelope
	if code := doJSON(t, c, http.MethodGet, url, nil, &env); code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, code)
	}
	return env
}

// TestModerationBulk_FilterApproveStateScoped covers the "select all N matching"
// moderation path: the filter resolves to submitted rows only (a draft is left
// alone), the empty-filter guardrail needs all:true, and selectable_total counts
// only the actionable subset.
func TestModerationBulk_FilterApproveStateScoped(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	h1 := stageBytes(t, up, srv.URL, "s1.mp3", "alpha one content")
	h2 := stageBytes(t, up, srv.URL, "s2.mp3", "beta two content")
	stageBytes(t, up, srv.URL, "s3.mp3", "gamma three content") // stays a draft
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{stagedTagsetID(t, up, srv.URL, h1), stagedTagsetID(t, up, srv.URL, h2)}}, nil)

	// total = all non-approved (2 submitted + 1 draft); selectable = submitted only.
	env := getEnvelope(t, admin, srv.URL+"/api/admin/moderation")
	if env.Total != 3 || env.Selectable != 2 {
		t.Fatalf("moderation list total=%d selectable=%d, want 3/2", env.Total, env.Selectable)
	}

	// Empty filter without all:true is refused.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/bulk",
		map[string]any{"action": "approve", "filter": map[string]any{"q": ""}}, nil); code != http.StatusBadRequest {
		t.Errorf("approve-all without all:true = %d, want 400", code)
	}

	// Approve everything matching: only the 2 submitted files publish; the draft
	// stays in the queue.
	var res struct {
		OK       bool `json:"ok"`
		Affected int  `json:"affected"`
	}
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/bulk",
		map[string]any{"action": "approve", "filter": map[string]any{"q": ""}, "all": true}, &res); code != http.StatusOK {
		t.Fatalf("approve-all = %d, want 200", code)
	}
	if !res.OK || res.Affected != 2 {
		t.Fatalf("approve-all = %+v, want affected 2", res)
	}
	if files := getFileItems(t, admin, srv.URL+"/api/files"); len(files) != 2 {
		t.Errorf("library after approve-all = %d, want 2", len(files))
	}
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/moderation"); env.Total != 1 {
		t.Errorf("queue after approve-all = %d, want 1 (the draft)", env.Total)
	}
}

// TestModerationBulk_ReturnByHashes covers an explicit-hash bulk return with one
// shared note.
func TestModerationBulk_ReturnByHashes(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	h1 := stageBytes(t, up, srv.URL, "r1.mp3", "return one content")
	h2 := stageBytes(t, up, srv.URL, "r2.mp3", "return two content")
	tid1 := stagedTagsetID(t, up, srv.URL, h1)
	tid2 := stagedTagsetID(t, up, srv.URL, h2)
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid1, tid2}}, nil)

	// A return with no note is rejected.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/bulk",
		map[string]any{"action": "return", "tagset_ids": []int64{tid1, tid2}}, nil); code != http.StatusBadRequest {
		t.Errorf("return without note = %d, want 400", code)
	}

	var res struct {
		Affected int `json:"affected"`
	}
	doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/bulk",
		map[string]any{"action": "return", "tagset_ids": []int64{tid1, tid2}, "note": "fix the artist tag"}, &res)
	if res.Affected != 2 {
		t.Fatalf("bulk return affected = %d, want 2", res.Affected)
	}
	env := getEnvelope(t, up, srv.URL+"/api/my/uploads")
	if env.Total != 2 {
		t.Fatalf("uploader sees %d staged, want 2", env.Total)
	}
	for _, it := range env.Items {
		if it["state"] != "returned" || it["note"] != "fix the artist tag" {
			t.Errorf("returned item = %+v, want returned with the note", it)
		}
	}
}

// TestMyUploadsBulk_SubmitAndRemove covers the uploader-side batch: submit every
// matching draft (filter), and remove an explicit hash.
func TestMyUploadsBulk_SubmitAndRemove(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	stageBytes(t, up, srv.URL, "d1.mp3", "draft one content")
	stageBytes(t, up, srv.URL, "d2.mp3", "draft two content")

	env := getEnvelope(t, up, srv.URL+"/api/my/uploads")
	if env.Total != 2 || env.Selectable != 2 {
		t.Fatalf("my uploads total=%d selectable=%d, want 2/2", env.Total, env.Selectable)
	}

	// Submit everything matching (a plain uploader queues, no self-approve).
	var sub struct {
		Submitted int  `json:"submitted"`
		Approved  bool `json:"approved"`
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk",
		map[string]any{"action": "submit", "filter": map[string]any{"q": ""}, "all": true}, &sub); code != http.StatusOK {
		t.Fatalf("bulk submit = %d, want 200", code)
	}
	if sub.Submitted != 2 || sub.Approved {
		t.Fatalf("bulk submit = %+v, want submitted 2, not approved", sub)
	}
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/moderation"); env.Selectable != 2 {
		t.Errorf("queue submitted after bulk submit = %d, want 2", env.Selectable)
	}

	// Remove a fresh draft by id → it leaves staging for Trash.
	h := stageBytes(t, up, srv.URL, "d3.mp3", "draft three content")
	var rem struct {
		Removed int `json:"removed"`
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk",
		map[string]any{"action": "remove", "tagset_ids": []int64{stagedTagsetID(t, up, srv.URL, h)}}, &rem); code != http.StatusOK {
		t.Fatalf("bulk remove = %d, want 200", code)
	}
	if rem.Removed != 1 {
		t.Errorf("bulk remove = %d, want 1", rem.Removed)
	}
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/trash"); env.Total != 1 {
		t.Errorf("trash after bulk remove = %d, want 1", env.Total)
	}
}

// TestTrashBulk_RestoreDeleteByFilter covers the Trash batch: restore everything
// matching, then permanently delete everything matching. The empty filter needs
// all:true.
func TestTrashBulk_RestoreDeleteByFilter(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	// Two approved-then-trashed files.
	h1 := stageBytes(t, up, srv.URL, "t1.mp3", "trash one content")
	h2 := stageBytes(t, up, srv.URL, "t2.mp3", "trash two content")
	approveViaQueue(t, up, admin, srv.URL, h1)
	approveViaQueue(t, up, admin, srv.URL, h2)
	for _, h := range []string{h1, h2} {
		if code := doJSON(t, admin, http.MethodDelete, srv.URL+"/api/admin/files/"+h, nil, nil); code != http.StatusOK {
			t.Fatalf("soft-delete %s = %d, want 200", h, code)
		}
	}
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/trash"); env.Total != 2 {
		t.Fatalf("trash before restore = %d, want 2", env.Total)
	}

	// Guardrail: empty filter without all:true is refused.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/trash/bulk",
		map[string]any{"action": "restore", "filter": map[string]any{"q": ""}}, nil); code != http.StatusBadRequest {
		t.Errorf("restore-all without all:true = %d, want 400", code)
	}

	// Restore everything matching → back in the library, trash empty.
	var res struct {
		Affected int `json:"affected"`
	}
	doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/trash/bulk",
		map[string]any{"action": "restore", "filter": map[string]any{"q": ""}, "all": true}, &res)
	if res.Affected != 2 {
		t.Fatalf("restore-all affected = %d, want 2", res.Affected)
	}
	if files := getFileItems(t, admin, srv.URL+"/api/files"); len(files) != 2 {
		t.Errorf("library after restore-all = %d, want 2", len(files))
	}

	// Re-trash, then permanently delete everything matching.
	for _, h := range []string{h1, h2} {
		doJSON(t, admin, http.MethodDelete, srv.URL+"/api/admin/files/"+h, nil, nil)
	}
	res.Affected = 0
	doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/trash/bulk",
		map[string]any{"action": "delete", "filter": map[string]any{"q": ""}, "all": true}, &res)
	if res.Affected != 2 {
		t.Fatalf("delete-all affected = %d, want 2", res.Affected)
	}
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/trash"); env.Total != 0 {
		t.Errorf("trash after delete-all = %d, want 0", env.Total)
	}
}

// TestModerationBulk_FilterByTerm covers that a search term scopes the resolved
// set: only the matching submitted file is approved.
func TestModerationBulk_FilterByTerm(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	h1 := stageBytes(t, up, srv.URL, "zebra-track.mp3", "zebra content")
	h2 := stageBytes(t, up, srv.URL, "lion-track.mp3", "lion content")
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{stagedTagsetID(t, up, srv.URL, h1), stagedTagsetID(t, up, srv.URL, h2)}}, nil)

	// Approve only files whose filename matches "zebra".
	var res struct {
		Affected int `json:"affected"`
	}
	doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/moderation/bulk",
		map[string]any{"action": "approve", "filter": map[string]any{"q": "zebra"}}, &res)
	if res.Affected != 1 {
		t.Fatalf("approve filter=zebra affected = %d, want 1", res.Affected)
	}
	// The lion file is still awaiting review.
	if env := getEnvelope(t, admin, srv.URL+"/api/admin/moderation"); env.Selectable != 1 {
		t.Errorf("submitted remaining = %d, want 1 (lion)", env.Selectable)
	}
}
