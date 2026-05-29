package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// adminHashPattern matches a lowercase SHA-256 hex digest. The handler
// validates the {hash} path param before touching the DB or disk so a
// malformed value returns a clean 400 instead of a storage-layer error.
var adminHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// adminDeleteFile handles DELETE /api/admin/files/{hash}. It deletes the DB
// record first, then the blob: with DB-first ordering a blob-delete failure
// leaves a reconcilable orphan rather than a dangling row pointing at nothing.
func (h *handler) adminDeleteFile(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid hash"})
		return
	}

	filenames, found, err := h.repo.DeleteFileByHash(r.Context(), hash)
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
		// The row is gone but the blob lingers — the reconciler will sweep it.
		// Mirror the upload handler's orphan log format and still report success.
		log.Printf("orphan blob: hash=%s err=%v", hash, err)
	}

	if filenames == nil {
		filenames = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"hash":         hash,
		"blob_removed": blobRemoved,
		"filenames":    filenames,
	})
}

// adminPrune handles POST /api/admin/prune. An empty body is treated as a dry
// run. The request body is {"confirm": bool}; confirm=true deletes every
// dangling record (a files row whose backing blob directory is missing).
func (h *handler) adminPrune(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}

	result, err := database.PruneDangling(r.Context(), h.repo, h.storage, body.Confirm)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "prune scan failed"})
		return
	}

	if !body.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"scanned":        result.Scanned,
			"dangling":       fileRefsJSON(result.Dangling),
			"dangling_count": len(result.Dangling),
		})
		return
	}

	failed := make([]map[string]any, 0, len(result.Failed))
	for _, f := range result.Failed {
		failed = append(failed, map[string]any{"hash": f.Hash, "error": f.Err})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"dry_run":      false,
		"scanned":      result.Scanned,
		"pruned":       fileRefsJSON(result.Pruned),
		"pruned_count": len(result.Pruned),
		"failed":       failed,
	})
}

// fileRefsJSON shapes FileRefs into the {hash, filenames} objects the API
// returns, ensuring filenames is always a JSON array (never null).
func fileRefsJSON(refs []database.FileRef) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		names := ref.Filenames
		if names == nil {
			names = []string{}
		}
		out = append(out, map[string]any{"hash": ref.Hash, "filenames": names})
	}
	return out
}
