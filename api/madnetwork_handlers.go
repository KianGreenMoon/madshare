package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// /api/madnetwork — the merged madnetwork catalog browse (federation F2,
// docs/architecture/federation.md §Catalog), backing the /madnetwork
// drill-down. Read-only over the per-peer cached catalogs; gated
// madnetwork.access. Browse-only in F2 — playing/downloading remote content
// arrives with F3 (direct transfer).

// The view/merge machinery lives on MadnetworkBrowse (madnetwork_browse.go)
// since the browse gained its second consumer, the app facade. These handler
// methods are delegators kept under their old names so every madnetwork
// handler file keeps reading as it did — and so no handler can acquire a
// private copy of a rule the facade would then not share.

func (h *handler) includeSelf() bool { return h.mn.includeSelf() }

// defaultReachableWindowSec mirrors config.DefaultReachableWindowSec for the
// paths that carry no configured window (tests via NewRouter). The running
// server always passes the (validated, ≥ min) config value.
const defaultReachableWindowSec = 180

func (h *handler) reachWindow() int64 { return h.mn.reachWindow() }
func (h *handler) pullWindow() int64  { return h.mn.pullWindow() }

// reachWindows is the pair of freshness cutoffs a request is judged by, and the
// rule that picks between them. Passed around as one value so no caller can
// apply a window without the class that selects it.
type reachWindows struct{ tight, pull int64 }

// ok reports whether a source counts as reachable, from the three facts its row
// carries: when it was last seen, when a first-hand attempt last failed against
// it, and which class of observer watches it.
func (rw reachWindows) ok(r database.SourceReach) bool {
	return database.ReachableAt(r, rw.tight, rw.pull)
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
	return h.mn.View(ctx)
}

func (h *handler) inboundHealthy() bool { return h.mn.inboundHealthy() }

func (h *handler) reachCutoff() reachWindows { return h.mn.reachCutoff() }

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

func (h *handler) mergeOpts(ctx context.Context) mergeOpts { return h.mn.mergeOpts(ctx) }

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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) // ≤0 = the cap, clamped in Artists
	artists, next, err := h.mn.Artists(r.Context(), r.URL.Query().Get("q"),
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
	albums, err := h.mn.Albums(r.Context(), artist, h.madnetworkViewFor(r))
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
type MadnetworkTrack struct {
	Title    string  `json:"title"`
	Artist   string  `json:"artist,omitempty"` // performer display (may differ from the grouping artist)
	Track    *int64  `json:"track_number,omitempty"`
	Disc     *int64  `json:"disc_number,omitempty"`
	Duration float64 `json:"duration,omitempty"`

	// TagsetID is the local appearance behind a self-held track (hearts,
	// playlists); zero when this node does not publish the track itself.
	TagsetID int64 `json:"tagset_id,omitempty"`

	// CoverHash / CoverExt are the row's album-cover verdict, elected from the
	// contributing sources' claims by the voices rule (coverBallot) and
	// fetchable via /api/madnetwork/cover/{hash}. Track-level because a merged
	// row IS its sources — two versions of an album on the network can
	// legitimately paint two arts, and hiding that would decide a dispute the
	// voices rule exists to display.
	CoverHash string `json:"cover_hash,omitempty"`
	CoverExt  string `json:"cover_ext,omitempty"`

	Versions []MadnetworkVersion `json:"versions"`
}

type MadnetworkVersion struct {
	Renditions []federation.CatalogRendition `json:"renditions"`
	Holders    []MadnetworkHolder            `json:"holders"`
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

type MadnetworkHolder struct {
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
	tracks, err := h.mn.Tracks(r.Context(), artist, album, h.madnetworkViewFor(r))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tracks": tracks})
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
func mergeMadnetworkTracks(rows []*database.MadnetworkTrackRow, opts mergeOpts) []*MadnetworkTrack {
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

	tracks := []*MadnetworkTrack{}
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
		t := &MadnetworkTrack{
			Title: group[0].Entry.Title,
			Track: group[0].Entry.TrackNumber,
			Disc:  group[0].Entry.DiscNumber,
		}
		for _, row := range group {
			if t.Artist == "" {
				t.Artist = trackCredit(row)
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
		var ballot coverBallot
		for _, row := range group {
			ballot.add(row.Entry.CoverHash, row.Entry.CoverExt, row.SourceKey, row.Self)
		}
		t.CoverHash, t.CoverExt = ballot.winner(opts.branches)
		t.Versions = mergeVersions(group, opts)
		tracks = append(tracks, t)
	}
	return tracks
}

// trackCredit is the performer to show for one cached row.
//
// It is database's effectiveTrackArtist over text rather than entities: the
// performer of a track with no artist tag IS its album artist, and only then the
// Unknown bucket. The local library resolves that at import and can never hand
// out an empty credit; a cached row carries whatever its node published, which
// for a file tagged with an album artist and no artist tag is nothing at all. A
// client renders this field verbatim — as it should, the server decides the
// names — so the fallback has to happen here or the row draws a blank.
//
// GroupArtist is the last resort because it is never empty: it is the bucket the
// row was found in, which is the album's artist by construction.
func trackCredit(row *database.MadnetworkTrackRow) string {
	for _, s := range []string{row.Entry.Artist, row.Entry.AlbumArtist, row.GroupArtist} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// mergeVersions unions a track group's rows into display versions: two claimed
// recordings are the same version iff they share a rendition content hash
// (same bytes somewhere = same audio for sure). Everything else stays a
// separate version — recordings are never merged on text alone.
func mergeVersions(group []*database.MadnetworkTrackRow, opts mergeOpts) []MadnetworkVersion {
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

	versions := []MadnetworkVersion{}
	for _, r := range roots {
		var v MadnetworkVersion
		v.Renditions = []federation.CatalogRendition{}
		v.Holders = []MadnetworkHolder{}
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
					v.Holders = append(v.Holders, MadnetworkHolder{Name: opts.selfName, Self: true, Reachable: true})
				} else {
					v.Holders = append(v.Holders, MadnetworkHolder{
						Name: row.SourceName, LastSeen: row.SourceLastSeen,
						Reachable: opts.reach.ok(database.SourceReach{
							LastSeen:      row.SourceLastSeen,
							UnreachableAt: row.SourceUnreachableAt,
							Pinged:        row.SourcePinged,
						}),
						Key: row.SourceKey,
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

	res, err := h.mn.Search(r.Context(), q, h.madnetworkViewFor(r))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "artists": res.Artists, "albums": res.Albums, "tracks": res.Tracks,
	})
}
