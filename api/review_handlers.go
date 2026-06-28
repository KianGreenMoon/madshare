package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// Review-bucket handlers (docs/architecture/moderation.md): the
// uploader-facing staging flow under /api/my/uploads and the moderator queue
// under /api/admin/moderation. States: draft -> submitted -> approved, with
// returned for "fix and resubmit".

// reviewItem is the JSON shape of one staged file, shared by the My-uploads
// and moderation listings. Uploader fields are present only in the latter.
type reviewItem struct {
	Hash        string   `json:"hash"`
	Filename    string   `json:"filename"`
	MimeType    string   `json:"mime_type"`
	ByteSize    int64    `json:"byte_size"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	AlbumArtist string   `json:"album_artist"`
	Album       string   `json:"album"`
	TrackNumber *int64   `json:"track_number"` // null when untagged (sorts the grouped view)
	DiscNumber  *int64   `json:"disc_number"`  // null when untagged (groups the grouped view)
	Year        int64    `json:"year,omitempty"`
	Duration    *float64 `json:"duration"` // seconds; null when not extracted
	State       string   `json:"state"`
	Note        string   `json:"note,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	SubmittedAt int64    `json:"submitted_at,omitempty"`
	UploaderID  int64    `json:"uploader_id,omitempty"`
	Uploader    string   `json:"uploader,omitempty"`
	// ArtistHasImage / AlbumHasImage drive the grouped view's "Add cover"
	// affordance (offered only when the entity has no cover yet).
	ArtistHasImage bool `json:"artist_has_image"`
	AlbumHasImage  bool `json:"album_has_image"`
	// Duplicate marks a queue row that duplicates already-approved content
	// (recordings P3); set only on the moderation listing. The queue highlights
	// it and the matching submission could never have self-approved.
	Duplicate bool `json:"duplicate,omitempty"`
}

func toReviewItem(e *database.ReviewEntry) reviewItem {
	var dur *float64
	if e.DurationSeconds.Valid {
		dur = &e.DurationSeconds.Float64
	}
	var trackNum *int64
	if e.TrackNumber.Valid {
		trackNum = &e.TrackNumber.Int64
	}
	var discNum *int64
	if e.DiscNumber.Valid {
		discNum = &e.DiscNumber.Int64
	}
	return reviewItem{
		Hash:           e.Hash,
		Filename:       e.Filename,
		MimeType:       e.MimeType,
		ByteSize:       e.ByteSize,
		URL:            "/files/" + e.ObjectKey,
		Title:          e.Title,
		Artist:         e.Artist.String,
		AlbumArtist:    e.AlbumArtist.String,
		Album:          e.Album.String,
		TrackNumber:    trackNum,
		DiscNumber:     discNum,
		Year:           e.Year.Int64,
		Duration:       dur,
		State:          e.ReviewState,
		Note:           e.ReviewNote.String,
		CreatedAt:      e.CreatedAt,
		SubmittedAt:    e.SubmittedAt.Int64,
		UploaderID:     e.UploaderID.Int64,
		Uploader:       e.UploaderName.String,
		ArtistHasImage: e.ArtistHasImage,
		AlbumHasImage:  e.AlbumHasImage,
	}
}

// myUploads handles GET /api/my/uploads — the caller's staged files (drafts,
// submitted, returned), paged + filtered + sorted like the live library
// (docs/architecture/file-list-scaling.md). Returns {total, selectable_total,
// items}: selectable_total is the editable subset (draft + returned) so the UI's
// "Select all N matching" banner counts only the rows it can act on. Gated on
// file.upload.
func (h *handler) myUploads(w http.ResponseWriter, r *http.Request) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}
	base := database.ReviewFilter{OwnerID: id.UserID, Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountUploadsByUser(r.Context(), base)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	selFilter := base
	selFilter.States = []string{database.ReviewDraft, database.ReviewReturned}
	selectable, err := h.repo.CountUploadsByUser(r.Context(), selFilter)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	var entries []*database.ReviewEntry
	if limit > 0 {
		entries, err = h.repo.ListUploadsByUserPage(r.Context(), database.ReviewListQuery{
			ReviewFilter: base, Sort: q.Get("sort"), Limit: limit, Offset: offset,
		})
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}
	items := make([]reviewItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toReviewItem(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "selectable_total": selectable, "limit": limit, "offset": offset, "items": items,
	})
}

