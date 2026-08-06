package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The swarm admin surface (docs/architecture/swarm-admin.md): what this node
// moves, and how fast.
//
// It shares its row set with /admin/cache and mints no parallel endpoints for
// what that page already owns — removal, partial reaping, claims and cache audio
// are ITS routes, called from here. The two pages are lenses on one node: a lens
// may duplicate a view, never an editor.

// swarmPageSize bounds one page of the listing.
const swarmPageSize = 100

// errNoStore is what the swarm endpoints answer with when the repository is not
// the concrete database (test embeddings): the page's queries are SQL, not part
// of the Repository interface, so there is nothing to answer from.
var errNoStore = errors.New("swarm: no database")

// swarmFilterFrom reads the shared filter off the query string. The scope
// vocabulary is closed: anything unrecognised means "all", so a stale link can
// only ever show too much, never the wrong half.
func swarmFilterFrom(r *http.Request) database.SwarmFilter {
	q := r.URL.Query()
	scope := database.SwarmScope(q.Get("scope"))
	switch scope {
	case database.SwarmScopeLibrary, database.SwarmScopeCache:
	default:
		scope = database.SwarmScopeAll
	}
	state := q.Get("state")
	switch state {
	case database.SwarmStateLive, database.SwarmStateReview,
		database.SwarmStateTrashed, database.SwarmStatePrivate:
	default:
		state = database.SwarmStateAny
	}
	return database.SwarmFilter{
		Scope: scope,
		State: state,
		Q:     strings.TrimSpace(q.Get("q")),
		Field: q.Get("field"),
	}
}

// adminSwarmList handles GET /api/admin/swarm: one page of every blob this node
// has bytes for, with its all-time traffic.
func (h *handler) adminSwarmList(w http.ResponseWriter, r *http.Request) {
	db, ok := h.repo.(*database.DB)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "items": []any{}, "total": 0, "selectable_total": 0, "bytes": 0})
		return
	}
	q := database.SwarmQuery{
		SwarmFilter: swarmFilterFrom(r),
		Sort:        r.URL.Query().Get("sort"),
		Limit:       swarmPageSize,
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		q.Limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		q.Offset = n
	}
	rows, err := db.ListSwarmFiles(r.Context(), q)
	if err != nil {
		log.Printf("swarm list: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	total, bytes, err := db.CountSwarmFiles(r.Context(), q.SwarmFilter)
	if err != nil {
		log.Printf("swarm count: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	live := map[string]federation.TransferStats{}
	if h.federation != nil {
		for _, t := range h.federation.ActiveTransfers() {
			live[t.Hash] = t
		}
	}
	session := h.sessionTraffic()
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, swarmRowJSON(row, session, live))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "items": items, "total": total,
		// Every row is actionable, so the selectable total is the total; the field
		// exists because that is what a select-all-N banner reads.
		"selectable_total": total,
		"bytes":            bytes,
	})
}

// swarmRowJSON renders one row: its stored facts, the session's contribution to
// its counters, and — when it is moving right now — the live transfer.
func swarmRowJSON(row *database.SwarmFileRow, session federation.TrafficSnapshot,
	live map[string]federation.TransferStats) map[string]any {
	out := map[string]any{
		"hash": row.Hash, "byte_size": row.ByteSize,
		"in_library": row.InLibrary, "in_cache": row.InCache,
		"title": row.Title, "artist": row.Artist, "album": row.Album,
		"filename": row.Filename, "added_at": row.AddedAt,
		"share_depth": row.ShareDepth, "seedable": row.Seedable(),
		"up_bytes": row.Up, "down_bytes": row.Down,
	}
	if row.Wasted > 0 {
		out["wasted_bytes"] = row.Wasted
	}
	if row.LastAt > 0 {
		out["last_at"] = row.LastAt
	}
	if row.ReviewState != "" {
		out["review_state"] = row.ReviewState
	}
	if row.Trashed {
		out["trashed"] = true
	}
	if row.RecordingID > 0 {
		out["recording_id"] = row.RecordingID
	}
	if row.ObjectKey != "" {
		out["object_key"] = row.ObjectKey
	}
	// The session's half is added rather than folded in: the stored counters lag
	// by at most one flush, and the page must not appear to stall while a
	// transfer is visibly running.
	if s, ok := session.Hashes[row.Hash]; ok {
		out["session"] = map[string]any{
			"up_bytes": s.Up, "down_bytes": s.Down, "wasted_bytes": s.Wasted}
	}
	if t, ok := live[row.Hash]; ok {
		out["transfer"] = transferJSON(t)
	}
	return out
}

