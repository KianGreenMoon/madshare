package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Default display names for the "unknown" buckets. Untagged tracks resolve to
// these canonical entities instead of empty strings: artists.name / albums.title
// and media_metadata.title are required non-empty (migration 016). normalizeKey
// of each is the bucket's dedup key ("unknown artist" / "other"), so a track
// tagged literally this way converges on the same bucket.
const (
	DefaultArtistName = "Unknown artist"
	DefaultAlbumTitle = "Other"
)

// Errors returned by the rename operations.
var (
	// ErrEntityNotFound is returned when no artist/album entity has the given id.
	ErrEntityNotFound = errors.New("entity not found")
	// ErrNameConflict is returned when a rename would collide with an existing
	// entity (the new normalized name/title is already taken). That is a merge,
	// not a rename.
	ErrNameConflict = errors.New("name already in use")
	// ErrMergeSelf is returned when a merge names the same entity as both source
	// and target.
	ErrMergeSelf = errors.New("cannot merge an entity into itself")
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
// else DefaultArtistName (the unknown-artist bucket). Mirrors today's
// COALESCE(NULLIF(album_artist,”), NULLIF(artist,”)) but also skips
// whitespace-only tags, which normalize to the same empty bucket anyway. The
// returned display string keeps the original casing (trimmed).
func effectiveArtist(albumArtist, artist string) string {
	for _, s := range []string{albumArtist, artist} {
		if normalizeKey(s) != "" {
			return strings.TrimSpace(norm.NFC.String(s))
		}
	}
	return DefaultArtistName
}

// effectiveAlbum is the album display title used for the album entity: the
// track's album tag trimmed, or DefaultAlbumTitle ("Other") when the tag is
// empty/whitespace. Mirrors effectiveArtist so untagged tracks land in one named
// bucket per artist rather than an empty-titled row.
func effectiveAlbum(album string) string {
	if normalizeKey(album) == "" {
		return DefaultAlbumTitle
	}
	return strings.TrimSpace(norm.NFC.String(album))
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
//   - artist  = effectiveArtist(album_artist, artist); empty tags →
//     DefaultArtistName ("Unknown artist") bucket.
//   - album   = (artist_id, normalizeKey(effectiveAlbum(title))); empty title →
//     DefaultAlbumTitle ("Other") bucket under that artist.
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

// resolveArtistTx get-or-creates the artist entity for a display name within an
// open transaction, returning its id. The display string is stored as-is on
// first insert (first spelling wins); norm_name is the dedup key.
func resolveArtistTx(ctx context.Context, tx *sql.Tx, displayName string, now int64) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO artists (name, norm_name, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(norm_name) DO UPDATE SET name = artists.name
		 RETURNING id`,
		displayName, normalizeKey(displayName), now,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("resolve artist: %w", err)
	}
	return id, nil
}

// ResolveArtistID get-or-creates the artist entity for a display name and
// returns its id. Used by cover-write paths, which may target an artist whose
// only attachment is the cover. Idempotent.
func (db *DB) ResolveArtistID(ctx context.Context, name string) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("resolve artist id: begin: %w", err)
	}
	defer tx.Rollback()
	id, err := resolveArtistTx(ctx, tx, strings.TrimSpace(norm.NFC.String(name)), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("resolve artist id: commit: %w", err)
	}
	return id, nil
}

// ResolveAlbumID get-or-creates the (artist, album) entity and returns the album
// id. The artist is treated as the album artist. Used by cover-write paths.
func (db *DB) ResolveAlbumID(ctx context.Context, artist, album string) (int64, error) {
	_, albumID, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{AlbumArtist: artist, Album: album})
	return albumID, err
}

// LookupArtistID returns the artists.id for a display name (matched by
// normalized key), or found=false when no such entity exists. It never creates a
// row — read paths must not materialize entities for missing names.
func (db *DB) LookupArtistID(ctx context.Context, name string) (int64, bool, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM artists WHERE norm_name = ?`, normalizeKey(name),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup artist id: %w", err)
	}
	return id, true, nil
}

// LookupAlbumID returns the albums.id for (artist, album) matched by their
// normalized keys, or found=false. Lookup-only (never creates).
func (db *DB) LookupAlbumID(ctx context.Context, artist, album string) (int64, bool, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT al.id FROM albums al
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE ar.norm_name = ? AND al.norm_title = ?`,
		normalizeKey(artist), normalizeKey(album),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup album id: %w", err)
	}
	return id, true, nil
}

// RenameArtist changes the display name (and dedup key) of an artist entity in
// place. Its tracks and cover follow via their FKs — no string rewrite. Returns
// ErrEntityNotFound when no artist has the id, or ErrNameConflict when the new
// normalized name already belongs to a *different* artist (that is a merge, not
// a rename). Renaming to a different casing/spacing of the same name is allowed
// (the norm key is unchanged, only the display name updates).
func (db *DB) RenameArtist(ctx context.Context, artistID int64, newName string) error {
	display := strings.TrimSpace(norm.NFC.String(newName))
	newNorm := normalizeKey(newName)
	if newNorm == "" {
		// A name can't be empty (artists.name is required non-empty). Renaming to
		// the bucket's own name ("Unknown artist") instead collides with it below
		// as a normal merge.
		return ErrNameConflict
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename artist: begin: %w", err)
	}
	defer tx.Rollback()

	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM artists WHERE norm_name = ?`, newNorm).Scan(&existing)
	if err == nil && existing != artistID {
		return ErrNameConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rename artist: check conflict: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE artists SET name = ?, norm_name = ? WHERE id = ?`, display, newNorm, artistID)
	if err != nil {
		return fmt.Errorf("rename artist: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEntityNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rename artist: commit: %w", err)
	}
	return nil
}

// RenameAlbum changes the title (and dedup key) of an album entity in place. Its
// tracks and cover follow via their FKs. Returns ErrEntityNotFound when no album
// has the id, or ErrNameConflict when the new normalized title already belongs
// to a *different* album under the same artist (that is a merge).
func (db *DB) RenameAlbum(ctx context.Context, albumID int64, newTitle string) error {
	display := strings.TrimSpace(norm.NFC.String(newTitle))
	newNorm := normalizeKey(newTitle)
	if newNorm == "" {
		// A title can't be empty (albums.title is required non-empty). Renaming to
		// the bucket's own title ("Other") instead collides with it below as a
		// normal merge.
		return ErrNameConflict
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename album: begin: %w", err)
	}
	defer tx.Rollback()

	var artistID int64
	err = tx.QueryRowContext(ctx, `SELECT artist_id FROM albums WHERE id = ?`, albumID).Scan(&artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEntityNotFound
	}
	if err != nil {
		return fmt.Errorf("rename album: load: %w", err)
	}

	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM albums WHERE artist_id = ? AND norm_title = ?`, artistID, newNorm).Scan(&existing)
	if err == nil && existing != albumID {
		return ErrNameConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rename album: check conflict: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE albums SET title = ?, norm_title = ? WHERE id = ?`, display, newNorm, albumID); err != nil {
		return fmt.Errorf("rename album: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rename album: commit: %w", err)
	}
	return nil
}

// MergeArtists merges artist `fromID` into `intoID` ("from is the same as
// into"): all of from's tracks and albums move onto into, albums that collide on
// norm_title collapse into into's album (tracks repointed, cover moved only if
// into's album lacks one), into gains from's artist cover only if it has none,
// then from is deleted. The raw tags on the files are left untouched (overlay).
// Returns ErrMergeSelf when the ids are equal, ErrEntityNotFound when either id
// is unknown. Runs in one transaction so a failure leaves both entities intact.
//
// media_metadata.{artist,album}_id are RESTRICT (no cascade), so every track is
// repointed off from before from (and any collapsed album) is deleted; deleting
// an artist/album cascades only its cover rows.
func (db *DB) MergeArtists(ctx context.Context, fromID, intoID int64) error {
	if fromID == intoID {
		return ErrMergeSelf
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("merge artists: begin: %w", err)
	}
	defer tx.Rollback()

	for _, id := range []int64{fromID, intoID} {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM artists WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrEntityNotFound
		} else if err != nil {
			return fmt.Errorf("merge artists: verify: %w", err)
		}
	}

	// 1. Collapse from-albums that collide with an into-album on norm_title.
	//    Read the collisions fully before mutating (single-conn pool: a tx can't
	//    run a statement while its own Rows is open).
	type collision struct{ fromAlbum, intoAlbum int64 }
	rows, err := tx.QueryContext(ctx,
		`SELECT bf.id, ba.id
		 FROM albums bf
		 JOIN albums ba ON ba.artist_id = ? AND ba.norm_title = bf.norm_title
		 WHERE bf.artist_id = ?`, intoID, fromID)
	if err != nil {
		return fmt.Errorf("merge artists: find collisions: %w", err)
	}
	var collisions []collision
	for rows.Next() {
		var c collision
		if err := rows.Scan(&c.fromAlbum, &c.intoAlbum); err != nil {
			rows.Close()
			return fmt.Errorf("merge artists: scan collision: %w", err)
		}
		collisions = append(collisions, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("merge artists: collisions: %w", err)
	}
	rows.Close()

	for _, c := range collisions {
		if err := moveAlbumCoverIfAbsentTx(ctx, tx, c.fromAlbum, c.intoAlbum); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE media_metadata SET album_id = ?, artist_id = ? WHERE album_id = ?`,
			c.intoAlbum, intoID, c.fromAlbum); err != nil {
			return fmt.Errorf("merge artists: repoint collapsed tracks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM albums WHERE id = ?`, c.fromAlbum); err != nil {
			return fmt.Errorf("merge artists: delete collapsed album: %w", err)
		}
	}

	// 2. Move from's remaining (non-colliding) albums onto into.
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET artist_id = ? WHERE artist_id = ?`, intoID, fromID); err != nil {
		return fmt.Errorf("merge artists: move albums: %w", err)
	}
	// 3. Repoint from's remaining tracks (those of the moved albums).
	if _, err := tx.ExecContext(ctx, `UPDATE media_metadata SET artist_id = ? WHERE artist_id = ?`, intoID, fromID); err != nil {
		return fmt.Errorf("merge artists: repoint tracks: %w", err)
	}
	// 4. Give into from's artist cover only if into has none.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO artist_images (artist_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 SELECT ?, object_key, mime_type, updated_at, base_key, source_ext, variants_ready
		 FROM artist_images WHERE artist_id = ?
		   AND NOT EXISTS (SELECT 1 FROM artist_images WHERE artist_id = ?)`,
		intoID, fromID, intoID); err != nil {
		return fmt.Errorf("merge artists: move artist cover: %w", err)
	}
	// 5. Delete from (cascade removes its now-orphan artist_images; it has no
	//    albums or tracks left referencing it).
	if _, err := tx.ExecContext(ctx, `DELETE FROM artists WHERE id = ?`, fromID); err != nil {
		return fmt.Errorf("merge artists: delete source: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge artists: commit: %w", err)
	}
	return nil
}

