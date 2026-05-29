package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Settings keys (see migration 006_access_mgmt.sql).
const (
	settingAutoDeriveEnabled  = "access.autoderive.enabled"
	settingAutoDeriveLicenses = "access.autoderive.licenses"
)

// GetSetting returns the value for key. ok is false (no error) when unset.
func (db *DB) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts a key/value setting.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AutoDerivePolicy is the opt-in license->guest_playable auto-publish setting
// (auth.md §5.1). When Enabled, any file whose license is in Licenses and which
// has not been manually overridden gets guest_playable granted.
type AutoDerivePolicy struct {
	Enabled  bool
	Licenses []string
}

// GetAutoDerivePolicy reads the current auto-derivation policy. A missing
// setting reads as disabled with an empty allow-list.
func (db *DB) GetAutoDerivePolicy(ctx context.Context) (AutoDerivePolicy, error) {
	var p AutoDerivePolicy
	enabled, _, err := db.GetSetting(ctx, settingAutoDeriveEnabled)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == "1"
	licenses, _, err := db.GetSetting(ctx, settingAutoDeriveLicenses)
	if err != nil {
		return p, err
	}
	p.Licenses = splitLicenses(licenses)
	return p, nil
}

// SetAutoDerivePolicy persists the auto-derivation policy. It does not itself
// apply the policy to existing files — callers use ApplyAutoDerive for that.
func (db *DB) SetAutoDerivePolicy(ctx context.Context, p AutoDerivePolicy) error {
	val := "0"
	if p.Enabled {
		val = "1"
	}
	if err := db.SetSetting(ctx, settingAutoDeriveEnabled, val); err != nil {
		return err
	}
	return db.SetSetting(ctx, settingAutoDeriveLicenses, strings.Join(normalizeLicenses(p.Licenses), ","))
}

// ApplyAutoDerive grants guest_playable to every file whose license is on the
// allow-list and which has not been manually overridden. It only ever grants
// (never revokes) and is a no-op when the policy is disabled. Returns the number
// of files newly granted.
func (db *DB) ApplyAutoDerive(ctx context.Context) (int64, error) {
	p, err := db.GetAutoDerivePolicy(ctx)
	if err != nil {
		return 0, err
	}
	return db.applyAutoDerive(ctx, p, "")
}

// autoDeriveGuest applies the auto-derivation policy to a single file (by hash),
// used after a license change. Returns whether the file was granted.
func (db *DB) autoDeriveGuest(ctx context.Context, hash string) (bool, error) {
	p, err := db.GetAutoDerivePolicy(ctx)
	if err != nil {
		return false, err
	}
	n, err := db.applyAutoDerive(ctx, p, hash)
	return n > 0, err
}

// applyAutoDerive runs the grant UPDATE for the given policy. When hash is
// non-empty the update is scoped to that one file.
func (db *DB) applyAutoDerive(ctx context.Context, p AutoDerivePolicy, hash string) (int64, error) {
	if !p.Enabled || len(p.Licenses) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(p.Licenses))
	args := make([]any, 0, len(p.Licenses)+1)
	for i, lic := range p.Licenses {
		placeholders[i] = "?"
		args = append(args, lic)
	}
	q := `UPDATE files SET guest_playable = 1
	      WHERE guest_playable = 0 AND guest_playable_manual = 0
	        AND license IN (` + strings.Join(placeholders, ",") + `)`
	if hash != "" {
		q += ` AND hash = ?`
		args = append(args, hash)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// splitLicenses parses a stored comma-separated allow-list into a slice.
func splitLicenses(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return normalizeLicenses(strings.Split(s, ","))
}

// normalizeLicenses trims whitespace and drops empty entries.
func normalizeLicenses(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
