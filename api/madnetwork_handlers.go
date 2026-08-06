package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// /api/madnetwork — the merged madnetwork catalog browse (federation F2,
// docs/architecture/federation.md §Catalog), backing the /madnetwork
// drill-down. Read-only over the per-peer cached catalogs; gated
// madnetwork.access. Browse-only in F2 — playing/downloading remote content
// arrives with F3 (direct transfer).

// includeSelf reports whether the merged view folds this node's own published
// set in — on exactly when the federation node runs (h.madnetworkName is its
// display name). With federation disabled the view stays what the friends
// provide: nothing.
func (h *handler) includeSelf() bool { return h.madnetworkName != "" }

// defaultReachableWindowSec mirrors config.DefaultReachableWindowSec for the
// paths that carry no configured window (tests via NewRouter). The running
// server always passes the (validated, ≥ min) config value.
const defaultReachableWindowSec = 180

// reachWindow is the availability freshness window in seconds: a friend is
// "reachable" (its exclusively-held tracks are shown) when last_seen is within
// it. Several × the node's 1-minute refresh cadence, so a single missed ping
// never flips reachability — the margin is the anti-flap guarantee.
func (h *handler) reachWindow() int64 {
	if h.reachWindowSec > 0 {
		return int64(h.reachWindowSec)
	}
	return defaultReachableWindowSec
}

// pullWindow is the same guarantee for a node nothing pings on our behalf — a
// member the frontier rotation reached and no friend of ours vouches for (F7
// item 10, docs/architecture/federation.md §Availability, "Two clocks, two
// windows"). Its liveness clock is the catalog pull, so its window is three
// catalog cycles rather than three ping rounds: the window measures how recently
// we would have NOTICED, and judging such a node by the ping's window hid most
// of what discovery had just made visible.
//
// Never narrower than the ping window — an operator who widens
// reachable_window_sec past three cycles means it for every node.
func (h *handler) pullWindow() int64 {
	pull := int64(federation.PullFreshnessWindow / time.Second)
	if w := h.reachWindow(); w > pull {
		return w
	}
	return pull
}

// reachWindows is the pair of freshness cutoffs a request is judged by, and the
// rule that picks between them. Passed around as one value so no caller can
// apply a window without the class that selects it.
type reachWindows struct{ tight, pull int64 }

// ok reports whether a source last seen at lastSeen counts as reachable. pinged
// is the row's class: true when something pings it every minute (our own friend,
// or a friend relaying a first-hand hint about it).
func (rw reachWindows) ok(lastSeen int64, pinged bool) bool {
	return database.ReachableAt(lastSeen, pinged, rw.tight, rw.pull)
}

// madnetworkViewFor is madnetworkView plus the request's optional single-node
// restriction — a node's shelf. Three forms of ?source=: a node KEY (what a node
// page is addressed by), "self" for our own published library, and a bare
// catalog-source id (the local row number, kept because the browse has always
// taken it).
//
// A source id we cannot parse falls back to the merged view — a stale row number
// should land somewhere useful. A KEY we cannot resolve does NOT: it is an
// explicit request for one node, and answering it with the whole community's
// catalog is the one answer that is certainly wrong. It gets the empty view
// instead, which the view machinery already supports (§Browsing a single node).
func (h *handler) madnetworkViewFor(r *http.Request) database.MadnetworkView {
	v := h.madnetworkView(r.Context())
	src := r.URL.Query().Get("source")
	switch {
	case src == "":
		return v
	case src == "self":
		v.SelfOnly = true
		return v
	}
	if id, err := strconv.ParseInt(src, 10, 64); err == nil {
		if id > 0 {
			v.SourceID = id
		}
		return v
	}
	key, err := federation.NormalizeKey(src)
	if err != nil {
		return v // not a key and not an id: the merged view, as before
	}
	if self := h.selfNode(r.Context(), v); self != nil && self.Key == key {
		v.SelfOnly = true
		return v
	}
	if node, found, err := h.madnetwork.MadnetworkSourceByKey(r.Context(), key, v); err == nil && found {
		v.SourceID = node.ID
		return v
	}
	// Asked for a specific node we hold nothing from: the empty shelf is the
	// truthful answer, and it keeps this one path from silently widening into
	// "everything".
	v.SourceID = database.NoSourceID
	return v
}

