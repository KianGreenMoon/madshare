package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"daemonlord.ygg/madshare/auth"
)

// CountUsers returns the number of rows in the users table. Used by the
// first-run admin bootstrap.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user and returns its id.
func (db *DB) CreateUser(ctx context.Context, username, passwordHash string, changeRequired bool) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_change_required, created_at)
		 VALUES (?, ?, ?, ?)`,
		username, passwordHash, boolToInt(changeRequired), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByUsername returns the user row for username, or (nil, nil) on miss.
func (db *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	var changeReq, disabled int
	err := db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_change_required, disabled, created_at
		 FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &changeReq, &disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.PasswordChangeRequired = changeReq != 0
	u.Disabled = disabled != 0
	return &u, nil
}

// ListUsers returns all users (id, username, disabled), ordered by username.
// Used by the admin UI to populate access-group membership pickers.
func (db *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, username, disabled, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.ID, &u.Username, &disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, &u)
	}
	return out, rows.Err()
}

// AssignRoleByName grants the named built-in role to a user. A duplicate
// assignment is ignored.
func (db *DB) AssignRoleByName(ctx context.Context, userID int64, roleName string) error {
	res, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO user_roles (user_id, role_id)
		 SELECT ?, id FROM roles WHERE name = ?`, userID, roleName)
	if err != nil {
		return err
	}
	// SELECT matched no role => the role name was wrong; surface it.
	if n, _ := res.RowsAffected(); n == 0 {
		if exists, err := db.roleExists(ctx, roleName); err != nil {
			return err
		} else if !exists {
			return fmt.Errorf("database: unknown role %q", roleName)
		}
	}
	return nil
}

func (db *DB) roleExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM roles WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetPassword updates a user's password hash and the change-required flag.
func (db *DB) SetPassword(ctx context.Context, userID int64, passwordHash string, changeRequired bool) error {
	_, err := db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_change_required = ? WHERE id = ?`,
		passwordHash, boolToInt(changeRequired), userID)
	return err
}

// userPermissions returns the union of permissions across a user's roles.
func (db *DB) userPermissions(ctx context.Context, userID int64) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT rp.permission
		 FROM user_roles ur JOIN role_permissions rp ON rp.role_id = ur.role_id
		 WHERE ur.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	perms := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		perms[p] = true
	}
	return perms, rows.Err()
}

// CreateSession inserts a session row keyed by the token hash.
func (db *DB) CreateSession(ctx context.Context, tokenHash string, userID, expiresAt int64) error {
	now := time.Now().Unix()
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)`, tokenHash, userID, now, expiresAt, now)
	return err
}

// DeleteSession removes a single session (logout).
func (db *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions removes every session for a user ("log out everywhere").
func (db *DB) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// SessionIdentity resolves a session token hash to an identity (implements
// auth.Store). Returns (nil, nil) when the session is unknown, expired, or its
// user is disabled. Touches last_seen_at on success.
func (db *DB) SessionIdentity(ctx context.Context, tokenHash string) (*auth.Identity, error) {
	var (
		userID    int64
		username  string
		changeReq int
		expiresAt int64
	)
	err := db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.password_change_required, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND u.disabled = 0`, tokenHash).
		Scan(&userID, &username, &changeReq, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() >= expiresAt {
		// Expired: best-effort cleanup, treat as anonymous.
		_ = db.DeleteSession(ctx, tokenHash)
		return nil, nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, time.Now().Unix(), tokenHash); err != nil {
		return nil, err
	}
	return db.buildIdentity(ctx, userID, username, changeReq != 0)
}

// CreateToken inserts an API token row keyed by its hash and returns the row id.
// expiresAt <= 0 means the token does not expire.
func (db *DB) CreateToken(ctx context.Context, userID int64, name, tokenHash string, expiresAt int64) (int64, error) {
	var exp sql.NullInt64
	if expiresAt > 0 {
		exp = sql.NullInt64{Int64: expiresAt, Valid: true}
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO api_tokens (user_id, name, token_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`, userID, name, tokenHash, time.Now().Unix(), exp)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TokenIdentity resolves an API token hash to an identity (implements
// auth.Store). Returns (nil, nil) when the token is unknown, revoked, expired,
// or its user is disabled. Touches last_used_at on success.
func (db *DB) TokenIdentity(ctx context.Context, tokenHash string) (*auth.Identity, error) {
	var (
		tokenID   int64
		userID    int64
		username  string
		changeReq int
		expiresAt sql.NullInt64
		revokedAt sql.NullInt64
	)
	err := db.QueryRowContext(ctx,
		`SELECT t.id, u.id, u.username, u.password_change_required, t.expires_at, t.revoked_at
		 FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = ? AND u.disabled = 0`, tokenHash).
		Scan(&tokenID, &userID, &username, &changeReq, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		return nil, nil
	}
	now := time.Now().Unix()
	if expiresAt.Valid && now >= expiresAt.Int64 {
		return nil, nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, tokenID); err != nil {
		return nil, err
	}
	return db.buildIdentity(ctx, userID, username, changeReq != 0)
}

// ListTokens returns a user's API tokens (no hashes), newest first.
func (db *DB) ListTokens(ctx context.Context, userID int64) ([]*APIToken, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, user_id, name, created_at, last_used_at, expires_at, revoked_at
		 FROM api_tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RevokeToken marks a user's token revoked. found is false (no error) when the
// token does not belong to the user or does not exist.
func (db *DB) RevokeToken(ctx context.Context, userID, tokenID int64) (found bool, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), tokenID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (db *DB) buildIdentity(ctx context.Context, userID int64, username string, changeRequired bool) (*auth.Identity, error) {
	perms, err := db.userPermissions(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &auth.Identity{
		UserID:                 userID,
		Username:               username,
		Permissions:            perms,
		PasswordChangeRequired: changeRequired,
	}, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
