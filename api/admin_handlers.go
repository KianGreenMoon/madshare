package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/prune"
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

// bulkHashCap bounds an explicit hash list in a bulk request, so a single call
// can't carry an unbounded body. The filter path (no hashes) has no cap — it
// resolves the set server-side. Hand-picked selections never approach this.
const bulkHashCap = 5000

// bulkEditPatch is a bulk metadata edit: the per-file tag set plus the access
// fields, applied to every resolved file (change-only / never-clear is the
// client's job — only filled fields are sent). License/Guest mirror the per-file
// access endpoints and need the content-access store (h.manage).
type bulkEditPatch struct {
	metadataPatchRequest
	License *string `json:"license"`
	Guest   *bool   `json:"guest"`
}

func (p *bulkEditPatch) hasTags() bool {
	m := p.metadataPatchRequest
	return m.Title != nil || m.Album != nil || m.AlbumArtist != nil || m.Artist != nil ||
		m.Genre != nil || m.Composer != nil || m.Comment != nil ||
		m.TrackNumber != nil || m.TrackTotal != nil || m.DiscNumber != nil || m.Year != nil
}
func (p *bulkEditPatch) hasAccess() bool { return p != nil && (p.License != nil || p.Guest != nil) }

// adminBulkFiles handles POST /api/admin/files/bulk — a bulk action over an
// explicit hash set OR everything matching a filter
// (docs/architecture/file-list-scaling.md). Actions: "trash" (move to Trash,
// needs file.delete) and "edit" (apply a tag/access patch, needs metadata.edit).
// The route admits either capability; this handler enforces the per-action gate.
//
// Exactly one of {hashes, filter} must be given. An empty filter means the
// whole (matching) library, which is refused unless "all": true — the guardrail
// the UI pairs with a strong confirm.
func (h *handler) adminBulkFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // a large hash list is still small
	var req struct {
		Action string   `json:"action"`
		Hashes []string `json:"hashes"`
		Filter *struct {
			Q        string `json:"q"`
			Field    string `json:"field"`
			ArtistID *int64 `json:"artist_id"`
			AlbumID  *int64 `json:"album_id"`
		} `json:"filter"`
		All   bool           `json:"all"`
		Patch *bulkEditPatch `json:"patch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.Action != "trash" && req.Action != "edit" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	// Per-action authorization (the route admits either capability).
	if h.authzEnabled {
		id := auth.FromContext(r.Context())
		need := auth.PermFileDelete
		if req.Action == "edit" {
			need = auth.PermMetadataEdit
		}
		if !id.Has(need) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
			return
		}
	}

	hasHashes := len(req.Hashes) > 0
	hasFilter := req.Filter != nil
	if hasHashes == hasFilter { // both, or neither
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of hashes or filter"})
		return
	}

	// Resolve the target set to a hash list (explicit, validated; or filter).
	var hashes []string
	if hasHashes {
		if len(req.Hashes) > bulkHashCap {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "too many hashes"})
			return
		}
		for _, hsh := range req.Hashes {
			if !adminHashPattern.MatchString(hsh) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
				return
			}
		}
		hashes = req.Hashes
	} else {
		filter := database.FileFilter{
			Q:        strings.TrimSpace(req.Filter.Q),
			QField:   normalizeQField(req.Filter.Field),
			ArtistID: req.Filter.ArtistID,
			AlbumID:  req.Filter.AlbumID,
		}
		if filter.Q == "" && filter.ArtistID == nil && filter.AlbumID == nil && !req.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on the whole library without "all": true`})
			return
		}
		var err error
		hashes, err = h.repo.FileHashesByFilter(r.Context(), filter)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	if req.Action == "edit" {
		h.bulkEditFiles(w, r, hashes, req.Patch)
		return
	}

	// action == "trash"
	if len(hashes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0})
		return
	}
	affected, err := h.repo.BulkSoftDeleteByHashes(r.Context(), hashes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	h.audit(r.Context(), "file.bulk_trash", "files", fmt.Sprintf("%d trashed", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

// bulkEditFiles applies a tag/access patch across the resolved hash set and
// reports the count plus any per-file failures. Tags go through the repository's
// batched BulkUpdateFileMetadata (one transaction per chunk — the per-file entity
// re-resolution can't collapse to a single UPDATE, but the transactions can);
// access (license/guest) carries one value for the whole set, so it collapses to
// a single guarded UPDATE through the content-access store, which an access edit
// needs h.manage to be wired for. License is applied before guest so an explicit
// guest wins over any license auto-derive.
func (h *handler) bulkEditFiles(w http.ResponseWriter, r *http.Request, hashes []string, patch *bulkEditPatch) {
	tags := patch != nil && patch.hasTags()
	access := patch.hasAccess()
	if !tags && !access {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nothing to update"})
		return
	}
	if access && h.manage == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "access editing unavailable"})
		return
	}
	if len(hashes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0, "failed": []any{}})
		return
	}

	failed := make([]map[string]string, 0)
	affected := 0
	if tags {
		mp := database.MetadataPatch{
			Title: patch.Title, Album: patch.Album, AlbumArtist: patch.AlbumArtist, Artist: patch.Artist,
			Genre: patch.Genre, Composer: patch.Composer, Comment: patch.Comment,
			TrackNumber: patch.TrackNumber, TrackTotal: patch.TrackTotal, DiscNumber: patch.DiscNumber, Year: patch.Year,
		}
		n, notFound, err := h.repo.BulkUpdateFileMetadata(r.Context(), hashes, mp)
		if errors.Is(err, database.ErrInvalidMetadata) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		affected = n
		for _, hsh := range notFound {
			failed = append(failed, map[string]string{"hash": hsh, "error": database.ErrFileNotFound.Error()})
		}
	}
	if access {
		var accN int
		if patch.License != nil {
			n, err := h.manage.BulkSetLicense(r.Context(), hashes, *patch.License)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
				return
			}
			accN = n
		}
		if patch.Guest != nil {
			n, err := h.manage.BulkSetGuestPlayable(r.Context(), hashes, *patch.Guest)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
				return
			}
			accN = n
		}
		// When only access changed, the affected count is the rows the access
		// UPDATE matched (tag-mode already reports the metadata rows updated).
		if !tags {
			affected = accN
		}
	}
	h.audit(r.Context(), "metadata.bulk_edit", "files", fmt.Sprintf("%d updated", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected, "failed": failed})
}

