package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/sources"
)

// adminSourcesList handles GET /api/admin/sources: the configured symlink data
// sources with their last-scan summary, plus whether the symlink kind is enabled
// (any [sources].symlink_roots configured) and the allowed roots for the Add
// form. Link-health and per-storage byte accounting are reported separately
// (data-sources P5/P6). 503 when no manager is wired.
func (h *handler) adminSourcesList(w http.ResponseWriter, r *http.Request) {
	if h.sourcesMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "data sources unavailable"})
		return
	}
	list, err := h.sourcesMgr.List(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"ok":      true,
		"enabled": h.sourcesMgr.Enabled(),
		"roots":   h.sourcesMgr.Roots(),
		"sources": list,
	}
	// Links-storage health/accounting (data-sources P5): one walk of the links
	// tree reporting how many links exist, how many are broken (dangling), and the
	// external bytes referenced. Best-effort — a walk error omits the figures
	// rather than failing the whole listing.
	if h.linker != nil {
		if u, err := h.linker.Usage(); err != nil {
			log.Printf("links usage: %v", err)
		} else {
			resp["links"] = map[string]any{
				"count":          u.Count,
				"broken":         u.Broken,
				"external_bytes": u.ExternalBytes,
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminSourcesAdd handles POST /api/admin/sources: validate {kind,name,root}
// against the symlink_roots allow-list, record the source as 'scanning', and
// launch the scan in the background (202-style — the scan outlives the request,
// like prune). The created source is returned immediately with status=scanning;
// poll GET /api/admin/sources for the outcome. 503 when no manager is wired.
func (h *handler) adminSourcesAdd(w http.ResponseWriter, r *http.Request) {
	if h.sourcesMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "data sources unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	// Only the symlink kind exists in v0 (s3 is future). Default an empty kind to
	// symlink so a minimal client need not send it.
	kind := strings.TrimSpace(body.Kind)
	if kind == "" {
		kind = database.SourceKindSymlink
	}
	if kind != database.SourceKindSymlink {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported source kind"})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name is required"})
		return
	}

	src, err := h.sourcesMgr.Add(r.Context(), body.Name, body.Root, actorID(r.Context()))
	switch {
	case errors.Is(err, sources.ErrDisabled):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "symlink sources are not configured"})
		return
	case errors.Is(err, sources.ErrRootNotAllowed):
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "root is not under an allowed directory"})
		return
	case errors.Is(err, sources.ErrInvalidRoot):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "root must be an absolute path to an existing directory"})
		return
	case errors.Is(err, sources.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "a source scan is already running"})
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "source.add", src.ID, "symlink source "+src.Name+" -> "+src.Root)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "source": src})
}
