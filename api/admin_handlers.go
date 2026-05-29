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
	h.audit(r.Context(), "file.delete", hash, strings.Join(filenames, ", "))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"hash":         hash,
		"blob_removed": blobRemoved,
		"filenames":    filenames,
	})
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
