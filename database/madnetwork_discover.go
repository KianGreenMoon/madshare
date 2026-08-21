package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Discovery lanes for the /madnetwork landing view (docs/ui/madnetwork-page.md
// §Lane definitions). Each lane is ONE ranking over the merged, availability-
// filtered set — never a recommendation, never a network-global chart: every
// number here is counted from what THIS node happens to have cached, which is
// what makes the lanes impossible to game from outside and impossible to grow
// into the reputation score the design refuses.
//
// A lane is computed in two passes on purpose. SQL groups the merged rows into
// logical tracks and ranks the identities; the handler then fetches the raw rows
// behind the chosen identities and merges them through the same
// mergeMadnetworkTracks the drill-down and search use, so a lane row is a full
// track with its versions, holders and local url — one row anatomy on the page,
// not two.

// Lane names. They are part of the API surface (the landing view asks for them
// by name), so they are values rather than an ordering of a slice somewhere.
const (
	LaneLocal   = "local"   // local library — what this node publishes
	LaneMissing = "missing" // missing here — held out there, not on this node
	LaneNew     = "new"     // new on the network (to us)
	LaneHeld    = "held"    // most held here
	LaneRare    = "rare"    // only one node has it
	LaneFriends = "friends" // from direct friends
)

// LaneNames is the landing view's order, top to bottom.
var LaneNames = []string{LaneLocal, LaneMissing, LaneNew, LaneHeld, LaneRare, LaneFriends}

// ValidLane reports whether name is a lane this store can rank.
func ValidLane(name string) bool {
	for _, l := range LaneNames {
		if l == name {
			return true
		}
	}
	return false
}

// LaneCandidate is one logical track as a lane ranks it — the identity plus the
// facts that put it there. It carries no display text beyond the buckets: the
// row itself is fetched afterwards, and duplicating the tags here would be a
// second place for them to disagree.
type LaneCandidate struct {
	Ident  string // trackFullIdent — the merge key, and the only SQL-side handle
	Artist string // grouping artist bucket (the drill address)
	Album  string

	// Title / Disc / Track are the rest of that identity as separate values, so
	// the caller can pair a candidate with the merged track it stands for
	// WITHOUT re-deriving the ident string in Go. It could not do so reliably:
	// SQLite's lower() is ASCII-only and Go's is not, so the two disagree on
	// exactly the titles a music library is full of. Absent disc/track numbers
	// are -1 here, matching the ident's COALESCE.
	Title string
	Disc  int64
	Track int64

	// Holders is the number of distinct nodes offering this track, Friends how
	// many of those are direct friends of ours, and HolderKeys their public keys
	// — the input to the branch weighting, which needs identities rather than a
	// count (one branch may be several nodes).
	Holders    int
	Friends    int
	HolderKeys []string

	// Self is set when this node's own library holds the track.
	Self bool

	// FirstSeen is the earliest moment any source showed it to us (0 = unknown,
	// which is what rows cached before migration 037 carry), and LastSeen the
	// newest contact with any holder.
	FirstSeen int64
	LastSeen  int64
	// SourceName is ONE holder's label, chosen by an aggregate. It names the
	// node exactly when Holders is 1 — the "only <node> has it" line — and means
	// nothing when there are several, so nothing else should read it.
	SourceName string

	// Branches is filled by the caller's branch weighting, not by SQL.
	Branches int
}

// laneRowsCTE is the row source every lane ranks over: the same merged union the
// counting queries use, plus the per-row facts a lane needs — which node the row
// came from, whether that node is a direct friend, and when we first saw it.
// Self rows contribute no source and no date: our own library is not a node that
// showed us anything.
func laneRowsCTE(view MadnetworkView) string {
	remote := `
	SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	       c.title AS title, c.track_number AS track_number, c.disc_number AS disc_number,
	       s.public_key AS source_key, ` + sourceLabelExpr + ` AS source_label,
	       ` + srcLastSeen + ` AS source_last_seen,
	       (COALESCE(s.trust_state, '') = 'friend') AS is_friend,
	       c.first_seen AS first_seen, 0 AS is_self, 0 AS self_at
	FROM federation_catalog c` + sourceJoin("c") + `
	WHERE ` + notBlocked + reachClause(view) + sourceClause(view)

	// The self half is NOT filtered by sharing scope, and that is the difference
	// between a lane and the browse. A lane asks whether WE have something —
	// which is a question about the local library, not about what we advertise —
	// so `has_self` has to mean "in this server's library". Filtering it here put
	// a recording scoped Local into "Missing here", i.e. offered a person a track
	// they already had, while the Local library lane one screen up left it out.
	self := `
	SELECT ` + selfAkeyExpr + ` AS akey, ` + selfAlbExpr + ` AS alb,
	       m.title AS title, m.track_number AS track_number, m.disc_number AS disc_number,
	       '' AS source_key, '' AS source_label, 0 AS source_last_seen,
	       0 AS is_friend, 0 AS first_seen, 1 AS is_self, m.created_at AS self_at
	FROM tagsets m` + recordingJoin + `
	LEFT JOIN artists par ON par.id = m.artist_id
	LEFT JOIN artists aar ON aar.id = m.album_artist_id
	LEFT JOIN albums al   ON al.id  = m.album_id
	WHERE ` + visibleTagset

	switch {
	case view.includeRemote() && view.includeOwn():
		return remote + " UNION ALL " + self
	case view.includeRemote():
		return remote
	case view.includeOwn():
		return self
	default:
		// The view that includes neither half — shaped like the others and
		// guaranteed empty, so every ranking above it stays one query.
		return `
	SELECT '' AS akey, '' AS alb, '' AS title, NULL AS track_number, NULL AS disc_number,
	       '' AS source_key, '' AS source_label, 0 AS source_last_seen,
	       0 AS is_friend, 0 AS first_seen, 1 AS is_self, 0 AS self_at
	WHERE 0`
	}
}

// laneRanking is a lane's SQL half: what disqualifies a group, and how the rest
// are ordered. Both read the AGGREGATE names laneAggregates publishes, never the
// row columns underneath them — the aggregation is wrapped in a subquery so the
// two can never be confused for each other.
func laneRanking(lane string) (filter, order string) {
	switch lane {
	case LaneLocal:
		// This node's own library, newest appearance first — "what this server
		// has been adding", the counterpart of LaneNew pointed inward. It is the
		// only ranking of a library's own contents that is a fact rather than a
		// taste, and it needs no weighting: one source, nothing to corroborate
		// against. ALL of it, sharing scope included: this lane is a doorway to
		// the library page, not a view of the network (§Rework).
		return "has_self = 1", "self_at DESC, ident"
	case LaneMissing:
		// Ranked by holders here and re-sorted by branch count in Go, exactly
		// like LaneHeld: "what can I get that I don't have" is a popularity
		// question too, and it is the lane the page opens with.
		return "has_self = 0 AND holders > 0", "holders DESC, ident"
	case LaneNew:
		return "first_at > 0", "first_at DESC, holders DESC, ident"
	case LaneHeld:
		// Ranked by holders here and re-sorted by branch count in Go. The
		// two-step is exact, not an approximation: branches never exceed
		// holders, so the top K by holders always contains the top K by
		// branches (docs/ui/madnetwork-page.md §Lane definitions).
		return "holders > 0", "holders DESC, ident"
	case LaneRare:
		// One holder is one branch whatever the graph says, so this lane needs
		// no weighting at all. Ordered by that holder's freshness: what
		// distinguishes one rarity from another is whether you can fetch it now.
		return "holders = 1 AND has_self = 0", "last_at DESC, first_at DESC, ident"
	case LaneFriends:
		// Weighted by construction, so it is deliberately not re-sorted in Go:
		// every direct friend is the root of its own branch, which makes the
		// friend count a branch count already. Re-weighting here would replace
		// that with the wider one and lose the lane's whole subject.
		return "friends > 0", "friends DESC, holders DESC, ident"
	}
	return "", ""
}

// laneAggregates groups the merged rows into logical tracks — one row per
// identity, carrying every fact any lane ranks by. COALESCE keeps the empty
// aggregate a zero rather than a NULL, so the scan side needs no null types for
// facts that are simply absent.
func laneAggregates(view MadnetworkView) string {
	return `
	SELECT ` + trackFullIdent + ` AS ident, MIN(akey) AS artist, MIN(alb) AS album,
	       MIN(title) AS title, MIN(COALESCE(disc_number, -1)) AS disc,
	       MIN(COALESCE(track_number, -1)) AS track,
	       MAX(is_self) AS has_self,
	       COUNT(DISTINCT NULLIF(source_key, '')) AS holders,
	       COUNT(DISTINCT CASE WHEN is_friend = 1 THEN NULLIF(source_key, '') END) AS friends,
	       COALESCE(GROUP_CONCAT(DISTINCT NULLIF(source_key, '')), '') AS holder_keys,
	       COALESCE(MIN(NULLIF(first_seen, 0)), 0) AS first_at,
	       COALESCE(MAX(source_last_seen), 0) AS last_at,
	       COALESCE(MIN(NULLIF(source_label, '')), '') AS source_label,
	       COALESCE(MAX(self_at), 0) AS self_at
	FROM (` + laneRowsCTE(view) + `)
	GROUP BY ident`
}

// MadnetworkLaneCandidates ranks the merged view for one lane and returns up to
// limit candidates. It is the SQL half of the lane; the branch weighting, the
// per-source cap and the row fetch are the caller's.
func (db *DB) MadnetworkLaneCandidates(ctx context.Context, lane string, view MadnetworkView, limit int) ([]*LaneCandidate, error) {
	filter, order := laneRanking(lane)
	if filter == "" {
		return nil, fmt.Errorf("madnetwork lane: unknown lane %q", lane)
	}
	if limit <= 0 {
		return []*LaneCandidate{}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT ident, artist, album, title, disc, track, has_self, holders, friends,
		       holder_keys, first_at, last_at, source_label
		FROM (`+laneAggregates(view)+`)
		WHERE `+filter+`
		ORDER BY `+order+`
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("madnetwork lane %s: %w", lane, err)
	}
	defer rows.Close()

	out := []*LaneCandidate{}
	for rows.Next() {
		var c LaneCandidate
		var self int
		var keys string
		if err := rows.Scan(&c.Ident, &c.Artist, &c.Album, &c.Title, &c.Disc, &c.Track,
			&self, &c.Holders, &c.Friends, &keys, &c.FirstSeen, &c.LastSeen,
			&c.SourceName); err != nil {
			return nil, fmt.Errorf("scan madnetwork lane row: %w", err)
		}
		c.Self = self == 1
		if keys != "" {
			c.HolderKeys = strings.Split(keys, ",") // hex keys; the separator cannot occur in one
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// MadnetworkRowsForIdents fetches the raw merged rows behind a set of lane
// identities — both halves of the union, in one round trip each — so the caller
// can run them through the same merge the drill-down uses. Order is irrelevant
// here: the caller restores the lane's ranking by ident.
func (db *DB) MadnetworkRowsForIdents(ctx context.Context, idents []string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	if len(idents) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(idents))
	for _, id := range idents {
		args = append(args, id)
	}
	ph := "(" + strings.TrimSuffix(strings.Repeat("?,", len(idents)), ",") + ")"

	rows, err := db.remoteTrackRows(ctx, view, trackFullIdent+" IN "+ph, args...)
	if err != nil {
		return nil, err
	}
	own, err := db.ownTrackRows(ctx, view, selfTrackFullIdent+" IN "+ph, args...)
	if err != nil {
		return nil, err
	}
	return append(rows, own...), nil
}

// selfTrackFullIdent is trackFullIdent expressed over the own-rows join, whose
// columns are named for the tagsets table rather than the bucket aliases. The
// two MUST fold to the same string for the same track — that identity is what
// makes "we publish this too" a self holder on a remote row instead of a
// duplicate line under it.
const selfTrackFullIdent = `unicode_lower(` + selfAkeyExpr + `) || char(31) || unicode_lower(` + selfAlbExpr + `) ||
	char(31) || COALESCE(m.disc_number, -1) || char(31) || COALESCE(m.track_number, -1) ||
	char(31) || unicode_lower(m.title)`

// CapPerSource fills a lane round-robin across the nodes that supplied it,
// letting no single node contribute more than its share until the rest have had
// their turn (docs/ui/madnetwork-page.md §"The per-source cap"). Reaching a node
// for the first time makes its whole library new to us at once; without this a
// single node owns the "new" lane for as long as nothing newer happens.
//
// The quota adapts to the candidate set (limit/sources, at least one) and the
// lane still fills from the passed-over candidates in rank order, so a one-node
// network sees no difference at all. Only the landing view's digest is capped —
// "See all" is the same ranking with this step skipped, which is how "lanes
// rank, they never hide" stays literally true.
func CapPerSource(candidates []*LaneCandidate, limit int) []*LaneCandidate {
	if limit <= 0 || len(candidates) <= limit {
		return candidates
	}
	sources := map[string]bool{}
	for _, c := range candidates {
		for _, k := range c.HolderKeys {
			sources[k] = true
		}
	}
	if len(sources) < 2 {
		return candidates[:limit] // nothing to spread across
	}
	quota := (limit + len(sources) - 1) / len(sources)
	if quota < 1 {
		quota = 1
	}

	used := map[string]int{}
	out := make([]*LaneCandidate, 0, limit)
	passed := make([]*LaneCandidate, 0, len(candidates))
	for _, c := range candidates {
		if len(out) == limit {
			break
		}
		room := len(c.HolderKeys) == 0 // a self-only row is charged to nobody
		for _, k := range c.HolderKeys {
			if used[k] < quota {
				room = true
				break
			}
		}
		if !room {
			passed = append(passed, c)
			continue
		}
		for _, k := range c.HolderKeys {
			used[k]++
		}
		out = append(out, c)
	}
	// The quota is a preference, not a ceiling on the lane: if spreading could
	// not fill it, the best of what was passed over comes back, in rank order.
	for _, c := range passed {
		if len(out) == limit {
			break
		}
		out = append(out, c)
	}
	return out
}

// BranchMap maps a node key to the direct friends it reaches us through
// (federation.MapNode.Via) — the branch attribution every trust-weighted count
// on the madnetwork page is computed from.
//
// A key absent from the map is its own voice. That is the honest reading for a
// node we cache but cannot currently place on the graph, and it is also the
// degradation when there is no graph at all (no federation node, no gossip yet):
// one source, one voice — the same rule in a smaller world, never a wrong
// answer.
type BranchMap map[string][]string

// Voices counts the distinct voices behind a set of holder keys — the ONE place
// the sybil rule is written, so that no surface can apply half of it. One branch
// is one voice (docs/architecture/federation-trust.md §Trust graph): a farm of a
// thousand keys behind a single friendship counts once, which is the whole
// reason popularity is allowed to order anything here.
//
// Empty keys are ignored; a caller with an unkeyed holder passes a token that
// distinguishes it, because "we don't know who this is" must never collapse two
// holders into one voice. self adds this node's own library, which is a voice
// like any other.
func (bm BranchMap) Voices(keys []string, self bool) int {
	seen := map[string]bool{}
	for _, key := range keys {
		if key == "" {
			continue
		}
		via := bm[key]
		if len(via) == 0 {
			seen[key] = true // unplaceable: it speaks for itself, once
			continue
		}
		for _, b := range via {
			seen[b] = true
		}
	}
	if self {
		seen["self"] = true
	}
	return len(seen)
}

// WeightByBranch fills each candidate's Branches — how many distinct voices its
// holders amount to — and re-sorts by it, so what a lane leads with is what the
// most independent sources agree on rather than what the most keys claim.
//
// Applied to the two lanes SQL ranks by a raw count (LaneHeld, LaneMissing; see
// laneWeighted in the api package). The two-step is exact rather than an
// approximation: branches never exceed holders, so the top K by holders always
// contains the top K by branches.
func WeightByBranch(candidates []*LaneCandidate, branches BranchMap) {
	for _, c := range candidates {
		c.Branches = branches.Voices(c.HolderKeys, c.Self)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Branches != candidates[j].Branches {
			return candidates[i].Branches > candidates[j].Branches
		}
		return candidates[i].Holders > candidates[j].Holders
	})
}
