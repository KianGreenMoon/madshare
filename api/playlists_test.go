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
			{ItemID: 1, Hash: "aa", ObjectKey: "aa/x.mp3", MimeType: "audio/mpeg"},
			{ItemID: 2, Hash: "bb", ObjectKey: "bb/y.mp3", MimeType: "audio/mpeg", Trashed: true},
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

	// Unknown/trashed seed hash → 400, not 500.
	h = plHandler(&fakeRepo{playlistErr: database.ErrFileNotFound})
	rr = httptest.NewRecorder()
	h.createPlaylist(rr, plReq(http.MethodPost, "/api/playlists", `{"name":"x","hashes":["nope"]}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad hash: status = %d, want 400", rr.Code)
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

func TestAddPlaylistItems_BadHashIs400(t *testing.T) {
	h := plHandler(&fakeRepo{playlistErr: database.ErrFileNotFound})
	rr := httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items", `{"hashes":["zz"]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}

	h = plHandler(&fakeRepo{})
	rr = httptest.NewRecorder()
	h.addPlaylistItems(rr, plReq(http.MethodPost, "/api/playlists/1/items", `{"hashes":[]}`, "id", "1"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty hashes: status = %d, want 400", rr.Code)
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
	h.toggleFavorite(rr, plReq(http.MethodPost, "/api/favorites/aa", "", "hash", "aa"))
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
	h.toggleFavorite(rr, plReq(http.MethodPost, "/api/favorites/zz", "", "hash", "zz"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown hash: status = %d, want 404", rr.Code)
	}
}

func TestListFavorites_ReturnsHashes(t *testing.T) {
	h := plHandler(&fakeRepo{favoriteHashes: []string{"aa", "bb"}})
	rr := httptest.NewRecorder()
	h.listFavorites(rr, plReq(http.MethodGet, "/api/favorites", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Hashes) != 2 || resp.Hashes[0] != "aa" {
		t.Errorf("hashes = %v, want [aa bb]", resp.Hashes)
	}
}
