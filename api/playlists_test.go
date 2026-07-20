package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// plReq builds a playlist request with an authenticated identity and the given
// chi URL params (key/value pairs).
func plReq(method, target, body string, params ...string) *http.Request {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithIdentity(ctx, &auth.Identity{
		UserID:      7,
		Username:    "alice",
		Permissions: map[string]bool{auth.PermContentAccess: true},
	})
	return req.WithContext(ctx)
}

func plHandler(repo *fakeRepo) *handler {
	return &handler{repo: repo, authzEnabled: true}
}

func TestPlaylists_AnonymousIs401(t *testing.T) {
	h := plHandler(&fakeRepo{})
	// No identity in context — the defensive in-handler check must refuse even
	// if the route gate were bypassed.
	req := httptest.NewRequest(http.MethodGet, "/api/playlists", nil)
	rr := httptest.NewRecorder()
	h.listPlaylists(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestGetPlaylist_ForeignIDIs404(t *testing.T) {
	h := plHandler(&fakeRepo{playlistErr: database.ErrPlaylistNotFound})
	rr := httptest.NewRecorder()
	h.getPlaylist(rr, plReq(http.MethodGet, "/api/playlists/42", "", "id", "42"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (foreign ids must not leak as 403)", rr.Code)
	}
}

func TestGetPlaylist_TrashedItemStatus(t *testing.T) {
	repo := &fakeRepo{
		playlistGet: &database.Playlist{ID: 1, Name: "L", Kind: database.PlaylistRegular},
		playlistItems: []*database.PlaylistItemEntry{
			{ItemID: 1, TagsetID: 11, ObjectKey: "aa/x.mp3", MimeType: "audio/mpeg"},
			{ItemID: 2, TagsetID: 12, ObjectKey: "bb/y.mp3", MimeType: "audio/mpeg", Trashed: true},
		},
	}
	h := plHandler(repo)
	rr := httptest.NewRecorder()
	h.getPlaylist(rr, plReq(http.MethodGet, "/api/playlists/1", "", "id", "1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []struct {
			Status string `json:"status"`
			URL    string `json:"url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 2 || resp.Items[0].Status != "ok" || resp.Items[1].Status != "trashed" {
		t.Errorf("items = %+v, want statuses [ok trashed]", resp.Items)
	}
	if resp.Items[0].URL != "/files/aa/x.mp3" {
		t.Errorf("url = %s, want /files/aa/x.mp3", resp.Items[0].URL)
	}
}

func TestCreatePlaylist_Validation(t *testing.T) {
	h := plHandler(&fakeRepo{})
	rr := httptest.NewRecorder()
	h.createPlaylist(rr, plReq(http.MethodPost, "/api/playlists", `{"name":"  "}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("blank name: status = %d, want 400", rr.Code)
	}

	// Unknown/unavailable seed appearance → 400, not 500.
	h = plHandler(&fakeRepo{playlistErr: database.ErrFileNotFound})
	rr = httptest.NewRecorder()
	h.createPlaylist(rr, plReq(http.MethodPost, "/api/playlists", `{"name":"x","tagset_ids":[999]}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad tagset: status = %d, want 400", rr.Code)
	}
}

func TestRenameDeletePlaylist_FavoritesIs403(t *testing.T) {
	h := plHandler(&fakeRepo{playlistErr: database.ErrPlaylistSystem})

	rr := httptest.NewRecorder()
	h.renamePlaylist(rr, plReq(http.MethodPatch, "/api/playlists/1", `{"name":"x"}`, "id", "1"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("rename favorites: status = %d, want 403", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.deletePlaylist(rr, plReq(http.MethodDelete, "/api/playlists/1", "", "id", "1"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("delete favorites: status = %d, want 403", rr.Code)
	}
}

func TestAddPlaylistItems_BadTagsetIs400(t *testing.T) {
	h := plHandler(&fakeRepo{playlistErr: database.ErrFileNotFound})
	rr := httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items", `{"tagset_ids":[999]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}

	h = plHandler(&fakeRepo{})
	rr = httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items", `{"tagset_ids":[]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty tagset_ids: status = %d, want 400", rr.Code)
	}
}

func TestReorderPlaylist_BadPermutationIs400(t *testing.T) {
	h := plHandler(&fakeRepo{playlistErr: database.ErrBadReorder})
	rr := httptest.NewRecorder()
	h.reorderPlaylist(rr, plReq(http.MethodPut, "/api/playlists/1/items", `{"item_ids":[1,2]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestToggleFavorite_ResponseShape(t *testing.T) {
	h := plHandler(&fakeRepo{favoriteLiked: true})
	rr := httptest.NewRecorder()
	h.toggleFavorite(rr, plReq(http.MethodPost, "/api/favorites/11", "", "tagsetID", "11"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Liked bool `json:"liked"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Liked {
		t.Errorf("liked = false, want true")
	}

	h = plHandler(&fakeRepo{playlistErr: database.ErrFileNotFound})
	rr = httptest.NewRecorder()
	h.toggleFavorite(rr, plReq(http.MethodPost, "/api/favorites/999", "", "tagsetID", "999"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown tagset: status = %d, want 404", rr.Code)
	}
}

// Remote madnetwork items (docs/ui/madnetwork-page.md §Remote tracks): a
// remote-only add is accepted, the toggle validates the hash, and the
// favorites listing carries both halves of the liked set.
func TestPlaylistRemote_AddAndToggle(t *testing.T) {
	h := plHandler(&fakeRepo{})
	rr := httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items",
		`{"remote":[{"hash":"`+strings.Repeat("a", 64)+`","title":"Far Song"}]}`, "id", "1"))
	if rr.Code != http.StatusOK {
		t.Errorf("remote-only add: status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	h = plHandler(&fakeRepo{playlistErr: database.ErrBadRemoteRef})
	rr = httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items",
		`{"remote":[{"hash":"nope"}]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad remote hash add: status = %d, want 400", rr.Code)
	}

	h = plHandler(&fakeRepo{favoriteLiked: true})
	rr = httptest.NewRecorder()
	h.toggleRemoteFavorite(rr, plReq(http.MethodPost, "/api/favorites/remote/x",
		`{"title":"Far Song"}`, "hash", strings.Repeat("b", 64)))
	if rr.Code != http.StatusOK {
		t.Fatalf("remote toggle: status = %d, want 200", rr.Code)
	}
	var resp struct {
		Liked bool `json:"liked"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || !resp.Liked {
		t.Errorf("remote toggle response = %s (err %v), want liked:true", rr.Body.String(), err)
	}

	rr = httptest.NewRecorder()
	h.toggleRemoteFavorite(rr, plReq(http.MethodPost, "/api/favorites/remote/x", `{}`, "hash", "zz"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad remote hash toggle: status = %d, want 400", rr.Code)
	}
}

func TestListFavorites_IncludesRemoteHashes(t *testing.T) {
	h := plHandler(&fakeRepo{favoriteTagsetIDs: []int64{11}, favoriteRemoteHashes: []string{strings.Repeat("c", 64)}})
	rr := httptest.NewRecorder()
	h.listFavorites(rr, plReq(http.MethodGet, "/api/favorites", ""))
	var resp struct {
		TagsetIDs    []int64  `json:"tagset_ids"`
		RemoteHashes []string `json:"remote_hashes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.TagsetIDs) != 1 || len(resp.RemoteHashes) != 1 {
		t.Errorf("favorites = %+v, want both halves", resp)
	}
}

func TestGetPlaylist_RemoteItemShape(t *testing.T) {
	hash := strings.Repeat("d", 64)
	h := plHandler(&fakeRepo{
		playlistGet: &database.Playlist{ID: 1, Name: "Mixed", Kind: database.PlaylistRegular},
		playlistItems: []*database.PlaylistItemEntry{
			{ItemID: 1, RemoteHash: hash, Available: true},
			{ItemID: 2, RemoteHash: hash[:63] + "e", Available: false},
		},
	})
	rr := httptest.NewRecorder()
	h.getPlaylist(rr, plReq(http.MethodGet, "/api/playlists/1", "", "id", "1"))
	var resp struct {
		Items []struct {
			URL    string `json:"url"`
			Status string `json:"status"`
			Remote bool   `json:"remote"`
			Hash   string `json:"hash"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(resp.Items))
	}
	ok := resp.Items[0]
	if !ok.Remote || ok.Hash != hash || ok.URL != "/api/madnetwork/stream/"+hash || ok.Status != "ok" {
		t.Errorf("available remote item = %+v, want stream url + status ok", ok)
	}
	if resp.Items[1].Status != "unavailable" {
		t.Errorf("unavailable remote item status = %q, want unavailable", resp.Items[1].Status)
	}
}

func TestListFavorites_ReturnsTagsetIDs(t *testing.T) {
	h := plHandler(&fakeRepo{favoriteTagsetIDs: []int64{11, 12}})
	rr := httptest.NewRecorder()
	h.listFavorites(rr, plReq(http.MethodGet, "/api/favorites", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		TagsetIDs []int64 `json:"tagset_ids"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.TagsetIDs) != 2 || resp.TagsetIDs[0] != 11 {
		t.Errorf("tagset_ids = %v, want [11 12]", resp.TagsetIDs)
	}
}