// transferJSON renders a live fetch for the page's progress bar and info panel.
func transferJSON(t federation.TransferStats) map[string]any {
	out := map[string]any{
		"hash": t.Hash, "size": t.Size, "progress": t.Progress, "mode": t.Mode,
		"chunks": t.Chunks, "chunks_done": t.ChunksDone,
	}
	if t.Retries > 0 {
		out["retries"] = t.Retries
	}
	if t.Failovers > 0 {
		out["failovers"] = t.Failovers
	}
	if t.Stalls > 0 {
		out["stalls"] = t.Stalls
	}
	if t.Corrupt > 0 {
		out["corrupt"] = t.Corrupt
	}
	if t.FirstByte > 0 {
		out["first_byte_ms"] = t.FirstByte.Milliseconds()
	}
	if t.Elapsed > 0 {
		out["elapsed_ms"] = t.Elapsed.Milliseconds()
	}
	if len(t.Providers) > 0 {
		provs := make([]map[string]any, 0, len(t.Providers))
		for _, p := range t.Providers {
			provs = append(provs, map[string]any{
				"name": p.Name, "key": p.PublicKey, "bytes": p.Bytes,
				"chunks": p.Chunks, "failures": p.Failures, "dropped": p.Dropped,
			})
		}
		out["providers"] = provs
	}
	return out
}

