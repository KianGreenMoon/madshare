package api

// Full Library · All Appearances — the live twin of the Trash Appearances
// lens (docs/architecture/file-management-view.md). One row per live approved
// appearance, addressed by tagset id; Play resolves to the recording's
// ladder-best surviving rendition, exactly like the listening surfaces.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// adminAppearancesList handles GET /api/admin/appearances — one page of the
// live lens, filtered + sorted like the other file surfaces. Returns
// {total, items}; every row is selectable, so there is no selectable_total.
// Newest first (tagset creation time).
func (h *handler) adminAppearancesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	filter := database.FileFilter{Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountAppearances(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}

	var entries []*database.AppearanceEntry
	if limit > 0 {
		sort := q.Get("sort")
		if sort == "" {
			sort = "created_desc"
		}
		entries, err = h.repo.ListAppearancesPage(r.Context(), database.FileListQuery{
			FileFilter: filter, Sort: sort, Limit: limit, Offset: offset,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	// The row is an APPEARANCE: tagset_id is its identity. Hash/URL/size come
	// from the recording's serving rendition — empty when the recording is
	// dormant (all renditions removed), which the UI shows as unplayable.
	type appearanceItem struct {
		TagsetID    int64  `json:"tagset_id"`
		RecordingID int64  `json:"recording_id"`
		Hash        string `json:"hash"`
		Filename    string `json:"filename"`
		Title       string `json:"title"`
		Artist      string `json:"artist"`
		AlbumArtist string `json:"album_artist"`
		Album       string `json:"album"`
		TrackNumber *int64 `json:"track_number"`
		DiscNumber  *int64 `json:"disc_number"` // null when untagged (groups the grouped view)
		Year        int64  `json:"year,omitempty"`
		ByteSize    int64  `json:"byte_size"`
		URL         string `json:"url"`
		CreatedAt   int64  `json:"created_at"`
		// License / GuestPlayable render the Access column (recording-level;
		// edited via PATCH /api/admin/recordings/{id}/access).
		License        string `json:"license"`
		GuestPlayable  bool   `json:"guest_playable"`
		ArtistHasImage bool   `json:"artist_has_image"`
		AlbumHasImage  bool   `json:"album_has_image"`
	}

	items := make([]appearanceItem, 0, len(entries))
	for _, e := range entries {
		var trackNum, discNum *int64
		if e.TrackNumber.Valid {
			trackNum = &e.TrackNumber.Int64
		}
		if e.DiscNumber.Valid {
			discNum = &e.DiscNumber.Int64
		}
		url := ""
		if e.ObjectKey != "" {
			url = "/files/" + e.ObjectKey
		}
		items = append(items, appearanceItem{
			TagsetID:       e.TagsetID,
			RecordingID:    e.RecordingID,
			Hash:           e.Hash,
			Filename:       e.Filename,
			Title:          e.Title,
			Artist:         e.Artist,
			AlbumArtist:    e.AlbumArtist.String,
			Album:          e.Album,
			TrackNumber:    trackNum,
			DiscNumber:     discNum,
			Year:           e.Year,
			ByteSize:       e.ByteSize,
			URL:            url,
			CreatedAt:      e.CreatedAt,
			License:        e.License.String,
			GuestPlayable:  e.GuestPlayable,
			ArtistHasImage: e.ArtistHasImage,
			AlbumHasImage:  e.AlbumHasImage,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "limit": limit, "offset": offset, "items": items,
	})
}

// appearancesBulk handles POST /api/admin/appearances/bulk — a bulk action
// ("trash" / "edit" / "recode") over an explicit tagset_ids list OR everything
// live matching a filter ("select all N matching"). Per-action gate mirroring
// the files bulk: trash → file.delete, edit/recode → metadata.edit. Unlike the
// Trash lens's edit, the live edit may carry access (license/guest), forwarded
// to each appearance's recording. "recode" is the bulk charset fix
// (docs/architecture/tag-suggestions.md): reinterpret each row's stored text
// tags in the given charset; unreinterpretable/unchanged fields stay put.
func (h *handler) appearancesBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Action    string  `json:"action"`
		TagsetIDs []int64 `json:"tagset_ids"`
		Filter    *struct {
			Q        string `json:"q"`
			Field    string `json:"field"`
			ArtistID *int64 `json:"artist_id"`
			AlbumID  *int64 `json:"album_id"`
		} `json:"filter"`
		All     bool           `json:"all"`
		Patch   *bulkEditPatch `json:"patch"`
		Charset string         `json:"charset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.Action != "trash" && req.Action != "edit" && req.Action != "recode" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	if req.Action == "recode" && !media.ValidCharset(req.Charset) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown charset"})
		return
	}
	if h.authzEnabled {
		need := auth.PermFileDelete
		if req.Action == "edit" || req.Action == "recode" {
			need = auth.PermMetadataEdit
		}
		if id := auth.FromContext(r.Context()); id == nil || !id.Has(need) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
			return
		}
	}

	// Resolve the target set: exactly one of {tagset_ids, filter}.
	hasIDs := len(req.TagsetIDs) > 0
	hasFilter := req.Filter != nil
	if hasIDs == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of tagset_ids or filter"})
		return
	}
	var tagsetIDs []int64
	if hasIDs {
		var err error
		tagsetIDs, err = normalizeBulkTagsetIDs(req.TagsetIDs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	} else {
		q := strings.TrimSpace(req.Filter.Q)
		if len(q) > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
			return
		}
		// An artist/album pin scopes the set (the entity view's whole-entity
		// delete), so it needs no "all" guard; a truly empty filter does.
		if q == "" && req.Filter.ArtistID == nil && req.Filter.AlbumID == nil && !req.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on the whole library without "all": true`})
			return
		}
		var err error
		tagsetIDs, err = h.repo.AppearanceIDsByFilter(r.Context(), database.FileFilter{
			Q: q, QField: normalizeQField(req.Filter.Field),
			ArtistID: req.Filter.ArtistID, AlbumID: req.Filter.AlbumID,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	if req.Action == "edit" {
		h.bulkEditLiveAppearances(w, r, tagsetIDs, req.Patch)
		return
	}

	if req.Action == "recode" {
		charset := req.Charset
		affected, notFound, err := h.repo.RecodeTagsetsText(r.Context(), tagsetIDs, sql.NullInt64{},
			func(s string) (string, bool) { return media.ReencodeLatin1(s, charset) })
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		failed := make([]map[string]any, 0, len(notFound))
		for _, id := range notFound {
			failed = append(failed, map[string]any{"tagset_id": id, "error": "appearance not found"})
		}
		h.audit(r.Context(), "metadata.bulk_recode", "appearances", fmt.Sprintf("%d recoded as %s", affected, charset))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected, "failed": failed})
		return
	}

	// trash: one batched deleted_at flip (soft delete never cascades — the
	// blobs and recordings stay; restore is the Trash lens's job).
	affected, err := h.repo.BulkTrashTagsets(r.Context(), tagsetIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	h.audit(r.Context(), "appearance.bulk_trash", "appearances", fmt.Sprintf("%d trashed", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

// bulkEditLiveAppearances — the live lens's arm of the shared bulk edit — lives
// with its two siblings in api/bulk_edit.go.
