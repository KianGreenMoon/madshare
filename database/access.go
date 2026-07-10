package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// licenseClause is the SQL predicate (no bind args) that is true when a
// recording's license matches the live auto-derive policy. license and the
// guest flags live on the recording — one audio identity, one license
// (docs/architecture/recording-tagsets.md, decision 9) — so callers must have
// the recordings join aliased `r` (see tagsetJoin). Uses INSTR for exact
// substring matching — no LIKE wildcards, so license strings with '%' or '_'
// are safe. guest_playable_manual = 0 guards it: an explicit admin decision
// always wins.
var licenseClause = `(
  r.guest_playable_manual = 0
  AND r.license IS NOT NULL AND r.license != ''
  AND EXISTS (SELECT 1 FROM settings WHERE key = '` + settingAutoDeriveEnabled + `' AND value = '1')
  AND INSTR(',' || COALESCE((SELECT value FROM settings WHERE key = '` + settingAutoDeriveLicenses + `'), '') || ',',
            ',' || r.license || ',') > 0
)`

// guestAccessibleExpr is the SQL expression (for SELECT lists) that reflects
// whether a track's recording is effectively guest-accessible — either
// explicitly granted or via the live license policy. No bind args. Yields 0 or
// 1 in SQLite.
var guestAccessibleExpr = `(r.guest_playable = 1 OR ` + licenseClause + `)`

// accessClause is the SQL predicate (reused by the guest listing filters) that
// decides whether the recording aliased `r` is reachable without a content
// capability — i.e. by an anonymous or capability-less request. It is the
// guest-playable / license policy only and takes no bind parameters. Callers
// holding content.access bypass it and use the unfiltered listings.
var accessClause = `(
  r.guest_playable = 1
  OR ` + licenseClause + `
)`

// FileAccessibleByHash reports whether an anonymous / capability-less request
// may play/download the blob with the given content hash. The gate is
// recording-level (recording-tagsets P1): the blob serves publicly iff it is a
// surviving rendition of a recording with at least one approved, non-trashed
// appearance AND the recording passes the guest-playable / license policy.
// Callers must short-circuit this for identities holding the content.access
// permission, which may reach any live approved blob.
func (db *DB) FileAccessibleByHash(ctx context.Context, hash string) (bool, error) {
	var ok bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM files f
			JOIN recordings r ON r.id = f.recording_id
			WHERE f.hash = ? AND f.deleted_at IS NULL
			  AND EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = r.id
			                AND t.deleted_at IS NULL AND t.review_state = 'approved')
			  AND `+accessClause+`
		)`, hash).Scan(&ok)
	return ok, err
}

// liveFileRecordingSubquery resolves a live (non-trashed) file's recording id —
// the shared guard for the access setters, addressed by content hash. "Live"
// means the file's *recording* still has a non-trashed appearance, the same
// rule FileAccessibleByHash enforces below: a rendition that no tagset was read
// from (appearance dedup dropped it — merge, absorb) is still a rendition of a
// live recording, so its hash must reach the setters (recording-tagsets P7).
const liveFileRecordingSubquery = `
	SELECT f.recording_id FROM files f
	WHERE EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = f.recording_id AND t.deleted_at IS NULL)`

// SetGuestPlayable sets the guest-playable flag on the recording of the file
// with the given hash. Any explicit set is a manual decision
// (guest_playable_manual = 1), so the auto-derivation policy will never
// override it (auth.md §5.1). found is false (no error) when no live file
// matches.
func (db *DB) SetGuestPlayable(ctx context.Context, hash string, guest bool) (found bool, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE recordings SET guest_playable = ?, guest_playable_manual = 1
		  WHERE id IN (`+liveFileRecordingSubquery+` AND f.hash = ?)`,
		boolToInt(guest), hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetLicense sets (or clears, with "") the license metadata on the recording of
// the file with the given hash. Access derived from the license is evaluated
// live at query time via accessClause; this function only stores the metadata.
func (db *DB) SetLicense(ctx context.Context, hash, license string) (found bool, err error) {
	var lic sql.NullString
	if license != "" {
		lic = sql.NullString{String: license, Valid: true}
	}
	res, err := db.ExecContext(ctx,
		`UPDATE recordings SET license = ?
		  WHERE id IN (`+liveFileRecordingSubquery+` AND f.hash = ?)`, lic, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BulkSetGuestPlayable sets the same guest-playable flag on the recording of
// every live file in the hash set, in one chunked transaction. A bulk edit
// carries a single value for all files, so this collapses what was a write
// transaction per file into one guarded UPDATE per chunk. Returns the number of
// live files whose recording was addressed (renditions sharing a recording each
// count, matching the per-file history).
func (db *DB) BulkSetGuestPlayable(ctx context.Context, hashes []string, guest bool) (int, error) {
	return db.bulkSetRecordingColumn(ctx, hashes,
		`UPDATE recordings SET guest_playable = ?, guest_playable_manual = 1 WHERE id IN `,
		boolToInt(guest))
}

// BulkSetLicense sets (or clears, with "") the same license on the recording of
// every live file in the hash set, in one chunked transaction — the bulk
// counterpart to SetLicense.
func (db *DB) BulkSetLicense(ctx context.Context, hashes []string, license string) (int, error) {
	var lic sql.NullString
	if license != "" {
		lic = sql.NullString{String: license, Valid: true}
	}
	return db.bulkSetRecordingColumn(ctx, hashes,
		`UPDATE recordings SET license = ? WHERE id IN `, lic)
}

// bulkSetRecordingColumn runs a single-value guarded UPDATE (prefix ending in
// `id IN `) over the recordings of the live files in the hash set, in one
// transaction, chunked to stay within SQLite's bound-parameter limit. lead are
// the SET-clause args that precede the hash placeholders. Returns the number of
// matched live files (not recordings), so the reported count matches the
// selection the caller acted on.
func (db *DB) bulkSetRecordingColumn(ctx context.Context, hashes []string, updatePrefix string, lead ...any) (int, error) {
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
		end := min(i+chunk, len(hashes))
		batch := hashes[i:end]
		placeholders := make([]string, len(batch))
		hashArgs := make([]any, 0, len(batch))
		for j, h := range batch {
			placeholders[j] = "?"
			hashArgs = append(hashArgs, h)
		}
		in := `(` + liveFileRecordingSubquery + ` AND f.hash IN (` + strings.Join(placeholders, ",") + `))`

		// Count the matched files first: the UPDATE's RowsAffected counts
		// recordings, which undercounts when two selected renditions share one.
		var matched int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM files f
			  WHERE EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = f.recording_id AND t.deleted_at IS NULL)
			    AND f.hash IN (`+strings.Join(placeholders, ",")+`)`, hashArgs...).Scan(&matched); err != nil {
			return 0, fmt.Errorf("bulk set recording column: count: %w", err)
		}

		args := append([]any{}, lead...)
		args = append(args, hashArgs...)
		if _, err := tx.ExecContext(ctx, updatePrefix+in, args...); err != nil {
			return 0, fmt.Errorf("bulk set recording column: %w", err)
		}
		total += matched
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}
