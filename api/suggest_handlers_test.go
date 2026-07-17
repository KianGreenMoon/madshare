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
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/storages"
	"daemonlord.ygg/madshare/tagsource"
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

	// Without a wired lookup client + enabled settings, the musicbrainz source
	// does not exist — naming it is refused (the "chip absent while disabled"
	// guarantee, tag-suggestions.md).
	rr = httptest.NewRecorder()
	suggestHandler(ownedDraftRepo(), t).tagsetSuggestions(rr, suggestReq("?sources=musicbrainz", owner, 7))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("disabled musicbrainz: status = %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	suggestHandler(ownedDraftRepo(), t).tagsetSuggestions(rr, suggestReq("?sources=discogs", owner, 7))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown source: status = %d, want 400", rr.Code)
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

// fakeManage stubs only the tagsource policy; every other ManageStore method
// panics if reached (embedded nil interface).
type fakeManage struct {
	ManageStore
	policy database.TagsourcePolicy
}

func (f *fakeManage) GetTagsourcePolicy(_ context.Context) (database.TagsourcePolicy, error) {
	return f.policy, nil
}

// mbHandler is suggestHandler plus an enabled musicbrainz source pointed at a
// fake AcoustID endpoint.
func mbHandler(t *testing.T, repo *fakeRepo, endpoint string) *handler {
	h := suggestHandler(repo, t)
	h.manage = &fakeManage{policy: database.TagsourcePolicy{MusicBrainzEnabled: true, AcoustIDKey: "k"}}
	h.acoustid = tagsource.NewAcoustID()
	h.acoustid.Endpoint = endpoint
	return h
}

// mbSearchHandler is suggestHandler plus an enabled musicbrainz source whose
// TEXT-SEARCH client points at a fake endpoint (no AcoustID client wired).
func mbSearchHandler(t *testing.T, repo *fakeRepo, endpoint string) *handler {
	h := suggestHandler(repo, t)
	h.manage = &fakeManage{policy: database.TagsourcePolicy{MusicBrainzEnabled: true, AcoustIDKey: "k"}}
	h.musicbrainz = tagsource.NewMusicBrainz()
	h.musicbrainz.Endpoint = endpoint
	return h
}

// fingerprintedRepo is ownedDraftRepo with an analyzed origin file.
func fingerprintedRepo() *fakeRepo {
	r := ownedDraftRepo()
	fp := media.Fingerprint{Raw: []uint32{2515916061, 2516440381, 2516442428}}
	r.suggestSubject.Fingerprint = fp.Packed()
	r.suggestSubject.Duration = sql.NullFloat64{Float64: 237, Valid: true}
	return r
}

func TestSuggestions_MusicBrainz(t *testing.T) {
	owner := map[string]bool{auth.PermFileUpload: true}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"status":"ok","results":[{"score":0.9,"recordings":[
			{"title":"Found Title","artists":[{"name":"Found Artist"}]}]}]}`))
	}))
	defer srv.Close()

	// Enabled but not requested → advertised in external_sources, no outbound call.
	rr := httptest.NewRecorder()
	mbHandler(t, fingerprintedRepo(), srv.URL).tagsetSuggestions(rr, suggestReq("", owner, 7))
	var resp struct {
		OK          bool `json:"ok"`
		Suggestions []struct {
			Source string `json:"source"`
			Error  string `json:"error"`
			Tags   map[string]any
		} `json:"suggestions"`
		External []string `json:"external_sources"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.External) != 1 || resp.External[0] != "musicbrainz" {
		t.Errorf("external_sources = %v, want [musicbrainz]", resp.External)
	}
	if calls != 0 {
		t.Errorf("outbound calls before explicit request = %d, want 0", calls)
	}

	// Explicit request → lookup runs, candidates returned, chip not re-advertised.
	rr = httptest.NewRecorder()
	mbHandler(t, fingerprintedRepo(), srv.URL).tagsetSuggestions(rr, suggestReq("?sources=musicbrainz", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	resp.Suggestions = nil
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("outbound calls = %d, want 1", calls)
	}
	if len(resp.Suggestions) != 1 || resp.Suggestions[0].Source != "musicbrainz" ||
		resp.Suggestions[0].Tags["title"] != "Found Title" {
		t.Errorf("suggestions = %s", rr.Body.String())
	}
	if len(resp.External) != 0 {
		t.Errorf("external_sources after querying = %v, want empty", resp.External)
	}
}