// MergeAlbums merges album `fromID` into `intoID`: from's tracks are repointed
// onto into (and into's artist, keeping artist_id/album_id consistent), into
// gains from's cover only if it has none, then from is deleted. Useful for two
// albums of one artist that are really the same release under different titles.
// Returns ErrMergeSelf / ErrEntityNotFound. from's artist, if left with nothing,
// becomes a harmless orphan (invisible to listings).
func (db *DB) MergeAlbums(ctx context.Context, fromID, intoID int64) error {
	if fromID == intoID {
		return ErrMergeSelf
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("merge albums: begin: %w", err)
	}
	defer tx.Rollback()

	var intoArtist int64
	if err := tx.QueryRowContext(ctx, `SELECT artist_id FROM albums WHERE id = ?`, intoID).Scan(&intoArtist); errors.Is(err, sql.ErrNoRows) {
		return ErrEntityNotFound
	} else if err != nil {
		return fmt.Errorf("merge albums: load target: %w", err)
	}
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id = ?`, fromID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrEntityNotFound
	} else if err != nil {
		return fmt.Errorf("merge albums: verify source: %w", err)
	}

	if err := moveAlbumCoverIfAbsentTx(ctx, tx, fromID, intoID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE media_metadata SET album_id = ?, artist_id = ? WHERE album_id = ?`,
		intoID, intoArtist, fromID); err != nil {
		return fmt.Errorf("merge albums: repoint tracks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM albums WHERE id = ?`, fromID); err != nil {
		return fmt.Errorf("merge albums: delete source: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge albums: commit: %w", err)
	}
	return nil
}

