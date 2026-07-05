package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"github.com/go-chi/chi/v5"
)

// tagsetReq builds a request whose {tagsetID} path param is set, optionally with
// an identity carrying the given permissions.
func tagsetReq(method, tagsetID, body string, perms map[string]bool) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/", nil)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tagsetID", tagsetID)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if perms != nil {
		ctx = auth.WithIdentity(ctx, &auth.Identity{UserID: 3, Username: "mod", Permissions: perms})
	}
	return r.WithContext(ctx)
}

// TestModerationApprove_PerPieceDecisions checks the approve body's drop_bytes /
// force_new reach the repo (recording-tagsets P4).
func TestModerationApprove_PerPieceDecisions(t *testing.T) {
	repo := &fakeRepo{reviewUpdateFound: true}
	h := &handler{repo: repo, authzEnabled: true}

	rr := httptest.NewRecorder()
	h.moderationApprove(rr, tagsetReq(http.MethodPost, "5", `{"drop_bytes":true,"force_new":true}`, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if repo.lastReviewTagset != 5 || !repo.lastApproveDrop || !repo.lastApproveForceNew {
		t.Errorf("decisions = tagset %d drop %v forceNew %v, want 5/true/true",
			repo.lastReviewTagset, repo.lastApproveDrop, repo.lastApproveForceNew)
	}

	// An empty body is a plain approve (keep bytes, keep recording).
	repo2 := &fakeRepo{reviewUpdateFound: true}
	h2 := &handler{repo: repo2, authzEnabled: true}
	rr2 := httptest.NewRecorder()
	h2.moderationApprove(rr2, tagsetReq(http.MethodPost, "7", "", nil))
	if rr2.Code != http.StatusOK || repo2.lastApproveDrop || repo2.lastApproveForceNew {
		t.Errorf("plain approve = %d drop %v forceNew %v, want 200/false/false", rr2.Code, repo2.lastApproveDrop, repo2.lastApproveForceNew)
	}
}

// TestModerationDiscard_GateAndTrash checks the per-row discard trashes the
// appearance and needs file.delete on top of content.moderate.
func TestModerationDiscard_GateAndTrash(t *testing.T) {
	// Without file.delete → 403.
	repo := &fakeRepo{}
	h := &handler{repo: repo, authzEnabled: true}
	rr := httptest.NewRecorder()
	h.moderationDiscard(rr, tagsetReq(http.MethodPost, "9", "", map[string]bool{auth.PermContentModerate: true}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("discard without file.delete = %d, want 403", rr.Code)
	}

	// With file.delete → trashes the one appearance.
	rr2 := httptest.NewRecorder()
	h.moderationDiscard(rr2, tagsetReq(http.MethodPost, "9", "", map[string]bool{auth.PermContentModerate: true, auth.PermFileDelete: true}))
	if rr2.Code != http.StatusOK {
		t.Fatalf("discard = %d, want 200; body=%s", rr2.Code, rr2.Body.String())
	}
	if len(repo.bulkDiscardTagsets) != 1 || repo.bulkDiscardTagsets[0] != 9 {
		t.Errorf("trashed = %v, want [9]", repo.bulkDiscardTagsets)
	}
}
