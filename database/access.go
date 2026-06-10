package database

import (
	"context"
	"database/sql"
)

// licenseClause is the SQL predicate (no bind args) that is true when a file's
// license matches the live auto-derive policy. Uses INSTR for exact substring
// matching — no LIKE wildcards, so license strings with '%' or '_' are safe.
// guest_playable_manual = 0 guards it: an explicit admin decision always wins.
var licenseClause = `(
  f.guest_playable_manual = 0
  AND f.license IS NOT NULL AND f.license != ''
  AND EXISTS (SELECT 1 FROM settings WHERE key = '` + settingAutoDeriveEnabled + `' AND value = '1')
  AND INSTR(',' || COALESCE((SELECT value FROM settings WHERE key = '` + settingAutoDeriveLicenses + `'), '') || ',',
            ',' || f.license || ',') > 0
)`

// guestAccessibleExpr is the SQL expression (for SELECT lists) that reflects
// whether a file is effectively guest-accessible — either explicitly granted or
// via the live license policy. No bind args. Yields 0 or 1 in SQLite.
var guestAccessibleExpr = `(f.guest_playable = 1 OR ` + licenseClause + `)`

// accessClause is the SQL predicate (reused by the guest listing filters) that
// decides whether the file aliased `f` is reachable without a content
// capability — i.e. by an anonymous or capability-less request. It is the
// guest-playable / license policy only and takes no bind parameters. Callers
// holding content.access bypass it and use the unfiltered listings.
var accessClause = `(
  f.guest_playable = 1
  OR ` + licenseClause + `
)`

// FileAccessibleByHash reports whether an anonymous / capability-less request
// may play/download the file with the given content hash (the guest-playable /
// license policy). It returns false for unknown hashes, for soft-deleted
// (trashed) files, and for files pending review. Callers must short-circuit
// this for identities holding the content.access permission, which may reach
// any live approved file.
func (db *DB) FileAccessibleByHash(ctx context.Context, hash string) (bool, error) {
	var ok bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM files f
			WHERE f.hash = ? AND `+visibleFile+` AND `+accessClause+`
		)`, hash).Scan(&ok)
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
