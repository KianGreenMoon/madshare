package api

import (
	"net/http"
	"sort"
	"strings"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// /api/madnetwork — the merged madnetwork catalog browse (federation F2,
// docs/architecture/federation.md §Catalog), backing the /madnetwork
// drill-down. Read-only over the per-peer cached catalogs; gated
// madnetwork.access. Browse-only in F2 — playing/downloading remote content
// arrives with F3 (direct transfer).

// madnetworkSummary handles GET /api/madnetwork/summary: each friend's sync
// state plus the merged distinct-track count — the page's status strip.
func (h *handler) madnetworkSummary(w http.ResponseWriter, r *http.Request) {
	friends, tracks, err := h.madnetwork.MadnetworkSummary(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if friends == nil {
		friends = []*database.MadnetworkFriend{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "friends": friends, "tracks": tracks})
}

// madnetworkArtists handles GET /api/madnetwork/artists[?q=]: the merged
// artist list (album-artist grouping, like the local library).
func (h *handler) madnetworkArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := h.madnetwork.MadnetworkArtists(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if artists == nil {
		artists = []*database.MadnetworkArtist{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "artists": artists})
}

// madnetworkAlbums handles GET /api/madnetwork/albums?artist=<name>.
func (h *handler) madnetworkAlbums(w http.ResponseWriter, r *http.Request) {
	artist := r.URL.Query().Get("artist")
	if artist == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "artist is required"})
		return
	}
	albums, err := h.madnetwork.MadnetworkAlbums(r.Context(), artist)
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

	Versions []madnetworkVersion `json:"versions"`
}

type madnetworkVersion struct {
	Renditions []federation.CatalogRendition `json:"renditions"`
	Holders    []madnetworkHolder            `json:"holders"`
	License    string                        `json:"license,omitempty"`
	Guest      bool                          `json:"guest_playable,omitempty"`
}

type madnetworkHolder struct {
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
}

// madnetworkTracks handles GET /api/madnetwork/tracks?artist=&album=: the
// album's merged track rows with their versions.
func (h *handler) madnetworkTracks(w http.ResponseWriter, r *http.Request) {
	artist, album := r.URL.Query().Get("artist"), r.URL.Query().Get("album")
	if artist == "" || album == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "artist and album are required"})
		return
	}
	rows, err := h.madnetwork.MadnetworkTracks(r.Context(), artist, album)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tracks": mergeMadnetworkTracks(rows)})
}

// mergeMadnetworkTracks folds raw per-(peer,appearance) rows into logical
// tracks and versions. Rows arrive in display order; groups keep first-seen
// order.
func mergeMadnetworkTracks(rows []*database.MadnetworkTrackRow) []*madnetworkTrack {
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
		}
		t.Versions = mergeVersions(group)
		tracks = append(tracks, t)
	}
	return tracks
}

// mergeVersions unions a track group's rows into display versions: two claimed
// recordings are the same version iff they share a rendition content hash
// (same bytes somewhere = same audio for sure). Everything else stays a
// separate version — recordings are never merged on text alone.
func mergeVersions(group []*database.MadnetworkTrackRow) []madnetworkVersion {
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
		for _, i := range sets[r] {
			row := group[i]
			for _, rd := range row.Entry.Renditions {
				if rd.Hash == "" || seenHash[rd.Hash] {
					continue
				}
				seenHash[rd.Hash] = true
				v.Renditions = append(v.Renditions, rd)
			}
			if !seenPeer[row.PeerID] {
				seenPeer[row.PeerID] = true
				v.Holders = append(v.Holders, madnetworkHolder{Name: row.PeerName, LastSeen: row.PeerLastSeen})
			}
			if v.License == "" {
				v.License = row.Entry.License
			}
			v.Guest = v.Guest || row.Entry.GuestPlayable
		}
		versions = append(versions, v)
	}
	// Most widely held version first — the doc's default pick for a crossing.
	sort.SliceStable(versions, func(a, b int) bool {
		return len(versions[a].Holders) > len(versions[b].Holders)
	})
	return versions
}
