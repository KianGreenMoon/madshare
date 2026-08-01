package api

// Federation F8 item 3 — the quality-upgrade page's endpoints
// (docs/architecture/federation.md §Quality upgrades, "The upgrade scan").
//
// Read and decide, nothing else. Fetching an upgrade is the ORDINARY download
// path (POST /api/madnetwork/download), so there is no second way into the
// library here and no second place where bytes arrive unverified: a materialized
// upgrade goes through the review bucket and gets re-fingerprinted locally like
// anything else pulled off the network.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
)

const upgradesPageSize = 100

// upgradesList handles GET /api/admin/upgrades — findings newest-first.
//
//	?disposition=new|dismissed|materialized|all   default: new (the open ones)
//	?limit&offset                                 paging
func (h *handler) upgradesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := upgradesPageSize
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	disposition := q.Get("disposition")
	switch disposition {
	case "", "all", database.UpgradeNew, database.UpgradeDismissed, database.UpgradeMaterialized:
	default:
		http.Error(w, "unknown disposition", http.StatusBadRequest)
		return
	}

	rows, total, err := h.madnetwork.ListUpgrades(r.Context(), disposition,
		time.Now().Unix()-h.reachWindow(), limit, offset)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []*database.UpgradeRow{}
	}
	// The freshness verdict is computed here rather than in SQL for the same
	// reason the browse does it: a source is greyed, never hidden, and the rule
	// that picks its window is the one Go and SQL both have to agree on.
	reach := h.reachCutoff()
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, map[string]any{
			"id": u.ID, "recording_id": u.RecordingID, "title": u.Title, "artist": u.Artist,
			"remote_hash": u.RemoteHash, "match": u.Match, "ber": u.BER,
			"disposition": u.Disposition, "first_seen": u.FirstSeen, "last_seen": u.LastSeen,
			"ours": renditionJSON(u.Ours), "offered": renditionJSON(u.Offered),
			"source": u.Source, "source_key": u.SourceKey,
			"source_reachable": reach.ok(u.SourceSeen, u.SourcePinged),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": total, "items": out})
}

// upgradeDecide handles PATCH /api/admin/upgrades/{id} — body {"disposition": …}.
// Dismissing is the whole reason findings are stored: the next catalog sync
// re-measures the same comparison, and must not ask again.
func (h *handler) upgradeDecide(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid upgrade id", http.StatusBadRequest)
		return
	}
	var body struct {
		Disposition string `json:"disposition"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	found, err := h.madnetwork.SetUpgradeDisposition(r.Context(), id, body.Disposition)
	if err != nil {
		http.Error(w, "unknown disposition", http.StatusBadRequest)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
