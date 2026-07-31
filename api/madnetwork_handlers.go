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

// madnetworkViewFor is madnetworkView plus the request's optional single-node
// restriction — the "By node" shelf (?source=<id>, or ?source=self for our own
// published library). An unparseable source is the merged view rather than an
// error: a stale link should land somewhere useful.
func (h *handler) madnetworkViewFor(r *http.Request) database.MadnetworkView {
	v := h.madnetworkView(r.Context())
	switch src := r.URL.Query().Get("source"); {
	case src == "":
	case src == "self":
		v.SelfOnly = true
	default:
		if id, err := strconv.ParseInt(src, 10, 64); err == nil && id > 0 {
			v.SourceID = id
		}
	}
	return v
}

func (h *handler) madnetworkView(ctx context.Context) database.MadnetworkView {
	v := database.MadnetworkView{IncludeSelf: h.includeSelf(), DefaultShareDepth: federation.DepthFriends}
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
	v.Cutoff = time.Now().Unix() - h.reachWindow()
	return v
}

// inboundHealthy reports the node's self-health (true when federation is off or
// absent — there is nothing to fail open for).
func (h *handler) inboundHealthy() bool {
	return h.federation == nil || h.federation.InboundHealthy()
}

// reachCutoff is the freshness cutoff used to *display* holder reachability (the
// ⓘ panel greys a holder past it). Always now−window, independent of the browse
// filter cutoff — so when the view fails open (showing everything), stale holders
// still read as stale rather than all looking reachable.
func (h *handler) reachCutoff() int64 { return time.Now().Unix() - h.reachWindow() }

// madnetworkSummary handles GET /api/madnetwork/summary: each friend's sync
// state plus the merged distinct-track count — the page's status strip. With
// the own set merged in, self_name labels this node's contribution.
func (h *handler) madnetworkSummary(w http.ResponseWriter, r *http.Request) {
	friends, tracks, err := h.madnetwork.MadnetworkSummary(r.Context(), h.madnetworkView(r.Context()))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if friends == nil {
		friends = []*database.MadnetworkFriend{}
	}
	resp := map[string]any{"ok": true, "friends": friends, "tracks": tracks,
		"inbound_healthy": h.inboundHealthy()}
	if h.includeSelf() {
		resp["self_name"] = h.madnetworkName
	}
	writeJSON(w, http.StatusOK, resp)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tracks": mergeMadnetworkTracks(rows, h.madnetworkName, h.reachCutoff())})
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
// order. selfName labels the self holder of own rows.
func mergeMadnetworkTracks(rows []*database.MadnetworkTrackRow, selfName string, reachCutoff int64) []*madnetworkTrack {
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
		t.Versions = mergeVersions(group, selfName, reachCutoff)
		tracks = append(tracks, t)
	}
	return tracks
}

// mergeVersions unions a track group's rows into display versions: two claimed
// recordings are the same version iff they share a rendition content hash
// (same bytes somewhere = same audio for sure). Everything else stays a
// separate version — recordings are never merged on text alone.
func mergeVersions(group []*database.MadnetworkTrackRow, selfName string, reachCutoff int64) []madnetworkVersion {
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
					v.Holders = append(v.Holders, madnetworkHolder{Name: selfName, Self: true, Reachable: true})
				} else {
					v.Holders = append(v.Holders, madnetworkHolder{
						Name: row.SourceName, LastSeen: row.SourceLastSeen,
						Reachable: reachCutoff <= 0 || row.SourceLastSeen >= reachCutoff,
						Key:       row.SourceKey,
					})
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
		versions = append(versions, v)
	}
	// Most widely held version first — the doc's default pick for a crossing.
	sort.SliceStable(versions, func(a, b int) bool {
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
	artists := []*database.MadnetworkArtist{}
	if strings.TrimSpace(q) != "" { // an empty query lists everything — search shows nothing
		found, _, err := h.madnetwork.MadnetworkArtists(r.Context(), q, view, madnetworkSearchArtistCap, "")
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if found != nil {
			artists = found
		}
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
	rc := h.reachCutoff()
merge:
	for _, b := range order {
		group := groups[b]
		for _, t := range mergeMadnetworkTracks(group, h.madnetworkName, rc) {
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
