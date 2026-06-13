package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// adminHashPattern matches a lowercase SHA-256 hex digest. The handler
// validates the {hash} path param before touching the DB or disk so a
// malformed value returns a clean 400 instead of a storage-layer error.
var adminHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// adminDeleteFile handles DELETE /api/admin/files/{hash}. It soft-deletes the
// file (sets deleted_at, blob stays on disk). The file moves to the trash
// bucket; use DELETE /api/admin/trash/{hash} for permanent removal.
func (h *handler) adminDeleteFile(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
		return
	}

	filenames, found, err := h.repo.SoftDeleteFileByHash(r.Context(), hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}

	if filenames == nil {
		filenames = []string{}
	}
	h.audit(r.Context(), "file.trash", hash, strings.Join(filenames, ", "))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"hash":      hash,
		"filenames": filenames,
	})
}

// adminTrashList handles GET /api/admin/trash. It returns all soft-deleted
// files ordered by deletion time descending.
func (h *handler) adminTrashList(w http.ResponseWriter, r *http.Request) {
	entries, err := h.repo.ListTrashedFiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}

	type trashItem struct {
		ID       int64  `json:"id"`
		Hash     string `json:"hash"`
		Filename string `json:"filename"`
		Title    string `json:"title"`
		Artist   string `json:"artist"`
		// AlbumArtist is needed so the Trash page's metadata editor can prefill
		// it — the editor writes all four base tags, so an absent prefill would
		// silently clear album_artist on save.
		AlbumArtist string `json:"album_artist"`
		Album       string `json:"album"`
		// TrackNumber + Year feed the grouped "By artist / album" sort.
		TrackNumber *int64 `json:"track_number"`
		Year        int64  `json:"year,omitempty"`
		ByteSize    int64  `json:"byte_size"`
		URL         string `json:"url"`
		DeletedAt   int64  `json:"deleted_at"`
		// ReviewState lets the Trash page badge rows that re-enter the
		// moderation queue (not the library) when restored.
		ReviewState string `json:"review_state"`
		// ArtistHasImage / AlbumHasImage drive the grouped view's "Add cover"
		// affordance (offered only when the entity has no cover yet).
		ArtistHasImage bool `json:"artist_has_image"`
		AlbumHasImage  bool `json:"album_has_image"`
	}

	items := make([]trashItem, 0, len(entries))
	for _, e := range entries {
		var trackNum *int64
		if e.TrackNumber.Valid {
			trackNum = &e.TrackNumber.Int64
		}
		items = append(items, trashItem{
			ID:             e.ID,
			Hash:           e.Hash,
			Filename:       e.Filename,
			Title:          e.Title,
			Artist:         e.Artist,
			AlbumArtist:    e.AlbumArtist.String,
			Album:          e.Album,
			TrackNumber:    trackNum,
			Year:           e.Year,
			ByteSize:       e.ByteSize,
			URL:            "/files/" + e.ObjectKey,
			DeletedAt:      e.DeletedAt.Int64,
			ReviewState:    e.ReviewState,
			ArtistHasImage: e.ArtistHasImage,
			AlbumHasImage:  e.AlbumHasImage,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// adminTrashHardDelete handles DELETE /api/admin/trash/{hash}. It permanently
// removes the DB row and the blob. HardDeleteFileByHash only matches trashed
// files, so live files return 404 atomically — no TOCTOU window.
func (h *handler) adminTrashHardDelete(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
		return
	}

	filenames, found, err := h.repo.HardDeleteTrashedFileByHash(r.Context(), hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}

	blobRemoved, err := h.storage.DeleteAll(hash)
	if err != nil {
		log.Printf("orphan blob: hash=%s err=%v", hash, err)
	}

	if filenames == nil {
		filenames = []string{}
	}
	h.audit(r.Context(), "file.delete", hash, strings.Join(filenames, ", "))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"hash":         hash,
		"blob_removed": blobRemoved,
		"filenames":    filenames,
	})
}

// adminTrashRestore handles POST /api/admin/trash/{hash}/restore. It restores
// a trashed file back to the live library.
func (h *handler) adminTrashRestore(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
		return
	}

	found, err := h.repo.RestoreFileByHash(r.Context(), hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}

	h.audit(r.Context(), "file.restore", hash, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash})
}

// adminPrune handles POST /api/admin/prune. An empty body is treated as a dry
// run. The request body is {"confirm": bool, "deep": bool}: confirm=true deletes
// every flagged record, and deep=true additionally rehashes each present blob to
// flag corrupted content (an integrity scan), not just missing files.
func (h *handler) adminPrune(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
		Deep    bool `json:"deep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}

	result, err := database.PruneDangling(r.Context(), h.repo, h.storage, body.Confirm, body.Deep)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "prune scan failed"})
		return
	}

	if !body.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"deep":           result.Deep,
			"scanned":        result.Scanned,
			"dangling":       danglingJSON(result.Dangling),
			"dangling_count": len(result.Dangling),
		})
		return
	}

	failed := make([]map[string]any, 0, len(result.Failed))
	for _, f := range result.Failed {
		failed = append(failed, map[string]any{"hash": f.Hash, "error": f.Err})
	}
	h.audit(r.Context(), "file.prune", "",
		fmt.Sprintf("deep=%v pruned %d, failed %d", result.Deep, len(result.Pruned), len(result.Failed)))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"dry_run":      false,
		"deep":         result.Deep,
		"scanned":      result.Scanned,
		"pruned":       danglingJSON(result.Pruned),
		"pruned_count": len(result.Pruned),
		"failed":       failed,
	})
}

// volumeStats is the disk-capacity portion of the storage response. It is
// present only for backends backed by a real filesystem (local disk); an
// object store (future S3) omits it (storageStatsResp.Volume is nil/null).
type volumeStats struct {
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// storageStatsResp is the body of GET /api/admin/storage. LibraryBytes (the
// app's own footprint, from the DB) is meaningful for every backend; Volume is
// the whole-disk capacity and is null when the backend has no fixed capacity.
type storageStatsResp struct {
	Backend      string       `json:"backend"`
	Location     string       `json:"location"`
	LibraryBytes int64        `json:"library_bytes"`
	Volume       *volumeStats `json:"volume"`
}

// adminStorageStats handles GET /api/admin/storage. It merges the backend's
// capacity (storage.Stats — disk statfs today) with the library's on-disk
// footprint (DB SUM of byte_size) so the admin dashboard can show free/used
// disk space and how much of it Madshare occupies.
func (h *handler) adminStorageStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.storage.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	libBytes, err := h.repo.LibraryByteSize(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}

	resp := storageStatsResp{Backend: st.Backend, Location: st.Location, LibraryBytes: libBytes}
	if st.HasVolume {
		var pct float64
		if st.TotalBytes > 0 {
			pct = float64(st.UsedBytes) / float64(st.TotalBytes) * 100
		}
		resp.Volume = &volumeStats{
			TotalBytes:  st.TotalBytes,
			FreeBytes:   st.FreeBytes,
			UsedBytes:   st.UsedBytes,
			UsedPercent: pct,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// danglingJSON shapes DanglingRefs into {hash, filenames, reason} objects,
// ensuring filenames is always a JSON array (never null).
func danglingJSON(refs []database.DanglingRef) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		names := ref.Filenames
		if names == nil {
			names = []string{}
		}
		out = append(out, map[string]any{"hash": ref.Hash, "filenames": names, "reason": ref.Reason})
	}
	return out
}