// adminTrashList handles GET /api/admin/trash — soft-deleted files, paged +
// filtered + sorted like the live library (docs/architecture/file-list-scaling.md).
// Returns {total, items}; every trashed row is selectable, so there is no
// separate selectable_total. Default order is newest-deleted-first.
func (h *handler) adminTrashList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	filter := database.FileFilter{Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountTrashedFiles(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}

	var entries []*database.FileListEntry
	if limit > 0 {
		sort := q.Get("sort")
		if sort == "" {
			sort = "deleted_desc"
		}
		entries, err = h.repo.ListTrashedFilesPage(r.Context(), database.FileListQuery{
			FileFilter: filter, Sort: sort, Limit: limit, Offset: offset,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "limit": limit, "offset": offset, "items": items,
	})
}

// adminTrashHardDelete handles DELETE /api/admin/trash/{hash}. It permanently
// removes the trashed appearance and cascades from the tagset (recording-tagsets
// P2): a non-last appearance keeps the blob, a last appearance takes the
// recording and all its files, whose bytes are reclaimed here after the row is
// gone. HardDeleteTrashedFileByHash only matches a trashed appearance, so live
// files return 404 atomically — no TOCTOU window.
func (h *handler) adminTrashHardDelete(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
		return
	}

	filenames, blobs, found, err := h.repo.HardDeleteTrashedFileByHash(r.Context(), hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}

	// Reclaim only the blobs the cascade actually took down (empty when a
	// non-last appearance was dropped and the recording's files survive). Each
	// blob carries its storage kind, so the reclaim is storage-aware: a links
	// import is unlinked (the symlink only — never the external target), a local
	// blob is os.RemoveAll'd.
	blobRemoved := false
	for _, b := range blobs {
		removed, rerr := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash)
		if rerr != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, rerr)
			continue
		}
		blobRemoved = blobRemoved || removed
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

