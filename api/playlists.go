package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// registerPlaylists mounts the playlist + favorites endpoints
// (docs/plans/playlists.md). They are only registered when auth is configured
// (playlists are per-user, meaningless without identities) and every route
// requires content.access — RequirePermission already 401s anonymous requests,
// so a reachable handler always has a user id in context.
func registerPlaylists(r chi.Router, d Deps, h *handler) {
	guard := d.protect(auth.PermContentAccess)
	r.With(guard).Get("/api/playlists", h.listPlaylists)
	r.With(guard).Post("/api/playlists", h.createPlaylist)
	r.With(guard).Get("/api/playlists/{id}", h.getPlaylist)
	r.With(guard).Patch("/api/playlists/{id}", h.renamePlaylist)
	r.With(guard).Delete("/api/playlists/{id}", h.deletePlaylist)
	r.With(guard).Post("/api/playlists/{id}/items", h.addPlaylistItems)
	r.With(guard).Delete("/api/playlists/{id}/items/{itemID}", h.removePlaylistItem)
	r.With(guard).Put("/api/playlists/{id}/items", h.reorderPlaylist)
	r.With(guard).Post("/api/favorites/{hash}", h.toggleFavorite)
	r.With(guard).Get("/api/favorites", h.listFavorites)
}

// maxPlaylistName caps playlist names; maxPlaylistBatch caps the hashes/ids
// accepted per request, so a single call can't insert unbounded rows.
const (
	maxPlaylistName  = 200
	maxPlaylistBatch = 5000
)

// playlistUser extracts the authenticated user id. The permission gate makes
// anonymous requests unreachable; this is the defensive belt for misuse of the
// handler outside that gate.
func playlistUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return 0, false
	}
	return id.UserID, true
}

func playlistID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

type playlistSummary struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	TrackCount int    `json:"track_count"`
	UpdatedAt  int64  `json:"updated_at"`
}

func toPlaylistSummary(p *database.Playlist) playlistSummary {
	return playlistSummary{ID: p.ID, Name: p.Name, Kind: p.Kind, TrackCount: p.TrackCount, UpdatedAt: p.UpdatedAt}
}

