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

// BulkSetGuestPlayableByTagsets / BulkSetLicenseByTagsets are the bulk access
// setters addressed by **tagset id** — the Full Library · All Appearances
// lens's bulk edit. Access lives on the recording, so each live approved
// appearance in the set forwards the value to its recording; the returned
// count is the number of matched appearances (appearances sharing a recording
// each count, mirroring the hash-addressed setters).
func (db *DB) BulkSetGuestPlayableByTagsets(ctx context.Context, tagsetIDs []int64, guest bool) (int, error) {
	return db.bulkSetRecordingColumnByTagsets(ctx, tagsetIDs,
		`UPDATE recordings SET guest_playable = ?, guest_playable_manual = 1 WHERE id IN `,
		boolToInt(guest))
}

func (db *DB) BulkSetLicenseByTagsets(ctx context.Context, tagsetIDs []int64, license string) (int, error) {
	var lic sql.NullString
	if license != "" {
		lic = sql.NullString{String: license, Valid: true}
	}
	return db.bulkSetRecordingColumnByTagsets(ctx, tagsetIDs,
		`UPDATE recordings SET license = ? WHERE id IN `, lic)
}

// bulkSetRecordingColumnByTagsets runs a single-value guarded UPDATE keyed by tagset id,
// guarded to live approved appearances (a trashed or staged row never edits
// access from this path).
func (db *DB) bulkSetRecordingColumnByTagsets(ctx context.Context, tagsetIDs []int64, updatePrefix string, lead ...any) (int, error) {
	if len(tagsetIDs) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	const liveApproved = `t.deleted_at IS NULL AND t.review_state = 'approved'`
	total := 0
	const chunk = 400
	for i := 0; i < len(tagsetIDs); i += chunk {
		batch := tagsetIDs[i:min(i+chunk, len(tagsetIDs))]
		placeholders := make([]string, len(batch))
		idArgs := make([]any, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = "?"
			idArgs = append(idArgs, id)
		}
		ph := strings.Join(placeholders, ",")

		var matched int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tagsets t WHERE `+liveApproved+` AND t.id IN (`+ph+`)`,
			idArgs...).Scan(&matched); err != nil {
			return 0, fmt.Errorf("bulk set recording column by tagsets: count: %w", err)
		}

		in := `(SELECT DISTINCT t.recording_id FROM tagsets t WHERE ` + liveApproved + ` AND t.id IN (` + ph + `))`
		args := append([]any{}, lead...)
		args = append(args, idArgs...)
		if _, err := tx.ExecContext(ctx, updatePrefix+in, args...); err != nil {
			return 0, fmt.Errorf("bulk set recording column by tagsets: %w", err)
		}
		total += matched
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}