// adminSwarmSummary handles GET /api/admin/swarm/summary: the strip above the
// list — all-time and session totals, the caps in force, what is moving, and who
// is pulling from us.
func (h *handler) adminSwarmSummary(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "federation": h.federation != nil}

	if db, ok := h.repo.(*database.DB); ok {
		total, err := db.SwarmTrafficTotals(r.Context())
		if err != nil {
			log.Printf("swarm totals: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		resp["all_time"] = map[string]any{
			"up_bytes": total.Up, "down_bytes": total.Down, "wasted_bytes": total.Wasted}
	}

	session := h.sessionTraffic()
	resp["session"] = swarmSessionJSON(session)
	resp["peers"] = swarmPeersJSON(session)

	active := []map[string]any{}
	if h.federation != nil {
		for _, t := range h.federation.ActiveTransfers() {
			active = append(active, transferJSON(t))
		}
	}
	resp["active"] = active

	if limits, err := h.swarmLimits(r); err == nil {
		resp["limits"] = limits
	}
	// The cache half of the strip comes from the cache page's own figures, not a
	// second count of the same directory.
	if h.madnetwork != nil {
		if p, err := h.madnetwork.GetMadnetworkPolicy(r.Context()); err == nil {
			resp["seeding"] = map[string]any{"enabled": p.SeedEnabled, "cache": p.SeedCache}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// swarmSessionJSON renders the session counters. `since` is omitted rather than
// stamped when there is no node: a zero time.Time serializes to the year 1, and
// a page showing "since 01/01/0001" is worse than one showing nothing.
func swarmSessionJSON(s federation.TrafficSnapshot) map[string]any {
	out := map[string]any{
		"up_bytes": s.Up, "down_bytes": s.Down, "wasted_bytes": s.Wasted,
	}
	if !s.Since.IsZero() {
		out["since"] = s.Since.Unix()
	}
	return out
}

// swarmPeersJSON renders the counterparties, most active first.
func swarmPeersJSON(session federation.TrafficSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(session.Peers))
	for _, p := range session.Peers {
		row := map[string]any{"up_bytes": p.Up, "down_bytes": p.Down}
		if p.Key != "" {
			row["key"] = p.Key
		}
		if p.Addr != "" {
			row["addr"] = p.Addr
		}
		if !p.LastAt.IsZero() {
			row["last_at"] = p.LastAt.Unix()
		}
		out = append(out, row)
	}
	return out
}

// adminSwarmLive handles GET /api/admin/swarm/live: the small payload the page
// polls. Session counters, what is moving, who is pulling — and the per-hash
// session deltas, so a visible row's numbers move without re-listing the page.
func (h *handler) adminSwarmLive(w http.ResponseWriter, r *http.Request) {
	session := h.sessionTraffic()
	active := []map[string]any{}
	if h.federation != nil {
		for _, t := range h.federation.ActiveTransfers() {
			active = append(active, transferJSON(t))
		}
	}
	rows := map[string]any{}
	// Only the hashes the caller can see are worth sending. An empty list means
	// "just the totals", which is what an idle page asks for.
	for _, hash := range r.URL.Query()["hash"] {
		if c, ok := session.Hashes[strings.ToLower(hash)]; ok {
			rows[hash] = map[string]any{
				"up_bytes": c.Up, "down_bytes": c.Down, "wasted_bytes": c.Wasted}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"session": swarmSessionJSON(session),
		"active":  active,
		"peers":   swarmPeersJSON(session),
		"rows":    rows,
	})
}

// adminSwarmFile handles GET /api/admin/swarm/{hash}: one blob's full picture —
// its row, its traffic, who holds it now, and the live transfer if it is moving.
func (h *handler) adminSwarmFile(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	db, ok := h.repo.(*database.DB)
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, err := db.GetSwarmFile(r.Context(), hash)
	if err != nil {
		log.Printf("swarm file %s: %v", hash, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.NotFound(w, r)
		return
	}
	live := map[string]federation.TransferStats{}
	if h.federation != nil {
		for _, t := range h.federation.ActiveTransfers() {
			live[t.Hash] = t
		}
	}
	out := swarmRowJSON(row, h.sessionTraffic(), live)
	// Who else has these bytes — the question the cache page deliberately left to
	// this one. Live, not recorded: it is a fact about now.
	if h.madnetwork != nil {
		if claims, err := h.madnetwork.MadnetworkCacheClaims(r.Context(), hash); err == nil {
			holders := make([]map[string]any, 0, len(claims))
			for _, c := range claims {
				holders = append(holders, map[string]any{
					"key": c.SourceKey, "name": c.SourceName, "title": c.Title,
					"artist": c.Artist, "album": c.Album, "last_seen": c.LastSeen,
				})
			}
			out["holders"] = holders
		}
	}
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

// swarmLimits reports the caps in force and where each came from.
func (h *handler) swarmLimits(r *http.Request) (map[string]any, error) {
	db, ok := h.repo.(*database.DB)
	if !ok {
		return nil, errNoStore
	}
	up, down, err := db.GetSwarmRates(r.Context())
	if err != nil {
		return nil, err
	}
	describe := func(override *int, effective int64) map[string]any {
		out := map[string]any{"source": "config"}
		if override != nil {
			out["source"] = "override"
			out["override_kib"] = *override
		}
		// The effective value comes from the node when one runs, since it is the
		// thing actually holding the bucket; with federation off there is nothing
		// enforcing anything, and the override is all there is to report.
		out["effective_kib"] = effective / 1024
		return out
	}
	var upBps, downBps int64
	if h.federation != nil {
		upBps, downBps = h.federation.SwarmRates()
	} else {
		if up != nil {
			upBps = int64(*up) * 1024
		}
		if down != nil {
			downBps = int64(*down) * 1024
		}
	}
	return map[string]any{
		"up": describe(up, upBps), "down": describe(down, downBps),
	}, nil
}

// adminSwarmLimitsGet handles GET /api/admin/swarm/limits.
func (h *handler) adminSwarmLimitsGet(w http.ResponseWriter, r *http.Request) {
	limits, err := h.swarmLimits(r)
	if err != nil {
		log.Printf("swarm limits: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	limits["ok"] = true
	writeJSON(w, http.StatusOK, limits)
}

// adminSwarmLimitsSet handles POST /api/admin/swarm/limits.
//
// Three-valued per field, the same shape share_depth uses and for the same
// reason: absent ≠ null ≠ a number. Absent leaves the cap alone, null clears the
// override back to the config file, and a number pins it — including 0, which
// means unlimited and is a real override rather than a synonym for "unset".
//
// Its own endpoint rather than a field on /api/admin/settings/madnetwork:
// THAT handler decodes the seed switches as plain bools with hard-coded
// defaults, so a client posting only rates would switch seeding on and
// autoapprove off as a side effect.
func (h *handler) adminSwarmLimitsSet(w http.ResponseWriter, r *http.Request) {
	db, ok := h.repo.(*database.DB)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	var body struct {
		UpKiB   json.RawMessage `json:"up_kib"`
		DownKiB json.RawMessage `json:"down_kib"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	cur, curDown, err := db.GetSwarmRates(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	up, ok := swarmRateField(w, body.UpKiB, cur, "up_kib")
	if !ok {
		return
	}
	down, ok := swarmRateField(w, body.DownKiB, curDown, "down_kib")
	if !ok {
		return
	}
	if err := db.SetSwarmRates(r.Context(), up, down); err != nil {
		log.Printf("set swarm rates: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// Put it in force now rather than whenever the node next resolves: someone
	// setting a cap is usually watching a link that is saturated right now.
	if h.federation != nil {
		h.federation.RefreshRates()
	}
	h.audit(r.Context(), "swarm.limits", "", swarmRateAudit("up", up)+" "+swarmRateAudit("down", down))

	limits, err := h.swarmLimits(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	limits["ok"] = true
	writeJSON(w, http.StatusOK, limits)
}

// swarmRateField decodes one three-valued rate field. ok is false when the
// caller has already been answered with a 400.
func swarmRateField(w http.ResponseWriter, raw json.RawMessage, current *int, name string) (*int, bool) {
	if len(raw) == 0 {
		return current, true // absent: unchanged
	}
	if string(raw) == "null" {
		return nil, true // explicit null: back to the config file
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": name + " must be null (inherit the config) or a KiB/s value ≥ 0"})
		return nil, false
	}
	return &n, true
}

func swarmRateAudit(name string, v *int) string {
	if v == nil {
		return name + "=inherit"
	}
	return name + "=" + strconv.Itoa(*v)
}

// adminSwarmForget handles POST /api/admin/swarm/stats/forget: drop the
// accounting rows for a set of blobs.
//
// The only thing that deletes traffic history. Removing a cached blob or
// trashing a recording deliberately does not — the bytes really moved — so
// forgetting is an explicit act, and one that lowers the node's all-time totals
// because those totals ARE these rows.
func (h *handler) adminSwarmForget(w http.ResponseWriter, r *http.Request) {
	db, ok := h.repo.(*database.DB)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	var body struct {
		Hashes []string `json:"hashes"`
		All    bool     `json:"all"`
		Filter struct {
			Scope string `json:"scope"`
			State string `json:"state"`
			Q     string `json:"q"`
			Field string `json:"field"`
		} `json:"filter"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	hashes := body.Hashes
	if len(hashes) == 0 {
		filter := database.SwarmFilter{
			Scope: database.SwarmScope(body.Filter.Scope),
			State: body.Filter.State,
			Q:     strings.TrimSpace(body.Filter.Q),
			Field: body.Filter.Field,
		}
		if filter.Scope != database.SwarmScopeLibrary && filter.Scope != database.SwarmScopeCache {
			filter.Scope = database.SwarmScopeAll
		}
		// The guardrail every bulk endpoint here shares: an empty filter means
		// everything, and erasing the node's whole history has to be asked for.
		if filter.Q == "" && !body.All {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": `refusing to forget every blob's traffic without "all": true`})
			return
		}
		resolved, err := db.SwarmFileHashes(r.Context(), filter)
		if err != nil {
			log.Printf("swarm forget resolve: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		hashes = resolved
	}
	clean := make([]string, 0, len(hashes))
	for _, raw := range hashes {
		h := strings.ToLower(strings.TrimSpace(raw))
		if isSHA256Hex(h) {
			clean = append(clean, h)
		}
	}
	n, err := db.ForgetSwarmTraffic(r.Context(), clean)
	if err != nil {
		log.Printf("swarm forget: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "swarm.stats.forget", "", strconv.Itoa(n)+" blob(s)")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "forgotten": n})
}

// adminSwarmPeers handles GET /api/admin/swarm/peers: who this node has traded
// with, all time, busiest first.
//
// The companion to the F7 member quotas — those bound what a member may cost us,
// this says what one has. It lives here and nowhere else: /admin/network owns
// the same nodes as trust decisions, and a byte column there would put one
// number under two owners.
func (h *handler) adminSwarmPeers(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "federation": h.federation != nil}
	db, ok := h.repo.(*database.DB)
	if !ok {
		resp["peers"] = []any{}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	stored, err := db.ListSwarmPeerTraffic(r.Context())
	if err != nil {
		log.Printf("swarm peers: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	// The session half, folded in the same way the file rows fold theirs: the
	// stored counters lag by at most one flush, and a page watching a transfer
	// must not appear to stall. Unplaceable requesters are summed into the
	// bucket, exactly as the flusher will persist them.
	session := h.sessionTraffic()
	live := map[string]federation.TrafficCounters{}
	for _, p := range session.Peers {
		c := live[p.Key]
		c.Up += p.Up
		c.Down += p.Down
		live[p.Key] = c
	}

	rows := make([]map[string]any, 0, len(stored)+len(live))
	var bucket map[string]any
	seen := map[string]bool{}
	var upTotal, downTotal int64
	emit := func(p database.SwarmPeerTraffic) {
		seen[p.Key] = true
		row := map[string]any{
			"key": p.Key, "kind": p.Kind,
			"up_bytes": p.Up, "down_bytes": p.Down,
		}
		if p.Name != "" {
			row["name"] = p.Name
		}
		if p.FirstAt > 0 {
			row["first_at"] = p.FirstAt
		}
		if p.LastAt > 0 {
			row["last_at"] = p.LastAt
		}
		upTotal, downTotal = upTotal+p.Up, downTotal+p.Down
		if c, ok := live[p.Key]; ok {
			row["session"] = map[string]any{"up_bytes": c.Up, "down_bytes": c.Down}
			upTotal, downTotal = upTotal+c.Up, downTotal+c.Down
		}
		if p.Key == "" {
			bucket = row // rendered apart: an aggregate of strangers is not a peer
			return
		}
		rows = append(rows, row)
	}
	for _, p := range stored {
		emit(p)
	}

	// Counterparties this process has traded with but no flush has written yet.
	// Naming them costs one more query and keeps the panel from listing nobody
	// while the summary strip says two nodes are pulling.
	var fresh []string
	for key := range live {
		if key != "" && !seen[key] {
			fresh = append(fresh, key)
		}
	}
	if len(fresh) > 0 {
		sort.Strings(fresh) // a map's order is not an order
		resolved, err := db.ResolveSwarmPeers(r.Context(), fresh)
		if err != nil {
			log.Printf("swarm peers resolve: %v", err)
		} else {
			for _, p := range resolved {
				emit(p)
			}
		}
	}
	if bucket == nil && live[""].Up+live[""].Down > 0 {
		// Unplaced traffic this session, nothing stored yet.
		c := live[""]
		bucket = map[string]any{
			"key": "", "kind": "unplaced", "up_bytes": int64(0), "down_bytes": int64(0),
			"session": map[string]any{"up_bytes": c.Up, "down_bytes": c.Down},
		}
		upTotal, downTotal = upTotal+c.Up, downTotal+c.Down
	}

	// Busiest first, session bytes included — otherwise a node that has pulled a
	// gigabyte since the last flush sorts below one that moved a byte last year.
	sort.SliceStable(rows, func(i, j int) bool {
		return swarmRowBytes(rows[i]) > swarmRowBytes(rows[j])
	})
	resp["peers"] = rows
	if bucket != nil {
		resp["unplaced"] = bucket
	}
	resp["totals"] = map[string]any{"up_bytes": upTotal, "down_bytes": downTotal}
	writeJSON(w, http.StatusOK, resp)
}

// isPublicKeyHex reports whether s could be an ed25519 public key as this
// codebase writes them. Same shape as a content hash, and deliberately not the
// same function: a key and a hash are different things, and a reader following
// this call should not land in the upload code.
func isPublicKeyHex(s string) bool { return isSHA256Hex(s) }

// swarmRowBytes is one peer row's total, stored plus this session's.
func swarmRowBytes(row map[string]any) int64 {
	total, _ := row["up_bytes"].(int64)
	if d, ok := row["down_bytes"].(int64); ok {
		total += d
	}
	if s, ok := row["session"].(map[string]any); ok {
		up, _ := s["up_bytes"].(int64)
		down, _ := s["down_bytes"].(int64)
		total += up + down
	}
	return total
}

// adminSwarmPeersForget handles POST /api/admin/swarm/peers/forget: drop the
// all-time row for a set of counterparties (the empty key being the bucket).
//
// The peer-side twin of stats/forget, and deliberately independent of it:
// forgetting what a blob moved does not debit the nodes that moved it, and this
// does not rewrite any blob's history. The two ledgers count the same bytes and
// neither is derived from the other, so the page never sums across them.
func (h *handler) adminSwarmPeersForget(w http.ResponseWriter, r *http.Request) {
	db, ok := h.repo.(*database.DB)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	var body struct {
		Keys []string `json:"keys"`
		All  bool     `json:"all"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var n int
	var err error
	switch {
	case len(body.Keys) > 0:
		clean := make([]string, 0, len(body.Keys))
		for _, k := range body.Keys {
			key := strings.ToLower(strings.TrimSpace(k))
			// The empty key is legal — it addresses the bucket — but nothing else
			// that is not a public key is.
			if key == "" || isPublicKeyHex(key) {
				clean = append(clean, key)
			}
		}
		n, err = db.ForgetSwarmPeerTraffic(r.Context(), clean)
	case body.All:
		n, err = db.ForgetAllSwarmPeerTraffic(r.Context())
	default:
		// The guardrail every bulk endpoint here shares: erasing the whole
		// history has to be asked for, never implied by an empty selection.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": `refusing to forget every counterparty without "all": true`})
		return
	}
	if err != nil {
		log.Printf("swarm peers forget: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "swarm.peers.forget", "", strconv.Itoa(n)+" node(s)")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "forgotten": n})
}
