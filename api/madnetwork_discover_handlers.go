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

// laneTitles are the headings, and the vocabulary is deliberate on two counts.
// "here" says these numbers are this node's view, never a network-wide chart.
// And no title says "your": the library and the friend list belong to the node's
// owner, not to whoever is signed in — most readers are users of somebody else's
// server (docs/ui/madnetwork-page.md §The page stops saying "your"). What
// replaces the possessive is where the content sits, here or not here, which is
// a fact and true for every reader.
var laneTitles = map[string]string{
	database.LaneLocal:   "Local library",
	database.LaneMissing: "Missing here",
	database.LaneNew:     "New on the network",
	database.LaneHeld:    "Most held here",
	database.LaneRare:    "Only one node has it",
	database.LaneFriends: "From direct friends",
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

// laneWeighted reports whether a lane is re-sorted by branch count in Go — the
// lanes SQL ranks by a raw holder count, and only those (F7 item 10):
//
//   - "friends" is already branch-weighted by construction, since every direct
//     friend is the root of its own branch;
//   - "rare" is one holder, which is one branch whatever the graph says;
//   - "new" ranks by date and not by agreement at all.
//
// Stated as a predicate rather than left in the caller so the answer to "which
// popularity counts are trust-weighted" is one readable list.
func laneWeighted(lane string) bool {
	return lane == database.LaneHeld || lane == database.LaneMissing
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
	opts := h.mergeOpts(r.Context()) // one branch-map read for every lane on the page

	lanes := []laneResponse{}
	for _, name := range database.LaneNames {
		lane, err := h.buildLane(r.Context(), name, view, opts, laneRows, 0)
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
	lane, err := h.buildLane(r.Context(), name, view, h.mergeOpts(r.Context()), laneSeeAllRows, offset)
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
	opts mergeOpts, limit, offset int) (laneResponse, error) {
	out := laneResponse{Name: name, Title: laneTitles[name], Tracks: []laneTrack{}}
	// The Local library lane is the one view on this page that is about our own
	// shelf rather than about the network, so it carries the whole library —
	// recordings scoped Local included. Its "See all" goes to the library page,
	// which would show them anyway; leaving them out here would only make the
	// lane disagree with the page it leads to.
	if name == database.LaneLocal {
		view.AllOwn = true
	}

	want := offset + limit + 1 // the +1 answers "is there more" without a count
	candidates, err := h.madnetwork.MadnetworkLaneCandidates(ctx, name, view, laneCandidateBudget(want))
	if err != nil {
		return out, err
	}
	if laneWeighted(name) {
		database.WeightByBranch(candidates, opts.branches)
	}
	// "Is there more" is decided on the RANKING, before any capping — the cap
	// changes which rows the digest shows, never how much the lane holds.
	out.More = len(candidates) > offset+limit
	if offset == 0 && laneCapped(name) {
		// Capped against the rows actually shown, not against the extra
		// probe row: a quota computed over limit+1 hands the dominant node
		// one more slot than the digest has room to justify.
		candidates = database.CapPerSource(candidates, limit)
	}
	if offset >= len(candidates) {
		return out, nil
	}
	candidates = candidates[offset:]
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	idents := make([]string, 0, len(candidates))
	for _, c := range candidates {
		idents = append(idents, c.Ident)
	}
	rows, err := h.madnetwork.MadnetworkRowsForIdents(ctx, idents, view)
	if err != nil {
		return out, err
	}
	out.Tracks = h.renderLaneTracks(candidates, rows, opts)
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
func (h *handler) renderLaneTracks(candidates []*database.LaneCandidate, rows []*database.MadnetworkTrackRow, opts mergeOpts) []laneTrack {
	type bucket struct{ artist, album string }
	groups := map[bucket][]*database.MadnetworkTrackRow{}
	for _, row := range rows {
		b := bucket{strings.ToLower(row.GroupArtist), strings.ToLower(row.GroupAlbum)}
		groups[b] = append(groups[b], row)
	}

	merged := map[laneKey]*madnetworkTrack{}
	display := map[laneKey]bucket{}
	for b, group := range groups {
		sortMadnetworkRows(group)
		for _, t := range mergeMadnetworkTracks(group, opts) {
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
		row := laneTrack{
			madnetworkTrack: t,
			GroupArtist:     d.artist,
			Album:           d.album,
			Holders:         c.Holders,
			Branches:        c.Branches,
			FirstSeen:       c.FirstSeen,
			SelfHeld:        c.Self,
		}
		// The candidate's source label is one holder's, picked by the aggregate
		// — which names the node exactly when there is only one, and means
		// nothing at all when there are several. Only the exact case is sent:
		// the ⓘ panel is where a multi-holder row's sources actually live.
		if c.Holders == 1 {
			row.SourceName = c.SourceName
		}
		out = append(out, row)
		delete(merged, k) // a merged track fills one lane row, however it was ranked
	}
	return out
}

// branchesByKey maps every node we can see to the direct friends it reaches us
// through — the branch attribution every trust-weighted count on this page is
// computed from (one branch is one voice,
// docs/architecture/federation-trust.md §Trust graph).
//
// Empty when there is no federation node, no graph yet, or the read fails, which
// degrades every weighted surface to one source one voice: the same rule in a
// smaller world, never a wrong answer. That is also why an error is swallowed
// rather than failing the request — a browse is not worth refusing over a
// ranking input, and the fallback understates corroboration rather than
// inventing it.
func (h *handler) branchesByKey(ctx context.Context) database.BranchMap {
	if h.federation == nil {
		return nil
	}
	branches, err := h.federation.BranchMap(ctx)
	if err != nil {
		return nil
	}
	return branches
}
