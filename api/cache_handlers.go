package api

import (
	"context"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
)

// The madnetwork cache control surface (docs/architecture/madnetwork-cache.md):
// see what the swarm fetched, and clean it up. Primarily a deletion surface,
// which is why it sits in the admin group behind file.delete; materialize and
// download keep their own gates on their existing endpoints.
//
// Every listing read goes through one filter (database.MadnetworkCacheFilter),
// shared by the page, the headline figures and the select-all-N resolver, so a
// bulk removal can never act on a different set than the one on screen.

// cachePageSize bounds one page of the listing.
const cachePageSize = 100

// cacheFilterFrom reads the shared filter off the query string.
func cacheFilterFrom(r *http.Request) database.MadnetworkCacheFilter {
	return database.MadnetworkCacheFilter{
		Q:     strings.TrimSpace(r.URL.Query().Get("q")),
		Field: r.URL.Query().Get("field"),
	}
}

// liveTransferHashes is the set of hashes being fetched right now. It is what
// keeps a running transfer's `.part` file out of the reaper's way — the only
// thing that knows a partial is alive rather than abandoned.
func (h *handler) liveTransferHashes() map[string]bool {
	if h.federation == nil {
		return nil
	}
	live := map[string]bool{}
	for _, t := range h.federation.ActiveTransfers() {
		live[t.Hash] = true
	}
	return live
}

