package auth

import (
	"context"
	"errors"
	"time"
)

// Built-in role names (seeded by migration 003_auth.sql).
const (
	RoleAdmin     = "admin"
	RoleModerator = "moderator"
	RoleUploader  = "uploader"
	RoleListener  = "listener"
)

// SessionTTL is the lifetime of a new login session.
const SessionTTL = 30 * 24 * time.Hour

// ErrNoAdminCredential is returned by Bootstrap when the database has no users
// and no initial admin password was supplied — an unrecoverable startup state.
var ErrNoAdminCredential = errors.New("auth: no users exist and no initial admin password was provided")

// BootstrapStore is the persistence Bootstrap needs.
type BootstrapStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, username, passwordHash string, changeRequired bool) (int64, error)
	AssignRoleByName(ctx context.Context, userID int64, roleName string) error
}

// Bootstrap creates the first admin account when the users table is empty,
// using the supplied initial password (which the caller sourced from config or
// the environment). The new admin must change its password on first login.
//
//   - users empty, password set   -> creates admin, returns created=true
//   - users empty, password empty -> returns ErrNoAdminCredential
//   - users already exist         -> no-op, returns created=false
//
// When created=false and the caller still has an initial password configured,
// it should warn that the stale secret can be removed.
func Bootstrap(ctx context.Context, store BootstrapStore, username, password string) (created bool, err error) {
	n, err := store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if password == "" {
		return false, ErrNoAdminCredential
	}
	if username == "" {
		username = "admin"
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}
	id, err := store.CreateUser(ctx, username, hash, true)
	if err != nil {
		return false, err
	}
	if err := store.AssignRoleByName(ctx, id, RoleAdmin); err != nil {
		return false, err
	}
	return true, nil
}