func (h *handler) madnetworkView(ctx context.Context) database.MadnetworkView {
	now := time.Now().Unix()
	v := database.MadnetworkView{
		IncludeSelf: h.includeSelf(), DefaultShareDepth: federation.DepthFriends,
		// Set before either early return: which window a node BELONGS to is a
		// fact about who watches it, and the rows still carry it when this
		// request is not filtering — that is what lets the ⓘ panel grey a holder
		// correctly while the browse shows everything.
		PingedSince: now - h.reachWindow(),
	}
	p, err := h.madnetwork.GetMadnetworkPolicy(ctx)
	if err == nil {
		v.DefaultShareDepth = p.DefaultShareDepth
	}
	if !h.inboundHealthy() {
		return v // fail open
	}
	if err == nil && !p.HideUnavailable {
		return v // hiding disabled by the admin
	}
	v.Cutoff, v.PullCutoff = now-h.reachWindow(), now-h.pullWindow()
	return v
}

// inboundHealthy reports the node's self-health (true when federation is off or
// absent — there is nothing to fail open for).
func (h *handler) inboundHealthy() bool {
	return h.federation == nil || h.federation.InboundHealthy()
}

// reachCutoff is the freshness cutoffs used to *display* holder reachability (the
// ⓘ panel greys a holder past them). Always now−window, independent of the browse
// filter cutoffs — so when the view fails open (showing everything), stale holders
// still read as stale rather than all looking reachable.
func (h *handler) reachCutoff() reachWindows {
	now := time.Now().Unix()
	return reachWindows{tight: now - h.reachWindow(), pull: now - h.pullWindow()}
}

// mergeOpts is everything folding raw rows into display tracks needs beyond the
// rows themselves. Bundled into one value deliberately: each field is a rule the
// merge must not be able to skip — how a holder is labelled, when it counts as
// reachable, and whose voice it is — and a positional parameter list is how one
// of them gets forgotten at the fourth call site.
type mergeOpts struct {
	selfName string
	reach    reachWindows
	branches database.BranchMap
}

func (h *handler) mergeOpts(ctx context.Context) mergeOpts {
	return mergeOpts{
		selfName: h.madnetworkName,
		reach:    h.reachCutoff(),
		branches: h.branchesByKey(ctx),
	}
}

// madnetworkSummary handles GET /api/madnetwork/summary: every node whose
// catalog this server holds — its own included, at 0 hops — plus the merged
// distinct-track count. It backs the page's status line, the Nodes lane and the
// directory at /madnetwork/nodes, which is why the list arrives in display order
// (docs/ui/madnetwork-nodes.md §Ordering) rather than each surface sorting it.
func (h *handler) madnetworkSummary(w http.ResponseWriter, r *http.Request) {
	nodes, tracks, err := h.nodeList(r.Context(), h.madnetworkView(r.Context()))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "nodes": nodes, "tracks": tracks,
		"inbound_healthy": h.inboundHealthy(),
	})
}

// madnetworkArtistPageSize bounds one page of Browse all. The list is the
// community's whole output now, so it is paged and windowed like the library's
// (docs/ui/madnetwork-page.md §Scale stops being optional) rather than sent
// whole and rendered whole.
const madnetworkArtistPageSize = 80

// madnetworkArtists handles GET /api/madnetwork/artists[?q=&limit=&cursor=]: one
// keyset page of the merged artist list (album-artist grouping, like the local
// library). The cursor is opaque and comes only from a previous next_cursor.
func (h *handler) madnetworkArtists(w http.ResponseWriter, r *http.Request) {
	limit := madnetworkArtistPageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, madnetworkArtistPageSize)
	}
	artists, next, err := h.madnetwork.MadnetworkArtists(r.Context(), r.URL.Query().Get("q"),
		h.madnetworkViewFor(r), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if artists == nil {
		artists = []*database.MadnetworkArtist{}
	}
	resp := map[string]any{"ok": true, "artists": artists}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// madnetworkAlbums handles GET /api/madnetwork/albums?artist=<name>.
func (h *handler) madnetworkAlbums(w http.ResponseWriter, r *http.Request) {
	artist := r.URL.Query().Get("artist")
	if artist == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "artist is required"})
		return
	}
	albums, err := h.madnetwork.MadnetworkAlbums(r.Context(), artist, h.madnetworkViewFor(r))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if albums == nil {
		albums = []*database.MadnetworkAlbum{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "albums": albums})
}

// A logical track of the merged view: the same text offered by several friends
// is ONE row; its versions are the distinct claimed recordings behind it (the
// "N versions" answer to catalog crossing — recordings are never merged, only
// grouped for display when they share a rendition hash, which is proof of
// same bytes).
type madnetworkTrack struct {
	Title    string  `json:"title"`
	Artist   string  `json:"artist,omitempty"` // performer display (may differ from the grouping artist)
	Track    *int64  `json:"track_number,omitempty"`
	Disc     *int64  `json:"disc_number,omitempty"`
	Duration float64 `json:"duration,omitempty"`

	// TagsetID is the local appearance behind a self-held track (hearts,
	// playlists); zero when this node does not publish the track itself.
	TagsetID int64 `json:"tagset_id,omitempty"`

	Versions []madnetworkVersion `json:"versions"`
}

