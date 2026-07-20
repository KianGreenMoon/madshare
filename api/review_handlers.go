package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/go-chi/chi/v5"
)

// parseTagsetID reads and validates the {tagsetID} path param (the review row
// identity, recording-tagsets P4).
func parseTagsetID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "tagsetID")), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// normalizeBulkTagsetIDs validates an explicit bulk tagset-id list, rejecting an
// over-long list or a non-positive id. Shared by the staging bulk paths.
func normalizeBulkTagsetIDs(raw []int64) ([]int64, error) {
	if len(raw) == 0 {
		return nil, errors.New("tagset_ids is required")
	}
	if len(raw) > bulkHashCap {
		return nil, errors.New("too many ids")
	}
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, errors.New("invalid tagset id")
		}
		out = append(out, id)
	}
	return out, nil
}

// metadataJSONTagset is the tagset-addressed echo of a metadata edit/read: the
// same field map as metadataJSON but keyed by tagset_id instead of a blob hash.
func metadataJSONTagset(tagsetID int64, m *database.MediaMetadata) map[string]any {
	j := metadataJSON("", m)
	delete(j, "hash")
	j["tagset_id"] = tagsetID
	return j
}

// applyTagsetMetadataPatch is the shared core of the tagset-addressed tag edits
// (My-uploads owner edit and the moderator's queue edit): it decodes the patch,
// writes it to the appearance, and echoes the combined row. Authorization runs
// before this is called.
func (h *handler) applyTagsetMetadataPatch(w http.ResponseWriter, r *http.Request, tagsetID int64, auditDetail string) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req metadataPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	patch := database.MetadataPatch{
		Title: req.Title, Album: req.Album, AlbumArtist: req.AlbumArtist, Artist: req.Artist,
		Genre: req.Genre, Composer: req.Composer, Comment: req.Comment,
		TrackNumber: req.TrackNumber, TrackTotal: req.TrackTotal, DiscNumber: req.DiscNumber, Year: req.Year,
	}
	meta, err := h.repo.UpdateTagsetMetadata(r.Context(), tagsetID, patch)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, database.ErrInvalidMetadata) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "metadata.edit", fmt.Sprintf("tagset:%d", tagsetID), auditDetail)
	writeJSON(w, http.StatusOK, metadataJSONTagset(tagsetID, meta))
}

// Review-bucket handlers (docs/architecture/moderation.md): the
// uploader-facing staging flow under /api/my/uploads and the moderator queue
// under /api/admin/moderation. States: draft -> submitted -> approved, with
// returned for "fix and resubmit".

// reviewItem is the JSON shape of one staged file, shared by the My-uploads
// and moderation listings. Uploader fields are present only in the latter.
type reviewItem struct {
	TagsetID    int64    `json:"tagset_id"` // the appearance — the row's identity (P4)
	Hash        string   `json:"hash"`      // origin blob (preview URL + admin ops)
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
	// Classification of the submission against the library (recording-tagsets P4;
	// moderation listing only). Class is one of new_recording / new_appearance /
	// no_new_bytes; RecordingID names where it lands; Collides marks an offered
	// appearance identical to one already approved. The ladder compare is fetched
	// per-row from the classify endpoint when a moderator expands the card.
	Class       string `json:"class,omitempty"`
	RecordingID int64  `json:"recording_id,omitempty"`
	Collides    bool   `json:"collides,omitempty"`
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
		TagsetID:       e.TagsetID,
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
	tagsetID, ok := h.ownStagedTagset(w, r)
	if !ok {
		return
	}
	h.applyTagsetMetadataPatch(w, r, tagsetID, "own staged appearance")
}

// myUploadMetadataGet handles GET /api/my/uploads/{hash}/metadata — the
// owner-scoped read of a staged file's full editable tag set, for the edit modal
// to populate before editing. Same ownership + editable-state guard as the PATCH.
func (h *handler) myUploadMetadataGet(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := h.ownStagedTagset(w, r)
	if !ok {
		return
	}
	meta, err := h.repo.TagsetMetadataByID(r.Context(), tagsetID)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, metadataJSONTagset(tagsetID, meta))
}