// adminCacheList handles GET /api/admin/cache: one page of cached blobs, in the
// envelope file-list.js consumes ({items, total, selectable_total}).
func (h *handler) adminCacheList(w http.ResponseWriter, r *http.Request) {
	q := database.MadnetworkCacheQuery{
		MadnetworkCacheFilter: cacheFilterFrom(r),
		Sort:                  r.URL.Query().Get("sort"),
		Limit:                 cachePageSize,
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		q.Limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		q.Offset = n
	}
	rows, err := h.repo.ListMadnetworkCachePage(r.Context(), q)
	if err != nil {
		log.Printf("cache list: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	rows = h.forgetVanished(rows)
	total, bytes, err := h.repo.CountMadnetworkCache(r.Context(), q.MadnetworkCacheFilter)
	if err != nil {
		log.Printf("cache count: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	items := rows
	if items == nil {
		items = []*database.MadnetworkCacheEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "items": items, "total": total,
		// Every row is actionable here — there is no half-selectable state — but
		// the field is what the shared list reads to offer "select all N".
		"selectable_total": total,
		"bytes":            bytes,
	})
}

// forgetVanished drops the index rows on this page whose file is no longer
// there, and returns the page without them. Bounded by the page size, so it is a
// hundred stats at worst — the price of never showing a row that is not real.
//
// The rows the operator is LOOKING at are the ones worth checking individually;
// the whole index is swept by the summary (which reads the directory anyway),
// at startup, and by Rescan.
func (h *handler) forgetVanished(rows []*database.MadnetworkCacheEntry) []*database.MadnetworkCacheEntry {
	if h.cacheDir == "" {
		return rows
	}
	live, dropped := rows[:0], 0
	for _, e := range rows {
		if info, err := os.Stat(filepath.Join(h.cacheDir, e.Hash)); err == nil && !info.IsDir() {
			live = append(live, e)
			continue
		}
		// Absence is proof enough: the directory is what the swarm reads, so a
		// row without a file describes nothing.
		h.dropCacheIndex(e.Hash)
		dropped++
	}
	if dropped > 0 {
		// Worth a line: files disappearing without the server doing it is
		// something an operator either did on purpose or wants to know about.
		log.Printf("madnetwork cache: %d file(s) removed outside the server; index updated", dropped)
	}
	return live
}

// sweepVanished drops every index row whose file is gone. Called where the
// directory is being read anyway, so it costs a query rather than any extra I/O.
func (h *handler) sweepVanished(ctx context.Context) {
	db, ok := h.repo.(*database.DB)
	if !ok || h.cacheDir == "" {
		return
	}
	if n, err := database.DropMissingMadnetworkCacheRows(ctx, db, h.cacheDir); err != nil {
		log.Printf("sweep vanished cache rows: %v", err)
	} else if n > 0 {
		log.Printf("madnetwork cache: %d file(s) removed outside the server; index updated", n)
	}
}

// adminCacheSummary handles GET /api/admin/cache/summary: how full the cache is,
// what is arriving, and what is abandoned.
//
// It sweeps the index first. This endpoint already lists the cache directory (to
// count abandoned partials), so noticing that files have been deleted behind the
// server's back is nearly free here — and it is the figure most worth being
// right, since it is what the page and the dashboard report.
func (h *handler) adminCacheSummary(w http.ResponseWriter, r *http.Request) {
	h.sweepVanished(r.Context())
	entries, bytes, err := h.repo.CountMadnetworkCache(r.Context(), database.MadnetworkCacheFilter{})
	if err != nil {
		log.Printf("cache summary: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"ok": true, "entries": entries, "bytes": bytes}

	live := h.liveTransferHashes()
	inFlight := []map[string]any{}
	if h.federation != nil {
		for _, t := range h.federation.ActiveTransfers() {
			inFlight = append(inFlight, map[string]any{
				"hash": t.Hash, "size": t.Size, "progress": t.Progress, "mode": t.Mode,
			})
		}
	}
	resp["in_flight"] = inFlight
	// Whether a federation node is running. The cache is listed, played,
	// downloaded and cleaned either way — but MATERIALIZE goes through
	// POST /api/madnetwork/download, which is only registered with a node, so the
	// page hides that action rather than offering one that 404s.
	resp["federation"] = h.federation != nil

	if h.cacheDir != "" {
		n, b, err := database.CountAbandonedPartials(h.cacheDir, live)
		if err != nil {
			log.Printf("count abandoned partials: %v", err)
		}
		resp["partials"] = map[string]any{"count": n, "bytes": b}
	}
	// Whether these bytes are being served to the community, so the page can say
	// plainly what removing one costs.
	if h.madnetwork != nil {
		if p, err := h.madnetwork.GetMadnetworkPolicy(r.Context()); err == nil {
			resp["seeding"] = map[string]any{"enabled": p.SeedEnabled, "cache": p.SeedCache}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminCacheAudio handles GET /api/admin/cache/{hash}/audio — the page's Play
// and (with ?download=1) Download, served straight off the cache directory.
//
// Deliberately NOT the madnetwork streaming relay. That relay is registered only
// when a federation node runs, and the cache outlives federation being switched
// off — which is precisely when someone comes here to reclaim the disk. Asking
// the mesh for a file already on this disk would be indirection that buys
// nothing and breaks in the one case that matters. http.ServeContent gives
// native Range, so seeking works.
func (h *handler) adminCacheAudio(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) || h.cacheDir == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(h.cacheDir, hash))
	if err != nil {
		// Asked for bytes that are not there: the row describing them is wrong,
		// and this request just proved it.
		h.dropCacheIndex(hash)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	name := h.cachedDownloadName(r.Context(), hash, "")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	// ServeContent types the response from the name's extension and falls back to
	// sniffing the bytes — which is the right order here, since a blob adopted
	// from a pre-existing cache has no remembered name at all.
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// adminCacheClaims handles GET /api/admin/cache/{hash}/claims: what sources
// currently say this blob is. The rare view — one hash at a time, because it
// walks the cached catalogs.
func (h *handler) adminCacheClaims(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	if h.madnetwork == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claims": []any{}})
		return
	}
	claims, err := h.madnetwork.MadnetworkCacheClaims(r.Context(), hash)
	if err != nil {
		log.Printf("cache claims %s: %v", hash, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if claims == nil {
		claims = []*database.MadnetworkCacheClaim{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claims": claims})
}

// adminCacheBulk handles POST /api/admin/cache/bulk — removal, by explicit set
// or over the whole matching filter.
func (h *handler) adminCacheBulk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string   `json:"action"`
		Hashes []string `json:"hashes"`
		All    bool     `json:"all"`
		Filter struct {
			Q     string `json:"q"`
			Field string `json:"field"`
		} `json:"filter"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Action != "remove" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": `unknown action (want "remove")`})
		return
	}

	hashes := body.Hashes
	if len(hashes) == 0 {
		filter := database.MadnetworkCacheFilter{
			Q: strings.TrimSpace(body.Filter.Q), Field: body.Filter.Field,
		}
		// The guardrail every bulk endpoint here shares: an empty filter means
		// "everything", and wiping the whole cache has to be asked for.
		if filter.Q == "" && !body.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": `refusing to clear the whole cache without "all": true`})
			return
		}
		resolved, err := h.repo.MadnetworkCacheHashes(r.Context(), filter)
		if err != nil {
			log.Printf("cache bulk resolve: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		hashes = resolved
	}

	removed, freed := 0, int64(0)
	for _, raw := range hashes {
		hash := strings.ToLower(strings.TrimSpace(raw))
		if !isSHA256Hex(hash) {
			continue
		}
		n, ok := h.removeCached(hash)
		if ok {
			removed++
			freed += n
		}
	}
	h.audit(r.Context(), "madnetwork.cache.remove", "",
		strconv.Itoa(removed)+" blob(s), "+strconv.FormatInt(freed, 10)+" bytes")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "bytes": freed})
}

// removeCached deletes one cache file and its index row, returning the bytes
// freed. The FILE goes first: it is what the swarm reads, and the row only
// describes it. A file already gone is a success — the caller's job was to make
// both agree that it is not there.
//
// Removing bytes some request is mid-read is safe: POSIX keeps the open
// descriptor alive across the unlink, which is what EvictCachedBlob already
// relies on. Note this does NOT go through federation: the cache outlives
// federation being switched off, and it must stay cleanable then.
func (h *handler) removeCached(hash string) (int64, bool) {
	if h.cacheDir == "" {
		return 0, false
	}
	path := filepath.Join(h.cacheDir, hash)
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("remove cached blob %s: %v", hash, err)
		return 0, false
	}
	h.dropCacheIndex(hash)
	return size, true
}

// adminCacheRescan handles POST /api/admin/cache/rescan: make the index agree
// with the directory on demand, for a cache that was changed underneath us.
func (h *handler) adminCacheRescan(w http.ResponseWriter, r *http.Request) {
	db, ok := h.repo.(*database.DB)
	if !ok || h.cacheDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": 0, "dropped": 0})
		return
	}
	added, dropped, err := database.ReconcileMadnetworkCache(r.Context(), db, h.cacheDir)
	if err != nil {
		log.Printf("cache rescan: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "dropped": dropped})
}

// adminCacheReapPartials handles POST /api/admin/cache/partials/reap: delete the
// scratch files of fetches that died, which nothing swept before this existed.
// Transfers running right now are excluded — their partials are not abandoned.
func (h *handler) adminCacheReapPartials(w http.ResponseWriter, r *http.Request) {
	if h.cacheDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": 0, "bytes": 0})
		return
	}
	removed, freed, err := database.ReapAbandonedPartials(h.cacheDir, h.liveTransferHashes())
	if err != nil {
		log.Printf("reap abandoned partials: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "madnetwork.cache.reap", "",
		strconv.Itoa(removed)+" partial(s), "+strconv.FormatInt(freed, 10)+" bytes")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "bytes": freed})
}
