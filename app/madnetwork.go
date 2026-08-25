package app

import (
	"context"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/database"
)

// This file is the embedder's two madnetwork surfaces beyond the mesh itself
// (docs/architecture/embedding.md §"The madnetwork browse and the publish
// picker"; the design they serve is docs/plans/full-node-mode.md):
//
//   - Madnetwork() — browsing the community through this node's OWN cached
//     catalogs, which is what pairing buys a full-node madplayer. Before it, a
//     paired player held the catalogs and had no way to show them: its browse
//     merged only signed-in servers over HTTP.
//   - the sharing arm (W2) — SetShareDepth / ShareDepths / Published, the
//     organize-then-share surface. On Instance rather than Network() on
//     purpose (the doc's call): scopes are DB columns an owner curates before
//     any mesh is up, and they need no running node.

// Madnetwork is the merged community browse: every cached catalog this node
// pulled as a member, folded with its own published set, under exactly the
// rules the web UI's /madnetwork uses — availability windows and fail-open,
// version folding on shared rendition hashes, branch-weighted version order,
// cover election. It is api.MadnetworkBrowse behind the same interface-shaped
// facade Library() is: the method set is the promise, the row types are the
// HTTP contract served as they are.
//
// Addressing is by NAME, not id — the merged catalog has no entity ids (the
// web UI's rows work the same way). An album's address is its grouping artist
// plus its title.
//
// Like Library(), these calls bypass the permission layer (madnetwork.access
// lives in the handler stack): correct for a single-user player, a decision
// anywhere else.
type Madnetwork interface {
	// Artists is one keyset page of the merged artist list (album-artist
	// grouping). cursor is opaque: pass back what the previous call returned;
	// empty result cursor = last page. limit ≤ 0 means the server's page cap.
	Artists(ctx context.Context, q string, limit int, cursor string) ([]*database.MadnetworkArtist, string, error)
	// AlbumsByArtist is one artist's merged albums, covers elected.
	AlbumsByArtist(ctx context.Context, artist string) ([]*database.MadnetworkAlbum, error)
	// TracksByAlbum is one album's merged tracks with their versions. A
	// version's holders carry node KEYS — the list a fetch hands to
	// Network.Fetch — and versions[0].Renditions[0] is the default pick, an
	// ordering the client must not re-derive.
	TracksByAlbum(ctx context.Context, artist, album string) ([]*api.MadnetworkTrack, error)
	// Search is the three-section merged search (artists, albums, playable
	// tracks), matching performers as well as album artists.
	Search(ctx context.Context, q string) (*api.MadnetworkSearchResults, error)
}

// Madnetwork returns the community browse, or false when this configuration
// runs no federation node — mirroring the web UI, which registers the
// /madnetwork pages only where the node runs. (The cached catalogs outlive the
// node in the database, but a browse over a community this node is not
// currently in would show holders nothing can fetch from.)
func (i *Instance) Madnetwork() (Madnetwork, bool) {
	if i.node == nil {
		return nil, false
	}
	return madnetworkFacade{&api.MadnetworkBrowse{
		Store:          i.deps.Madnetwork,
		Node:           i.deps.Federation,
		SelfName:       i.deps.MadnetworkName,
		ReachWindowSec: i.deps.ReachableWindowSec,
	}}, true
}

// madnetworkFacade fixes every call to the merged default view. The single-node
// shelf (?source=) stays a page concept; an embedder that wants one browses by
// what the rows already carry.
type madnetworkFacade struct{ b *api.MadnetworkBrowse }

func (m madnetworkFacade) Artists(ctx context.Context, q string, limit int, cursor string) ([]*database.MadnetworkArtist, string, error) {
	return m.b.Artists(ctx, q, m.b.View(ctx), limit, cursor)
}

func (m madnetworkFacade) AlbumsByArtist(ctx context.Context, artist string) ([]*database.MadnetworkAlbum, error) {
	return m.b.Albums(ctx, artist, m.b.View(ctx))
}

func (m madnetworkFacade) TracksByAlbum(ctx context.Context, artist, album string) ([]*api.MadnetworkTrack, error) {
	return m.b.Tracks(ctx, artist, album, m.b.View(ctx))
}

func (m madnetworkFacade) Search(ctx context.Context, q string) (*api.MadnetworkSearchResults, error) {
	return m.b.Search(ctx, q, m.b.View(ctx))
}

// ── The sharing arm (full-node-mode.md W2) ───────────────────────────────────

// SetShareDepth pins the sharing scope of the recordings behind the given
// appearances — the facade twin of the Recordings lens's scope chip, tagset-
// addressed because an appearance is what browse rows carry. The three legal
// scopes are federation.DepthPrivate / DepthFriends / DepthUnlimited, and
// database.ShareDepthUpdate carries the three-valued update (pin, or Inherit
// to fall back to the node default — which an embedded player pins to Local,
// so Inherit IS "stop sharing" there).
//
// A pin is the ONLY gate between a player's disk and the catalog
// (full-node-mode.md: player scans land approved with no review queue), so
// nothing here is bulk-implicit: the caller names appearances it just listed.
// Friends see a change on their next catalog pull — the scope is authoritative
// immediately (bytes and catalog read it live), the listing propagates on the
// sync cadence.
func (i *Instance) SetShareDepth(ctx context.Context, tagsetIDs []int64, depth database.ShareDepthUpdate) error {
	_, err := i.db.SetShareDepthByTagsets(ctx, tagsetIDs, depth)
	return err
}

// ShareDepths reports the pinned scope per appearance; an id absent from the
// map inherits the node default. This is what a publish picker renders beside
// the rows it shows.
func (i *Instance) ShareDepths(ctx context.Context, tagsetIDs []int64) (map[int64]int, error) {
	return i.db.TagsetShareDepths(ctx, tagsetIDs)
}

// Published lists exactly what the network can currently see — the visible
// half of the responsibility the closed default protects (full-node-mode.md
// P2's "Published" view). Rows carry their effective depth and whether it is a
// pin of their own; on a player every row is pinned, because the default
// publishes nothing.
func (i *Instance) Published(ctx context.Context) ([]*database.SharedTrack, error) {
	return i.db.SharedTracks(ctx)
}