// ownStagedTagset validates the {tagsetID} path param and authorizes the caller
// as the owner of an editable (draft or returned) appearance. It writes the
// error response and returns ok=false on any failure — 404 on anything the
// caller may not see, so other users' staged appearances are never revealed.
func (h *handler) ownStagedTagset(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return 0, false
	}
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return 0, false
	}
	state, owner, deleted, found, err := h.repo.TagsetReviewInfo(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return 0, false
	}
	editable := state == database.ReviewDraft || state == database.ReviewReturned
	owned := owner.Valid && owner.Int64 == id.UserID
	if !found || deleted || !editable || !owned {
		http.NotFound(w, r)
		return 0, false
	}
	return tagsetID, true
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
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	found, err := h.repo.DiscardOwnUpload(r.Context(), tagsetID, id.UserID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.trash", fmt.Sprintf("tagset:%d", tagsetID), "owner-discard")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID})
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
		TagsetIDs []int64 `json:"tagset_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ids, err := normalizeBulkTagsetIDs(body.TagsetIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	submitted, flagged, err := h.submitStaged(r.Context(), id, ids)
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
	}
	if flagged > 0 {
		resp["warning"] = duplicateWarning(flagged, canSelfApprove)
	}
	writeJSON(w, http.StatusOK, resp)
}

// submitStaged applies the "send to approval" transition to a set of the
// caller's staged hashes, returning the totals: submitted (rows that actually
// transitioned, queued or self-approved) and flagged (duplicate-flagged rows
// among them). content.moderate holders self-approve their own submissions —
// except a duplicate-flagged one, which always goes to the queue for a human look
// (recordings P3, docs/architecture/recordings.md). Shared by the single /submit
// endpoint and the /bulk submit action so their semantics stay identical.
//
// The files are partitioned by the per-file duplicate check into buckets that
// share a transition, and each bucket is one batched UPDATE + one summary audit
// row — instead of an UPDATE plus an audit INSERT per hash, which was a
// SQLITE_BUSY source over the "select all matching" scope. The dup check is a
// read (no write lock), so looping it is cheap.
func (h *handler) submitStaged(ctx context.Context, id *auth.Identity, tagsetIDs []int64) (submitted, flagged int, err error) {
	canSelfApprove := id.Has(auth.PermContentModerate)

	var approveIDs, submitIDs, flaggedIDs []int64
	for _, tid := range tagsetIDs {
		// A submission that matches already-published audio is the duplicate that
		// always queues (never self-approves) — read off the classification.
		sc, ok, cerr := h.repo.ClassifySubmission(ctx, tid)
		if cerr != nil {
			return 0, 0, cerr
		}
		dup := ok && sc.MatchedExisting
		switch {
		case dup: // a duplicate always goes to the queue, even for a moderator
			flaggedIDs = append(flaggedIDs, tid)
		case canSelfApprove:
			approveIDs = append(approveIDs, tid)
		default:
			submitIDs = append(submitIDs, tid)
		}
	}

	from := []string{database.ReviewDraft, database.ReviewReturned}
	apply := func(set []int64, to, action, detail string) (int, error) {
		if len(set) == 0 {
			return 0, nil
		}
		n, aerr := h.repo.BulkUpdateReviewState(ctx, set, database.ReviewTransition{
			From: from, To: to, OwnerID: id.UserID, StampSubmittedAt: true,
		})
		if aerr != nil {
			return 0, aerr
		}
		if n > 0 {
			h.audit(ctx, action, "tagsets", fmt.Sprintf("%d %s", n, detail))
		}
		return n, nil
	}

	approved, err := apply(approveIDs, database.ReviewApproved, "file.bulk_approve", "self-approved")
	if err != nil {
		return 0, 0, err
	}
	if approved > 0 {
		h.repointRemotes(ctx) // freshly approved content may back remote playlist rows
	}
	submittedClean, err := apply(submitIDs, database.ReviewSubmitted, "file.bulk_submit", "submitted")
	if err != nil {
		return 0, 0, err
	}
	flaggedFound, err := apply(flaggedIDs, database.ReviewSubmitted, "file.bulk_submit", "submitted (duplicate-flagged)")
	if err != nil {
		return 0, 0, err
	}
	return approved + submittedClean + flaggedFound, flaggedFound, nil
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
// guardrail the other bulk endpoints use.
type reviewBulkRequest struct {
	Action    string  `json:"action"`
	TagsetIDs []int64 `json:"tagset_ids"`
	Filter    *struct {
		Q     string `json:"q"`
		Field string `json:"field"`
	} `json:"filter"`
	All     bool   `json:"all"`
	Note    string `json:"note"`
	Charset string `json:"charset"` // action "recode" only: the target charset
}

// myUploadsBulk handles POST /api/my/uploads/bulk — a bulk action over the
// caller's own staged files: "submit" (send to approval), "remove" (discard to
// Trash), or "recode" (the bulk charset fix: reinterpret the stored text tags
// in the given charset, docs/architecture/tag-suggestions.md). Targets an
// explicit hash list OR everything the owner has matching a filter (draft +
// returned only — submitted files can't be withdrawn or edited). Gated on
// file.upload; ownership scoping happens in resolveOwnUploadBulk.
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
	if req.Action != "submit" && req.Action != "remove" && req.Action != "recode" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown action"})
		return
	}
	if req.Action == "recode" && !media.ValidCharset(req.Charset) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown charset"})
		return
	}
	ids, ok := h.resolveOwnUploadBulk(w, r.Context(), id.UserID, req)
	if !ok {
		return
	}

	if req.Action == "recode" {
		// The explicit-ids path is trusted only as far as ownership: the owner
		// scope restricts the recode to the caller's own editable staging.
		charset := req.Charset
		affected, _, err := h.repo.RecodeTagsetsText(r.Context(), ids,
			sql.NullInt64{Int64: id.UserID, Valid: true},
			func(s string) (string, bool) { return media.ReencodeLatin1(s, charset) })
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
			return
		}
		if affected > 0 {
			h.audit(r.Context(), "metadata.bulk_recode", "files", fmt.Sprintf("%d recoded as %s (owner)", affected, charset))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
		return
	}

	if req.Action == "submit" {
		submitted, flagged, err := h.submitStaged(r.Context(), id, ids)
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
	// audit row, replacing the per-row loop (two autocommit writes per appearance).
	removed, err := h.repo.BulkDiscardOwnUploads(r.Context(), ids, id.UserID)
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
func (h *handler) resolveOwnUploadBulk(w http.ResponseWriter, ctx context.Context, ownerID int64, req reviewBulkRequest) ([]int64, bool) {
	hasIDs := len(req.TagsetIDs) > 0
	hasFilter := req.Filter != nil
	if hasIDs == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of tagset_ids or filter"})
		return nil, false
	}
	if hasIDs {
		ids, err := normalizeBulkTagsetIDs(req.TagsetIDs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return nil, false
		}
		return ids, true
	}
	q := strings.TrimSpace(req.Filter.Q)
	if len(q) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return nil, false
	}
	if q == "" && !req.All {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `refusing to act on every staged appearance without "all": true`})
		return nil, false
	}
	ids, err := h.repo.UploadTagsetIDsByUserFilter(ctx, database.ReviewFilter{
		OwnerID: ownerID, Q: q, QField: normalizeQField(req.Filter.Field),
		States: []string{database.ReviewDraft, database.ReviewReturned},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return nil, false
	}
	return ids, true
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
		// Classify the submission against the library so the queue can head each
		// row with case A/B/C + the appearance-collision flag (recording-tagsets
		// P4); Duplicate (self-approve suppression, recordings P3) is the
		// "matched existing audio" subset. Best-effort: a check error leaves the
		// row unclassified rather than failing the whole queue.
		if sc, ok, cerr := h.repo.ClassifySubmission(r.Context(), e.TagsetID); cerr == nil && ok {
			item.Class = sc.Case
			item.RecordingID = sc.RecordingID
			item.Collides = sc.CollidesAppearance
			item.Duplicate = sc.MatchedExisting
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
	ids, ok := h.resolveModerationBulk(w, r.Context(), req)
	if !ok {
		return
	}

	// Each action is one batched transaction over the resolved set (the per-row
	// loop opened two autocommit writes — the state flip + a per-row audit — per
	// row, a SQLITE_BUSY source under the "select all matching" scope). The audit
	// collapses to one summary row, as the other bulk paths already do.
	var affected int
	var err error
	switch req.Action {
	case "approve":
		affected, err = h.repo.BulkUpdateReviewState(r.Context(), ids, database.ReviewTransition{
			From: []string{database.ReviewSubmitted, database.ReviewReturned}, To: database.ReviewApproved,
		})
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_approve", "tagsets", fmt.Sprintf("%d approved", affected))
			h.repointRemotes(r.Context())
		}
	case "return":
		affected, err = h.repo.BulkUpdateReviewState(r.Context(), ids, database.ReviewTransition{
			From: []string{database.ReviewSubmitted, database.ReviewReturned}, To: database.ReviewReturned, Note: note,
		})
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_return", "tagsets", fmt.Sprintf("%d returned: %s", affected, note))
		}
	case "discard":
		// Discard is a soft delete of the appearance (tagset Trash).
		affected, err = h.repo.BulkTrashTagsets(r.Context(), ids)
		if affected > 0 {
			h.audit(r.Context(), "file.bulk_trash", "tagsets", fmt.Sprintf("%d discarded", affected))
		}
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}

// resolveModerationBulk turns a reviewBulkRequest into the matching submitted
// tagset ids — exactly one of {tagset_ids, filter}. Writes the HTTP error and
// returns ok=false on any problem.
func (h *handler) resolveModerationBulk(w http.ResponseWriter, ctx context.Context, req reviewBulkRequest) ([]int64, bool) {
	hasIDs := len(req.TagsetIDs) > 0
	hasFilter := req.Filter != nil
	if hasIDs == hasFilter {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide exactly one of tagset_ids or filter"})
		return nil, false
	}
	if hasIDs {
		ids, err := normalizeBulkTagsetIDs(req.TagsetIDs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return nil, false
		}
		return ids, true
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
	ids, err := h.repo.PendingReviewTagsetIDsByFilter(ctx, database.ReviewFilter{
		Q: q, QField: normalizeQField(req.Filter.Field), States: []string{database.ReviewSubmitted},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return nil, false
	}
	return ids, true
}

// moderationApprove handles POST /api/admin/moderation/{tagsetID}/approve —
// publishes a submitted (or returned: the moderator changed his mind)
// appearance into the library and clears any return note. Gated on
// content.moderate.
func (h *handler) moderationApprove(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	// Optional per-piece decisions (recording-tagsets P4); an empty body is a
	// plain approve (keep bytes, keep recording assignment).
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		DropBytes bool `json:"drop_bytes"` // keep the appearance, soft-remove the submitted rendition
		ForceNew  bool `json:"force_new"`  // split into a new pinned recording first
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	found, err := h.repo.ApproveSubmission(r.Context(), tagsetID, body.DropBytes, body.ForceNew)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	detail := ""
	if body.ForceNew {
		detail = "force-new"
	}
	if body.DropBytes {
		detail = strings.TrimSpace(detail + " drop-bytes")
	}
	h.audit(r.Context(), "file.approve", fmt.Sprintf("tagset:%d", tagsetID), detail)
	h.repointRemotes(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID, "state": database.ReviewApproved})
}

// moderationDiscard handles POST /api/admin/moderation/{tagsetID}/discard —
// denies one submission by trashing its appearance (tagset Trash; the blob and
// recording stay, so a case-B better rendition keeps serving). A discard is a
// soft delete, so it needs file.delete on top of the route's content.moderate.
func (h *handler) moderationDiscard(w http.ResponseWriter, r *http.Request) {
	if h.authzEnabled {
		if id := auth.FromContext(r.Context()); id == nil || !id.Has(auth.PermFileDelete) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
			return
		}
	}
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	n, err := h.repo.BulkTrashTagsets(r.Context(), []int64{tagsetID})
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.trash", fmt.Sprintf("tagset:%d", tagsetID), "moderator-discard")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID})
}

// moderationReturn handles POST /api/admin/moderation/{hash}/return
// {note:"..."} — sends a submitted file back to its uploader with a short
// required message (also re-returns with a fresh note when the moderator
// edits his feedback). Gated on content.moderate.
func (h *handler) moderationReturn(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
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
	found, err := h.repo.UpdateReviewState(r.Context(), tagsetID, database.ReviewTransition{
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
	h.audit(r.Context(), "file.return", fmt.Sprintf("tagset:%d", tagsetID), note)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID, "state": database.ReviewReturned})
}

// moderationMetadataGet handles GET /api/admin/moderation/{tagsetID}/metadata —
// the full editable tags of one submission's appearance, for the moderator's
// edit modal to prefill. Gated on content.moderate.
func (h *handler) moderationMetadataGet(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	meta, err := h.repo.TagsetMetadataByID(r.Context(), tagsetID)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, metadataJSONTagset(tagsetID, meta))
}

// moderationMetadata handles PATCH /api/admin/moderation/{tagsetID}/metadata —
// the moderator edits one submission's appearance tags before approving.
// Gated on content.moderate (a moderator may edit any submission's tags — the
// tagset addressing scopes it to the exact appearance under review).
func (h *handler) moderationMetadata(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	h.applyTagsetMetadataPatch(w, r, tagsetID, "moderator edit")
}

// moderationClassify handles GET /api/admin/moderation/{tagsetID}/classify — the
// full classification of one staged submission (recording-tagsets P4): case
// A/B/C, the recording it lands on, the appearance-collision flag, and the
// quality-ladder compare (current best rendition vs the submitted blob) the
// moderator's expanded card renders. Gated on content.moderate. 404 on a hash
// that is not a live pending submission (revealing nothing about the library).
func (h *handler) moderationClassify(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	sc, ok, err := h.repo.ClassifySubmission(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	resp := map[string]any{
		"tagset_id":             tagsetID,
		"class":                 sc.Case,
		"recording_id":          sc.RecordingID,
		"matched":               sc.MatchedExisting,
		"collides":              sc.CollidesAppearance,
		"submitted_is_new_best": sc.SubmittedIsNewBest,
		"submitted":             renditionJSON(sc.Submitted),
	}
	if sc.CurrentBest != nil {
		resp["current_best"] = renditionJSON(*sc.CurrentBest)
	}
	writeJSON(w, http.StatusOK, resp)
}

// renditionJSON is the wire shape of one rendition in the classify compare: the
// tech fields the quality ladder ranks on plus its blob hash. Zero tech fields
// mean "unknown" (ffprobe absent), which the client renders as a dash.
func renditionJSON(r database.Rendition) map[string]any {
	return map[string]any{
		"file_id":     r.FileID,
		"hash":        r.Hash,
		"codec":       r.Codec,
		"bitrate":     r.Bitrate,
		"sample_rate": r.SampleRate,
		"bit_depth":   r.BitDepth,
		"byte_size":   r.ByteSize,
	}
}
