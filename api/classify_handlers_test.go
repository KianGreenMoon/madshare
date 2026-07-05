package api

import (
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
)

// TestReview_ClassifiesSubmission checks the moderation queue heads each row
// with its classification (recording-tagsets P4) and that the per-submission
// classify endpoint is gated and returns the full class. A synthetic upload has
// no acoustic match, so it classifies as a new recording (case A) end-to-end;
// the B/C derivations are covered by database/classify_test.go.
func TestReview_ClassifiesSubmission(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", code)
	}

	// The queue row carries class + a false duplicate flag.
	type classRow struct {
		Hash      string `json:"hash"`
		Class     string `json:"class"`
		Duplicate bool   `json:"duplicate"`
	}
	rows := getStaged[classRow](t, admin, srv.URL+"/api/admin/moderation")
	if len(rows) != 1 || rows[0].Class != database.SubmissionNewRecording {
		t.Fatalf("queue = %+v, want one %q row", rows, database.SubmissionNewRecording)
	}
	if rows[0].Duplicate {
		t.Error("fresh upload flagged as duplicate")
	}

	// The classify endpoint returns the full classification to a moderator.
	var cl struct {
		Class   string `json:"class"`
		Matched bool   `json:"matched"`
	}
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, tid, "classify"), nil, &cl); code != http.StatusOK {
		t.Fatalf("classify = %d, want 200", code)
	}
	if cl.Class != database.SubmissionNewRecording || cl.Matched {
		t.Errorf("classify = %+v, want new_recording / matched=false", cl)
	}

	// A plain uploader (no content.moderate) is forbidden.
	if code := doJSON(t, up, http.MethodGet, modAction(srv.URL, tid, "classify"), nil, nil); code != http.StatusForbidden {
		t.Errorf("uploader classify = %d, want 403", code)
	}
	// An unknown tagset id is not a live pending submission.
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, 999999, "classify"), nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown classify = %d, want 404", code)
	}
}
