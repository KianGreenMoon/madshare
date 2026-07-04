package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Playlist kinds. Favorites is a per-user system playlist (one per user,
// created lazily) — it cannot be renamed or deleted, only its items change.
const (
	PlaylistRegular   = "regular"
	PlaylistFavorites = "favorites"
)

// FavoritesName is the display name of the system favorites playlist.
const FavoritesName = "Favorites"

var (
	// ErrPlaylistNotFound — no playlist with that id belongs to the user. The
	// API maps this to 404 (not 403) so foreign ids don't leak existence.
	ErrPlaylistNotFound = errors.New("playlist not found")
	// ErrPlaylistSystem — the operation (rename/delete) is not allowed on a
	// system playlist (favorites).
	ErrPlaylistSystem = errors.New("system playlist cannot be modified")
	// ErrBadReorder — the reorder id list is not a permutation of the
	// playlist's current item ids.
	ErrBadReorder = errors.New("reorder ids do not match playlist items")
)

// Playlist is a row in the playlists table plus its item count.
type Playlist struct {
	ID         int64
	UserID     int64
	Name       string
	Kind       string
	TrackCount int
	CreatedAt  int64
	UpdatedAt  int64
}

// PlaylistItemEntry is a playlist item joined with its tagset (the appearance
// the user picked — recording-tagsets P1, decision 4) and the recording's
// ladder-best rendition (ObjectKey/MimeType/DurationSeconds — what the row
// plays; zero values when no rendition survives). Trashed reports the
// appearance is unavailable (tagset trashed / not approved / recording
// dormant): metadata stays visible but the track is not playable
// (docs/api/playlists.md).
type PlaylistItemEntry struct {
	ItemID          int64
	TagsetID        int64
	ObjectKey       string
	MimeType        string
	Title           sql.NullString
	Artist          sql.NullString
	Album           sql.NullString
	DurationSeconds sql.NullFloat64
	Trashed         bool
}

// ListPlaylists returns the user's playlists (favorites first, then regular by
// name) with item counts. It does not create the favorites row — callers that
// must surface it use EnsureFavoritesPlaylist first.
func (db *DB) ListPlaylists(ctx context.Context, userID int64) ([]*Playlist, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.user_id, p.name, p.kind, p.created_at, p.updated_at,
		       (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		FROM playlists p
		WHERE p.user_id = ?
		ORDER BY p.kind = 'favorites' DESC, p.name COLLATE NOCASE, p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Playlist
	for rows.Next() {
		p := &Playlist{}
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.CreatedAt, &p.UpdatedAt, &p.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EnsureFavoritesPlaylist returns the id of the user's favorites playlist,
// creating it if absent. Idempotent (the partial unique index makes a
// concurrent double-create lose cleanly).
func (db *DB) EnsureFavoritesPlaylist(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM playlists WHERE user_id = ? AND kind = 'favorites'`, userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`INSERT INTO playlists (user_id, name, kind, created_at, updated_at) VALUES (?, ?, 'favorites', ?, ?)`,
		userID, FavoritesName, now, now)
	if err != nil {
		// Lost a create race: the unique index rejected the second insert.
		if err2 := db.QueryRowContext(ctx,
			`SELECT id FROM playlists WHERE user_id = ? AND kind = 'favorites'`, userID).Scan(&id); err2 == nil {
			return id, nil
		}
		return 0, err
	}
	return res.LastInsertId()
}

// CreatePlaylist creates a regular playlist for the user, optionally seeded
// with items (tagset ids, in order). Validation matches AddPlaylistItems:
// every id must name a visible (approved, non-trashed, playable) appearance or
// the whole create fails with ErrFileNotFound.
func (db *DB) CreatePlaylist(ctx context.Context, userID int64, name string, tagsetIDs []int64) (*Playlist, error) {
	now := time.Now().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO playlists (user_id, name, kind, created_at, updated_at) VALUES (?, ?, 'regular', ?, ?)`,
		userID, name, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	added, err := addItemsTx(ctx, tx, id, tagsetIDs, false, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Playlist{ID: id, UserID: userID, Name: name, Kind: PlaylistRegular,
		TrackCount: added, CreatedAt: now, UpdatedAt: now}, nil
}

// GetPlaylist returns the user's playlist and its items in order. Unavailable
// appearances (trashed / unapproved tagset, or a dormant recording with no
// surviving rendition) stay listed with Trashed=true; hard-deleted tagsets are
// gone via FK cascade.
func (db *DB) GetPlaylist(ctx context.Context, userID, playlistID int64) (*Playlist, []*PlaylistItemEntry, error) {
	p := &Playlist{}
	err := db.QueryRowContext(ctx, `
		SELECT id, user_id, name, kind, created_at, updated_at
		FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).
		Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrPlaylistNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT i.id, m.id, COALESCE(f.object_key, ''), COALESCE(f.mime_type, ''),
		       m.title, m.artist, m.album, mm.duration_seconds,
		       (m.deleted_at IS NOT NULL OR m.review_state <> 'approved' OR f.id IS NULL)
		FROM playlist_items i
		JOIN tagsets m ON m.id = i.tagset_id`+recordingJoin+bestRenditionJoin(true)+`
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE i.playlist_id = ?
		ORDER BY i.position, i.id`, playlistID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]*PlaylistItemEntry, 0)
	for rows.Next() {
		e := &PlaylistItemEntry{}
		if err := rows.Scan(&e.ItemID, &e.TagsetID, &e.ObjectKey, &e.MimeType,
			&e.Title, &e.Artist, &e.Album, &e.DurationSeconds, &e.Trashed); err != nil {
			return nil, nil, err
		}
		items = append(items, e)
	}
	p.TrackCount = len(items)
	return p, items, rows.Err()
}

// RenamePlaylist renames a regular playlist. Favorites returns ErrPlaylistSystem.
func (db *DB) RenamePlaylist(ctx context.Context, userID, playlistID int64, name string) error {
	kind, err := db.playlistKind(ctx, userID, playlistID)
	if err != nil {
		return err
	}
	if kind != PlaylistRegular {
		return ErrPlaylistSystem
	}
	_, err = db.ExecContext(ctx,
		`UPDATE playlists SET name = ?, updated_at = ? WHERE id = ?`,
		name, time.Now().Unix(), playlistID)
	return err
}

// DeletePlaylist deletes a regular playlist (items cascade). Favorites returns
// ErrPlaylistSystem.
func (db *DB) DeletePlaylist(ctx context.Context, userID, playlistID int64) error {
	kind, err := db.playlistKind(ctx, userID, playlistID)
	if err != nil {
		return err
	}
	if kind != PlaylistRegular {
		return ErrPlaylistSystem
	}
	_, err = db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, playlistID)
	return err
}

// AddPlaylistItems appends tracks (by tagset id, in order) to the user's
// playlist. Every id must name a visible appearance or the whole batch fails
// with ErrFileNotFound — the add is atomic. On the favorites playlist, tagsets
// already present are skipped (Like is idempotent).
func (db *DB) AddPlaylistItems(ctx context.Context, userID, playlistID int64, tagsetIDs []int64) (added int, err error) {
	kind, err := db.playlistKind(ctx, userID, playlistID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added, err = addItemsTx(ctx, tx, playlistID, tagsetIDs, kind == PlaylistFavorites, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// addItemsTx verifies each tagset is a visible appearance and appends items
// after the playlist's current max position. dedupe skips tagsets already in
// the list (favorites semantics). Touches updated_at when anything was added.
func addItemsTx(ctx context.Context, tx *sql.Tx, playlistID int64, tagsetIDs []int64, dedupe bool, now int64) (int, error) {
	if len(tagsetIDs) == 0 {
		return 0, nil
	}
	var pos int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&pos); err != nil {
		return 0, err
	}
	added := 0
	for _, tagsetID := range tagsetIDs {
		var ok int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM tagsets m WHERE m.id = ? AND `+visibleTagset, tagsetID).Scan(&ok)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrFileNotFound
		}
		if err != nil {
			return 0, err
		}
		if dedupe {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ? AND tagset_id = ?`,
				playlistID, tagsetID).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				continue
			}
		}
		pos++
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, tagset_id, position, added_at) VALUES (?, ?, ?, ?)`,
			playlistID, tagsetID, pos, now); err != nil {
			return 0, err
		}
		added++
	}
	if added > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlists SET updated_at = ? WHERE id = ?`, now, playlistID); err != nil {
			return 0, err
		}
	}
	return added, nil
}

