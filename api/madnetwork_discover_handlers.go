package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/database"
)

// The /madnetwork landing view (docs/ui/madnetwork-page.md §Lane definitions).
//
// An alphabet answers "show me everything, in an order unrelated to anything I
// care about". These endpoints answer questions instead — what can I get that I
// don't have, what appeared since I last looked, what does my community actually
// have, what is nearly gone, what did the people I chose personally bring. Each
// is a plain fact about the cached catalogs, computed at request time from what
// THIS node can see, and nothing here infers a taste or builds a profile.

const (
	// laneRows is the digest: how many rows a lane shows on the landing view.
	laneRows = 8
	// laneSeeAllRows is one page of a lane's own view, and laneCandidateCap the
	// ceiling on how deep a ranking is computed at all. The cap exists because
	// the "most held" ranking is finished in Go (branch weighting), so the rows
	// it re-sorts have to be a bounded set rather than the whole catalog.
	laneSeeAllRows     = 50
	laneCandidateCap   = 2000
	laneCandidateSlack = 4 // extra candidates per row, so capping has room to spread
)

// laneTitles are the headings, and the vocabulary is deliberate: "here" and
// "your" say these counts are this node's view, never a network-wide chart.
var laneTitles = map[string]string{
	database.LaneMissing: "Not in your library",
	database.LaneNew:     "New on the network",
	database.LaneHeld:    "Most held here",
	database.LaneRare:    "Only one node has it",
	database.LaneFriends: "From your direct friends",
}

// laneCapped reports whether a lane's digest limits how much one node may
// contribute. Only the two lanes a single node's VOLUME could own: reaching a
// node for the first time makes its whole library new to us at once, and one
// node with a big exclusive shelf would otherwise be the entire rare lane. The
// three corroboration-ranked lanes need no cap — volume from one node cannot
// lift a row in them at all, so capping there would only show worse answers.
func laneCapped(lane string) bool {
	return lane == database.LaneNew || lane == database.LaneRare
}

// laneTrack is one lane row: the merged track exactly as the drill-down renders
// it, plus the address to drill to and the facts that explain why it is here.
// Every lane row is explainable — that is a rule of the page, not a nicety.
type laneTrack struct {
	*madnetworkTrack
	GroupArtist string `json:"group_artist"`
	Album       string `json:"album"`
	Holders     int    `json:"holders"`
	Branches    int    `json:"branches,omitempty"`
	FirstSeen   int64  `json:"first_seen,omitempty"`
	SourceName  string `json:"source_name,omitempty"`
	SelfHeld    bool   `json:"self_held,omitempty"`
}

type laneResponse struct {
	Name   string      `json:"name"`
	Title  string      `json:"title"`
	Tracks []laneTrack `json:"tracks"`
	// More reports that the lane has rows beyond the ones returned, so the UI
	// offers "See all" only when there is a beyond.
	More bool `json:"more"`
}

// madnetworkDiscover handles GET /api/madnetwork/discover: every lane's digest
// in one round trip, since the landing view shows them all at once.
func (h *handler) madnetworkDiscover(w http.ResponseWriter, r *http.Request) {
	view := h.madnetworkView(r.Context())
	branches := h.branchesByKey(r.Context())

	lanes := []laneResponse{}
	for _, name := range database.LaneNames {
		lane, err := h.buildLane(r.Context(), name, view, branches, laneRows, 0)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if len(lane.Tracks) == 0 {
			continue // an empty lane is noise, not information
		}
		lanes = append(lanes, lane)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lanes": lanes})
}

// madnetworkLane handles GET /api/madnetwork/lane?name=&offset=: one lane on its
// own page, in the same ranking with the per-source cap lifted — the cap is a
// property of the eight-row digest, not of the ranking, so "See all" really does
// show all of it.
func (h *handler) madnetworkLane(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !database.ValidLane(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown lane"})
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	view := h.madnetworkView(r.Context())
	lane, err := h.buildLane(r.Context(), name, view, h.branchesByKey(r.Context()), laneSeeAllRows, offset)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "lane": lane, "offset": offset, "limit": laneSeeAllRows,
	})
}