// myUploadMetadata handles PATCH /api/my/uploads/{hash}/metadata — the
// owner-scoped tag edit for staged files. Same body/pointer semantics as the
// metadata.edit endpoint, but authorized by ownership + editable state (draft
// or returned) instead of the metadata.edit permission. 404 on anything the
// caller may not edit, so it does not reveal other users' staged files.
func (h *handler) myUploadMetadata(w http.ResponseWriter, r *http.Request) {
	hash, ok := h.ownStagedHash(w, r)
	if !ok {
		return
	}
	h.applyMetadataPatch(w, r, hash, "own staged file")
}

// myUploadMetadataGet handles GET /api/my/uploads/{hash}/metadata — the
// owner-scoped read of a staged file's full editable tag set, for the edit modal
// to populate before editing. Same ownership + editable-state guard as the PATCH.
func (h *handler) myUploadMetadataGet(w http.ResponseWriter, r *http.Request) {
	hash, ok := h.ownStagedHash(w, r)
	if !ok {
		return
	}
	meta, err := h.repo.FileMetadataByHash(r.Context(), hash)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, metadataJSON(hash, meta))
}

// ownStagedHash validates the {hash} path param and authorizes the caller as the
// owner of an editable (draft or returned) staged file. It writes the error
// response and returns ok=false on any failure — 404 on anything the caller may
// not see, so other users' staged files are never revealed.
func (h *handler) ownStagedHash(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return "", false
	}
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return "", false
	}
	state, uploadedBy, deleted, found, err := h.repo.FileReviewInfo(r.Context(), hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return "", false
	}
	editable := state == database.ReviewDraft || state == database.ReviewReturned
	owned := uploadedBy.Valid && uploadedBy.Int64 == id.UserID
	if !found || deleted || !editable || !owned {
		http.NotFound(w, r)
		return "", false
	}
	return hash, true
}

// myUploadDiscard handles DELETE /api/my/uploads/{hash} — the owner removes
// one of his own staged files (draft or returned → Trash, the regular soft
// delete; an admin can still restore it). Submitted files cannot be removed
// (no withdraw once sent to approval). 404 on anything the caller may not
// remove, revealing nothing about other users' files.
func (h *handler) myUploadDiscard(w http.ResponseWriter, r *http.Request) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	found, err := h.repo.DiscardOwnUpload(r.Context(), hash, id.UserID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.trash", hash, "owner-discard")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash})
}

// submitMyUploads handles POST /api/my/uploads/submit {hashes:[...]} — the
// "Send to approval" action. Each owned draft/returned file transitions to
// submitted; for content.moderate holders it goes straight to approved
// (moderators are the trusted uploaders — self-approve, no queue wait).
// Returns per-hash results; a hash that is not the caller's editable file
// reports ok=false without failing the batch.
func (h *handler) submitMyUploads(w http.ResponseWriter, r *http.Request) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.Hashes) == 0 {
		http.Error(w, "hashes is required", http.StatusBadRequest)
		return
	}

	hashes, err := normalizeBulkHashes(body.Hashes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	submitted, _, flagged, results, err := h.submitStaged(r.Context(), id, hashes)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	canSelfApprove := id.Has(auth.PermContentModerate)
	resp := map[string]any{
		"ok":        true,
		"submitted": submitted,
		// True only when self-approve was in effect AND nothing was held back as a
		// duplicate — i.e. everything actually reached the library.
		"approved": canSelfApprove && flagged == 0,
		"flagged":  flagged,
		"results":  results,
	}
	if flagged > 0 {
		resp["warning"] = duplicateWarning(flagged, canSelfApprove)
	}
	writeJSON(w, http.StatusOK, resp)
}

// submitResult is the per-hash outcome of a submit (ok=false when the hash was
// not the caller's editable file). Surfaced by the single /submit endpoint.
type submitResult struct {
	Hash string `json:"hash"`
	OK   bool   `json:"ok"`
}