type madnetworkVersion struct {
	Renditions []federation.CatalogRendition `json:"renditions"`
	Holders    []madnetworkHolder            `json:"holders"`
	License    string                        `json:"license,omitempty"`
	Guest      bool                          `json:"guest_playable,omitempty"`

	// Voices is how many independent branches those holders amount to — the
	// number versions are actually ordered by (F7 item 10). Sent only when it is
	// SMALLER than the holder count, which is the only case where it tells the
	// reader something the holder list does not: several nodes, one voice.
	Voices int `json:"voices,omitempty"`
	// voices is the same count, always set, because the ordering needs it for
	// every version and JSON is a display concern.
	voices int

	// URL is the direct local play address when the version's ladder-best
	// rendition is in this node's library — no relay hop through the cache.
	URL string `json:"url,omitempty"`
}

type madnetworkHolder struct {
	Name      string `json:"name"`
	LastSeen  int64  `json:"last_seen"`
	Self      bool   `json:"self,omitempty"`      // this server
	Reachable bool   `json:"reachable,omitempty"` // seen within the freshness window
	// Key is the holder's node key, so the UI can link a holder to its place on
	// the network map (F7 item 7): finding out who served something starts from
	// the content that exposed it, not from an admin remembering to go and look
	// at a diagram. Empty for the self holder — we are not on our own map as a
	// stranger.
	Key string `json:"key,omitempty"`
}

// madnetworkTracks handles GET /api/madnetwork/tracks?artist=&album=: the
// album's merged track rows with their versions — friends' cached rows plus,
// when the federation node runs, this node's own published rows.
func (h *handler) madnetworkTracks(w http.ResponseWriter, r *http.Request) {
	artist, album := r.URL.Query().Get("artist"), r.URL.Query().Get("album")
	if artist == "" || album == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "artist and album are required"})
		return
	}
	view := h.madnetworkViewFor(r)
	rows, err := h.madnetwork.MadnetworkTracks(r.Context(), artist, album, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	own, err := h.madnetwork.MadnetworkOwnTracks(r.Context(), artist, album, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if len(own) > 0 {
		rows = append(rows, own...)
		sortMadnetworkRows(rows)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tracks": mergeMadnetworkTracks(rows, h.mergeOpts(r.Context()))})
}

// sortMadnetworkRows restores display order over a combined remote+own row
// set — the Go mirror of the SQL ORDER BY (disc IS NULL last, disc, track
// with SQLite's NULL-first, title, self rows first within a track).
func sortMadnetworkRows(rows []*database.MadnetworkTrackRow) {
	val := func(p *int64, null int64) int64 {
		if p == nil {
			return null
		}
		return *p
	}
	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a], rows[b]
		da, db_ := ra.Entry.DiscNumber == nil, rb.Entry.DiscNumber == nil
		if da != db_ {
			return db_ // non-null discs first
		}
		if x, y := val(ra.Entry.DiscNumber, 0), val(rb.Entry.DiscNumber, 0); x != y {
			return x < y
		}
		if x, y := val(ra.Entry.TrackNumber, math.MinInt64), val(rb.Entry.TrackNumber, math.MinInt64); x != y {
			return x < y
		}
		if x, y := strings.ToLower(ra.Entry.Title), strings.ToLower(rb.Entry.Title); x != y {
			return x < y
		}
		return ra.SourceID < rb.SourceID // self (0) first, then cached sources as before
	})
}

// mergeMadnetworkTracks folds raw per-(source,appearance) rows into logical
// tracks and versions. Rows arrive in display order; groups keep first-seen
// order.
func mergeMadnetworkTracks(rows []*database.MadnetworkTrackRow, opts mergeOpts) []*madnetworkTrack {
	type ident struct {
		disc, track int64
		title       string
	}
	key := func(e *federation.CatalogEntry) ident {
		id := ident{disc: -1, track: -1, title: strings.ToLower(e.Title)}
		if e.DiscNumber != nil {
			id.disc = *e.DiscNumber
		}
		if e.TrackNumber != nil {
			id.track = *e.TrackNumber
		}
		return id
	}

	tracks := []*madnetworkTrack{}
	groups := map[ident][]*database.MadnetworkTrackRow{}
	order := []ident{}
	for _, row := range rows {
		k := key(&row.Entry)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], row)
	}

	for _, k := range order {
		group := groups[k]
		t := &madnetworkTrack{
			Title: group[0].Entry.Title,
			Track: group[0].Entry.TrackNumber,
			Disc:  group[0].Entry.DiscNumber,
		}
		for _, row := range group {
			if t.Artist == "" {
				t.Artist = row.Entry.Artist
			}
			if t.Duration == 0 {
				t.Duration = row.Entry.Duration
			}
			if t.TagsetID == 0 && row.Self {
				if id, err := strconv.ParseInt(row.Entry.Key, 10, 64); err == nil {
					t.TagsetID = id
				}
			}
		}
		t.Versions = mergeVersions(group, opts)
		tracks = append(tracks, t)
	}
	return tracks
}

