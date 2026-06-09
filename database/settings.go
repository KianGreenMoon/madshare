package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Settings keys (see migration 006_access_mgmt.sql; settings is a generic
// key/value table, so new keys need no migration).
const (
	settingAutoDeriveEnabled  = "access.autoderive.enabled"
	settingAutoDeriveLicenses = "access.autoderive.licenses"
	settingTrashRestorePolicy = "upload.trash_restore_policy"
)

// Trash-restore policy modes — what may happen to a trashed file whose content
// is uploaded again. See docs/plans/upload-rework.md §3b.
const (
	TrashReuploadRestores = "reupload_restores" // default: re-uploading the bytes restores it (historical behavior)
	TrashInform           = "inform"            // don't restore; tell the uploader to ask an admin
	TrashUploaderRestore  = "uploader_restore"  // an uploader may restore via POST /api/files/{hash}/restore
)

// ValidTrashRestorePolicy reports whether m is a known policy mode.
func ValidTrashRestorePolicy(m string) bool {
	switch m {
	case TrashReuploadRestores, TrashInform, TrashUploaderRestore:
		return true
	}
	return false
}

// GetTrashRestorePolicy reads the trash-restore policy. A missing or unrecognised
// value reads as the default (reupload_restores — the historical behavior).
func (db *DB) GetTrashRestorePolicy(ctx context.Context) (string, error) {
	v, ok, err := db.GetSetting(ctx, settingTrashRestorePolicy)
	if err != nil {
		return TrashReuploadRestores, err
	}
	if !ok || !ValidTrashRestorePolicy(v) {
		return TrashReuploadRestores, nil
	}
	return v, nil
}

// SetTrashRestorePolicy persists the trash-restore policy.
func (db *DB) SetTrashRestorePolicy(ctx context.Context, mode string) error {
	if !ValidTrashRestorePolicy(mode) {
		return errors.New("invalid trash restore policy")
	}
	return db.SetSetting(ctx, settingTrashRestorePolicy, mode)
}

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

// AutoDerivePolicy is the opt-in license-based guest-access setting (auth.md
// §5.1). When Enabled, files whose license is in Licenses are guest-accessible
// via a live query-time check in accessClause (no writes to guest_playable).
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

// SetAutoDerivePolicy persists the auto-derivation policy atomically. Access
// derived from the policy takes effect immediately at query time via accessClause.
func (db *DB) SetAutoDerivePolicy(ctx context.Context, p AutoDerivePolicy) error {
	val := "0"
	if p.Enabled {
		val = "1"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, upsert, settingAutoDeriveEnabled, val); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, upsert, settingAutoDeriveLicenses, strings.Join(normalizeLicenses(p.Licenses), ",")); err != nil {
		return err
	}
	return tx.Commit()
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
