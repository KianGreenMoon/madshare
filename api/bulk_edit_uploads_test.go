package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
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

// artistOf reads one appearance's artist straight from the store, so an
// assertion about what a write touched doesn't depend on which listing endpoint
// still carries the row — an approved appearance has left staging.
func artistOf(t *testing.T, db *database.DB, tagsetID int64) string {
	t.Helper()
	var artist sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT artist FROM tagsets WHERE id = ?`, tagsetID).Scan(&artist); err != nil {
		t.Fatalf("read artist of tagset %d: %v", tagsetID, err)
	}
	return artist.String
}

// TestMyUploads_EditsAreOwnAndStagedOnly is the authorization matrix behind the
// staging tab's tag edits. Single-row and bulk answer to the same rule, so both
// are run against the same five appearances: an uploader may retag their own
// draft and their own returned row, and nothing else.
//
// The two halves of the rule each need their own row to be worth anything.
// Ownership alone doesn't grant the edit — the submitted and approved rows are
// the uploader's own, and once sent to approval a file is no longer theirs to
// change under a moderator. Staging state alone doesn't either — another
// uploader's row is a draft, and still refused. A refusal is a 404 rather than a
// 403, because a 403 would confirm the appearance exists.
func TestMyUploads_EditsAreOwnAndStagedOnly(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	makeUser(t, db, "other", "uploader-pass-2", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	other := clientFor(t, srv.URL, "other", "uploader-pass-2")

	// One row per state that matters. The ids are captured while everything is
	// still staged: an approved appearance leaves /api/my/uploads, so it could
	// not be looked up afterwards.
	stage := func(c *http.Client, name, content string) int64 {
		t.Helper()
		return stagedTagsetID(t, c, srv.URL, stageBytes(t, c, srv.URL, name, content))
	}
	ownDraft := stage(up, "m1.mp3", "matrix draft content")
	ownReturned := stage(up, "m2.mp3", "matrix returned content")
	ownSubmitted := stage(up, "m3.mp3", "matrix submitted content")
	ownApproved := stage(up, "m4.mp3", "matrix approved content")
	foreignDraft := stage(other, "m5.mp3", "matrix foreign content")

	submit := func(tid int64) {
		t.Helper()
		if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
			map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
			t.Fatalf("submit %d = %d, want 200", tid, code)
		}
	}
	submit(ownReturned)
	if code := doJSON(t, admin, http.MethodPost, modAction(srv.URL, ownReturned, "return"),
		map[string]any{"note": "fix the artist tag"}, nil); code != http.StatusOK {
		t.Fatalf("return = %d, want 200", code)
	}
	submit(ownSubmitted)
	submit(ownApproved)
	if code := doJSON(t, admin, http.MethodPost, modAction(srv.URL, ownApproved, "approve"),
		map[string]any{}, nil); code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", code)
	}

	cases := []struct {
		name     string
		tid      int64
		editable bool
	}{
		{"own draft", ownDraft, true},
		{"own returned", ownReturned, true},
		{"own submitted", ownSubmitted, false},
		{"own approved", ownApproved, false},
		{"another uploader's draft", foreignDraft, false},
	}

	// Single row: the PATCH and the GET that feeds the edit modal share one guard,
	// so a read that leaks is as much a failure as a write that lands.
	const viaPatch = "Patched One By One"
	for _, c := range cases {
		want := http.StatusNotFound
		if c.editable {
			want = http.StatusOK
		}
		if code := doJSON(t, up, http.MethodGet, muMeta(srv.URL, c.tid), nil, nil); code != want {
			t.Errorf("%s: GET metadata = %d, want %d", c.name, code, want)
		}
		if code := doJSON(t, up, http.MethodPatch, muMeta(srv.URL, c.tid),
			map[string]any{"artist": viaPatch}, nil); code != want {
			t.Errorf("%s: PATCH metadata = %d, want %d", c.name, code, want)
		}
	}
	for _, c := range cases {
		want := ""
		if c.editable {
			want = viaPatch
		}
		if got := artistOf(t, db, c.tid); got != want {
			t.Errorf("%s after the single-row edits: artist = %q, want %q", c.name, got, want)
		}
	}

	// Bulk: one call naming every row, including the four the caller may not
	// touch. They don't fail the batch — they are simply outside the owner scope
	// the write runs under, so `affected` counts the two that were in reach.
	const viaBulk = "Patched In Bulk"
	var res struct {
		OK       bool `json:"ok"`
		Affected int  `json:"affected"`
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk", map[string]any{
		"action":     "edit",
		"tagset_ids": []int64{ownDraft, ownReturned, ownSubmitted, ownApproved, foreignDraft},
		"patch":      map[string]any{"artist": viaBulk},
	}, &res); code != http.StatusOK {
		t.Fatalf("bulk edit = %d, want 200", code)
	}
	if !res.OK || res.Affected != 2 {
		t.Fatalf("bulk edit = %+v, want affected 2 (own draft + own returned)", res)
	}
	for _, c := range cases {
		want := ""
		if c.editable {
			want = viaBulk
		}
		if got := artistOf(t, db, c.tid); got != want {
			t.Errorf("%s after the bulk edit: artist = %q, want %q", c.name, got, want)
		}
	}

	// The filter path resolves its own set server-side, so "select all matching"
	// cannot be talked into a wider one either.
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/bulk", map[string]any{
		"action": "edit", "filter": map[string]any{"q": "", "field": ""}, "all": true,
		"patch": map[string]any{"album": "Everything I Can Reach"},
	}, &res); code != http.StatusOK {
		t.Fatalf("filter edit = %d, want 200", code)
	}
	if res.Affected != 2 {
		t.Errorf("filter edit affected = %d, want 2 — the caller's editable staging is the whole set", res.Affected)
	}
}