// mergeVersions unions a track group's rows into display versions: two claimed
// recordings are the same version iff they share a rendition content hash
// (same bytes somewhere = same audio for sure). Everything else stays a
// separate version — recordings are never merged on text alone.
func mergeVersions(group []*database.MadnetworkTrackRow, opts mergeOpts) []madnetworkVersion {
	// Union-find over the group's rows, linked by shared hashes.
	parent := make([]int, len(group))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	byHash := map[string]int{}
	for i, row := range group {
		for _, rd := range row.Entry.Renditions {
			if rd.Hash == "" {
				continue
			}
			if j, ok := byHash[rd.Hash]; ok {
				parent[find(i)] = find(j)
			} else {
				byHash[rd.Hash] = i
			}
		}
	}

	sets := map[int][]int{}
	roots := []int{}
	for i := range group {
		r := find(i)
		if _, seen := sets[r]; !seen {
			roots = append(roots, r)
		}
		sets[r] = append(sets[r], i)
	}

	versions := []madnetworkVersion{}
	for _, r := range roots {
		var v madnetworkVersion
		v.Renditions = []federation.CatalogRendition{}
		v.Holders = []madnetworkHolder{}
		seenHash := map[string]bool{}
		seenPeer := map[int64]bool{}
		objectKeys := map[string]string{} // local hash -> files object key (self rows)
		// voiceKeys is the holder identity the branch weighting counts, which is
		// the node KEY and not the source row: an unkeyed holder (a row cached
		// before keys, a test double) gets a token of its own, because "we cannot
		// place this node" must widen to one voice per node, never narrow to one
		// voice for all of them.
		var voiceKeys []string
		var selfHolds bool
		for _, i := range sets[r] {
			row := group[i]
			for _, rd := range row.Entry.Renditions {
				if rd.Hash == "" || seenHash[rd.Hash] {
					continue
				}
				seenHash[rd.Hash] = true
				// The fingerprint claim is evidence for the contradiction checks,
				// not something a browser has any use for — and it is ~340 bytes
				// per rendition. Drop it on the way out.
				rd.Fingerprint = nil
				v.Renditions = append(v.Renditions, rd)
			}
			if !seenPeer[row.SourceID] {
				seenPeer[row.SourceID] = true
				if row.Self {
					selfHolds = true
					v.Holders = append(v.Holders, madnetworkHolder{Name: opts.selfName, Self: true, Reachable: true})
				} else {
					v.Holders = append(v.Holders, madnetworkHolder{
						Name: row.SourceName, LastSeen: row.SourceLastSeen,
						Reachable: opts.reach.ok(row.SourceLastSeen, row.SourcePinged),
						Key:       row.SourceKey,
					})
					if row.SourceKey != "" {
						voiceKeys = append(voiceKeys, row.SourceKey)
					} else {
						voiceKeys = append(voiceKeys, "src:"+strconv.FormatInt(row.SourceID, 10))
					}
				}
			}
			for hash, key := range row.ObjectKeys {
				objectKeys[hash] = key
			}
			if v.License == "" {
				v.License = row.Entry.License
			}
			v.Guest = v.Guest || row.Entry.GuestPlayable
		}
		// Ladder-best rendition first (F3: the version-level Play/Download act
		// on renditions[0]) — the same deterministic quality ladder that ranks
		// local renditions, fed with the advertised quality facts.
		ranked := make([]database.Rendition, len(v.Renditions))
		byHash2 := map[string]federation.CatalogRendition{}
		for i, rd := range v.Renditions {
			ranked[i] = database.Rendition{Hash: rd.Hash, Codec: rd.Codec,
				Bitrate: int(rd.Bitrate), SampleRate: int(rd.SampleRate),
				BitDepth: int(rd.BitDepth), ByteSize: rd.Size}
			byHash2[rd.Hash] = rd
		}
		for i, rr := range database.RankRenditions(ranked) {
			v.Renditions[i] = byHash2[rr.Hash]
		}
		// Direct local play URL when the version's default pick lives in this
		// node's library.
		if len(v.Renditions) > 0 {
			if key, ok := objectKeys[v.Renditions[0].Hash]; ok {
				v.URL = "/files/" + key
			}
		}
		v.voices = opts.branches.Voices(voiceKeys, selfHolds)
		// Reported only when it is news — when independent agreement is thinner
		// than the holder count suggests. Sending it always would put "1 voice"
		// beside every single-holder row on the page, which is wallpaper.
		if v.voices < len(v.Holders) {
			v.Voices = v.voices
		}
		versions = append(versions, v)
	}
	// Most widely held version first — the doc's default pick for a crossing —
	// counted in VOICES, not in holders (F7 item 10). This is the sharpest place
	// the sybil rule applies on the whole page: a crossing's leading version is
	// what Play, Queue and Materialize act on, so a farm of keys behind one
	// friendship could otherwise make its claim the default pick for everyone
	// who browses to that track. Behind one friendship it is one voice, and it
	// leads only if nobody independent disagrees.
	sort.SliceStable(versions, func(a, b int) bool {
		if versions[a].voices != versions[b].voices {
			return versions[a].voices > versions[b].voices
		}
		return len(versions[a].Holders) > len(versions[b].Holders)
	})
	return versions
}