func TestSuggestions_MusicBrainzNoFingerprint(t *testing.T) {
	owner := map[string]bool{auth.PermFileUpload: true}
	rr := httptest.NewRecorder()
	// ownedDraftRepo has no fingerprint → error chip, not a failed endpoint.
	mbHandler(t, ownedDraftRepo(), "http://unreachable.invalid").
		tagsetSuggestions(rr, suggestReq("?sources=musicbrainz", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Suggestions []struct {
			Source string `json:"source"`
			Error  string `json:"error"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Suggestions) != 1 || resp.Suggestions[0].Source != "musicbrainz" || resp.Suggestions[0].Error == "" {
		t.Errorf("want a musicbrainz error chip, got %s", rr.Body.String())
	}
}

func TestSuggestions_MusicBrainzBusy(t *testing.T) {
	owner := map[string]bool{auth.PermFileUpload: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // service throttling → tagsource.ErrBusy
	}))
	defer srv.Close()
	rr := httptest.NewRecorder()
	mbHandler(t, fingerprintedRepo(), srv.URL).
		tagsetSuggestions(rr, suggestReq("?sources=musicbrainz", owner, 7))
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSuggestions_MusicBrainzTextSearch(t *testing.T) {
	owner := map[string]bool{auth.PermFileUpload: true}
	var calls int
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotQueries = append(gotQueries, r.URL.Query().Get("query"))
		w.Write([]byte(`{"recordings":[{"score":100,"title":"Searched Title",
			"artist-credit":[{"name":"Searched Artist","joinphrase":""}],"releases":[]}]}`))
	}))
	defer srv.Close()

	type resp struct {
		Suggestions []struct {
			Source string `json:"source"`
			Error  string `json:"error"`
			Tags   map[string]any
		} `json:"suggestions"`
	}

	// Explicit user query → passed through verbatim.
	rr := httptest.NewRecorder()
	mbSearchHandler(t, ownedDraftRepo(), srv.URL).
		tagsetSuggestions(rr, suggestReq("?sources=musicbrainz&query=hello+world", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got resp
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(gotQueries) != 1 || gotQueries[0] != "hello world" {
		t.Errorf("outbound queries = %q, want [hello world]", gotQueries)
	}
	if len(got.Suggestions) != 1 || got.Suggestions[0].Tags["title"] != "Searched Title" {
		t.Errorf("suggestions = %s", rr.Body.String())
	}

	// Empty query → seeded from the stored current tags (+ duration window
	// from the ffprobe fallback).
	seeded := ownedDraftRepo()
	seeded.suggestSubject.Title = "Stored Title"
	seeded.suggestSubject.Artist = sql.NullString{String: "Stored Artist", Valid: true}
	seeded.suggestSubject.TechDuration = sql.NullFloat64{Float64: 200, Valid: true}
	rr = httptest.NewRecorder()
	mbSearchHandler(t, seeded, srv.URL).
		tagsetSuggestions(rr, suggestReq("?sources=musicbrainz&query=", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("seeded: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	want := `recording:"Stored Title" AND artist:"Stored Artist" AND dur:[190000 TO 210000]`
	if len(gotQueries) != 2 || gotQueries[1] != want {
		t.Errorf("seeded outbound query = %q, want %q", gotQueries[len(gotQueries)-1], want)
	}

	// Empty query and nothing to seed from → error chip, no outbound call.
	rr = httptest.NewRecorder()
	mbSearchHandler(t, ownedDraftRepo(), srv.URL).
		tagsetSuggestions(rr, suggestReq("?sources=musicbrainz&query=", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("unseedable: status = %d; body=%s", rr.Code, rr.Body.String())
	}
	got = resp{}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Suggestions) != 1 || got.Suggestions[0].Error == "" {
		t.Errorf("unseedable: want an error chip, got %s", rr.Body.String())
	}
	if calls != 2 {
		t.Errorf("outbound calls = %d, want 2 (unseedable query never goes out)", calls)
	}

	// A query without sources=musicbrainz is ignored — local sources only.
	rr = httptest.NewRecorder()
	mbSearchHandler(t, ownedDraftRepo(), srv.URL).
		tagsetSuggestions(rr, suggestReq("?sources=id3v1&query=hello", owner, 7))
	if rr.Code != http.StatusOK {
		t.Fatalf("local+query: status = %d", rr.Code)
	}
	if calls != 2 {
		t.Errorf("outbound calls after local-only request = %d, want 2", calls)
	}
}
