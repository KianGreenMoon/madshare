package api

// The bulk tag edit — all three surfaces side by side: the Trash lens, the live
// All Appearances lens, and the uploader's own staging. They differ in whose
// rows they may touch, whether an access field is legal there, and what they
// report back; everything after that is one function, because a divergence in
// the shared tail is a silent bug (a copy that loses the ErrInvalidMetadata
// mapping answers a user's typo with a 500).
//
// Wire contract for all three: docs/api/bulk.md §"The edit patch".

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"daemonlord.ygg/madshare/database"
)

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
	if p == nil {
		return false
	}
	m := p.metadataPatchRequest
	return m.Title != nil || m.Album != nil || m.AlbumArtist != nil || m.Artist != nil ||
		m.Genre != nil || m.Composer != nil || m.Comment != nil ||
		m.TrackNumber != nil || m.TrackTotal != nil || m.DiscNumber != nil || m.Year != nil
}
func (p *bulkEditPatch) hasAccess() bool {
	return p != nil && (p.License != nil || p.Guest != nil || len(p.ShareDepth) > 0)
}

// tags is the DB-layer patch behind the request's tag fields — one mapping for
// every bulk-edit caller, so a field added to metadataPatchRequest cannot reach
// one of them and miss the others.
func (p *bulkEditPatch) tags() database.MetadataPatch {
	m := p.metadataPatchRequest
	return database.MetadataPatch{
		Title: m.Title, Album: m.Album, AlbumArtist: m.AlbumArtist, Artist: m.Artist,
		Genre: m.Genre, Composer: m.Composer, Comment: m.Comment,
		TrackNumber: m.TrackNumber, TrackTotal: m.TrackTotal, DiscNumber: m.DiscNumber, Year: m.Year,
	}
}

// checkTagsOnlyPatch validates a patch for a surface that does not own the
// recording behind the appearance: the Trash lens (access is meaningless on a
// trashed row) and the uploader's staging (access is not the uploader's to set).
// Both need at least one tag and no access field. It writes the 400 and returns
// false when that doesn't hold — surface names the refusing scope in the
// message, so a client can tell which of the two rules it hit.
func checkTagsOnlyPatch(w http.ResponseWriter, patch *bulkEditPatch, surface string) bool {
	if !patch.hasTags() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nothing to update"})
		return false
	}
	if patch.hasAccess() {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "access is a recording property; it cannot be edited from " + surface})
		return false
	}
	return true
}

// applyBulkTagPatch writes one tag patch across a resolved id set. A valid owner
// narrows the write to that user's own editable staging, so a scoped caller's
// explicit id list is trusted no further than ownership.
//
// It writes the HTTP error response itself and reports ok=false; on success it
// writes nothing, because the caller owns the reply — the live lens still has an
// access arm to run and one `affected` to reconcile across both.
//
// failed names the ids that matched nothing. A scoped caller should drop it:
// there a missing id is the scope working rather than a fault, and echoing it
// back would confirm the row exists.
func (h *handler) applyBulkTagPatch(w http.ResponseWriter, r *http.Request, tagsetIDs []int64,
	owner sql.NullInt64, patch *bulkEditPatch) (affected int, failed []map[string]any, ok bool) {

	affected, notFound, err := h.repo.BulkUpdateTagsetMetadata(r.Context(), tagsetIDs, owner, patch.tags())
	if errors.Is(err, database.ErrInvalidMetadata) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return 0, nil, false
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
		return 0, nil, false
	}
	failed = make([]map[string]any, 0, len(notFound))
	for _, id := range notFound {
		failed = append(failed, map[string]any{"tagset_id": id, "error": "appearance not found"})
	}
	return affected, failed, true
}

// bulkEditAppearances applies one bulk tag patch by tagset id — the Trash lens's
// "fix a tag before restoring". Tags only: access (license / guest) is a
// recording-level property and is meaningless on a trashed appearance, so the
// Trash scope never offers it and a patch carrying it is rejected.
func (h *handler) bulkEditAppearances(w http.ResponseWriter, r *http.Request, tagsetIDs []int64, patch *bulkEditPatch) {
	if !checkTagsOnlyPatch(w, patch, "Trash") {
		return
	}
	if len(tagsetIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0, "failed": []any{}})
		return
	}
	affected, failed, ok := h.applyBulkTagPatch(w, r, tagsetIDs, sql.NullInt64{}, patch)
	if !ok {
		return
	}
	h.audit(r.Context(), "metadata.bulk_edit", "appearances", fmt.Sprintf("%d updated", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected, "failed": failed})
}

// bulkEditLiveAppearances applies one bulk tag/access patch by tagset id. Tags
// go through the batched tagset patch; access (one value for the whole set)
// collapses to a guarded UPDATE per column on the recordings behind the set —
// license before guest, so an explicit guest wins over any license
// auto-derive. The only scope that owns those recordings, hence the only one
// that accepts an access field at all.
func (h *handler) bulkEditLiveAppearances(w http.ResponseWriter, r *http.Request, tagsetIDs []int64, patch *bulkEditPatch) {
	// Both predicates are false for a nil patch, so this guard is also what makes
	// patch safe to dereference below.
	tags := patch.hasTags()
	access := patch.hasAccess()
	if !tags && !access {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nothing to update"})
		return
	}
	if patch.License != nil && !knownLicenses[*patch.License] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown license"})
		return
	}
	depth, depthOK := parseShareDepthUpdate(patch.ShareDepth)
	if !depthOK {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid share_depth"})
		return
	}
	if len(tagsetIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0, "failed": []any{}})
		return
	}

	failed := make([]map[string]any, 0)
	affected := 0
	if tags {
		n, tagFailed, ok := h.applyBulkTagPatch(w, r, tagsetIDs, sql.NullInt64{}, patch)
		if !ok {
			return
		}
		affected, failed = n, tagFailed
	}
	if access {
		var accN int
		if patch.License != nil {
			n, err := h.repo.BulkSetLicenseByTagsets(r.Context(), tagsetIDs, *patch.License)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
				return
			}
			accN = n
		}
		if patch.Guest != nil {
			n, err := h.repo.BulkSetGuestPlayableByTagsets(r.Context(), tagsetIDs, *patch.Guest)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
				return
			}
			accN = n
		}
		if depth.Set {
			n, err := h.repo.BulkSetShareDepthByTagsets(r.Context(), tagsetIDs, depth)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
				return
			}
			accN = n
		}
		if !tags {
			affected = accN
		}
	}
	h.audit(r.Context(), "metadata.bulk_edit", "appearances", fmt.Sprintf("%d updated", affected))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected, "failed": failed})
}

// myUploadsBulkEdit applies one bulk tag patch across the caller's own staged
// appearances — the staging tab's "Edit tags…". The owner scope is what
// authorizes it (the caller holds file.upload, not metadata.edit), so ids
// outside their editable staging drop out in SQL and are not reported.
func (h *handler) myUploadsBulkEdit(w http.ResponseWriter, r *http.Request, tagsetIDs []int64, ownerID int64, patch *bulkEditPatch) {
	affected, _, ok := h.applyBulkTagPatch(w, r, tagsetIDs,
		sql.NullInt64{Int64: ownerID, Valid: true}, patch)
	if !ok {
		return
	}
	if affected > 0 {
		h.audit(r.Context(), "metadata.bulk_edit", "files", fmt.Sprintf("%d updated (owner)", affected))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": affected})
}
