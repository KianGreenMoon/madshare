package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// AlbumArtistTags is the subset of a track's tags the entity resolver needs to
// derive its artist/album entities. Year is 0 when unknown.
type AlbumArtistTags struct {
	Artist      string
	AlbumArtist string
	Album       string
	Year        int
}

// normalizeKey produces the deterministic dedup key for an artist or album
// display string: Unicode NFC → trim → collapse internal whitespace → lowercase.
// It is the single source of truth shared by the import path and the backfill so
// the two cannot drift (see docs/architecture/artist-album-model.md). No
// "the "-stripping or fuzzy folding in v0 — predictability over recall.
func normalizeKey(s string) string {
	s = norm.NFC.String(s)
	// strings.Fields splits on Unicode whitespace and drops empties, so Join
	// trims leading/trailing and collapses internal runs in one step.
	s = strings.Join(strings.Fields(s), " ")
	return strings.ToLower(s)
}

// effectiveArtist is the album-level artist used for browse-by-artist grouping:
// the first of album_artist, then artist, whose normalized key is non-empty,
// else "" (the unknown-artist bucket). Mirrors today's
// COALESCE(NULLIF(album_artist,”), NULLIF(artist,”)) but also skips
// whitespace-only tags, which normalize to the same empty bucket anyway. The
// returned display string keeps the original casing (trimmed).
func effectiveArtist(albumArtist, artist string) string {
	for _, s := range []string{albumArtist, artist} {
		if normalizeKey(s) != "" {
			return strings.TrimSpace(norm.NFC.String(s))
		}
	}
	return ""
}

// tagsFromMeta extracts the resolver inputs from a media_metadata struct. NULL
// fields read back as their zero value (empty string / 0), which the resolver
// treats as the unknown buckets.
func tagsFromMeta(m *MediaMetadata) AlbumArtistTags {
	return AlbumArtistTags{
		Artist:      m.Artist.String,
		AlbumArtist: m.AlbumArtist.String,
		Album:       m.Album.String,
		Year:        int(m.Year.Int64),
	}
}

// resolveAlbumArtist get-or-creates the artist and album entities for a track's
// tags and returns their ids. It is idempotent and concurrency-safe: each entity
// is an INSERT ... ON CONFLICT ... RETURNING on its UNIQUE key, so concurrent
// imports of the same artist/album converge on one row (same pattern as
// SetAlbumCoverIfAbsent). The whole resolution runs in one transaction so the
// album's artist_id FK is always consistent.
//
// Identity rules (docs/architecture/artist-album-model.md §"Identity rules"):
//   - artist  = effectiveArtist(album_artist, artist), "" → unknown-artist bucket.
//   - album   = (artist_id, normalizeKey(title)); empty title → unknown-album
//     bucket under that artist.
//   - year    = set on the album from the first track that supplies one; never
//     overwritten once present.
func (db *DB) resolveAlbumArtist(ctx context.Context, t AlbumArtistTags) (artistID, albumID int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve album/artist: begin: %w", err)
	}
	defer tx.Rollback()

	artistID, albumID, err = resolveAlbumArtistTx(ctx, tx, t)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("resolve album/artist: commit: %w", err)
	}
	return artistID, albumID, nil
}

// resolveAlbumArtistTx is the transaction-scoped core of resolveAlbumArtist, for
// callers already inside a transaction (InsertFile, UpdateFileMetadata). They
// must resolve within their existing tx rather than open a nested one: the pool
// is pinned to a single connection (Open), so a nested BeginTx would deadlock.
func resolveAlbumArtistTx(ctx context.Context, tx *sql.Tx, t AlbumArtistTags) (artistID, albumID int64, err error) {
	now := time.Now().Unix()

	artist := effectiveArtist(t.AlbumArtist, t.Artist)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO artists (name, norm_name, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(norm_name) DO UPDATE SET name = artists.name
		 RETURNING id`,
		artist, normalizeKey(artist), now,
	).Scan(&artistID); err != nil {
		return 0, 0, fmt.Errorf("resolve artist: %w", err)
	}

	var year any
	if t.Year > 0 {
		year = t.Year
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO albums (artist_id, title, norm_title, year, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(artist_id, norm_title) DO UPDATE SET year = COALESCE(albums.year, excluded.year)
		 RETURNING id`,
		artistID, strings.TrimSpace(norm.NFC.String(t.Album)), normalizeKey(t.Album), year, now,
	).Scan(&albumID); err != nil {
		return 0, 0, fmt.Errorf("resolve album: %w", err)
	}

	return artistID, albumID, nil
}

// BackfillEntities resolves and sets artist_id/album_id on every media_metadata
// row that still has either FK NULL, returning the number of rows updated. It is
// idempotent and re-runnable (already-resolved rows are skipped), and is run as a
// startup reconcile pass next to ReconcileOrphans. New uploads resolve entities
// inline at import time (Phase 2), so this is only for pre-existing rows.
//
// Rows are read fully into memory before resolving because the connection pool
// is pinned to a single connection (Open) — holding a Rows iterator open while
// the resolver opens its own transaction would deadlock.
func (db *DB) BackfillEntities(ctx context.Context) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT file_id, artist, album_artist, album, year
		 FROM media_metadata
		 WHERE artist_id IS NULL OR album_id IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("backfill entities: query: %w", err)
	}

	type pending struct {
		fileID int64
		tags   AlbumArtistTags
	}
	var todo []pending
	for rows.Next() {
		var (
			fileID                     int64
			artist, albumArtist, album sql.NullString
			year                       sql.NullInt64
		)
		if err := rows.Scan(&fileID, &artist, &albumArtist, &album, &year); err != nil {
			rows.Close()
			return 0, fmt.Errorf("backfill entities: scan: %w", err)
		}
		todo = append(todo, pending{
			fileID: fileID,
			tags: AlbumArtistTags{
				Artist:      artist.String,
				AlbumArtist: albumArtist.String,
				Album:       album.String,
				Year:        int(year.Int64),
			},
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("backfill entities: rows: %w", err)
	}
	rows.Close()

	var done int
	for _, p := range todo {
		artistID, albumID, err := db.resolveAlbumArtist(ctx, p.tags)
		if err != nil {
			return done, fmt.Errorf("backfill entities: resolve file %d: %w", p.fileID, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE media_metadata SET artist_id = ?, album_id = ? WHERE file_id = ?`,
			artistID, albumID, p.fileID,
		); err != nil {
			return done, fmt.Errorf("backfill entities: update file %d: %w", p.fileID, err)
		}
		done++
	}
	return done, nil
}
