package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Content-grant scope types (see migration 005_content_access.sql).
const (
	ScopeAll    = "all"
	ScopeArtist = "artist"
	ScopeAlbum  = "album"
	ScopeFile   = "file"
)

// accessClause is the SQL predicate (reused by listing filters) that decides
// whether the file aliased `f` (joined to media_metadata `m`) is reachable by
// the user bound to the first parameter. It does NOT account for the
// content.all permission — callers holding that bypass access checks entirely.
// Bind order: the user id (sql.NullInt64; invalid => anonymous, only guest
// files match).
//
// The license branch checks the auto-derive policy live at query time so that
// toggling the policy or its allow-list takes effect immediately without any
// bulk UPDATE. guest_playable_manual = 0 guards the branch: if an admin
// explicitly set the file's guest flag (to either value), that decision wins
// over the license policy.
const accessClause = `(
  f.guest_playable = 1
  OR (
    f.guest_playable_manual = 0
    AND f.license IS NOT NULL AND f.license != ''
    AND EXISTS (SELECT 1 FROM settings WHERE key = 'access.autoderive.enabled' AND value = '1')
    AND ',' || COALESCE((SELECT value FROM settings WHERE key = 'access.autoderive.licenses'), '') || ','
        LIKE '%,' || f.license || ',%'
  )
  OR EXISTS (
    SELECT 1 FROM access_group_members agm
    JOIN content_grants cg ON cg.group_id = agm.group_id
    WHERE agm.user_id = ?
    AND (
      cg.scope_type = 'all'
      OR (cg.scope_type = 'artist' AND cg.scope_artist = COALESCE(NULLIF(m.album_artist, ''), m.artist))
      OR (cg.scope_type = 'album'  AND cg.scope_artist = COALESCE(NULLIF(m.album_artist, ''), m.artist) AND cg.scope_album = COALESCE(m.album, ''))
      OR (cg.scope_type = 'file'   AND cg.scope_file_id = f.id)
    )
  )
)`

// FileAccessibleByHash reports whether the user (invalid userID = anonymous)
// may play/download the file with the given content hash. It returns false for
// unknown hashes and for soft-deleted (trashed) files. Callers must
// short-circuit this for identities holding the content.all permission.
func (db *DB) FileAccessibleByHash(ctx context.Context, hash string, userID sql.NullInt64) (bool, error) {
	var ok bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM files f
			LEFT JOIN media_metadata m ON m.file_id = f.id
			WHERE f.hash = ? AND f.deleted_at IS NULL AND `+accessClause+`
		)`, hash, userID).Scan(&ok)
	return ok, err
}

// SetGuestPlayable sets the guest-playable flag on the file with the given
// hash. Any explicit set is a manual decision (guest_playable_manual = 1), so
// the auto-derivation policy will never override it (auth.md §5.1). found is
// false (no error) when no file matches.
func (db *DB) SetGuestPlayable(ctx context.Context, hash string, guest bool) (found bool, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE files SET guest_playable = ?, guest_playable_manual = 1 WHERE hash = ? AND deleted_at IS NULL`,
		boolToInt(guest), hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetLicense sets (or clears, with "") the license metadata on a file. Access
// derived from the license is evaluated live at query time via accessClause;
// this function only stores the metadata.
func (db *DB) SetLicense(ctx context.Context, hash, license string) (found bool, err error) {
	var lic sql.NullString
	if license != "" {
		lic = sql.NullString{String: license, Valid: true}
	}
	res, err := db.ExecContext(ctx, `UPDATE files SET license = ? WHERE hash = ? AND deleted_at IS NULL`, lic, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AccessGroup is a row in access_groups.
type AccessGroup struct {
	ID   int64
	Name string
}

// CreateAccessGroup inserts a group and returns its id.
func (db *DB) CreateAccessGroup(ctx context.Context, name string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO access_groups (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAccessGroups returns all access groups ordered by name.
func (db *DB) ListAccessGroups(ctx context.Context) ([]AccessGroup, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM access_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessGroup
	for rows.Next() {
		var g AccessGroup
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddGroupMember adds a user to a group (idempotent).
func (db *DB) AddGroupMember(ctx context.Context, groupID, userID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO access_group_members (group_id, user_id) VALUES (?, ?)`, groupID, userID)
	return err
}

// DeleteAccessGroup removes a group (cascading to its members and grants).
// found is false (no error) when no group matches.
func (db *DB) DeleteAccessGroup(ctx context.Context, groupID int64) (found bool, err error) {
	res, err := db.ExecContext(ctx, `DELETE FROM access_groups WHERE id = ?`, groupID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GroupMember is a user belonging to an access group.
type GroupMember struct {
	UserID   int64
	Username string
}

// ListGroupMembers returns the members of a group, ordered by username.
func (db *DB) ListGroupMembers(ctx context.Context, groupID int64) ([]GroupMember, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT u.id, u.username
		 FROM access_group_members agm JOIN users u ON u.id = agm.user_id
		 WHERE agm.group_id = ? ORDER BY u.username`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.UserID, &m.Username); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveGroupMember removes a user from a group (idempotent — no error when the
// membership does not exist).
func (db *DB) RemoveGroupMember(ctx context.Context, groupID, userID int64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM access_group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
	return err
}

// ContentGrant is a row in content_grants.
type ContentGrant struct {
	ID        int64
	GroupID   int64
	ScopeType string
	Artist    sql.NullString
	Album     sql.NullString
	FileID    sql.NullInt64
}

// ListContentGrants returns the grants attached to a group, newest first.
func (db *DB) ListContentGrants(ctx context.Context, groupID int64) ([]ContentGrant, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, group_id, scope_type, scope_artist, scope_album, scope_file_id
		 FROM content_grants WHERE group_id = ? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentGrant
	for rows.Next() {
		var g ContentGrant
		if err := rows.Scan(&g.ID, &g.GroupID, &g.ScopeType, &g.Artist, &g.Album, &g.FileID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddContentGrant attaches a content scope to a group. For ScopeAll the scope
// values are ignored; for ScopeArtist/ScopeAlbum pass the names; for ScopeFile
// pass fileID.
func (db *DB) AddContentGrant(ctx context.Context, groupID int64, scopeType, artist, album string, fileID sql.NullInt64) (int64, error) {
	var a, al sql.NullString
	if artist != "" {
		a = sql.NullString{String: artist, Valid: true}
	}
	if album != "" {
		al = sql.NullString{String: album, Valid: true}
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO content_grants (group_id, scope_type, scope_artist, scope_album, scope_file_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		groupID, scopeType, a, al, fileID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteContentGrant removes a single grant. found is false (no error) when no
// grant matches the id.
func (db *DB) DeleteContentGrant(ctx context.Context, grantID int64) (found bool, err error) {
	res, err := db.ExecContext(ctx, `DELETE FROM content_grants WHERE id = ?`, grantID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetFileIDByHash returns the files.id for a content hash. found is false (no
// error) when no file matches.
func (db *DB) GetFileIDByHash(ctx context.Context, hash string) (id int64, found bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
