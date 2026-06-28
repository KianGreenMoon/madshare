package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

// BulkSetGuestPlayable sets the same guest-playable flag on every live file in
// the hash set, in one chunked transaction. A bulk edit carries a single value
// for all files, so this collapses what was a write transaction per file into one
// guarded UPDATE per chunk. Returns the number of live rows changed.
func (db *DB) BulkSetGuestPlayable(ctx context.Context, hashes []string, guest bool) (int, error) {
	return db.bulkSetFileColumn(ctx, hashes,
		`UPDATE files SET guest_playable = ?, guest_playable_manual = 1 WHERE deleted_at IS NULL AND hash IN `,
		boolToInt(guest))
}

// BulkSetLicense sets (or clears, with "") the same license on every live file in
// the hash set, in one chunked transaction — the bulk counterpart to SetLicense.
func (db *DB) BulkSetLicense(ctx context.Context, hashes []string, license string) (int, error) {
	var lic sql.NullString
	if license != "" {
		lic = sql.NullString{String: license, Valid: true}
	}
	return db.bulkSetFileColumn(ctx, hashes,
		`UPDATE files SET license = ? WHERE deleted_at IS NULL AND hash IN `, lic)
}

// bulkSetFileColumn runs a single-value guarded UPDATE (prefix ending in
// `hash IN `) over the hash set in one transaction, chunked to stay within
// SQLite's bound-parameter limit. lead are the SET-clause args that precede the
// hash placeholders. Returns the total rows affected.
func (db *DB) bulkSetFileColumn(ctx context.Context, hashes []string, updatePrefix string, lead ...any) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	total := 0
	const chunk = 400
	for i := 0; i < len(hashes); i += chunk {
		end := i + chunk
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]
		placeholders := make([]string, len(batch))
		args := append([]any{}, lead...)
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		res, err := tx.ExecContext(ctx, updatePrefix+`(`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk set file column: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}