// RemovePlaylistItem removes one item (by item id) from the user's playlist.
// found is false (no error) when the item is not in that playlist.
func (db *DB) RemovePlaylistItem(ctx context.Context, userID, playlistID, itemID int64) (found bool, err error) {
	if _, err := db.playlistKind(ctx, userID, playlistID); err != nil {
		return false, err
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE id = ? AND playlist_id = ?`, itemID, playlistID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		_, err = db.ExecContext(ctx,
			`UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().Unix(), playlistID)
	}
	return n > 0, err
}

// ReorderPlaylist rewrites the playlist's item order. itemIDs must be a
// permutation of the playlist's current item ids (ErrBadReorder otherwise);
// positions are renumbered 1..n, compacting any holes left by cascaded deletes.
func (db *DB) ReorderPlaylist(ctx context.Context, userID, playlistID int64, itemIDs []int64) error {
	if _, err := db.playlistKind(ctx, userID, playlistID); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM playlist_items WHERE playlist_id = ?`, playlistID)
	if err != nil {
		return err
	}
	current := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(itemIDs) != len(current) {
		return ErrBadReorder
	}
	seen := make(map[int64]bool, len(itemIDs))
	for _, id := range itemIDs {
		if !current[id] || seen[id] {
			return ErrBadReorder
		}
		seen[id] = true
	}

	for i, id := range itemIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_items SET position = ? WHERE id = ?`, i+1, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().Unix(), playlistID); err != nil {
		return err
	}
	return tx.Commit()
}

