package database

import (
	"context"
	"database/sql"
	"errors"
	"sort"
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
	// ErrBadRemoteRef — a remote track reference is malformed (the hash is not
	// a content hash).
	ErrBadRemoteRef = errors.New("invalid remote track reference")
)

// RemoteTrackRef identifies a remote madnetwork track in a playlist: the
// default rendition's content hash plus the display text captured at add time
// (the friend's catalog row may vanish later). docs/ui/madnetwork-page.md
// §Remote tracks in favorites & playlists.
type RemoteTrackRef struct {
	Hash   string `json:"hash"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
}

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
	Position        int64
	TagsetID        int64
	ObjectKey       string
	MimeType        string
	Title           sql.NullString
	Artist          sql.NullString
	Album           sql.NullString
	DurationSeconds sql.NullFloat64
	Trashed         bool

	// Remote madnetwork items (tagset_id NULL): the rendition hash the row
	// plays via the streaming relay, and whether any source can currently
	// provide it (a live local blob, or a friend advertising it).
	RemoteHash string
	Available  bool
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
// with items (tagset ids, then remote refs, in order). Validation matches
// AddPlaylistItems: every id must name a visible (approved, non-trashed,
// playable) appearance or the whole create fails with ErrFileNotFound; a
// malformed remote hash fails with ErrBadRemoteRef.
func (db *DB) CreatePlaylist(ctx context.Context, userID int64, name string, tagsetIDs []int64, remote []RemoteTrackRef) (*Playlist, error) {
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
	added, err := addItemsTx(ctx, tx, id, tagsetIDs, remote, false, now)
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
		SELECT i.id, i.position, m.id, COALESCE(f.object_key, ''), COALESCE(f.mime_type, ''),
		       m.title, m.artist, m.album, mm.duration_seconds,
		       (m.deleted_at IS NOT NULL OR m.review_state <> 'approved' OR f.id IS NULL)
		FROM playlist_items i
		JOIN tagsets m ON m.id = i.tagset_id`+recordingJoin+bestRenditionJoin(true)+`
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE i.playlist_id = ? AND i.tagset_id IS NOT NULL
		ORDER BY i.position, i.id`, playlistID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]*PlaylistItemEntry, 0)
	for rows.Next() {
		e := &PlaylistItemEntry{}
		if err := rows.Scan(&e.ItemID, &e.Position, &e.TagsetID, &e.ObjectKey, &e.MimeType,
			&e.Title, &e.Artist, &e.Album, &e.DurationSeconds, &e.Trashed); err != nil {
			return nil, nil, err
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Remote madnetwork items ride in a second pass (their availability check
	// runs against entirely different tables), then both merge on position.
	remote, err := db.remotePlaylistItems(ctx, playlistID)
	if err != nil {
		return nil, nil, err
	}
	if len(remote) > 0 {
		items = append(items, remote...)
		sort.SliceStable(items, func(a, b int) bool {
			if items[a].Position != items[b].Position {
				return items[a].Position < items[b].Position
			}
			return items[a].ItemID < items[b].ItemID
		})
	}
	p.TrackCount = len(items)
	return p, items, nil
}

// remotePlaylistItems lists a playlist's remote madnetwork rows. Available is
// true when some source can still provide the hash: a live local blob, or a
// friend advertising it (catalog or holdings) — the streaming relay
// short-circuits local hashes, so an available row always plays.
func (db *DB) remotePlaylistItems(ctx context.Context, playlistID int64) ([]*PlaylistItemEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT i.id, i.position, i.remote_hash,
		       COALESCE(i.remote_title, ''), COALESCE(i.remote_artist, ''), COALESCE(i.remote_album, ''),
		       (EXISTS(SELECT 1 FROM files f WHERE f.hash = i.remote_hash AND f.deleted_at IS NULL)
		        OR EXISTS(SELECT 1 FROM federation_catalog c`+sourceJoin("c")+`
		                  WHERE c.renditions LIKE '%' || i.remote_hash || '%' AND `+notBlocked+`)
		        OR EXISTS(SELECT 1 FROM federation_holdings h`+sourceJoin("h")+`
		                  WHERE h.hash = i.remote_hash AND `+notBlocked+`))
		FROM playlist_items i
		WHERE i.playlist_id = ? AND i.remote_hash IS NOT NULL
		ORDER BY i.position, i.id`, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*PlaylistItemEntry, 0)
	for rows.Next() {
		e := &PlaylistItemEntry{}
		var title, artist, album string
		if err := rows.Scan(&e.ItemID, &e.Position, &e.RemoteHash, &title, &artist, &album, &e.Available); err != nil {
			return nil, err
		}
		e.Title = sql.NullString{String: title, Valid: title != ""}
		e.Artist = sql.NullString{String: artist, Valid: artist != ""}
		e.Album = sql.NullString{String: album, Valid: album != ""}
		items = append(items, e)
	}
	return items, rows.Err()
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

// AddPlaylistItems appends tracks (by tagset id, then remote refs, in order)
// to the user's playlist. Every id must name a visible appearance or the whole
// batch fails with ErrFileNotFound — the add is atomic; a malformed remote
// hash fails with ErrBadRemoteRef. On the favorites playlist, tracks already
// present are skipped (Like is idempotent).
func (db *DB) AddPlaylistItems(ctx context.Context, userID, playlistID int64, tagsetIDs []int64, remote []RemoteTrackRef) (added int, err error) {
	kind, err := db.playlistKind(ctx, userID, playlistID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added, err = addItemsTx(ctx, tx, playlistID, tagsetIDs, remote, kind == PlaylistFavorites, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// addItemsTx verifies each tagset is a visible appearance (and each remote ref
// a well-formed content hash) and appends items after the playlist's current
// max position. dedupe skips tracks already in the list (favorites semantics).
// Touches updated_at when anything was added.
func addItemsTx(ctx context.Context, tx *sql.Tx, playlistID int64, tagsetIDs []int64, remote []RemoteTrackRef, dedupe bool, now int64) (int, error) {
	if len(tagsetIDs) == 0 && len(remote) == 0 {
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
	for _, ref := range remote {
		if !isContentHash(ref.Hash) {
			return 0, ErrBadRemoteRef
		}
		if dedupe {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ? AND remote_hash = ?`,
				playlistID, ref.Hash).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				continue
			}
		}
		pos++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO playlist_items (playlist_id, remote_hash, remote_title, remote_artist, remote_album, position, added_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			playlistID, ref.Hash, ref.Title, ref.Artist, ref.Album, pos, now); err != nil {
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

// ToggleRemoteFavorite flips the membership of a remote madnetwork track (by
// rendition hash) in the user's favorites playlist, creating the playlist on
// first use. The display text is captured on first like. Returns the resulting
// state; a malformed hash returns ErrBadRemoteRef.
func (db *DB) ToggleRemoteFavorite(ctx context.Context, userID int64, ref RemoteTrackRef) (liked bool, err error) {
	if !isContentHash(ref.Hash) {
		return false, ErrBadRemoteRef
	}
	favID, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	res, err := db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND remote_hash = ?`, favID, ref.Hash)
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
		INSERT INTO playlist_items (playlist_id, remote_hash, remote_title, remote_artist, remote_album, position, added_at)
		VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM playlist_items WHERE playlist_id = ?), ?)`,
		favID, ref.Hash, ref.Title, ref.Artist, ref.Album, favID, now); err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, favID)
	return true, err
}