// submitStaged applies the "send to approval" transition to a set of the
// caller's already-validated staged hashes, returning aggregate counts and
// per-hash results. content.moderate holders self-approve their own submissions
// — except a duplicate-flagged one, which always goes to the queue for a human
// look (recordings P3, docs/architecture/recordings.md). Shared by the single
// /submit endpoint and the /bulk submit action so their semantics stay identical.
func (h *handler) submitStaged(ctx context.Context, id *auth.Identity, hashes []string) (submitted, approved, flagged int, results []submitResult, err error) {
	canSelfApprove := id.Has(auth.PermContentModerate)
	results = make([]submitResult, 0, len(hashes))
	for _, hash := range hashes {
		dup, derr := h.repo.IsDuplicateSubmission(ctx, hash)
		if derr != nil {
			return submitted, approved, flagged, results, derr
		}
		selfApprove := canSelfApprove && !dup
		target := database.ReviewSubmitted
		if selfApprove {
			target = database.ReviewApproved
		}
		found, uerr := h.repo.UpdateReviewState(ctx, hash, database.ReviewTransition{
			From:             []string{database.ReviewDraft, database.ReviewReturned},
			To:               target,
			OwnerID:          id.UserID,
			StampSubmittedAt: true,
		})
		if uerr != nil {
			return submitted, approved, flagged, results, uerr
		}
		if found {
			submitted++
			if dup {
				flagged++
			}
			switch {
			case selfApprove:
				approved++
				h.audit(ctx, "file.approve", hash, "self")
			case dup:
				h.audit(ctx, "file.submit", hash, "duplicate-flagged")
			default:
				h.audit(ctx, "file.submit", hash, "")
			}
		}
		results = append(results, submitResult{Hash: hash, OK: found})
	}
	return submitted, approved, flagged, results, nil
}

// normalizeBulkHashes lower-cases + validates an explicit bulk hash list,
// rejecting an over-long list or any non-SHA-256 entry. Shared by the staging
// bulk paths.
func normalizeBulkHashes(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("hashes is required")
	}
	if len(raw) > bulkHashCap {
		return nil, errors.New("too many hashes")
	}
	out := make([]string, 0, len(raw))
	for _, h := range raw {
		h = strings.ToLower(strings.TrimSpace(h))
		if !isSHA256Hex(h) {
			return nil, errors.New("invalid hash (want 64 hex chars)")
		}
		out = append(out, h)
	}
	return out, nil
}

// reviewBulkRequest is the shared body of the two staging bulk endpoints: an
// explicit hash list OR a filter (resolved server-side), with `all` required to
// act on the whole (matching) set when the filter term is blank — the same
// guardrail adminBulkFiles uses.
type reviewBulkRequest struct {
	Action string   `json:"action"`
	Hashes []string `json:"hashes"`
	Filter *struct {
		Q     string `json:"q"`
		Field string `json:"field"`
	} `json:"filter"`
	All  bool   `json:"all"`
	Note string `json:"note"`
}

