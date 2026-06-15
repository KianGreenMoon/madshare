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
// reviewJoins adds the artist/album cover-presence joins reviewEntryColumns
// depends on. Appended to each staging query's FROM (after the media_metadata
// join) so the shared column list can reference aimg/alimg.
const reviewJoins = `
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

const reviewEntryColumns = `
	f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
	COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
	COALESCE(m.title, '') AS title,
	m.artist, m.album, m.album_artist, m.track_number, m.disc_number, m.year, m.duration_seconds,
	f.review_state, f.review_note, f.submitted_at, f.uploaded_by,
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

// ListUploadsByUser returns the user's staged files — non-trashed rows in any
// review state but approved — newest first. This backs the "My uploads" tab.
func (db *DB) ListUploadsByUser(ctx context.Context, userID int64) ([]*ReviewEntry, error) {
	q := `
		SELECT ` + reviewEntryColumns + `
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id` + reviewJoins + `
		WHERE f.uploaded_by = ? AND f.deleted_at IS NULL AND f.review_state <> 'approved'
		ORDER BY f.created_at DESC`

	rows, err := db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list uploads by user: %w", err)
	}
	defer rows.Close()

	out := make([]*ReviewEntry, 0)
	for rows.Next() {
		e, err := scanReviewEntry(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list uploads rows: %w", err)
	}
	return out, nil
}

// ListPendingReview returns every staged (non-trashed, non-approved) file with
// its uploader's name, ordered for by-uploader grouping in the moderation
// queue. Files with no recorded uploader (pre-auth rows) sort last.
func (db *DB) ListPendingReview(ctx context.Context) ([]*ReviewEntry, error) {
	q := `
		SELECT ` + reviewEntryColumns + `, u.username
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id` + reviewJoins + `
		LEFT JOIN users u ON u.id = f.uploaded_by
		WHERE f.deleted_at IS NULL AND f.review_state <> 'approved'
		ORDER BY u.username IS NULL, LOWER(u.username), f.created_at DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list pending review: %w", err)
	}
	defer rows.Close()

	out := make([]*ReviewEntry, 0)
	for rows.Next() {
		e, err := scanReviewEntry(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending review rows: %w", err)
	}
	return out, nil
}

// ReviewTransition describes one guarded review-state change. The update only
// applies when the file is non-trashed, its current state is in From, and —
// when OwnerID is non-zero — it was uploaded by that user. To==returned stores
// Note; every other target state clears the note.
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

// UpdateReviewState applies a guarded state transition to the file with the
// given content hash. The guard and update are a single UPDATE, so concurrent
// transitions cannot double-apply. found is false (no error) when no row
// satisfies the guard (unknown hash, trashed, wrong state, or wrong owner).
func (db *DB) UpdateReviewState(ctx context.Context, hash string, t ReviewTransition) (bool, error) {
	if len(t.From) == 0 {
		return false, errors.New("update review state: empty From set")
	}
	var note sql.NullString
	if t.To == ReviewReturned && t.Note != "" {
		note = sql.NullString{String: t.Note, Valid: true}
	}

	args := []any{t.To, note}
	q := `UPDATE files SET review_state = ?, review_note = ?`
	if t.StampSubmittedAt {
		q += `, submitted_at = ?`
		args = append(args, time.Now().Unix())
	}
	q += ` WHERE hash = ? AND deleted_at IS NULL AND review_state IN (?` +
		strings.Repeat(",?", len(t.From)-1) + `)`
	args = append(args, hash)
	for _, s := range t.From {
		args = append(args, s)
	}
	if t.OwnerID != 0 {
		q += ` AND uploaded_by = ?`
		args = append(args, t.OwnerID)
	}

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("update review state: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DiscardOwnUpload soft-deletes one of the owner's *editable* staged files
// (draft or returned) — the "Remove" action on the My-uploads tab. Submitted
// files are excluded (no withdraw once sent to approval) and so is anything
// the caller doesn't own; both report found=false. Guard and delete are one
// UPDATE, so a concurrent submit cannot slip through.
func (db *DB) DiscardOwnUpload(ctx context.Context, hash string, ownerID int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE files SET deleted_at = ?
		WHERE hash = ? AND deleted_at IS NULL AND uploaded_by = ?
		  AND review_state IN (?, ?)`,
		time.Now().Unix(), hash, ownerID, ReviewDraft, ReviewReturned)
	if err != nil {
		return false, fmt.Errorf("discard own upload: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StageRestoredFile re-stages a just-restored file as the restorer's draft:
// an approved-then-trashed file brought back by a re-upload (or an uploader
// restore) must re-enter the staging pipeline, not the live library — anyone
// with file.upload could otherwise republish any trashed file by re-sending
// its bytes. Ownership moves to the restorer so the file lands in *their*
// "My uploads" tab. Files trashed while pending are left untouched (their
// state already re-enters the queue) — hence the review_state guard. Returns
// whether a row was re-staged.
func (db *DB) StageRestoredFile(ctx context.Context, hash string, ownerID sql.NullInt64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE files
		SET review_state = ?, review_note = NULL, submitted_at = NULL,
		    uploaded_by = COALESCE(?, uploaded_by)
		WHERE hash = ? AND deleted_at IS NULL AND review_state = ?`,
		ReviewDraft, ownerID, hash, ReviewApproved)
	if err != nil {
		return false, fmt.Errorf("stage restored file: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FileReviewInfo returns the review state, uploader, and trash flag for the
// file with the given content hash — the narrow lookup used by the blob-access
// gate and ownership checks. found is false (no error) on unknown hashes.
func (db *DB) FileReviewInfo(ctx context.Context, hash string) (state string, uploadedBy sql.NullInt64, deleted bool, found bool, err error) {
	var deletedAt sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT review_state, uploaded_by, deleted_at FROM files WHERE hash = ?`, hash).
		Scan(&state, &uploadedBy, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.NullInt64{}, false, false, nil
	}
	if err != nil {
		return "", sql.NullInt64{}, false, false, fmt.Errorf("file review info: %w", err)
	}
	return state, uploadedBy, deletedAt.Valid, true, nil
}
