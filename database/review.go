package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// reviewEntryColumns is the shared SELECT list for the two staging listings.
// Keep in sync with scanReviewEntry.
// reviewJoins adds the tech row plus the artist/album cover-presence joins
// reviewEntryColumns depends on. Appended to each staging query's FROM (after
// the tagsets join aliased m) so the shared column list can reference
// mm/aimg/alimg.
const reviewJoins = `
	LEFT JOIN media_metadata mm ON mm.file_id = f.id
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

const reviewEntryColumns = `
	f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
	COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
	COALESCE(m.title, '') AS title,
	m.artist, m.album, m.album_artist, m.track_number, m.disc_number, m.year, mm.duration_seconds,
	m.review_state, m.review_note, m.submitted_at, m.created_by,
	CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
	CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image`

func scanReviewEntry(rows *sql.Rows, withUploader bool) (*ReviewEntry, error) {
	var e ReviewEntry
	var artistImg, albumImg int
	dest := []any{
		&e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
		&e.Filename, &e.Title, &e.Artist, &e.Album, &e.AlbumArtist, &e.TrackNumber, &e.DiscNumber, &e.Year, &e.DurationSeconds,
		&e.ReviewState, &e.ReviewNote, &e.SubmittedAt, &e.UploaderID,
		&artistImg, &albumImg,
	}
	if withUploader {
		dest = append(dest, &e.UploaderName)
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf("scan review entry: %w", err)
	}
	e.ArtistHasImage = artistImg == 1
	e.AlbumHasImage = albumImg == 1
	return &e, nil
}

// ReviewFilter narrows a staging listing or bulk operation. OwnerID (when
// non-zero) restricts to one uploader's files (the My-uploads scope). States,
// when non-empty, additionally restricts to those review states (on top of the
// implicit "not approved") — used to scope a count or a bulk resolver to just
// the actionable/selectable subset (submitted for moderation; draft+returned for
// My-uploads). Q / QField are the same search as the file listing (qFieldClause).
type ReviewFilter struct {
	OwnerID int64
	States  []string
	Q       string
	QField  string
}

// ReviewListQuery is a ReviewFilter plus presentation: an allow-listed sort token
// (reviewSortOrder) and a page window. Limit <= 0 means "no limit" (every match);
// Offset < 0 clamps to 0.
type ReviewListQuery struct {
	ReviewFilter
	Sort   string
	Limit  int
	Offset int
}

// reviewSortOrder maps a sort token to a safe ORDER BY for the staging listings.
// "uploader" requires the users join (pending-review only); "state" / "grouped"
// and the shared flat orders work for both. Unknown tokens fall back to
// newest-first. Every order ends with f.id so paging is stable across ties.
func reviewSortOrder(token string) string {
	switch token {
	case "uploader":
		return "u.username IS NULL, LOWER(u.username), f.created_at DESC, f.id DESC"
	case "state":
		return "CASE m.review_state WHEN 'returned' THEN 0 WHEN 'draft' THEN 1 ELSE 2 END, f.created_at DESC, f.id DESC"
	case "created_asc":
		return "f.created_at ASC, f.id ASC"
	case "grouped":
		return groupedSortOrder
	case "title_asc", "title_desc", "artist_asc", "artist_desc", "size_asc", "size_desc", "untagged_first":
		return fileSortOrder(token)
	default: // created_desc
		return "f.created_at DESC, f.id DESC"
	}
}

// appendReviewFilter adds the optional state restriction and the search predicate
// (with binds) onto a base WHERE + its base args. The base carries the scope's
// fixed predicate (non-approved, non-trashed, optionally one owner).
func appendReviewFilter(where string, args []any, f ReviewFilter) (string, []any) {
	if len(f.States) > 0 {
		where += " AND m.review_state IN (?" + strings.Repeat(",?", len(f.States)-1) + ")"
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	if frag, fragArgs := qFieldClause(f.Q, f.QField); frag != "" {
		where += " AND " + frag
		args = append(args, fragArgs...)
	}
	return where, args
}

// pendingReviewBase / uploadsBase are the scope predicates the staging queries
// share between their page, count, and hash-resolver forms. The lifecycle lives
// on the tagset (aliased m): non-trashed, in a non-approved state, owned — for
// the My-uploads scope — by the tagset's creator.
const pendingReviewBase = "m.deleted_at IS NULL AND m.review_state <> 'approved'"
const uploadsBase = "m.created_by = ? AND m.deleted_at IS NULL AND m.review_state <> 'approved'"

// reviewFrom is the staging queries' shared FROM: the files row with its
// offered tagset (alias m — the reviewable unit).
const reviewFrom = `
		FROM files f
		JOIN tagsets m ON m.origin_file_id = f.id`

func scanReviewRows(rows *sql.Rows, withUploader bool) ([]*ReviewEntry, error) {
	out := make([]*ReviewEntry, 0)
	for rows.Next() {
		e, err := scanReviewEntry(rows, withUploader)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("review rows: %w", err)
	}
	return out, nil
}

func scanHashes(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan review hash: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func pageWindow(sqlText string, args []any, limit, offset int) (string, []any) {
	if limit > 0 {
		if offset < 0 {
			offset = 0
		}
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	return sqlText, args
}

// ListUploadsByUser returns the user's staged files — non-trashed rows in any
// review state but approved — newest first. Non-paged wrapper over
// ListUploadsByUserPage; backs the "My uploads" tab's legacy callers.
func (db *DB) ListUploadsByUser(ctx context.Context, userID int64) ([]*ReviewEntry, error) {
	return db.ListUploadsByUserPage(ctx, ReviewListQuery{ReviewFilter: ReviewFilter{OwnerID: userID}, Sort: "created_desc"})
}

// ListUploadsByUserPage returns one filtered, sorted page of the user's staged
// files. With Limit <= 0 it returns every match (no window).
func (db *DB) ListUploadsByUserPage(ctx context.Context, q ReviewListQuery) ([]*ReviewEntry, error) {
	where, args := appendReviewFilter(uploadsBase, []any{q.OwnerID}, q.ReviewFilter)
	sqlText := `
		SELECT ` + reviewEntryColumns + reviewFrom + reviewJoins + `
		WHERE ` + where + `
		ORDER BY ` + reviewSortOrder(q.Sort)
	sqlText, args = pageWindow(sqlText, args, q.Limit, q.Offset)
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list uploads by user: %w", err)
	}
	defer rows.Close()
	return scanReviewRows(rows, false)
}

// CountUploadsByUser counts the user's staged files matching the filter, ignoring
// paging. Set States to scope the total to a subset (e.g. the selectable count).
func (db *DB) CountUploadsByUser(ctx context.Context, f ReviewFilter) (int, error) {
	where, args := appendReviewFilter(uploadsBase, []any{f.OwnerID}, f)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+reviewFrom+` WHERE `+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count uploads by user: %w", err)
	}
	return n, nil
}

// UploadHashesByUserFilter returns the owner's matching staged hashes (ordered by
// id) for "select all N matching". Callers set States to the editable/selectable
// set (draft + returned) so a bulk action never targets a submitted file.
func (db *DB) UploadHashesByUserFilter(ctx context.Context, f ReviewFilter) ([]string, error) {
	where, args := appendReviewFilter(uploadsBase, []any{f.OwnerID}, f)
	rows, err := db.QueryContext(ctx,
		`SELECT f.hash`+reviewFrom+` WHERE `+where+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("upload hashes by filter: %w", err)
	}
	defer rows.Close()
	return scanHashes(rows)
}

// ListPendingReview returns every staged (non-trashed, non-approved) file with
// its uploader's name, ordered for by-uploader grouping in the moderation queue.
// Non-paged wrapper over ListPendingReviewPage.
func (db *DB) ListPendingReview(ctx context.Context) ([]*ReviewEntry, error) {
	return db.ListPendingReviewPage(ctx, ReviewListQuery{Sort: "uploader"})
}

// ListPendingReviewPage returns one filtered, sorted page of the moderation
// queue (with uploader name). With Limit <= 0 it returns every match (no window).
func (db *DB) ListPendingReviewPage(ctx context.Context, q ReviewListQuery) ([]*ReviewEntry, error) {
	where, args := appendReviewFilter(pendingReviewBase, nil, q.ReviewFilter)
	sqlText := `
		SELECT ` + reviewEntryColumns + `, u.username` + reviewFrom + reviewJoins + `
		LEFT JOIN users u ON u.id = m.created_by
		WHERE ` + where + `
		ORDER BY ` + reviewSortOrder(q.Sort)
	sqlText, args = pageWindow(sqlText, args, q.Limit, q.Offset)
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending review: %w", err)
	}
	defer rows.Close()
	return scanReviewRows(rows, true)
}

// CountPendingReview counts the staged files matching the filter, ignoring
// paging. Set States to scope the total to a subset (e.g. the selectable count).
func (db *DB) CountPendingReview(ctx context.Context, f ReviewFilter) (int, error) {
	where, args := appendReviewFilter(pendingReviewBase, nil, f)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+reviewFrom+` WHERE `+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending review: %w", err)
	}
	return n, nil
}

// PendingReviewHashesByFilter returns the matching staged hashes (ordered by id)
// for "select all N matching". Callers set States to the actionable/selectable
// set (submitted) so a bulk action never targets a draft or returned file.
func (db *DB) PendingReviewHashesByFilter(ctx context.Context, f ReviewFilter) ([]string, error) {
	where, args := appendReviewFilter(pendingReviewBase, nil, f)
	rows, err := db.QueryContext(ctx,
		`SELECT f.hash`+reviewFrom+` WHERE `+where+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("pending review hashes by filter: %w", err)
	}
	defer rows.Close()
	return scanHashes(rows)
}

// ReviewTransition describes one guarded review-state change on the tagset.
// The update only applies when the tagset is non-trashed, its current state is
// in From, and — when OwnerID is non-zero — it was offered by that user.
// To==returned stores Note; every other target state clears the note.
type ReviewTransition struct {
	From []string
	To   string
	// Note is the moderator's message; stored only when To is returned.
	Note string
	// OwnerID, when non-zero, restricts the transition to files uploaded by
	// that user (the uploader-facing submit path).
	OwnerID int64
	// StampSubmittedAt records the transition time in submitted_at (set on
	// submit, including the trusted self-approve that skips the queue).
	StampSubmittedAt bool
}

// UpdateReviewState applies a guarded state transition to the tagset of the
// file with the given content hash. The guard and update are a single UPDATE,
// so concurrent transitions cannot double-apply. found is false (no error) when
// no row satisfies the guard (unknown hash, trashed, wrong state, or wrong
// owner).
func (db *DB) UpdateReviewState(ctx context.Context, hash string, t ReviewTransition) (bool, error) {
	if len(t.From) == 0 {
		return false, errors.New("update review state: empty From set")
	}
	var note sql.NullString
	if t.To == ReviewReturned && t.Note != "" {
		note = sql.NullString{String: t.Note, Valid: true}
	}

	args := []any{t.To, note}
	q := `UPDATE tagsets SET review_state = ?, review_note = ?`
	if t.StampSubmittedAt {
		q += `, submitted_at = ?`
		args = append(args, time.Now().Unix())
	}
	q += ` WHERE origin_file_id IN (SELECT id FROM files WHERE hash = ?)
	   AND deleted_at IS NULL AND review_state IN (?` +
		strings.Repeat(",?", len(t.From)-1) + `)`
	args = append(args, hash)
	for _, s := range t.From {
		args = append(args, s)
	}
	if t.OwnerID != 0 {
		q += ` AND created_by = ?`
		args = append(args, t.OwnerID)
	}

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("update review state: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BulkUpdateReviewState applies one guarded review-state transition to a set of
// hashes in a single transaction, chunked to stay within SQLite's bound-parameter
// limit. It is the batch counterpart to UpdateReviewState — same per-row guard
// (non-trashed, current state in From, owner matching when OwnerID is set) — so
// rows that don't satisfy the guard are simply skipped, and the returned count is
// how many actually transitioned. Replacing the per-hash loop (one autocommit
// write plus a per-row audit each) with one transaction is the SQLITE_BUSY fix the
// bulk soft-delete/restore paths already use.
func (db *DB) BulkUpdateReviewState(ctx context.Context, hashes []string, t ReviewTransition) (int, error) {
	if len(t.From) == 0 {
		return 0, errors.New("bulk update review state: empty From set")
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	var note sql.NullString
	if t.To == ReviewReturned && t.Note != "" {
		note = sql.NullString{String: t.Note, Valid: true}
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

		// SET-clause args first, then the guard args (From states, optional owner),
		// then the hash batch — the order the placeholders appear in the query.
		set := `review_state = ?, review_note = ?`
		args := []any{t.To, note}
		if t.StampSubmittedAt {
			set += `, submitted_at = ?`
			args = append(args, time.Now().Unix())
		}
		where := `deleted_at IS NULL AND review_state IN (?` + strings.Repeat(",?", len(t.From)-1) + `)`
		for _, s := range t.From {
			args = append(args, s)
		}
		if t.OwnerID != 0 {
			where += ` AND created_by = ?`
			args = append(args, t.OwnerID)
		}
		placeholders := make([]string, len(batch))
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET `+set+` WHERE `+where+
				` AND origin_file_id IN (SELECT id FROM files WHERE hash IN (`+strings.Join(placeholders, ",")+`))`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk update review state: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