// ── Merged search (docs/ui/madnetwork-page.md §Search) ───────────────────────

const (
	madnetworkSearchArtistCap = 5
	madnetworkSearchAlbumCap  = 5
	madnetworkSearchTrackCap  = 20
)

// madnetworkSearch handles GET /api/madnetwork/search?q= — the library-style
// three-section search over the merged (friends ∪ own) catalog. Field names
// mirror /api/search so the shared search view renders both; addressing is by
// display name (the merged catalog has no entity ids), and each playable track
// carries its default rendition hash plus a direct local url when self-held.
func (h *handler) madnetworkSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}

	type searchTrack struct {
		Title      string   `json:"title"`
		ArtistName string   `json:"artist_name,omitempty"`
		Artist     string   `json:"artist"` // grouping artist (drill address)
		AlbumTitle string   `json:"album_title"`
		Duration   *float64 `json:"duration_seconds,omitempty"`
		TagsetID   int64    `json:"tagset_id,omitempty"`
		Hash       string   `json:"hash"`
		URL        string   `json:"url,omitempty"` // local play address when self-held
	}

	view := h.madnetworkViewFor(r)
	// Search matches an artist in EITHER credit, so a performer the A-Z list
	// leaves out (they have no release of their own) is still findable by name —
	// the split the local library's search and browse make too.
	artists, err := h.madnetwork.MadnetworkSearchArtists(r.Context(), q, madnetworkSearchArtistCap, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if artists == nil {
		artists = []*database.MadnetworkArtist{}
	}

	albums, err := h.madnetwork.MadnetworkSearchAlbums(r.Context(), q, madnetworkSearchAlbumCap, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	rows, err := h.madnetwork.MadnetworkSearchTrackRows(r.Context(), q, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	// Group cross-album rows by their display-identity bucket and run the
	// album-scale merge per group, so version folding stays correct.
	type bucket struct{ artist, album string }
	groups := map[bucket][]*database.MadnetworkTrackRow{}
	order := []bucket{}
	for _, row := range rows {
		b := bucket{strings.ToLower(row.GroupArtist), strings.ToLower(row.GroupAlbum)}
		if _, seen := groups[b]; !seen {
			order = append(order, b)
		}
		groups[b] = append(groups[b], row)
	}
	tracks := []searchTrack{}
	opts := h.mergeOpts(r.Context())
merge:
	for _, b := range order {
		group := groups[b]
		for _, t := range mergeMadnetworkTracks(group, opts) {
			if len(t.Versions) == 0 || len(t.Versions[0].Renditions) == 0 {
				continue // nothing playable to offer from search
			}
			var dur *float64
			if t.Duration > 0 {
				d := t.Duration
				dur = &d
			}
			tracks = append(tracks, searchTrack{
				Title:      t.Title,
				ArtistName: t.Artist,
				Artist:     group[0].GroupArtist,
				AlbumTitle: group[0].GroupAlbum,
				Duration:   dur,
				TagsetID:   t.TagsetID,
				Hash:       t.Versions[0].Renditions[0].Hash,
				URL:        t.Versions[0].URL,
			})
			if len(tracks) >= madnetworkSearchTrackCap {
				break merge
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "artists": artists, "albums": albums, "tracks": tracks,
	})
}
