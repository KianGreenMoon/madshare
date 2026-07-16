package api

// Trash — the Recordings and Files perspectives (soft-delete.md). The
// Appearances perspective is the pre-existing /api/admin/trash surface
// (admin_handlers.go); these add the recording-grain and file-grain lenses:
// listing the trashed-recording bin / removed blobs, restoring them back into
// the library, and the Trash-only permanent delete (all file.delete-gated,
// matching the rest of the Trash page). Permanent deletion lives only here —
// /admin/library#recordings does soft ops only.

import (
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// bulkIDsMax caps an explicit-id bulk request (the Recordings/Files bins select
// concrete rows; there is no "select all N matching" over these lenses yet).
const bulkIDsMax = 10000

// reclaimBlobs removes the physical bytes of every hard-deleted blob, dispatching
// on storage kind (reclaimStorage), and returns how many were actually removed.
// Shared by the perspective delete handlers.
func (h *handler) reclaimBlobs(blobs []database.DeletedBlob) int {
	removed := 0
	for _, b := range blobs {
		ok, err := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash)
		if err != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, err)
			continue
		}
		if ok {
			removed++
		}
	}
	return removed
}

// ── Recordings perspective ──────────────────────────────────────────────────

// adminTrashRecordingsList handles GET /api/admin/trash/recordings — the
// trashed-recording bin (recordings wholly out of the library), paged, newest
// first, reusing the recordings-listing row shape. Gated file.delete.
func (h *handler) adminTrashRecordingsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	search := q.Get("q")
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountTrashedRecordings(r.Context(), search)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	var rows []database.RecordingRow
	if limit > 0 {
		rows, err = h.repo.ListTrashedRecordings(r.Context(), search, limit, offset)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}
	items := make([]recordingRowDTO, 0, len(rows))
	for _, rec := range rows {
		items = append(items, recordingRowDTO{
			ID: rec.ID, Title: rec.Title, Artist: rec.DisplayArtist,
			LiveRenditions: rec.LiveRenditions, RemovedFiles: rec.RemovedFiles,
			Appearances: rec.Appearances, TrashedAppearances: rec.TrashedTagsets,
			BestFormat: rec.BestFormat, Dormant: rec.Dormant, Pinned: rec.Pinned,
			License: rec.License, GuestPlayable: rec.GuestPlayable, CreatedAt: rec.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "items": items})
}

