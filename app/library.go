package app

import (
	"context"
	"strings"

	"daemonlord.ygg/madshare/database"
)

// Library is the browse and playback surface an embedder calls instead of the
// HTTP API. It is deliberately small: madshare's `database` package has hundreds
// of methods that change freely, and an embedder reaching into them directly is
// what turns every internal refactor into a client break. This method set is the
// part promised to stay — anything an embedder needs is added here, named, rather
// than borrowed from `database`.
//
// The row types ARE the HTTP contract (the handlers marshal them straight out),
// so they are returned as they are: a parallel set of structs would duplicate the
// one thing a second client must not duplicate.
//
// Two rules carry over from docs/ui/madplayer.md §"What the server already
// computes", and both matter more here than over HTTP, because there is no
// endpoint left to hide behind:
//
//   - **Do not re-sort and do not re-group.** The artist list is album-artists
//     only, the Unknown-artist/Other buckets sort last, and disc grouping follows
//     docs/architecture/disc-numbering.md. Every one of those is decided in SQL.
//   - **Do not re-derive which file to play.** A track row's ObjectKey is already
//     the recording's ladder-best surviving rendition.
//
// These calls run the FULL queries, not the guest ones — see
// docs/architecture/embedding.md §"Direct calls bypass the permission layer".
type Library interface {
	// Artists returns every album-artist in the library, in display order.
	Artists(ctx context.Context) ([]*database.ArtistEntry, error)
	// ArtistsPage is Artists keyset-paged for a windowed list. cursor is opaque:
	// pass back what the previous call returned, and never construct one. The
	// returned cursor is empty on the last page.
	ArtistsPage(ctx context.Context, cursor string, limit int) ([]*database.ArtistEntry, string, error)
	// AlbumsByArtist returns one artist's albums — in EITHER credit role, so an
	// album the artist only guests on is included.
	AlbumsByArtist(ctx context.Context, artistID int64) ([]*database.AlbumEntry, error)
	// TracksByAlbum returns one album's tracks, one row per appearance.
	TracksByAlbum(ctx context.Context, albumID int64) ([]*database.TrackEntry, error)
	// Search matches artists, albums and tracks (performers included, which is
	// why a name can be a search hit without being an artist row).
	Search(ctx context.Context, q string) (*database.SearchResults, error)
	// Renditions returns the surviving renditions of an appearance's recording in
	// quality-ladder order, best first — the quality picker's list, where index 0
	// is what "Auto" resolves to.
	Renditions(ctx context.Context, tagsetID int64) ([]database.DuplicateRendition, error)
	// BlobPath resolves a row's ObjectKey ("<hash>/<filename>") to a path on this
	// machine, probing the storages registry in the same precedence the HTTP blob
	// server does — so a linked import resolves through the links tree to the
	// external original, and a dangling link falls through as not-ok.
	//
	// This is the one call with no HTTP equivalent: over the wire the answer is a
	// URL, in-process it is a path a decoder can open. A false return on a track
	// the library lists is the normal "that drive isn't connected" case, not a
	// missing track.
	BlobPath(objectKey string) (string, bool)
}

// Library returns the embedder's browse surface.
func (i *Instance) Library() Library { return library{i} }

// library implements Library over the instance's store and storages registry.
type library struct{ inst *Instance }

func (l library) Artists(ctx context.Context) ([]*database.ArtistEntry, error) {
	return l.inst.db.ListArtists(ctx)
}

func (l library) ArtistsPage(ctx context.Context, cursor string, limit int) ([]*database.ArtistEntry, string, error) {
	return l.inst.db.ListArtistsPage(ctx, cursor, limit, false)
}

func (l library) AlbumsByArtist(ctx context.Context, artistID int64) ([]*database.AlbumEntry, error) {
	return l.inst.db.ListAlbumsByArtistID(ctx, artistID)
}

func (l library) TracksByAlbum(ctx context.Context, albumID int64) ([]*database.TrackEntry, error) {
	return l.inst.db.ListTracksByAlbumID(ctx, albumID)
}

func (l library) Search(ctx context.Context, q string) (*database.SearchResults, error) {
	return l.inst.db.Search(ctx, q)
}

func (l library) Renditions(ctx context.Context, tagsetID int64) ([]database.DuplicateRendition, error) {
	rends, err := l.inst.db.RecordingRenditionsByTagsetID(ctx, tagsetID)
	if err != nil {
		return nil, err
	}
	return database.RankDuplicateRenditions(rends), nil
}

func (l library) BlobPath(objectKey string) (string, bool) {
	hash, _, _ := strings.Cut(objectKey, "/")
	path, _, ok := l.inst.registry.Resolve(hash)
	return path, ok
}