// moveAlbumCoverIfAbsentTx copies the source album's cover row onto the target
// album only when the target has none — the "move covers if the target lacks
// one" rule shared by both merges. The source row is left as-is; it is removed
// by the caller's subsequent DELETE of the source album (cascade).
func moveAlbumCoverIfAbsentTx(ctx context.Context, tx *sql.Tx, fromAlbum, intoAlbum int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO album_images (album_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 SELECT ?, object_key, mime_type, updated_at, base_key, source_ext, variants_ready
		 FROM album_images WHERE album_id = ?
		   AND NOT EXISTS (SELECT 1 FROM album_images WHERE album_id = ?)`,
		intoAlbum, fromAlbum, intoAlbum); err != nil {
		return fmt.Errorf("move album cover: %w", err)
	}
	return nil
}

// resolveAlbumArtistTx is the transaction-scoped core of resolveAlbumArtist, for
// callers already inside a transaction (InsertFile, UpdateFileMetadata). They
// must resolve within their existing tx rather than open a nested one: the pool
// is pinned to a single connection (Open), so a nested BeginTx would deadlock.
func resolveAlbumArtistTx(ctx context.Context, tx *sql.Tx, t AlbumArtistTags) (artistID, albumID int64, err error) {
	now := time.Now().Unix()

	artistID, err = resolveArtistTx(ctx, tx, effectiveArtist(t.AlbumArtist, t.Artist), now)
	if err != nil {
		return 0, 0, err
	}

	var year any
	if t.Year > 0 {
		year = t.Year
	}
	albumTitle := effectiveAlbum(t.Album)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO albums (artist_id, title, norm_title, year, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(artist_id, norm_title) DO UPDATE SET year = COALESCE(albums.year, excluded.year)
		 RETURNING id`,
		artistID, albumTitle, normalizeKey(albumTitle), year, now,
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

// FoldUnknownBuckets folds the unknown-artist / unknown-album buckets — whose
// dedup keys migration 016 left at ” — onto their canonical keys
// (normalizeKey(DefaultArtistName) / normalizeKey(DefaultAlbumTitle)), so a track
// later tagged literally "Unknown artist" / "Other" converges on the same bucket
// the resolver now produces. When a real entity already holds the target key
// (a file was tagged that way before this change), the literal is merged into the
// bucket via the tested MergeArtists/MergeAlbums paths, keeping the bucket's
// canonical display name.
//
// Run at startup after BackfillEntities and BackfillCoverEntities (covers must
// already sit on the entity tables before a merge moves them). Idempotent: once
// no ” key remains it is a no-op.
func (db *DB) FoldUnknownBuckets(ctx context.Context) error {
	if err := db.foldUnknownArtist(ctx); err != nil {
		return err
	}
	return db.foldUnknownAlbums(ctx)
}

// foldUnknownArtist relabels the single unknown-artist bucket (norm_name = ”)
// onto normalizeKey(DefaultArtistName), merging any pre-existing literal first.
func (db *DB) foldUnknownArtist(ctx context.Context) error {
	targetNorm := normalizeKey(DefaultArtistName)

	var bucketID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM artists WHERE norm_name = ''`).Scan(&bucketID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no untagged-artist bucket exists
	}
	if err != nil {
		return fmt.Errorf("fold unknown artist: find bucket: %w", err)
	}

	var literalID int64
	err = db.QueryRowContext(ctx, `SELECT id FROM artists WHERE norm_name = ?`, targetNorm).Scan(&literalID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("fold unknown artist: find literal: %w", err)
	}
	if err == nil && literalID != bucketID {
		// Collapse the literal onto the bucket; MergeArtists handles its albums and
		// cover, and frees the target key by deleting the literal.
		if err := db.MergeArtists(ctx, literalID, bucketID); err != nil {
			return fmt.Errorf("fold unknown artist: merge literal: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE artists SET norm_name = ? WHERE id = ?`, targetNorm, bucketID); err != nil {
		return fmt.Errorf("fold unknown artist: relabel key: %w", err)
	}
	return nil
}

// foldUnknownAlbums relabels each artist's unknown-album bucket (norm_title = ”)
// onto normalizeKey(DefaultAlbumTitle), merging any pre-existing literal under the
// same artist first. Buckets are buffered before mutating: the pool is pinned to
// one connection, so a merge/update can't run while a Rows iterator is open.
func (db *DB) foldUnknownAlbums(ctx context.Context) error {
	targetNorm := normalizeKey(DefaultAlbumTitle)

	rows, err := db.QueryContext(ctx, `SELECT id, artist_id FROM albums WHERE norm_title = ''`)
	if err != nil {
		return fmt.Errorf("fold unknown albums: query buckets: %w", err)
	}
	type bucket struct{ id, artistID int64 }
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.id, &b.artistID); err != nil {
			rows.Close()
			return fmt.Errorf("fold unknown albums: scan: %w", err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("fold unknown albums: rows: %w", err)
	}
	rows.Close()

	for _, b := range buckets {
		var literalID int64
		err := db.QueryRowContext(ctx,
			`SELECT id FROM albums WHERE artist_id = ? AND norm_title = ?`,
			b.artistID, targetNorm).Scan(&literalID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("fold unknown albums: find literal: %w", err)
		}
		if err == nil && literalID != b.id {
			if err := db.MergeAlbums(ctx, literalID, b.id); err != nil {
				return fmt.Errorf("fold unknown albums: merge literal: %w", err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE albums SET norm_title = ? WHERE id = ?`, targetNorm, b.id); err != nil {
			return fmt.Errorf("fold unknown albums: relabel key: %w", err)
		}
	}
	return nil
}

// tableExists reports whether a table of the given name is present.
func (db *DB) tableExists(ctx context.Context, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("table exists %s: %w", name, err)
	}
	return n > 0, nil
}

// BackfillCoverEntities migrates the pre-entity, string-keyed cover rows that
// migration 014 set aside (album_images_old / artist_images_old) onto the new
// entity-id-keyed tables, then drops the leftovers. It must run after
// BackfillEntities so the artists/albums entities it resolves against exist.
//
// Idempotent: when the *_old tables are already gone (drained on an earlier
// start, or a fresh DB) it is a no-op. A cover whose string identity no longer
// resolves to an entity (e.g. an album that lost all its tracks) is dropped — a
// cover with no album is dead weight and unreachable.
func (db *DB) BackfillCoverEntities(ctx context.Context) error {
	if err := db.backfillAlbumCovers(ctx); err != nil {
		return err
	}
	return db.backfillArtistCovers(ctx)
}

func (db *DB) backfillAlbumCovers(ctx context.Context) error {
	ok, err := db.tableExists(ctx, "album_images_old")
	if err != nil || !ok {
		return err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT album_artist, album_title, object_key, mime_type, updated_at, base_key, source_ext, variants_ready
		 FROM album_images_old`)
	if err != nil {
		return fmt.Errorf("backfill album covers: query: %w", err)
	}
	type oldCover struct {
		artist, title       string
		objectKey, mimeType string
		updatedAt           int64
		baseKey, sourceExt  sql.NullString
		variantsReady       int
	}
	var olds []oldCover
	for rows.Next() {
		var o oldCover
		if err := rows.Scan(&o.artist, &o.title, &o.objectKey, &o.mimeType, &o.updatedAt,
			&o.baseKey, &o.sourceExt, &o.variantsReady); err != nil {
			rows.Close()
			return fmt.Errorf("backfill album covers: scan: %w", err)
		}
		olds = append(olds, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("backfill album covers: rows: %w", err)
	}
	rows.Close()

	var migrated, dropped int
	for _, o := range olds {
		albumID, found, err := db.LookupAlbumID(ctx, o.artist, o.title)
		if err != nil {
			return err
		}
		if !found {
			dropped++
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO album_images
			     (album_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			albumID, o.objectKey, o.mimeType, o.updatedAt, o.baseKey, o.sourceExt, o.variantsReady,
		); err != nil {
			return fmt.Errorf("backfill album covers: insert: %w", err)
		}
		migrated++
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE album_images_old`); err != nil {
		return fmt.Errorf("backfill album covers: drop old: %w", err)
	}
	if migrated > 0 || dropped > 0 {
		log.Printf("migrated %d album covers to entity ids (%d unresolved dropped)", migrated, dropped)
	}
	return nil
}

func (db *DB) backfillArtistCovers(ctx context.Context) error {
	ok, err := db.tableExists(ctx, "artist_images_old")
	if err != nil || !ok {
		return err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT artist_name, object_key, mime_type, updated_at, base_key, source_ext, variants_ready
		 FROM artist_images_old`)
	if err != nil {
		return fmt.Errorf("backfill artist covers: query: %w", err)
	}
	type oldCover struct {
		name                string
		objectKey, mimeType string
		updatedAt           int64
		baseKey, sourceExt  sql.NullString
		variantsReady       int
	}
	var olds []oldCover
	for rows.Next() {
		var o oldCover
		if err := rows.Scan(&o.name, &o.objectKey, &o.mimeType, &o.updatedAt,
			&o.baseKey, &o.sourceExt, &o.variantsReady); err != nil {
			rows.Close()
			return fmt.Errorf("backfill artist covers: scan: %w", err)
		}
		olds = append(olds, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("backfill artist covers: rows: %w", err)
	}
	rows.Close()

	var migrated, dropped int
	for _, o := range olds {
		artistID, found, err := db.LookupArtistID(ctx, o.name)
		if err != nil {
			return err
		}
		if !found {
			dropped++
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO artist_images
			     (artist_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			artistID, o.objectKey, o.mimeType, o.updatedAt, o.baseKey, o.sourceExt, o.variantsReady,
		); err != nil {
			return fmt.Errorf("backfill artist covers: insert: %w", err)
		}
		migrated++
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE artist_images_old`); err != nil {
		return fmt.Errorf("backfill artist covers: drop old: %w", err)
	}
	if migrated > 0 || dropped > 0 {
		log.Printf("migrated %d artist covers to entity ids (%d unresolved dropped)", migrated, dropped)
	}
	return nil
}