// listPlaylists handles GET /api/playlists — the user's playlists, favorites
// first. The favorites playlist is created lazily here so the UI can always
// offer it as an add-target.
func (h *handler) listPlaylists(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	if _, err := h.repo.EnsureFavoritesPlaylist(r.Context(), userID); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	lists, err := h.repo.ListPlaylists(r.Context(), userID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	items := make([]playlistSummary, 0, len(lists))
	for _, p := range lists {
		items = append(items, toPlaylistSummary(p))
	}
	writeJSON(w, http.StatusOK, items)
}

// createPlaylist handles POST /api/playlists {name, hashes?} — also the
// "save current queue as playlist" path.
func (h *handler) createPlaylist(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Hashes []string `json:"hashes"`
	}
	if !decodePlaylistBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > maxPlaylistName {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(body.Hashes) > maxPlaylistBatch {
		http.Error(w, "too many tracks", http.StatusBadRequest)
		return
	}
	p, err := h.repo.CreatePlaylist(r.Context(), userID, name, body.Hashes)
	if errors.Is(err, database.ErrFileNotFound) {
		http.Error(w, "unknown or unavailable track", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, toPlaylistSummary(p))
}

// getPlaylist handles GET /api/playlists/{id} — the playlist with its items in
// order. Trashed tracks carry status "trashed" (metadata visible, unplayable);
// hard-deleted tracks are simply absent.
func (h *handler) getPlaylist(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	p, entries, err := h.repo.GetPlaylist(r.Context(), userID, id)
	if errors.Is(err, database.ErrPlaylistNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type playlistItem struct {
		ItemID   int64    `json:"item_id"`
		Hash     string   `json:"hash"`
		URL      string   `json:"url"`
		MimeType string   `json:"mime_type"`
		Title    string   `json:"title"`
		Artist   string   `json:"artist"`
		Album    string   `json:"album"`
		Duration *float64 `json:"duration_seconds"`
		Status   string   `json:"status"`
	}
	items := make([]playlistItem, 0, len(entries))
	for _, e := range entries {
		var dur *float64
		if e.DurationSeconds.Valid {
			dur = &e.DurationSeconds.Float64
		}
		status := "ok"
		if e.Trashed {
			status = "trashed"
		}
		items = append(items, playlistItem{
			ItemID:   e.ItemID,
			Hash:     e.Hash,
			URL:      "/files/" + e.ObjectKey,
			MimeType: e.MimeType,
			Title:    e.Title.String,
			Artist:   e.Artist.String,
			Album:    e.Album.String,
			Duration: dur,
			Status:   status,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		playlistSummary
		Items []playlistItem `json:"items"`
	}{toPlaylistSummary(p), items})
}

// renamePlaylist handles PATCH /api/playlists/{id} {name}. Favorites is not
// renamable (403).
func (h *handler) renamePlaylist(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodePlaylistBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > maxPlaylistName {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	switch err := h.repo.RenamePlaylist(r.Context(), userID, id, name); {
	case errors.Is(err, database.ErrPlaylistNotFound):
		http.NotFound(w, r)
	case errors.Is(err, database.ErrPlaylistSystem):
		http.Error(w, "the favorites playlist cannot be renamed", http.StatusForbidden)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "name": name})
	}
}

// deletePlaylist handles DELETE /api/playlists/{id}. Favorites is not
// deletable (403).
func (h *handler) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	switch err := h.repo.DeletePlaylist(r.Context(), userID, id); {
	case errors.Is(err, database.ErrPlaylistNotFound):
		http.NotFound(w, r)
	case errors.Is(err, database.ErrPlaylistSystem):
		http.Error(w, "the favorites playlist cannot be deleted", http.StatusForbidden)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// addPlaylistItems handles POST /api/playlists/{id}/items {hashes}. The batch
// is atomic: any unknown or trashed hash rejects the whole request.
func (h *handler) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		Hashes []string `json:"hashes"`
	}
	if !decodePlaylistBody(w, r, &body) {
		return
	}
	if len(body.Hashes) == 0 || len(body.Hashes) > maxPlaylistBatch {
		http.Error(w, "hashes are required", http.StatusBadRequest)
		return
	}
	added, err := h.repo.AddPlaylistItemsByHash(r.Context(), userID, id, body.Hashes)
	switch {
	case errors.Is(err, database.ErrPlaylistNotFound):
		http.NotFound(w, r)
	case errors.Is(err, database.ErrFileNotFound):
		http.Error(w, "unknown or unavailable track", http.StatusBadRequest)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
	}
}

// removePlaylistItem handles DELETE /api/playlists/{id}/items/{itemID}.
func (h *handler) removePlaylistItem(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	itemID, err := strconv.ParseInt(chi.URLParam(r, "itemID"), 10, 64)
	if err != nil || itemID <= 0 {
		http.NotFound(w, r)
		return
	}
	found, err := h.repo.RemovePlaylistItem(r.Context(), userID, id, itemID)
	switch {
	case errors.Is(err, database.ErrPlaylistNotFound):
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	case !found:
		http.NotFound(w, r)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// reorderPlaylist handles PUT /api/playlists/{id}/items {item_ids} — the full
// new ordering, which must be a permutation of the playlist's current items.
func (h *handler) reorderPlaylist(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		ItemIDs []int64 `json:"item_ids"`
	}
	if !decodePlaylistBody(w, r, &body) {
		return
	}
	if len(body.ItemIDs) > maxPlaylistBatch {
		http.Error(w, "too many items", http.StatusBadRequest)
		return
	}
	switch err := h.repo.ReorderPlaylist(r.Context(), userID, id, body.ItemIDs); {
	case errors.Is(err, database.ErrPlaylistNotFound):
		http.NotFound(w, r)
	case errors.Is(err, database.ErrBadReorder):
		http.Error(w, "item_ids must be a permutation of the playlist's items", http.StatusBadRequest)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toggleFavorite handles POST /api/favorites/{hash} — the Like button. Flips
// membership in the favorites playlist and reports the resulting state.
func (h *handler) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	hash := chi.URLParam(r, "hash")
	liked, err := h.repo.ToggleFavorite(r.Context(), userID, hash)
	switch {
	case errors.Is(err, database.ErrFileNotFound):
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"liked": liked})
	}
}

// listFavorites handles GET /api/favorites — the user's liked hashes, for
// painting hearts on rows and the player bar.
func (h *handler) listFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := playlistUser(w, r)
	if !ok {
		return
	}
	hashes, err := h.repo.ListFavoriteHashes(r.Context(), userID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hashes": hashes})
}

// decodePlaylistBody decodes a small JSON body with the standard size cap,
// writing the 400 itself on failure.
func decodePlaylistBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}