// reclaimStorage removes a hard-deleted file's physical bytes, dispatching on its
// storage kind. A links import is unlinked via the Linker (os.Remove of the
// symlink only — the external target is NEVER touched, upholding the data-sources
// invariant), so the generic local DeleteAll (os.RemoveAll of a hash dir) is
// never invoked on a path that resolves through a link. A local blob (or an
// unknown/nil row) takes the historical DeleteAll path. f may be nil if the row
// vanished between lookup and delete; that falls through to the safe local path.
func (h *handler) reclaimStorage(f *database.File, hash string) (removed bool, err error) {
	if f != nil && f.StorageBackend == database.StorageBackendLinks {
		if h.linker == nil {
			// No links storage wired: cannot safely reclaim, and we must not run
			// the local DeleteAll on a links hash. Leave the symlink for the
			// reclaim tool rather than risk the wrong storage.
			return false, nil
		}
		if err := h.linker.Remove(hash); err != nil {
			return false, err
		}
		return true, nil
	}
	return h.storage.DeleteAll(hash)
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

// trashBulk handles POST /api/admin/trash/bulk — a bulk Trash action ("restore"
// / "delete" / "edit") over an explicit hash list OR everything trashed matching
// a filter ("select all N matching"). Gated on file.delete. It loops the same
// per-row logic the single-hash endpoints use (restore, storage-aware hard
// delete, tag edit), so the trashed/live guards are unchanged.
func (h *handler) trashBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Action string   `json:"action"`
		Hashes []string `json:"hashes"`
		Filter *struct {
			Q     string `json:"q"`
			Field string `json:"field"`
		} `json:"filter"`
		All   bool           `json:"all"`
		Patch *bulkEditPatch `json:"patch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.Action != "restore" && req.Action != "delete" && req.Action != "edit" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	// Per-action authorization (the route admits either capability).
	if h.authzEnabled {
		need := auth.PermFileDelete
		if req.Action == "edit" {
			need = auth.PermMetadataEdit
		}
		if id := auth.FromContext(r.Context()); id == nil || !id.Has(need) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
			return
		}
	}

	// Resolve the target set: exactly one of {hashes, filter}.
	hasHashes := len(req.Hashes) > 0
	hasFilter := req.Filter != nil
	if hasHashes == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of hashes or filter"})
		return
	}
	var hashes []string
	if hasHashes {
		var err error
		hashes, err = normalizeBulkHashes(req.Hashes)
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
		if q == "" && !req.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on the whole trash without "all": true`})
			return
		}
		var err error
		hashes, err = h.repo.TrashedFileHashesByFilter(r.Context(), database.FileFilter{Q: q, QField: normalizeQField(req.Filter.Field)})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	if req.Action == "edit" {
		h.bulkEditFiles(w, r, hashes, req.Patch)
		return
	}

	// Restore is a pure deleted_at flip with no storage side-effects, so it goes
	// through one batched transaction + one audit row (mirroring the send-to-trash
	// BulkSoftDeleteByHashes path). The old per-hash loop opened two autocommit
	// write transactions per file (restore + audit), which made restore far slower
	// than trashing and produced SQLITE_BUSY under concurrent write pressure.
	if req.Action == "restore" {
		affected, err := h.repo.BulkRestoreByHashes(r.Context(), hashes)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "file.bulk_restore", "files", fmt.Sprintf("%d restored", affected))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	// delete: the DB rows go in one batched transaction (each carrying its storage
	// kind out so the blob can be reclaimed once the row is gone), replacing the
	// old per-hash loop that opened two autocommit writes per file (delete + audit)
	// — the same slowness/SQLITE_BUSY fix already applied to bulk restore. Storage
	// reclamation (a local DeleteAll or a links unlink) is a filesystem op, so it
	// runs after the commit; a failure only orphans bytes (reconciled by prune),
	// never the reverse.
	deleted, blobs, err := h.repo.BulkHardDeleteTrashedByHashes(r.Context(), hashes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	for _, b := range blobs {
		if _, rerr := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash); rerr != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, rerr)
		}
	}
	h.audit(r.Context(), "file.bulk_delete", "files", fmt.Sprintf("%d deleted", deleted))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": deleted})
}

