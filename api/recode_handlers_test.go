package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

// postJSON builds a JSON POST with an identity carrying the given permissions.
func recodeReq(t *testing.T, url string, body map[string]any, perms map[string]bool) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if perms != nil {
		req = req.WithContext(auth.WithIdentity(req.Context(),
			&auth.Identity{UserID: 7, Username: "u", Permissions: perms}))
	}
	return req
}

func TestAppearancesBulk_Recode(t *testing.T) {
	moderator := map[string]bool{auth.PermMetadataEdit: true}
	uploader := map[string]bool{auth.PermFileUpload: true}

	run := func(repo *fakeRepo, body map[string]any, perms map[string]bool) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h := &handler{repo: repo, authzEnabled: true}
		h.appearancesBulk(rr, recodeReq(t, "/api/admin/appearances/bulk", body, perms))
		return rr
	}

	// Happy path: the wired recode function must be ReencodeLatin1(charset).
	repo := &fakeRepo{recodeAffected: 2}
	rr := run(repo, map[string]any{"action": "recode", "charset": "windows-1251", "tagset_ids": []int64{1, 2}}, moderator)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if repo.lastRecodeOwner.Valid {
		t.Error("admin recode must be unscoped (owner invalid)")
	}
	if got, ok := repo.lastRecodeFn("Ãðóïïà"); !ok || got != "Группа" {
		t.Errorf("recode fn = %q ok=%v, want Группа (cp1251 reinterpretation)", got, ok)
	}

	// Validation + gating.
	if rr := run(&fakeRepo{}, map[string]any{"action": "recode", "charset": "ebcdic", "tagset_ids": []int64{1}}, moderator); rr.Code != http.StatusBadRequest {
		t.Errorf("bad charset: status = %d, want 400", rr.Code)
	}
	if rr := run(&fakeRepo{}, map[string]any{"action": "recode", "charset": "windows-1251", "tagset_ids": []int64{1}}, uploader); rr.Code != http.StatusForbidden {
		t.Errorf("without metadata.edit: status = %d, want 403", rr.Code)
	}
}

func TestMyUploadsBulk_Recode(t *testing.T) {
	uploader := map[string]bool{auth.PermFileUpload: true}

	repo := &fakeRepo{recodeAffected: 3}
	rr := httptest.NewRecorder()
	h := &handler{repo: repo, authzEnabled: true}
	h.myUploadsBulk(rr, recodeReq(t, "/api/my/uploads/bulk",
		map[string]any{"action": "recode", "charset": "koi8-r", "tagset_ids": []int64{4, 5, 6}}, uploader))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Affected int  `json:"affected"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Affected != 3 {
		t.Errorf("response = %s", rr.Body.String())
	}
	// The owner scope must carry the caller's user id — the explicit id list is
	// trusted no further than ownership.
	if !repo.lastRecodeOwner.Valid || repo.lastRecodeOwner.Int64 != 7 {
		t.Errorf("owner scope = %+v, want valid user 7", repo.lastRecodeOwner)
	}

	rr = httptest.NewRecorder()
	h.myUploadsBulk(rr, recodeReq(t, "/api/my/uploads/bulk",
		map[string]any{"action": "recode", "charset": "ebcdic", "tagset_ids": []int64{4}}, uploader))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad charset: status = %d, want 400", rr.Code)
	}
}
