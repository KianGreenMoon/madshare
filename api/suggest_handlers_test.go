package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/storages"
)

// suggestReq builds a GET /api/tagsets/1/suggestions with the chi URL param
// wired and, unless anonymous, an identity carrying the given permissions.
func suggestReq(query string, perms map[string]bool, userID int64) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/tagsets/1/suggestions"+query, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tagsetID", "1")
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	if perms != nil {
		ctx = auth.WithIdentity(ctx, &auth.Identity{UserID: userID, Username: "u", Permissions: perms})
	}
	return req.WithContext(ctx)
}

func suggestHandler(repo *fakeRepo, t *testing.T) *handler {
	return &handler{repo: repo, authzEnabled: true, blobReg: storages.New(t.TempDir(), t.TempDir())}
}

// ownedDraftRepo stubs an existing draft owned by user 7 whose origin blob is
// not on disk (suggestions come back empty but the request succeeds).
func ownedDraftRepo() *fakeRepo {
	return &fakeRepo{
		reviewInfoState:     database.ReviewDraft,
		reviewInfoOwner:     sql.NullInt64{Int64: 7, Valid: true},
		reviewInfoFound:     true,
		suggestSubject:      &database.SuggestSubject{Hash: "deadbeef", MIMEType: "audio/mpeg"},
		suggestSubjectFound: true,
	}
}

func TestSuggestions_AuthzMatrix(t *testing.T) {
	uploader := map[string]bool{auth.PermFileUpload: true}
	moderator := map[string]bool{auth.PermMetadataEdit: true}

	cases := []struct {
		name  string
		repo  *fakeRepo
		perms map[string]bool
		user  int64
		want  int
	}{
		{"anonymous", ownedDraftRepo(), nil, 0, http.StatusUnauthorized},
		{"unknown tagset", &fakeRepo{}, uploader, 7, http.StatusNotFound},
		{"owner of editable draft", ownedDraftRepo(), uploader, 7, http.StatusOK},
		{"non-owner without metadata.edit", ownedDraftRepo(), uploader, 8, http.StatusNotFound},
		{"metadata.edit on any state", func() *fakeRepo {
			r := ownedDraftRepo()
			r.reviewInfoState = database.ReviewApproved
			return r
		}(), moderator, 9, http.StatusOK},
		{"owner but approved (not editable)", func() *fakeRepo {
			r := ownedDraftRepo()
			r.reviewInfoState = database.ReviewApproved
			return r
		}(), uploader, 7, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			suggestHandler(c.repo, t).tagsetSuggestions(rr, suggestReq("", c.perms, c.user))
			if rr.Code != c.want {
				t.Errorf("status = %d, want %d; body=%s", rr.Code, c.want, rr.Body.String())
			}
		})
	}
}

func TestSuggestions_ParamValidationAndShape(t *testing.T) {
	owner := map[string]bool{auth.PermFileUpload: true}

	rr := httptest.NewRecorder()
	suggestHandler(ownedDraftRepo(), t).tagsetSuggestions(rr, suggestReq("?charset=ebcdic", owner, 7))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad charset: status = %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	suggestHandler(ownedDraftRepo(), t).tagsetSuggestions(rr, suggestReq("?sources=musicbrainz", owner, 7))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown source: status = %d, want 400 (P0 has local sources only)", rr.Code)
	}

	// Happy path with the blob missing on disk: empty suggestions, not an error.
	rr = httptest.NewRecorder()
	suggestHandler(ownedDraftRepo(), t).tagsetSuggestions(rr, suggestReq("", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK          bool              `json:"ok"`
		TagsetID    int64             `json:"tagset_id"`
		Suggestions []json.RawMessage `json:"suggestions"`
		External    []string          `json:"external_sources"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.TagsetID != 1 || resp.Suggestions == nil || len(resp.Suggestions) != 0 || resp.External == nil {
		t.Errorf("response = %s", rr.Body.String())
	}
}
