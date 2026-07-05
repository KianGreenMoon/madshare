package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
)

// submitReq builds a POST /api/my/uploads/submit for one appearance, with an
// identity that always holds file.upload and, when moderator, content.moderate.
func submitReq(tagsetID int64, moderator bool) *http.Request {
	body, _ := json.Marshal(map[string][]int64{"tagset_ids": {tagsetID}})
	req := httptest.NewRequest(http.MethodPost, "/api/my/uploads/submit", bytes.NewReader(body))
	perms := map[string]bool{auth.PermFileUpload: true}
	if moderator {
		perms[auth.PermContentModerate] = true
	}
	ctx := auth.WithIdentity(req.Context(), &auth.Identity{UserID: 7, Username: "m", Permissions: perms})
	return req.WithContext(ctx)
}

type submitResp struct {
	Approved  bool   `json:"approved"`
	Submitted int    `json:"submitted"`
	Flagged   int    `json:"flagged"`
	Warning   string `json:"warning"`
}

func runSubmit(t *testing.T, repo *fakeRepo, tagsetID int64, moderator bool) submitResp {
	t.Helper()
	h := &handler{repo: repo, authzEnabled: true}
	rr := httptest.NewRecorder()
	h.submitMyUploads(rr, submitReq(tagsetID, moderator))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp submitResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestSubmit_DuplicateSuppressesSelfApprove(t *testing.T) {
	// The submission matches already-published audio (classified as a matched new
	// appearance) — the duplicate that always queues, even for a moderator.
	repo := &fakeRepo{reviewUpdateFound: true, classify: map[int64]database.SubmissionClass{
		1: {Case: database.SubmissionNewAppearance, MatchedExisting: true},
	}}
	resp := runSubmit(t, repo, 1, true) // moderator

	if resp.Approved {
		t.Error("approved=true; a duplicate must not self-approve even for a moderator")
	}
	if resp.Flagged != 1 || resp.Warning == "" {
		t.Errorf("flagged=%d warning=%q, want 1 and a non-empty warning", resp.Flagged, resp.Warning)
	}
	if repo.lastReviewTrans.To != database.ReviewSubmitted {
		t.Errorf("transition To=%q, want %q (sent for review)", repo.lastReviewTrans.To, database.ReviewSubmitted)
	}
}

func TestSubmit_ModeratorNonDuplicateSelfApproves(t *testing.T) {
	repo := &fakeRepo{reviewUpdateFound: true} // classify nil → default new_recording (not matched)
	resp := runSubmit(t, repo, 2, true)

	if !resp.Approved || resp.Flagged != 0 {
		t.Errorf("approved=%v flagged=%d, want true/0 (non-duplicate moderator submit)", resp.Approved, resp.Flagged)
	}
	if repo.lastReviewTrans.To != database.ReviewApproved {
		t.Errorf("transition To=%q, want %q (self-approved)", repo.lastReviewTrans.To, database.ReviewApproved)
	}
}

func TestSubmit_NonModeratorAlwaysQueues(t *testing.T) {
	repo := &fakeRepo{reviewUpdateFound: true}
	resp := runSubmit(t, repo, 3, false) // no content.moderate

	if resp.Approved {
		t.Error("a non-moderator can never self-approve")
	}
	if repo.lastReviewTrans.To != database.ReviewSubmitted {
		t.Errorf("transition To=%q, want %q", repo.lastReviewTrans.To, database.ReviewSubmitted)
	}
}
