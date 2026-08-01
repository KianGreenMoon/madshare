package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

// TestMyUploadsBulk_EditScopeAndRefusals covers the wiring of the staging bulk
// tag edit: the DB call must carry the caller's owner scope (the explicit id
// list is trusted no further than ownership), an empty patch is refused, and a
// patch reaching for recording-level access is refused rather than dropped.
func TestMyUploadsBulk_EditScopeAndRefusals(t *testing.T) {
	uploader := map[string]bool{auth.PermFileUpload: true}
	run := func(repo *fakeRepo, body map[string]any) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h := &handler{repo: repo, authzEnabled: true}
		h.myUploadsBulk(rr, recodeReq(t, "/api/my/uploads/bulk", body, uploader))
		return rr
	}

	repo := &fakeRepo{}
	rr := run(repo, map[string]any{
		"action": "edit", "tagset_ids": []int64{4, 5},
		"patch": map[string]any{"artist": "The Band", "year": "1971"},
	})
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
	if !resp.OK || resp.Affected != 2 {
		t.Errorf("response = %s, want ok with affected 2", rr.Body.String())
	}
	if !repo.lastMetaOwner.Valid || repo.lastMetaOwner.Int64 != 7 {
		t.Errorf("owner scope = %+v, want valid user 7", repo.lastMetaOwner)
	}
	if repo.lastMetaPatch.Artist == nil || *repo.lastMetaPatch.Artist != "The Band" ||
		repo.lastMetaPatch.Year == nil || *repo.lastMetaPatch.Year != "1971" {
		t.Errorf("patch = %+v, want artist + the extended year field carried through", repo.lastMetaPatch)
	}

	// An empty patch has nothing to write.
	if rr := run(&fakeRepo{}, map[string]any{"action": "edit", "tagset_ids": []int64{4}, "patch": map[string]any{}}); rr.Code != http.StatusBadRequest {
		t.Errorf("empty patch: status = %d, want 400", rr.Code)
	}
	if rr := run(&fakeRepo{}, map[string]any{"action": "edit", "tagset_ids": []int64{4}}); rr.Code != http.StatusBadRequest {
		t.Errorf("absent patch: status = %d, want 400", rr.Code)
	}

	// License / guest / share scope live on the recording, which an uploader does
	// not own: refused outright, never silently stripped.
	for _, access := range []map[string]any{
		{"artist": "x", "license": "cc-by"},
		{"artist": "x", "guest": true},
		{"artist": "x", "share_depth": 0},
	} {
		repo := &fakeRepo{}
		rr := run(repo, map[string]any{"action": "edit", "tagset_ids": []int64{4}, "patch": access})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("patch %v: status = %d, want 400", access, rr.Code)
		}
		if repo.metaCalls != 0 {
			t.Errorf("patch %v reached the store", access)
		}
		if !strings.Contains(rr.Body.String(), "from your uploads") {
			t.Errorf("patch %v: refusal = %s, want it to name this surface", access, rr.Body.String())
		}
	}
}

// TestBulkEditRefusalNamesItsSurface — the two scopes that don't own the
// recording share one guard, so the message must still say which rule was hit:
// a client showing it verbatim would otherwise send a Trash user to look at
// their uploads.
func TestBulkEditRefusalNamesItsSurface(t *testing.T) {
	repo := &fakeRepo{}
	rr := httptest.NewRecorder()
	h := &handler{repo: repo, authzEnabled: true}
	license, artist := "cc-by", "x"
	patch := &bulkEditPatch{License: &license}
	patch.Artist = &artist
	h.bulkEditAppearances(rr, recodeReq(t, "/api/admin/trash/bulk", nil, nil), []int64{4}, patch)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("trash access patch: status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "from Trash") {
		t.Errorf("refusal = %s, want it to name Trash", rr.Body.String())
	}
	if repo.metaCalls != 0 {
		t.Error("a refused patch reached the store")
	}
}

// TestMyUploadsBulk_EditOwnerAndStateScoped is the end-to-end half: the write
// must touch only the caller's own editable staging — another uploader's draft
// and the caller's own already-submitted row are left alone even when their ids
// are passed explicitly.
func TestMyUploadsBulk_EditOwnerAndStateScoped(t *testing.T) {
	srv, db := newAuthTestServer(t)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	makeUser(t, db, "other", "uploader-pass-2", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	other := clientFor(t, srv.URL, "other", "uploader-pass-2")

	h1 := stageBytes(t, up, srv.URL, "e1.mp3", "edit one content")
	h2 := stageBytes(t, up, srv.URL, "e2.mp3", "edit two content")
	h3 := stageBytes(t, up, srv.URL, "e3.mp3", "edit three content")
	foreign := stageBytes(t, other, srv.URL, "e4.mp3", "edit four content")
	tid1 := stagedTagsetID(t, up, srv.URL, h1)
	tid2 := stagedTagsetID(t, up, srv.URL, h2)
	tid3 := stagedTagsetID(t, up, srv.URL, h3)
	foreignTID := stagedTagsetID(t, other, srv.URL, foreign)

	// tid3 is sent to approval first: locked, so the edit must skip it.
	doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk",
		map[string]any{"action": "submit", "tagset_ids": []int64{tid3}}, nil)

	var res struct {
		OK       bool `json:"ok"`
		Affected int  `json:"affected"`
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk", map[string]any{
		"action": "edit", "tagset_ids": []int64{tid1, tid2, tid3, foreignTID},
		"patch": map[string]any{"album": "Studio Sessions"},
	}, &res); code != http.StatusOK {
		t.Fatalf("bulk edit = %d, want 200", code)
	}
	if !res.OK || res.Affected != 2 {
		t.Fatalf("bulk edit = %+v, want affected 2 (own drafts only)", res)
	}

	albums := map[int64]string{}
	env := getEnvelope(t, up, srv.URL+"/api/my/uploads?limit=1000")
	for _, it := range env.Items {
		albums[int64(it["tagset_id"].(float64))] = it["album"].(string)
	}
	if albums[tid1] != "Studio Sessions" || albums[tid2] != "Studio Sessions" {
		t.Errorf("own drafts = %q/%q, want the new album on both", albums[tid1], albums[tid2])
	}
	if albums[tid3] == "Studio Sessions" {
		t.Error("a submitted appearance was edited from the staging bulk path")
	}

	otherEnv := getEnvelope(t, other, srv.URL+"/api/my/uploads?limit=1000")
	for _, it := range otherEnv.Items {
		if it["album"] == "Studio Sessions" {
			t.Error("another uploader's draft was edited")
		}
	}
}