// adminPrune handles POST /api/admin/prune — it *starts* the single, server-wide
// prune background job and returns immediately (the scan, especially deep, is
// slow and now outlives the request). Body is {"confirm": bool, "deep": bool}:
// confirm=false starts a dry-run scan (the full damage sweep), confirm=true starts
// a prune that deletes exactly the set the last scan found and the admin reviewed.
// An empty body is treated as a scan. Returns 202 with the started snapshot, or
// 409 with the current snapshot when a prune is already running (so the page can
// render the in-progress state instead of starting a duplicate).
func (h *handler) adminPrune(w http.ResponseWriter, r *http.Request) {
	if h.pruneMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "prune unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		Confirm bool `json:"confirm"`
		Deep    bool `json:"deep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}

	by := ""
	if id := auth.FromContext(r.Context()); id != nil {
		by = id.Username
	}

	var (
		snap prune.Snapshot
		err  error
	)
	if body.Confirm {
		snap, err = h.pruneMgr.StartPrune(by)
	} else {
		snap, err = h.pruneMgr.StartScan(body.Deep, by)
	}
	switch {
	case errors.Is(err, prune.ErrBusy):
		writeJSON(w, http.StatusConflict, pruneStatusJSON(snap, "a prune is already running"))
		return
	case errors.Is(err, prune.ErrNoScan):
		writeJSON(w, http.StatusConflict, pruneStatusJSON(snap, "run a scan first"))
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "prune failed to start"})
		return
	}

	if body.Confirm {
		h.audit(r.Context(), "file.prune", "", fmt.Sprintf("deep=%v started prune", snap.Deep))
	}
	writeJSON(w, http.StatusAccepted, pruneStatusJSON(snap, ""))
}

// adminPruneStatus handles GET /api/admin/prune/status — the cheap, read-only,
// pollable shared view of the one prune operation. Any admin sees the same state:
// a running run's progress, or the persisted last-scan / last-prune summaries.
func (h *handler) adminPruneStatus(w http.ResponseWriter, r *http.Request) {
	if h.pruneMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "prune unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, pruneStatusJSON(h.pruneMgr.Snapshot(), ""))
}

// adminPruneCancel handles POST /api/admin/prune/cancel — stops the running prune.
// Reports whether a run was actually cancelled (false when already idle).
func (h *handler) adminPruneCancel(w http.ResponseWriter, r *http.Request) {
	if h.pruneMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "prune unavailable"})
		return
	}
	cancelled := h.pruneMgr.Cancel()
	if cancelled {
		h.audit(r.Context(), "file.prune", "", "cancelled")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": cancelled})
}

// pruneStatusJSON renders a prune snapshot into the response shape the admin page
// consumes. The DanglingRef / PruneFailure lists are converted via danglingJSON /
// failureJSON so their field names match the rest of the prune UI. errMsg, when
// non-empty, is included (used by the 409 responses).
func pruneStatusJSON(snap prune.Snapshot, errMsg string) map[string]any {
	out := map[string]any{
		"ok":    errMsg == "",
		"state": string(snap.State),
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	if snap.State == prune.StateRunning {
		out["phase"] = string(snap.Phase)
		out["deep"] = snap.Deep
		out["started_by"] = snap.StartedBy
		if snap.StartedAt != nil {
			out["started_at"] = snap.StartedAt
		}
		if snap.Progress != nil {
			out["progress"] = map[string]any{"scanned": snap.Progress.Scanned, "total": snap.Progress.Total}
		}
	}
	if snap.LastScan != nil {
		out["last_scan"] = snap.LastScan
	}
	if snap.LastPrune != nil {
		out["last_prune"] = snap.LastPrune
	}
	if d := snap.LastResult; d != nil {
		res := map[string]any{
			"kind":    d.Kind,
			"deep":    d.Deep,
			"scanned": d.Scanned,
			"outcome": d.Outcome,
		}
		if d.Kind == prune.KindScan {
			res["dangling"] = danglingJSON(d.Dangling)
			res["dangling_count"] = len(d.Dangling)
		} else {
			res["pruned"] = danglingJSON(d.Pruned)
			res["pruned_count"] = len(d.Pruned)
			res["failed"] = failureJSON(d.Failed)
		}
		out["last_result"] = res
	}
	return out
}

// failureJSON shapes prune failures as {hash, error} for the admin page.
func failureJSON(failures []database.PruneFailure) []map[string]any {
	out := make([]map[string]any, 0, len(failures))
	for _, f := range failures {
		out = append(out, map[string]any{"hash": f.Hash, "error": f.Err})
	}
	return out
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

// categoryUsage is one row of the storage response's per-category breakdown:
// the bytes occupied by that category (logical size — filesystem-compression-
// aware allocated sizing is intentionally out of scope; see storageStats).
type categoryUsage struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

// nonNegBytes converts a signed byte total (from a DB SUM) to uint64, clamping
// the impossible negative case to zero.
func nonNegBytes(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// storageStatsResp is the body of GET /api/admin/storage. Categories is the
// per-category footprint (audio, images, …) and LibraryBytes is their sum —
// both meaningful for every backend. Volume is the whole-disk capacity and is
// null when the backend has no fixed capacity (the future object-store path).
type storageStatsResp struct {
	Backend      string          `json:"backend"`
	Location     string          `json:"location"`
	LibraryBytes uint64          `json:"library_bytes"`
	Categories   []categoryUsage `json:"categories"`
	Volume       *volumeStats    `json:"volume"`
	// ExternalBytes is the bytes referenced by symlink imports (the links
	// storage), summed by stat-through-symlink. It lives physically OUTSIDE
	// data_dir, so it is reported separately and is NOT part of LibraryBytes or
	// the on-disk volume usage — importing 200 GB in place adds 0 on disk. Zero
	// when no links storage is wired. See docs/architecture/data-sources.md.
	ExternalBytes uint64 `json:"external_bytes"`
}

// adminStorageStats handles GET /api/admin/storage. It merges the backend's
// capacity (storage.Stats — disk statfs today) with the library's per-category
// footprint so the admin dashboard can show free/used disk space and how much
// of it Madshare occupies, broken down by category (audio, images, …).
func (h *handler) adminStorageStats(w http.ResponseWriter, r *http.Request) {
	resp, err := h.storageStats(r.Context())
	if err != nil {
		// One generic body for the client, but log the wrapped cause so an
		// operator can tell a statfs failure from a DB-query or image-walk
		// failure (all three otherwise look identical from the outside).
		log.Printf("storage stats: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// storageStats computes the storage breakdown: whole-volume capacity plus the
// per-category footprint. Sizing is hybrid: the files-table categories (audio,
// review, trash) come from one indexed byte_size sum (StorageByteBreakdown —
// instant and always fresh), while images are walked on disk (no byte size is
// tracked in the DB for cover variants). The audio/review/trash split is by
// state and is mutually exclusive. Sizes are logical (st_size / SUM(byte_size)),
// not filesystem-allocated: the payload is already-compressed media + images,
// so transparent FS compression saves negligibly and allocated sizing isn't
// worth the platform-specific stat-block accounting (docs/architecture/storage.md).
func (h *handler) storageStats(ctx context.Context) (*storageStatsResp, error) {
	st, err := h.storage.Stats()
	if err != nil {
		return nil, fmt.Errorf("backend stats: %w", err)
	}

	bd, err := h.repo.StorageByteBreakdown(ctx)
	if err != nil {
		return nil, fmt.Errorf("byte breakdown: %w", err)
	}
	imageBytes, err := storage.DirSize(h.imagesDir)
	if err != nil {
		return nil, fmt.Errorf("image dir size %q: %w", h.imagesDir, err)
	}

	// audio/review/trash are the same files table partitioned by state (one DB
	// sum), and images is the separate on-disk cover-variant tree — so the four
	// categories never double-count. audio = the live (approved, not-deleted)
	// library; review and trash are its non-approved and soft-deleted rows.
	cats := []categoryUsage{
		{Name: "audio", Bytes: nonNegBytes(bd.Library)},
		{Name: "review", Bytes: nonNegBytes(bd.Review)},
		{Name: "trash", Bytes: nonNegBytes(bd.Trash)},
		{Name: "images", Bytes: imageBytes},
	}
	var libBytes uint64
	for _, c := range cats {
		libBytes += c.Bytes
	}

	// The managed root (parent of the subtrees) is the meaningful "location";
	// fall back to the backend's own location when files_dir wasn't wired.
	location := h.filesDir
	if location == "" {
		location = st.Location
	}
	resp := &storageStatsResp{
		Backend:      st.Backend,
		Location:     location,
		LibraryBytes: libBytes,
		Categories:   cats,
	}
	// External (symlink-import) bytes are walked from the links storage and shown
	// separately — never folded into LibraryBytes or the volume usage above.
	if h.linker != nil {
		u, err := h.linker.Usage()
		if err != nil {
			return nil, fmt.Errorf("links usage: %w", err)
		}
		resp.ExternalBytes = u.ExternalBytes
	}
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
	return resp, nil
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