// DiscardOwnUpload soft-deletes one of the owner's *editable* staged files
// (draft or returned) — the "Remove" action on the My-uploads tab. Submitted
// files are excluded (no withdraw once sent to approval) and so is anything
// the caller doesn't own; both report found=false. Guard and delete are one
// UPDATE, so a concurrent submit cannot slip through.
func (db *DB) DiscardOwnUpload(ctx context.Context, hash string, ownerID int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE tagsets SET deleted_at = ?
		WHERE origin_file_id IN (SELECT id FROM files WHERE hash = ?)
		  AND deleted_at IS NULL AND created_by = ?
		  AND review_state IN (?, ?)`,
		time.Now().Unix(), hash, ownerID, ReviewDraft, ReviewReturned)
	if err != nil {
		return false, fmt.Errorf("discard own upload: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BulkDiscardOwnUploads soft-deletes a set of the owner's editable (draft or
// returned) staged files in a single transaction, chunked to stay within SQLite's
// bound-parameter limit. It is the batch counterpart to DiscardOwnUpload — same
// guard (non-trashed, owned, draft/returned) — so submitted, foreign, and unknown
// hashes are skipped, and the returned count is how many were removed. One
// transaction instead of one autocommit write (plus a per-row audit) per hash,
// the SQLITE_BUSY fix the other bulk paths use.
func (db *DB) BulkDiscardOwnUploads(ctx context.Context, hashes []string, ownerID int64) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	total := 0
	const chunk = 400
	for i := 0; i < len(hashes); i += chunk {
		end := i + chunk
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]
		args := make([]any, 0, len(batch)+4)
		args = append(args, now, ownerID, ReviewDraft, ReviewReturned)
		placeholders := make([]string, len(batch))
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = ? WHERE deleted_at IS NULL AND created_by = ?
			   AND review_state IN (?, ?)
			   AND origin_file_id IN (SELECT id FROM files WHERE hash IN (`+strings.Join(placeholders, ",")+`))`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk discard own uploads: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

// StageRestoredFile re-stages a just-restored file as the restorer's draft:
// an approved-then-trashed file brought back by a re-upload (or an uploader
// restore) must re-enter the staging pipeline, not the live library — anyone
// with file.upload could otherwise republish any trashed file by re-sending
// its bytes. Ownership (the tagset's created_by, mirrored to files.uploaded_by)
// moves to the restorer so the file lands in *their* "My uploads" tab. Tagsets
// trashed while pending are left untouched (their state already re-enters the
// queue) — hence the review_state guard. Returns whether a row was re-staged.
func (db *DB) StageRestoredFile(ctx context.Context, hash string, ownerID sql.NullInt64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("stage restored file: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE tagsets
		SET review_state = ?, review_note = NULL, submitted_at = NULL,
		    created_by = COALESCE(?, created_by)
		WHERE origin_file_id IN (SELECT id FROM files WHERE hash = ?)
		  AND deleted_at IS NULL AND review_state = ?`,
		ReviewDraft, ownerID, hash, ReviewApproved)
	if err != nil {
		return false, fmt.Errorf("stage restored file: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Keep the file's upload attribution in step with the tagset owner so
		// the blob-access gate (FileReviewInfo) recognises the restorer.
		if _, err := tx.ExecContext(ctx, `
			UPDATE files SET uploaded_by = COALESCE(?, uploaded_by) WHERE hash = ?`,
			ownerID, hash); err != nil {
			return false, fmt.Errorf("stage restored file: attribution: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("stage restored file: commit: %w", err)
	}
	return n > 0, nil
}

// FileReviewInfo returns the review state, uploader, and trash flag for the
// file with the given content hash (state and trash read from its tagset) —
// the narrow lookup used by the blob-access gate and ownership checks. found
// is false (no error) on unknown hashes.
func (db *DB) FileReviewInfo(ctx context.Context, hash string) (state string, uploadedBy sql.NullInt64, deleted bool, found bool, err error) {
	var deletedAt sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(t.review_state, 'approved'), f.uploaded_by, t.deleted_at
		FROM files f
		LEFT JOIN tagsets t ON t.origin_file_id = f.id
		WHERE f.hash = ?
		ORDER BY t.id
		LIMIT 1`, hash).
		Scan(&state, &uploadedBy, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.NullInt64{}, false, false, nil
	}
	if err != nil {
		return "", sql.NullInt64{}, false, false, fmt.Errorf("file review info: %w", err)
	}
	return state, uploadedBy, deletedAt.Valid, true, nil
}
