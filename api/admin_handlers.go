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
)

// adminHashPattern matches a lowercase SHA-256 hex digest. The handler
// validates the {hash} path param before touching the DB or disk so a
// malformed value returns a clean 400 instead of a storage-layer error.
var adminHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	// ShareDepth is the madnetwork share scope (F5), three-valued like the
	// single-recording setter: absent = unchanged, null = inherit the node
	// default, a number = pin it. See api/share_depth.go.
	ShareDepth json.RawMessage `json:"share_depth"`
}

func (p *bulkEditPatch) hasTags() bool {
	m := p.metadataPatchRequest
	return m.Title != nil || m.Album != nil || m.AlbumArtist != nil || m.Artist != nil ||
		m.Genre != nil || m.Composer != nil || m.Comment != nil ||
		m.TrackNumber != nil || m.TrackTotal != nil || m.DiscNumber != nil || m.Year != nil
}
func (p *bulkEditPatch) hasAccess() bool {
	return p != nil && (p.License != nil || p.Guest != nil || len(p.ShareDepth) > 0)
}

// bulkEditAppearances applies one bulk tag patch by tagset id — the Trash lens's
// "fix a tag before restoring". Tags only: access (license / guest) is a
// recording-level property and is meaningless on a trashed appearance, so the
// Trash scope never offers it and a patch carrying it is rejected.
func (h *handler) bulkEditAppearances(w http.ResponseWriter, r *http.Request, tagsetIDs []int64, patch *bulkEditPatch) {
	if !patch.hasTags() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nothing to update"})
		return
	}
	if patch.hasAccess() {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "access is a recording property; it cannot be edited from Trash"})
		return
	}
	if len(tagsetIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0, "failed": []any{}})
		return
	}

	mp := database.MetadataPatch{
		Title: patch.Title, Album: patch.Album, AlbumArtist: patch.AlbumArtist, Artist: patch.Artist,
		Genre: patch.Genre, Composer: patch.Composer, Comment: patch.Comment,
		TrackNumber: patch.TrackNumber, TrackTotal: patch.TrackTotal, DiscNumber: patch.DiscNumber, Year: patch.Year,
	}
	affected, notFound, err := h.repo.BulkUpdateTagsetMetadata(r.Context(), tagsetIDs, mp)
	if errors.Is(err, database.ErrInvalidMetadata) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	failed := make([]map[string]any, 0, len(notFound))
	for _, id := range notFound {
		failed = append(failed, map[string]any{"tagset_id": id, "error": "appearance not found"})
	}
	h.audit(r.Context(), "metadata.bulk_edit", "appearances", fmt.Sprintf("%d updated", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected, "failed": failed})
}

// adminTrashList handles GET /api/admin/trash — the Trash page's **Appearances**
// lens: soft-deleted appearances, paged + filtered + sorted like the live library
// (docs/architecture/file-list-scaling.md). One row per appearance, keyed by
// tagset id (recording-tagsets P7c). Returns {total, items}; every trashed row is
// selectable, so there is no separate selectable_total. Newest-deleted first.
func (h *handler) adminTrashList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	filter := database.FileFilter{Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountTrashedAppearances(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}

	var entries []*database.AppearanceEntry
	if limit > 0 {
		sort := q.Get("sort")
		if sort == "" {
			sort = "deleted_desc"
		}
		entries, err = h.repo.ListTrashedAppearancesPage(r.Context(), database.FileListQuery{
			FileFilter: filter, Sort: sort, Limit: limit, Offset: offset,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	// The row is an APPEARANCE (recording-tagsets P7c): tagset_id is its identity,
	// not hash — two trashed appearances can share one blob, and an appearance
	// whose origin blob was absorbed or purged has no hash at all (empty string,
	// so the UI hides preview and size).
	type trashItem struct {
		TagsetID    int64  `json:"tagset_id"`
		RecordingID int64  `json:"recording_id"`
		Hash        string `json:"hash"`
		Filename    string `json:"filename"`
		Title       string `json:"title"`
		Artist      string `json:"artist"`
		// AlbumArtist is needed so the Trash page's metadata editor can prefill
		// it — the editor writes all four base tags, so an absent prefill would
		// silently clear album_artist on save.
		AlbumArtist string `json:"album_artist"`
		Album       string `json:"album"`
		// TrackNumber + DiscNumber + Year feed the grouped "By artist / album" sort.
		TrackNumber *int64 `json:"track_number"`
		DiscNumber  *int64 `json:"disc_number"`
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
		items = append(items, trashItem{
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

// trashBulk handles POST /api/admin/trash/bulk — a bulk Trash action ("restore"
// / "delete" / "edit") over an explicit **tagset_ids** list OR everything trashed
// matching a filter ("select all N matching"). The unit is the appearance, not
// the blob (recording-tagsets P7c): two trashed appearances can share one blob,
// so the old hash-addressed bulk acted on both while the UI showed one row.
// Per-action gate: restore/delete → file.delete, edit → metadata.edit. Each
// action is one transaction; the trashed/live guards live in the DB ops (the
// delete path skips a live appearance).
func (h *handler) trashBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Action    string  `json:"action"`
		TagsetIDs []int64 `json:"tagset_ids"`
		Filter    *struct {
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
		if q == "" && !req.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on the whole trash without "all": true`})
			return
		}
		var err error
		tagsetIDs, err = h.repo.TrashedAppearanceIDsByFilter(r.Context(), database.FileFilter{Q: q, QField: normalizeQField(req.Filter.Field)})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
	}

	if req.Action == "edit" {
		h.bulkEditAppearances(w, r, tagsetIDs, req.Patch)
		return
	}

	// Restore is a pure deleted_at flip with no storage side-effects, so it goes
	// through one batched transaction + one audit row. The old per-hash loop
	// opened two autocommit write transactions per file (restore + audit), which
	// made restore far slower than trashing and produced SQLITE_BUSY under
	// concurrent write pressure.
	if req.Action == "restore" {
		affected, err := h.repo.BulkRestoreTagsets(r.Context(), tagsetIDs)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		h.audit(r.Context(), "appearance.bulk_restore", "appearances", fmt.Sprintf("%d restored", affected))
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
	deleted, blobs, err := h.repo.BulkHardDeleteTagsets(r.Context(), tagsetIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	for _, b := range blobs {
		if _, rerr := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash); rerr != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, rerr)
		}
	}
	h.audit(r.Context(), "appearance.bulk_delete", "appearances", fmt.Sprintf("%d deleted", deleted))
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