// recordingsRestore handles POST /api/admin/recordings/{recordingID}/restore —
// bring a whole recording back into the library (un-trash every appearance and,
// if dormant, restore its best rendition). Gated file.delete.
func (h *handler) recordingsRestore(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	found, err := h.repo.RestoreRecording(r.Context(), recID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "recording.restore", strconv.FormatInt(recID, 10), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// trashRecordingsBulk handles POST /api/admin/trash/recordings/bulk — the
// Recordings bin's "Restore selected" / "Delete selected" over an explicit id
// list or the whole bin (body {action, ids} / {action, all:true}). Gated
// file.delete.
func (h *handler) trashRecordingsBulk(w http.ResponseWriter, r *http.Request) {
	action, ids, all, ok := decodeBulkIDs(w, r)
	if !ok {
		return
	}
	if all {
		var err error
		if ids, err = h.repo.TrashedRecordingIDs(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}
	switch action {
	case "restore":
		n, err := h.repo.BulkRestoreRecordings(r.Context(), ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "recording.bulk_restore", "recordings", strconv.Itoa(n)+" restored")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": n})
	case "delete":
		n, blobs, err := h.repo.BulkHardDeleteRecordings(r.Context(), ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "recording.bulk_delete", "recordings", strconv.Itoa(n)+" deleted")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": n, "blobs_removed": h.reclaimBlobs(blobs)})
	}
}

// ── Files perspective ───────────────────────────────────────────────────────

type removedFileDTO struct {
	ID             int64    `json:"id"`
	Hash           string   `json:"hash"`
	Filename       string   `json:"filename"`
	Title          string   `json:"title"`
	Artist         string   `json:"artist"`
	Album          string   `json:"album"`
	ByteSize       int64    `json:"byte_size"`
	URL            string   `json:"url"`
	Duration       *float64 `json:"duration"`
	RemovedAt      int64    `json:"removed_at"`
	StorageBackend string   `json:"storage_backend"`
	RecordingID    int64    `json:"recording_id"`
}

// adminTrashFilesList handles GET /api/admin/trash/files — the removed-blob bin
// (files.deleted_at), paged, newest-removed first. Gated file.delete.
func (h *handler) adminTrashFilesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	filter := database.FileFilter{Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountRemovedFiles(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	var entries []*database.FileListEntry
	if limit > 0 {
		sort := q.Get("sort")
		if sort == "" {
			sort = "removed_desc"
		}
		entries, err = h.repo.ListRemovedFilesPage(r.Context(), database.FileListQuery{
			FileFilter: filter, Sort: sort, Limit: limit, Offset: offset,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}
	items := make([]removedFileDTO, 0, len(entries))
	for _, e := range entries {
		var dur *float64
		if e.DurationSeconds.Valid {
			dur = &e.DurationSeconds.Float64
		}
		items = append(items, removedFileDTO{
			ID: e.ID, Hash: e.Hash, Filename: e.Filename, Title: e.Title, Artist: e.Artist,
			Album: e.Album, ByteSize: e.ByteSize, URL: "/files/" + e.ObjectKey, Duration: dur,
			RemovedAt: e.DeletedAt.Int64, StorageBackend: e.StorageBackend, RecordingID: e.RecordingID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "items": items})
}

// renditionHardDelete handles DELETE /api/admin/renditions/{fileID} — the Files
// bin's permanent delete of a soft-removed blob: a non-last file drops only its
// blob (live appearances repointed), the last file cascade-prunes the recording.
// Refuses a live file (found=false → 404). Gated file.delete.
func (h *handler) renditionHardDelete(w http.ResponseWriter, r *http.Request) {
	fileID, err := strconv.ParseInt(chi.URLParam(r, "fileID"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "file id must be a positive integer", http.StatusBadRequest)
		return
	}
	blobs, found, err := h.repo.HardDeleteRemovedFile(r.Context(), fileID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.delete", strconv.FormatInt(fileID, 10), "removed rendition")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "blobs_removed": h.reclaimBlobs(blobs)})
}

// trashFilesBulk handles POST /api/admin/trash/files/bulk — the Files bin's
// "Restore selected" / "Delete selected" over an explicit file-id list or the
// whole bin (body {action, ids} / {action, all:true}). Gated file.delete.
func (h *handler) trashFilesBulk(w http.ResponseWriter, r *http.Request) {
	action, ids, all, ok := decodeBulkIDs(w, r)
	if !ok {
		return
	}
	if all {
		var err error
		if ids, err = h.repo.RemovedFileIDsByFilter(r.Context(), database.FileFilter{}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}
	switch action {
	case "restore":
		n, err := h.repo.BulkRestoreRemovedFiles(r.Context(), ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "file.bulk_restore", "files", strconv.Itoa(n)+" restored")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": n})
	case "delete":
		n, blobs, err := h.repo.BulkHardDeleteRemovedFiles(r.Context(), ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "file.bulk_delete", "files", strconv.Itoa(n)+" deleted")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": n, "blobs_removed": h.reclaimBlobs(blobs)})
	}
}

// decodeBulkIDs parses the shared bulk body for the Trash bins' id-addressed
// bulk endpoints: {action, ids} (a non-empty id list within the cap) or
// {action, all:true} (the whole bin — the UI's "Select all N"; the handler
// resolves the id set server-side). ids and all are mutually exclusive.
// Allowed actions: restore, delete. It writes the error response itself and
// returns ok=false on any problem.
func decodeBulkIDs(w http.ResponseWriter, r *http.Request) (action string, ids []int64, all bool, ok bool) {
	actions := []string{"restore", "delete"}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Action string  `json:"action"`
		IDs    []int64 `json:"ids"`
		All    bool    `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return "", nil, false, false
	}
	if !slices.Contains(actions, req.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return "", nil, false, false
	}
	if req.All && len(req.IDs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ids and all are mutually exclusive"})
		return "", nil, false, false
	}
	if !req.All && len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ids is required"})
		return "", nil, false, false
	}
	if len(req.IDs) > bulkIDsMax {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "too many ids"})
		return "", nil, false, false
	}
	return req.Action, req.IDs, req.All, true
}