// ToggleFavorite flips the membership of the appearance (by tagset id) in the
// user's favorites playlist, creating the playlist on first use. Returns the
// resulting state. Unknown or unavailable tagsets return ErrFileNotFound.
func (db *DB) ToggleFavorite(ctx context.Context, userID, tagsetID int64) (liked bool, err error) {
	favID, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		return false, err
	}
	var ok int
	err = db.QueryRowContext(ctx,
		`SELECT 1 FROM tagsets m WHERE m.id = ? AND `+visibleTagset, tagsetID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrFileNotFound
	}
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND tagset_id = ?`, favID, tagsetID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n > 0 {
		_, err = db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, favID)
		return false, err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO playlist_items (playlist_id, tagset_id, position, added_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM playlist_items WHERE playlist_id = ?), ?)`,
		favID, tagsetID, favID, now); err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, favID)
	return true, err
}

// ListFavoriteTagsetIDs returns the tagset ids of the user's visible favorites
// (unavailable appearances excluded — a grayed heart would suggest the track is
// likable). A user with no favorites playlist simply gets an empty list.
func (db *DB) ListFavoriteTagsetIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id
		FROM playlists p
		JOIN playlist_items i ON i.playlist_id = p.id
		JOIN tagsets m        ON m.id = i.tagset_id
		WHERE p.user_id = ? AND p.kind = 'favorites' AND `+visibleTagset+`
		ORDER BY i.position`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// playlistKind returns the kind of the user's playlist, or ErrPlaylistNotFound
// — the shared ownership check for all item-level operations.
func (db *DB) playlistKind(ctx context.Context, userID, playlistID int64) (string, error) {
	var kind string
	err := db.QueryRowContext(ctx,
		`SELECT kind FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPlaylistNotFound
	}
	return kind, err
}