// myUploadsBulk handles POST /api/my/uploads/bulk — a bulk action over the
// caller's own staged files: "submit" (send to approval) or "remove" (discard to
// Trash). Targets an explicit hash list OR everything the owner has matching a
// filter (draft + returned only — submitted files can't be withdrawn). Gated on
// file.upload.
func (h *handler) myUploadsBulk(w http.ResponseWriter, r *http.Request) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req reviewBulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.Action != "submit" && req.Action != "remove" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	hashes, ok := h.resolveOwnUploadBulk(w, r.Context(), id.UserID, req)
	if !ok {
		return
	}

	if req.Action == "submit" {
		submitted, _, flagged, _, err := h.submitStaged(r.Context(), id, hashes)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		canSelfApprove := id.Has(auth.PermContentModerate)
		resp := map[string]any{"ok": true, "submitted": submitted, "approved": canSelfApprove && flagged == 0, "flagged": flagged}
		if flagged > 0 {
			resp["warning"] = duplicateWarning(flagged, canSelfApprove)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// action == "remove" — one batched owner-scoped soft delete + one summary
	// audit row, replacing the per-hash loop (two autocommit writes per file).
	removed, err := h.repo.BulkDiscardOwnUploads(r.Context(), hashes, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	if removed > 0 {
		h.audit(r.Context(), "file.bulk_trash", "files", fmt.Sprintf("%d removed (owner-discard)", removed))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

// resolveOwnUploadBulk turns a reviewBulkRequest into the caller's matching
// staged hashes — exactly one of {hashes, filter} (filter resolves to the
// owner's draft+returned set). It writes the HTTP error and returns ok=false on
// any problem.
func (h *handler) resolveOwnUploadBulk(w http.ResponseWriter, ctx context.Context, ownerID int64, req reviewBulkRequest) ([]string, bool) {
	hasHashes := len(req.Hashes) > 0
	hasFilter := req.Filter != nil
	if hasHashes == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of hashes or filter"})
		return nil, false
	}
	if hasHashes {
		hashes, err := normalizeBulkHashes(req.Hashes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return nil, false
		}
		return hashes, true
	}
	q := strings.TrimSpace(req.Filter.Q)
	if len(q) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return nil, false
	}
	if q == "" && !req.All {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on every staged file without "all": true`})
		return nil, false
	}
	hashes, err := h.repo.UploadHashesByUserFilter(ctx, database.ReviewFilter{
		OwnerID: ownerID, Q: q, QField: normalizeQField(req.Filter.Field),
		States: []string{database.ReviewDraft, database.ReviewReturned},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return nil, false
	}
	return hashes, true
}

// duplicateWarning is the popup message for a submission that looked like a
// duplicate of already-approved content. For a moderator it explains why
// self-approve was withheld; for a regular uploader it is purely informational.
func duplicateWarning(n int, moderator bool) string {
	subject := "1 file looks like a duplicate"
	if n != 1 {
		subject = fmt.Sprintf("%d files look like duplicates", n)
	}
	if moderator {
		return subject + " of content already in the library — sent for review instead of auto-approving."
	}
	return subject + " of content already in the library — a moderator will take a look."
}

// moderationList handles GET /api/admin/moderation — staged files with their
// uploader, paged + filtered + sorted (docs/architecture/file-list-scaling.md).
// Returns {total, selectable_total, items}: selectable_total counts the
// submitted (actionable) subset so the "Select all N matching" banner reflects
// only the rows the bulk actions act on. Gated on content.moderate.
func (h *handler) moderationList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}
	base := database.ReviewFilter{Q: q.Get("q"), QField: normalizeQField(q.Get("field"))}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountPendingReview(r.Context(), base)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	selFilter := base
	selFilter.States = []string{database.ReviewSubmitted}
	selectable, err := h.repo.CountPendingReview(r.Context(), selFilter)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	var entries []*database.ReviewEntry
	if limit > 0 {
		sort := q.Get("sort")
		if sort == "" {
			sort = "uploader" // the by-uploader grouping order, as before
		}
		entries, err = h.repo.ListPendingReviewPage(r.Context(), database.ReviewListQuery{
			ReviewFilter: base, Sort: sort, Limit: limit, Offset: offset,
		})
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}
	items := make([]reviewItem, 0, len(entries))
	for _, e := range entries {
		item := toReviewItem(e)
		// Highlight rows that duplicate already-approved content so the moderator
		// looks before publishing (recordings P3). Best-effort: a check error
		// leaves the row unflagged rather than failing the whole queue.
		if dup, derr := h.repo.IsDuplicateSubmission(r.Context(), e.Hash); derr == nil {
			item.Duplicate = dup
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "selectable_total": selectable, "limit": limit, "offset": offset, "items": items,
	})
}

// moderationBulk handles POST /api/admin/moderation/bulk — a bulk moderation
// action ("approve" / "return" / "discard") over an explicit hash list OR every
// submitted file matching a filter ("select all N matching"). Gated on
// content.moderate. Resolves the set to submitted rows server-side, then loops
// the same guarded transitions the per-row endpoints use.
func (h *handler) moderationBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req reviewBulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.Action != "approve" && req.Action != "return" && req.Action != "discard" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	// Discard is a soft delete; the per-row path needs file.delete, so the bulk
	// discard does too (the route only checks content.moderate).
	if req.Action == "discard" && h.authzEnabled {
		if id := auth.FromContext(r.Context()); id == nil || !id.Has(auth.PermFileDelete) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
			return
		}
	}
	var note string
	if req.Action == "return" {
		note = strings.TrimSpace(req.Note)
		if note == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "note is required"})
			return
		}
		if len(note) > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "note too long (max 1000 bytes)"})
			return
		}
	}
	hashes, ok := h.resolveModerationBulk(w, r.Context(), req)
	if !ok {
		return
	}

	// Each action is one batched transaction over the resolved set (the per-row
	// loop opened two autocommit writes — the state flip + a per-row audit — per
	// hash, a SQLITE_BUSY source under the "select all matching" scope). The audit
	// collapses to one summary row, as the other bulk paths already do.
	var affected int
	var err error
	switch req.Action {
	case "approve":
		affected, err = h.repo.BulkUpdateReviewState(r.Context(), hashes, database.ReviewTransition{
			From: []string{database.ReviewSubmitted, database.ReviewReturned}, To: database.ReviewApproved,
		})
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_approve", "files", fmt.Sprintf("%d approved", affected))
		}
	case "return":
		affected, err = h.repo.BulkUpdateReviewState(r.Context(), hashes, database.ReviewTransition{
			From: []string{database.ReviewSubmitted, database.ReviewReturned}, To: database.ReviewReturned, Note: note,
		})
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_return", "files", fmt.Sprintf("%d returned: %s", affected, note))
		}
	case "discard":
		// Discard is a soft delete; reuse the batched send-to-trash path.
		affected, err = h.repo.BulkSoftDeleteByHashes(r.Context(), hashes)
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_trash", "files", fmt.Sprintf("%d discarded", affected))
		}
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

// resolveModerationBulk turns a reviewBulkRequest into the matching submitted
// hashes — exactly one of {hashes, filter}. Writes the HTTP error and returns
// ok=false on any problem.
func (h *handler) resolveModerationBulk(w http.ResponseWriter, ctx context.Context, req reviewBulkRequest) ([]string, bool) {
	hasHashes := len(req.Hashes) > 0
	hasFilter := req.Filter != nil
	if hasHashes == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of hashes or filter"})
		return nil, false
	}
	if hasHashes {
		hashes, err := normalizeBulkHashes(req.Hashes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return nil, false
		}
		return hashes, true
	}
	q := strings.TrimSpace(req.Filter.Q)
	if len(q) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return nil, false
	}
	if q == "" && !req.All {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on the whole queue without "all": true`})
		return nil, false
	}
	hashes, err := h.repo.PendingReviewHashesByFilter(ctx, database.ReviewFilter{
		Q: q, QField: normalizeQField(req.Filter.Field), States: []string{database.ReviewSubmitted},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return nil, false
	}
	return hashes, true
}

// moderationApprove handles POST /api/admin/moderation/{hash}/approve —
// publishes a submitted (or returned: the moderator changed his mind) file
// into the library and clears any return note. Gated on content.moderate.
func (h *handler) moderationApprove(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	found, err := h.repo.UpdateReviewState(r.Context(), hash, database.ReviewTransition{
		From: []string{database.ReviewSubmitted, database.ReviewReturned},
		To:   database.ReviewApproved,
	})
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.approve", hash, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash, "state": database.ReviewApproved})
}

// moderationReturn handles POST /api/admin/moderation/{hash}/return
// {note:"..."} — sends a submitted file back to its uploader with a short
// required message (also re-returns with a fresh note when the moderator
// edits his feedback). Gated on content.moderate.
func (h *handler) moderationReturn(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(body.Note)
	if note == "" {
		http.Error(w, "note is required", http.StatusBadRequest)
		return
	}
	if len(note) > 1000 {
		http.Error(w, "note too long (max 1000 bytes)", http.StatusBadRequest)
		return
	}
	found, err := h.repo.UpdateReviewState(r.Context(), hash, database.ReviewTransition{
		From: []string{database.ReviewSubmitted, database.ReviewReturned},
		To:   database.ReviewReturned,
		Note: note,
	})
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.return", hash, note)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash, "state": database.ReviewReturned})
}
