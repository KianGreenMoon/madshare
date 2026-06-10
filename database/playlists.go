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

// PlaylistItemEntry is a playlist item joined with its file and tags. Trashed
// reports the underlying file is soft-deleted: metadata stays visible but the
// track is not playable (docs/plans/playlists.md Decision §3).
type PlaylistItemEntry struct {
	ItemID          int64
	FileID          int64
	Hash            string
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
// with items (content hashes, in order). Hash validation matches
// AddPlaylistItemsByHash: every hash must name a live (non-trashed) file or
// the whole create fails with ErrFileNotFound.
func (db *DB) CreatePlaylist(ctx context.Context, userID int64, name string, hashes []string) (*Playlist, error) {
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
	added, err := addItemsTx(ctx, tx, id, hashes, false, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Playlist{ID: id, UserID: userID, Name: name, Kind: PlaylistRegular,
		TrackCount: added, CreatedAt: now, UpdatedAt: now}, nil
}

// GetPlaylist returns the user's playlist and its items in order. Trashed files
// stay listed (Trashed=true); hard-deleted files are gone via FK cascade.
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
		SELECT i.id, f.id, f.hash, f.object_key, f.mime_type,
		       m.title, m.artist, m.album, m.duration_seconds,
		       f.deleted_at IS NOT NULL
		FROM playlist_items i
		JOIN files f          ON f.id = i.file_id
		LEFT JOIN media_metadata m ON m.file_id = f.id
		WHERE i.playlist_id = ?
		ORDER BY i.position, i.id`, playlistID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]*PlaylistItemEntry, 0)
	for rows.Next() {
		e := &PlaylistItemEntry{}
		if err := rows.Scan(&e.ItemID, &e.FileID, &e.Hash, &e.ObjectKey, &e.MimeType,
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

// AddPlaylistItemsByHash appends tracks (by content hash, in order) to the
// user's playlist. Every hash must name a live (non-trashed) file or the whole
// batch fails with ErrFileNotFound — the add is atomic. On the favorites
// playlist, hashes already present are skipped (Like is idempotent).
func (db *DB) AddPlaylistItemsByHash(ctx context.Context, userID, playlistID int64, hashes []string) (added int, err error) {
	kind, err := db.playlistKind(ctx, userID, playlistID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added, err = addItemsTx(ctx, tx, playlistID, hashes, kind == PlaylistFavorites, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// addItemsTx resolves each hash to a live file and appends items after the
// playlist's current max position. dedupe skips files already in the list
// (favorites semantics). Touches updated_at when anything was added.
func addItemsTx(ctx context.Context, tx *sql.Tx, playlistID int64, hashes []string, dedupe bool, now int64) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	var pos int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&pos); err != nil {
		return 0, err
	}
	added := 0
	for _, hash := range hashes {
		var fileID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM files WHERE hash = ? AND deleted_at IS NULL`, hash).Scan(&fileID)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrFileNotFound
		}
		if err != nil {
			return 0, err
		}
		if dedupe {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ? AND file_id = ?`,
				playlistID, fileID).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				continue
			}
		}
		pos++
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, file_id, position, added_at) VALUES (?, ?, ?, ?)`,
			playlistID, fileID, pos, now); err != nil {
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

// ToggleFavorite flips the membership of the file (by content hash) in the
// user's favorites playlist, creating the playlist on first use. Returns the
// resulting state. Unknown or trashed hashes return ErrFileNotFound.
func (db *DB) ToggleFavorite(ctx context.Context, userID int64, hash string) (liked bool, err error) {
	favID, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		return false, err
	}
	var fileID int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM files WHERE hash = ? AND deleted_at IS NULL`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrFileNotFound
	}
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND file_id = ?`, favID, fileID)
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
		INSERT INTO playlist_items (playlist_id, file_id, position, added_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM playlist_items WHERE playlist_id = ?), ?)`,
		favID, fileID, favID, now); err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, favID)
	return true, err
}

// ListFavoriteHashes returns the content hashes of the user's live favorites
// (trashed files excluded — a grayed heart would suggest the track is likable).
// A user with no favorites playlist simply gets an empty list.
func (db *DB) ListFavoriteHashes(ctx context.Context, userID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.hash
		FROM playlists p
		JOIN playlist_items i ON i.playlist_id = p.id
		JOIN files f          ON f.id = i.file_id
		WHERE p.user_id = ? AND p.kind = 'favorites' AND f.deleted_at IS NULL
		ORDER BY i.position`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
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
