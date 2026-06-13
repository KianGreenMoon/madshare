package api

import (
	"encoding/json"
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
// submitted, returned), newest first. Gated on file.upload.
func (h *handler) myUploads(w http.ResponseWriter, r *http.Request) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	entries, err := h.repo.ListUploadsByUser(r.Context(), id.UserID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	items := make([]reviewItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toReviewItem(e))
	}
	writeJSON(w, http.StatusOK, items)
}

// myUploadMetadata handles PATCH /api/my/uploads/{hash}/metadata — the
// owner-scoped tag edit for staged files. Same body/pointer semantics as the
// metadata.edit endpoint, but authorized by ownership + editable state (draft
// or returned) instead of the metadata.edit permission. 404 on anything the
// caller may not edit, so it does not reveal other users' staged files.
func (h *handler) myUploadMetadata(w http.ResponseWriter, r *http.Request) {
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
	state, uploadedBy, deleted, found, err := h.repo.FileReviewInfo(r.Context(), hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	editable := state == database.ReviewDraft || state == database.ReviewReturned
	owned := uploadedBy.Valid && uploadedBy.Int64 == id.UserID
	if !found || deleted || !editable || !owned {
		http.NotFound(w, r)
		return
	}
	h.applyMetadataPatch(w, r, hash, "own staged file")
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

	selfApprove := id.Has(auth.PermContentModerate)
	target := database.ReviewSubmitted
	if selfApprove {
		target = database.ReviewApproved
	}

	type result struct {
		Hash string `json:"hash"`
		OK   bool   `json:"ok"`
	}
	results := make([]result, 0, len(body.Hashes))
	submitted := 0
	for _, raw := range body.Hashes {
		hash := strings.ToLower(strings.TrimSpace(raw))
		if !isSHA256Hex(hash) {
			http.Error(w, "invalid hash (want 64 hex chars)", http.StatusBadRequest)
			return
		}
		found, err := h.repo.UpdateReviewState(r.Context(), hash, database.ReviewTransition{
			From:             []string{database.ReviewDraft, database.ReviewReturned},
			To:               target,
			OwnerID:          id.UserID,
			StampSubmittedAt: true,
		})
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if found {
			submitted++
			if selfApprove {
				h.audit(r.Context(), "file.approve", hash, "self")
			} else {
				h.audit(r.Context(), "file.submit", hash, "")
			}
		}
		results = append(results, result{Hash: hash, OK: found})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"submitted": submitted,
		"approved":  selfApprove,
		"results":   results,
	})
}

// moderationList handles GET /api/admin/moderation — every staged file with
// its uploader, ordered for by-uploader grouping. Gated on content.moderate.
func (h *handler) moderationList(w http.ResponseWriter, r *http.Request) {
	entries, err := h.repo.ListPendingReview(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	items := make([]reviewItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toReviewItem(e))
	}
	writeJSON(w, http.StatusOK, items)
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