// buildLane ranks, trims and renders one lane. offset > 0 means the lane's own
// page, which skips the per-source cap: the cap shapes a digest, and a person
// who asked to see all of something has asked for the ranking itself.
func (h *handler) buildLane(ctx context.Context, name string, view database.MadnetworkView,
	branches map[string][]string, limit, offset int) (laneResponse, error) {
	out := laneResponse{Name: name, Title: laneTitles[name], Tracks: []laneTrack{}}

	want := offset + limit + 1 // the +1 answers "is there more" without a count
	candidates, err := h.madnetwork.MadnetworkLaneCandidates(ctx, name, view, laneCandidateBudget(want))
	if err != nil {
		return out, err
	}
	if name == database.LaneHeld {
		database.WeightByBranch(candidates, branches)
	}
	if offset == 0 && laneCapped(name) {
		candidates = database.CapPerSource(candidates, want)
	}
	if offset >= len(candidates) {
		return out, nil
	}
	candidates = candidates[offset:]
	if len(candidates) > limit {
		candidates, out.More = candidates[:limit], true
	}

	idents := make([]string, 0, len(candidates))
	for _, c := range candidates {
		idents = append(idents, c.Ident)
	}
	rows, err := h.madnetwork.MadnetworkRowsForIdents(ctx, idents, view)
	if err != nil {
		return out, err
	}
	out.Tracks = h.renderLaneTracks(candidates, rows)
	return out, nil
}

// laneCandidateBudget is how deep to rank. Capping and branch weighting both
// reorder within the candidate set, so it has to be wider than the rows asked
// for — but bounded, because this is a landing view and not a report.
func laneCandidateBudget(want int) int {
	budget := want * laneCandidateSlack
	if budget > laneCandidateCap {
		return laneCandidateCap
	}
	return budget
}

// laneKey is a merged track's identity as Go sees it — the Go-side twin of
// trackFullIdent, used only to pair a ranked candidate with the merged track it
// stands for. Both sides are folded with Go's ToLower here (never one side in
// SQL and the other in Go), so the pairing cannot drift on non-ASCII titles.
type laneKey struct {
	artist, album, title string
	disc, track          int64
}

func candidateKey(c *database.LaneCandidate) laneKey {
	return laneKey{
		artist: strings.ToLower(c.Artist), album: strings.ToLower(c.Album),
		title: strings.ToLower(c.Title), disc: c.Disc, track: c.Track,
	}
}

// renderLaneTracks merges the raw rows through the same path the drill-down and
// search use — one row anatomy on the page, not two — and returns them in the
// lane's ranking rather than the merge's.
func (h *handler) renderLaneTracks(candidates []*database.LaneCandidate, rows []*database.MadnetworkTrackRow) []laneTrack {
	type bucket struct{ artist, album string }
	groups := map[bucket][]*database.MadnetworkTrackRow{}
	for _, row := range rows {
		b := bucket{strings.ToLower(row.GroupArtist), strings.ToLower(row.GroupAlbum)}
		groups[b] = append(groups[b], row)
	}

	rc := h.reachCutoff()
	merged := map[laneKey]*madnetworkTrack{}
	display := map[laneKey]bucket{}
	for b, group := range groups {
		sortMadnetworkRows(group)
		for _, t := range mergeMadnetworkTracks(group, h.madnetworkName, rc) {
			k := laneKey{artist: b.artist, album: b.album, title: strings.ToLower(t.Title),
				disc: -1, track: -1}
			if t.Disc != nil {
				k.disc = *t.Disc
			}
			if t.Track != nil {
				k.track = *t.Track
			}
			if _, seen := merged[k]; !seen {
				merged[k] = t
				display[k] = bucket{group[0].GroupArtist, group[0].GroupAlbum}
			}
		}
	}

	out := []laneTrack{}
	for _, c := range candidates {
		k := candidateKey(c)
		t, ok := merged[k]
		if !ok || len(t.Versions) == 0 || len(t.Versions[0].Renditions) == 0 {
			continue // nothing playable to offer, so nothing to put in a lane
		}
		d := display[k]
		out = append(out, laneTrack{
			madnetworkTrack: t,
			GroupArtist:     d.artist,
			Album:           d.album,
			Holders:         c.Holders,
			Branches:        c.Branches,
			FirstSeen:       c.FirstSeen,
			SourceName:      c.SourceName,
			SelfHeld:        c.Self,
		})
		delete(merged, k) // a merged track fills one lane row, however it was ranked
	}
	return out
}

// branchesByKey maps every node we can see to the direct friends it reaches us
// through — the branch attribution the "most held" lane weights by (one branch
// is one voice, docs/architecture/federation.md §Trust graph). Empty when there
// is no federation node or no graph yet, which degrades the lane to one source
// one voice: the same rule in a smaller world, never a wrong answer.
func (h *handler) branchesByKey(ctx context.Context) map[string][]string {
	if h.federation == nil {
		return nil
	}
	m, err := h.federation.NetworkMap(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string][]string, len(m.Nodes))
	for _, n := range m.Nodes {
		if len(n.Via) > 0 {
			out[n.Key] = n.Via
		}
	}
	return out
}
