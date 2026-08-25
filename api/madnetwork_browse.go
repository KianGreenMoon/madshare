package api

import (
	"context"
	"strings"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// MadnetworkBrowse is the merged madnetwork browse — the machinery behind
// GET /api/madnetwork/{artists,albums,tracks,search} — as a callable surface.
//
// It exists because the browse gained a second consumer: app.Instance.Madnetwork()
// hands it to an embedder (a paired madplayer browsing the community through its
// OWN node, docs/architecture/embedding.md §"The madnetwork browse"), and the
// HTTP handlers call the same value. One code path, so an in-process client
// cannot drift from the web UI about what the merged view contains — which
// rules are at stake is exactly the §"What the server already computes" list:
// availability windows and fail-open, the self-merge, version folding on shared
// rendition hashes, branch-weighted version order, cover election.
//
// The zero value is not useful; build it with every field, as newHandler and
// the facade do. Store is required; Node may be nil (federation off — no
// branch weighting, no fail-open source, nobody placeable).
type MadnetworkBrowse struct {
	Store MadnetworkStore
	// Node is the running federation node, nil when federation is off or
	// compiled out. It supplies the branch attribution and the inbound-health
	// fail-open signal; nil degrades exactly as the handlers always have (one
	// source one voice, never fail open).
	Node FederationNode
	// SelfName is the node's display name, and doubles as the self-merge
	// switch: empty means no federation node runs, so the merged view folds no
	// own rows in (the handlers' includeSelf rule).
	SelfName string
	// ReachWindowSec is the availability freshness window in seconds; 0 falls
	// back to the default.
	ReachWindowSec int
}

// includeSelf reports whether the merged view folds this node's own published
// set in — on exactly when the federation node runs (SelfName is its display
// name). With federation disabled the view stays what the friends provide:
// nothing.
func (b *MadnetworkBrowse) includeSelf() bool { return b.SelfName != "" }

// reachWindow is the availability freshness window in seconds: a friend is
// "reachable" (its exclusively-held tracks are shown) when last_seen is within
// it. Several × the node's 1-minute refresh cadence, so a single missed ping
// never flips reachability — the margin is the anti-flap guarantee.
func (b *MadnetworkBrowse) reachWindow() int64 {
	if b.ReachWindowSec > 0 {
		return int64(b.ReachWindowSec)
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
func (b *MadnetworkBrowse) pullWindow() int64 {
	pull := int64(federation.PullFreshnessWindow / time.Second)
	if w := b.reachWindow(); w > pull {
		return w
	}
	return pull
}

// View is the merged default view — every reachable source plus, when the node
// runs, the own published set — with the availability cutoffs the admin's
// hide-unavailable setting and the node's own inbound health decide.
func (b *MadnetworkBrowse) View(ctx context.Context) database.MadnetworkView {
	now := time.Now().Unix()
	v := database.MadnetworkView{
		IncludeSelf: b.includeSelf(), DefaultShareDepth: federation.DepthFriends,
		// Set before either early return: which window a node BELONGS to is a
		// fact about who watches it, and the rows still carry it when this
		// request is not filtering — that is what lets the ⓘ panel grey a holder
		// correctly while the browse shows everything.
		PingedSince: now - b.reachWindow(),
	}
	p, err := b.Store.GetMadnetworkPolicy(ctx)
	if err == nil {
		v.DefaultShareDepth = p.DefaultShareDepth
	}
	if !b.inboundHealthy() {
		return v // fail open
	}
	if err == nil && !p.HideUnavailable {
		return v // hiding disabled by the admin
	}
	v.Cutoff, v.PullCutoff = now-b.reachWindow(), now-b.pullWindow()
	return v
}

// inboundHealthy reports the node's self-health (true when federation is off or
// absent — there is nothing to fail open for).
func (b *MadnetworkBrowse) inboundHealthy() bool {
	return b.Node == nil || b.Node.InboundHealthy()
}

// reachCutoff is the freshness cutoffs used to *display* holder reachability (the
// ⓘ panel greys a holder past them). Always now−window, independent of the browse
// filter cutoffs — so when the view fails open (showing everything), stale holders
// still read as stale rather than all looking reachable.
func (b *MadnetworkBrowse) reachCutoff() reachWindows {
	now := time.Now().Unix()
	return reachWindows{tight: now - b.reachWindow(), pull: now - b.pullWindow()}
}

// branchesByKey is the branch attribution for the voices rule, nil when there
// is no graph to attribute by (federation off, or the walk failed — in which
// case one source is one voice, which errs toward showing more).
func (b *MadnetworkBrowse) branchesByKey(ctx context.Context) database.BranchMap {
	if b.Node == nil {
		return nil
	}
	branches, err := b.Node.BranchMap(ctx)
	if err != nil {
		return nil
	}
	return branches
}

func (b *MadnetworkBrowse) mergeOpts(ctx context.Context) mergeOpts {
	return mergeOpts{
		selfName: b.SelfName,
		reach:    b.reachCutoff(),
		branches: b.branchesByKey(ctx),
	}
}

// ── The browse itself ────────────────────────────────────────────────────────

// Artists is one keyset page of the merged artist list (album-artist grouping,
// like the local library). cursor is opaque: pass back what the previous call
// returned, never construct one; the returned cursor is empty on the last page.
// limit is clamped to the page-size cap; ≤0 means the cap.
func (b *MadnetworkBrowse) Artists(ctx context.Context, q string, view database.MadnetworkView, limit int, cursor string) ([]*database.MadnetworkArtist, string, error) {
	if limit <= 0 || limit > madnetworkArtistPageSize {
		limit = madnetworkArtistPageSize
	}
	return b.Store.MadnetworkArtists(ctx, q, view, limit, cursor)
}

// Albums is one artist's merged albums, each carrying its elected cover
// (covers-federation M4). A cover-election failure costs the art, never the
// list — same soft rule as branchesByKey.
func (b *MadnetworkBrowse) Albums(ctx context.Context, artist string, view database.MadnetworkView) ([]*database.MadnetworkAlbum, error) {
	albums, err := b.Store.MadnetworkAlbums(ctx, artist, view)
	if err != nil {
		return nil, err
	}
	if claims, err := b.Store.MadnetworkAlbumCoverClaims(ctx, artist, view); err == nil && len(claims) > 0 {
		ballots := map[string]*coverBallot{}
		for _, c := range claims {
			bl := ballots[c.AlbumKey]
			if bl == nil {
				bl = &coverBallot{}
				ballots[c.AlbumKey] = bl
			}
			bl.add(c.CoverHash, c.CoverExt, c.SourceKey, c.Self)
		}
		branches := b.branchesByKey(ctx)
		for _, a := range albums {
			if bl := ballots[a.Key]; bl != nil {
				a.CoverHash, a.CoverExt = bl.winner(branches)
			}
		}
	}
	return albums, nil
}

// Tracks is one album's merged track rows with their versions — friends'
// cached rows plus, when the federation node runs, this node's own published
// rows, folded by the version rules (shared rendition hash = same version,
// branch-weighted order).
func (b *MadnetworkBrowse) Tracks(ctx context.Context, artist, album string, view database.MadnetworkView) ([]*MadnetworkTrack, error) {
	rows, err := b.Store.MadnetworkTracks(ctx, artist, album, view)
	if err != nil {
		return nil, err
	}
	own, err := b.Store.MadnetworkOwnTracks(ctx, artist, album, view)
	if err != nil {
		return nil, err
	}
	if len(own) > 0 {
		rows = append(rows, own...)
		sortMadnetworkRows(rows)
	}
	return mergeMadnetworkTracks(rows, b.mergeOpts(ctx)), nil
}

// MadnetworkSearchTrack is one playable track hit of the merged search. Field
// names mirror /api/search so the shared search view renders both; addressing
// is by display name (the merged catalog has no entity ids), and each track
// carries its default rendition hash plus a direct local url when self-held.
type MadnetworkSearchTrack struct {
	Title      string   `json:"title"`
	ArtistName string   `json:"artist_name,omitempty"`
	Artist     string   `json:"artist"` // grouping artist (drill address)
	AlbumTitle string   `json:"album_title"`
	Duration   *float64 `json:"duration_seconds,omitempty"`
	TagsetID   int64    `json:"tagset_id,omitempty"`
	Hash       string   `json:"hash"`
	URL        string   `json:"url,omitempty"` // local play address when self-held
}

// MadnetworkSearchResults is the three-section merged search
// (docs/ui/madnetwork-page.md §Search).
type MadnetworkSearchResults struct {
	Artists []*database.MadnetworkArtist     `json:"artists"`
	Albums  []*database.MadnetworkSearchAlbum `json:"albums"`
	Tracks  []MadnetworkSearchTrack          `json:"tracks"`
}

// Search is the library-style three-section search over the merged (friends ∪
// own) catalog. It matches an artist in EITHER credit, so a performer the A-Z
// list leaves out (they have no release of their own) is still findable by
// name — the split the local library's search and browse make too.
func (b *MadnetworkBrowse) Search(ctx context.Context, q string, view database.MadnetworkView) (*MadnetworkSearchResults, error) {
	artists, err := b.Store.MadnetworkSearchArtists(ctx, q, madnetworkSearchArtistCap, view)
	if err != nil {
		return nil, err
	}
	if artists == nil {
		artists = []*database.MadnetworkArtist{}
	}

	albums, err := b.Store.MadnetworkSearchAlbums(ctx, q, madnetworkSearchAlbumCap, view)
	if err != nil {
		return nil, err
	}
	b.electSearchAlbumCovers(ctx, albums, view)

	rows, err := b.Store.MadnetworkSearchTrackRows(ctx, q, view)
	if err != nil {
		return nil, err
	}

	// Group cross-album rows by their display-identity bucket and run the
	// album-scale merge per group, so version folding stays correct.
	type bucket struct{ artist, album string }
	groups := map[bucket][]*database.MadnetworkTrackRow{}
	order := []bucket{}
	for _, row := range rows {
		bk := bucket{strings.ToLower(row.GroupArtist), strings.ToLower(row.GroupAlbum)}
		if _, seen := groups[bk]; !seen {
			order = append(order, bk)
		}
		groups[bk] = append(groups[bk], row)
	}
	tracks := []MadnetworkSearchTrack{}
	opts := b.mergeOpts(ctx)
merge:
	for _, bk := range order {
		group := groups[bk]
		for _, t := range mergeMadnetworkTracks(group, opts) {
			if len(t.Versions) == 0 || len(t.Versions[0].Renditions) == 0 {
				continue // nothing playable to offer from search
			}
			var dur *float64
			if t.Duration > 0 {
				d := t.Duration
				dur = &d
			}
			tracks = append(tracks, MadnetworkSearchTrack{
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

	return &MadnetworkSearchResults{Artists: artists, Albums: albums, Tracks: tracks}, nil
}

// electSearchAlbumCovers fills the search page's album hits with their elected
// covers, one claims query per distinct artist among the hits (the cap is 5, so
// the fan-out is bounded and usually one). A failure costs the art, never the
// hit.
func (b *MadnetworkBrowse) electSearchAlbumCovers(ctx context.Context, hits []*database.MadnetworkSearchAlbum, view database.MadnetworkView) {
	if len(hits) == 0 {
		return
	}
	branches := b.branchesByKey(ctx)
	perArtist := map[string]map[string]*coverBallot{} // artist -> album key -> ballot
	for _, hit := range hits {
		ballots, ok := perArtist[hit.Artist]
		if !ok {
			claims, err := b.Store.MadnetworkAlbumCoverClaims(ctx, hit.Artist, view)
			if err != nil {
				continue
			}
			ballots = map[string]*coverBallot{}
			for _, c := range claims {
				bl := ballots[c.AlbumKey]
				if bl == nil {
					bl = &coverBallot{}
					ballots[c.AlbumKey] = bl
				}
				bl.add(c.CoverHash, c.CoverExt, c.SourceKey, c.Self)
			}
			perArtist[hit.Artist] = ballots
		}
		if bl := ballots[hit.Key]; bl != nil {
			hit.CoverHash, hit.CoverExt = bl.winner(branches)
		}
	}
}