// ListFavoriteRemoteHashes returns the remote hashes in the user's favorites —
// the mn: half of the client's liked set.
func (db *DB) ListFavoriteRemoteHashes(ctx context.Context, userID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT i.remote_hash
		FROM playlists p
		JOIN playlist_items i ON i.playlist_id = p.id
		WHERE p.user_id = ? AND p.kind = 'favorites' AND i.remote_hash IS NOT NULL
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

// RepointRemotePlaylistItems turns remote playlist rows whose hash now lives
// in the library (a live blob whose recording has a visible appearance) into
// normal local rows — the write-time half of the materialize repoint
// (docs/ui/madnetwork-page.md). A row whose playlist already contains the
// target appearance is dropped instead of duplicated. Idempotent; called after
// blobs land approved (moderation approve, madnetwork downloads) and once at
// startup as the catch-all sweep.
func (db *DB) RepointRemotePlaylistItems(ctx context.Context) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT i.id, i.playlist_id, (
			SELECT m.id FROM files f
			JOIN tagsets m ON m.recording_id = f.recording_id
			WHERE f.hash = i.remote_hash AND f.deleted_at IS NULL AND `+visibleTagset+`
			ORDER BY m.is_primary DESC, m.id LIMIT 1)
		FROM playlist_items i
		WHERE i.remote_hash IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type hit struct{ itemID, playlistID, tagsetID int64 }
	var hits []hit
	for rows.Next() {
		var itemID, playlistID int64
		var tagsetID sql.NullInt64
		if err := rows.Scan(&itemID, &playlistID, &tagsetID); err != nil {
			rows.Close()
			return 0, err
		}
		if tagsetID.Valid {
			hits = append(hits, hit{itemID, playlistID, tagsetID.Int64})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	repointed := 0
	for _, h := range hits {
		var dup int
		err := db.QueryRowContext(ctx,
			`SELECT 1 FROM playlist_items WHERE playlist_id = ? AND tagset_id = ?`,
			h.playlistID, h.tagsetID).Scan(&dup)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := db.ExecContext(ctx, `
				UPDATE playlist_items SET tagset_id = ?, remote_hash = NULL,
					remote_title = NULL, remote_artist = NULL, remote_album = NULL
				WHERE id = ?`, h.tagsetID, h.itemID); err != nil {
				return repointed, err
			}
		case err != nil:
			return repointed, err
		default:
			// The playlist already holds the local appearance — drop the remote twin.
			if _, err := db.ExecContext(ctx,
				`DELETE FROM playlist_items WHERE id = ?`, h.itemID); err != nil {
				return repointed, err
			}
		}
		repointed++
	}
	return repointed, nil
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
