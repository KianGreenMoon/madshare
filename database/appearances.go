package database

// Appearance-rooted listings: the Trash · Appearances lens (recording-tagsets
// P7c) and the Full Library · All Appearances lens — both one row per
// **appearance**, rooted `FROM tagsets` and addressed by tagset id.
//
// A files-rooted listing joined to each file's representative tagset is one
// row per *file*: an appearance that is not its blob's representative (a
// byte-dup draft, any second appearance) is listed nowhere, and hash-addressed
// row actions act on **every** matching tagset of that blob while the UI shows
// a single row. Rooting on the tagset fixes both.
//
// The two lenses bind `f` differently. Trash reads the **origin blob**
// (`origin_file_id` is provenance; LEFT JOIN — an appearance whose origin was
// absorbed or purged still belongs in Trash, just without preview/size). The
// live lens reads the recording's **ladder-best surviving rendition**, the
// same play resolution as the listening surfaces (LEFT — a dormant recording,
// all renditions removed, still lists its appearances, just unplayable).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AppearanceEntry is one appearance row. Blob-derived fields are zero when no
// blob backs the row (Hash == "" ⇒ no preview, no size): in Trash a purged /
// absorbed origin, in the live lens a dormant recording.
type AppearanceEntry struct {
	TagsetID    int64
	FileID      sql.NullInt64
	Hash        string
	Filename    string
	ByteSize    int64
	ObjectKey   string
	Title       string
	Artist      string
	AlbumArtist sql.NullString
	Album       string
	TrackNumber sql.NullInt64
	DiscNumber  sql.NullInt64
	Year        int64
	CreatedAt   int64
	DeletedAt   sql.NullInt64
	ReviewState string
	RecordingID int64
	// License / GuestPlayable mirror the recording's access (read-only here;
	// access is edited per recording) — the live lens's Access column.
	License       sql.NullString
	GuestPlayable bool
	// ArtistHasImage / AlbumHasImage drive the grouped view's "Add cover" hint.
	ArtistHasImage bool
	AlbumHasImage  bool
}

// trashAppearanceFrom roots the Trash lens on the tagset. `m` is the appearance
// (the row), `r` its recording, `f` the origin blob when one survives. The
// alias names match the shared predicates (qFieldClause reads `m.*` and
// `f.id`), so filtering behaves exactly as on the other file surfaces.
const trashAppearanceFrom = `
	FROM tagsets m
	JOIN recordings r ON r.id = m.recording_id
	LEFT JOIN files f ON f.id = m.origin_file_id
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

// appearanceColumns is the shared SELECT list over the (m, r, f, aimg, alimg)
// aliases — each lens prepends SELECT and appends its own FROM.
const appearanceColumns = `
		m.id, m.origin_file_id,
		COALESCE(f.hash, ''), COALESCE(f.byte_size, 0), COALESCE(f.object_key, ''),
		COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), COALESCE(f.hash, '')) AS filename,
		COALESCE(m.title,  '') AS title,
		COALESCE(m.artist, '') AS artist,
		m.album_artist,
		COALESCE(m.album,  '') AS album,
		m.track_number,
		m.disc_number,
		COALESCE(m.year, 0) AS year,
		m.created_at,
		m.deleted_at,
		m.review_state,
		m.recording_id,
		r.license,
		r.guest_playable,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image`

const trashAppearanceSelect = `
	SELECT` + appearanceColumns + trashAppearanceFrom

func scanAppearances(rows *sql.Rows, err error) ([]*AppearanceEntry, error) {
	if err != nil {
		return nil, fmt.Errorf("list appearances: %w", err)
	}
	defer rows.Close()
	out := make([]*AppearanceEntry, 0)
	for rows.Next() {
		var e AppearanceEntry
		if err := rows.Scan(&e.TagsetID, &e.FileID, &e.Hash, &e.ByteSize, &e.ObjectKey, &e.Filename,
			&e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.DiscNumber, &e.Year, &e.CreatedAt,
			&e.DeletedAt, &e.ReviewState, &e.RecordingID, &e.License, &e.GuestPlayable,
			&e.ArtistHasImage, &e.AlbumHasImage); err != nil {
			return nil, fmt.Errorf("scan appearance: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// appearanceTrashWhere is the lens predicate: the tagset carries the Trash mark.
// It reuses qFieldClause + the artist/album pins so Trash filters identically to
// the live library. Guest is never applied (Trash is admin-only).
func appearanceTrashWhere(f FileFilter) (string, []any) {
	where := "m.deleted_at IS NOT NULL"
	var args []any
	if frag, fragArgs := qFieldClause(f.Q, f.QField); frag != "" {
		where += " AND " + frag
		args = append(args, fragArgs...)
	}
	if f.ArtistID != nil {
		where += " AND (m.album_artist_id = ? OR m.artist_id = ?)"
		args = append(args, *f.ArtistID, *f.ArtistID)
	}
	if f.AlbumID != nil {
		where += " AND m.album_id = ?"
		args = append(args, *f.AlbumID)
	}
	return where, args
}

// appearanceTrashSort maps a sort token to a safe ORDER BY. The tie-break is the
// tagset id (the row identity), not the file id — two appearances can share one
// blob, and a blobless appearance has no file id at all.
func appearanceTrashSort(token string) string {
	switch token {
	case "deleted_asc":
		return "m.deleted_at ASC, m.id ASC"
	case "title_asc":
		return "LOWER(COALESCE(m.title, '')) ASC, m.id ASC"
	case "title_desc":
		return "LOWER(COALESCE(m.title, '')) DESC, m.id DESC"
	case "artist_asc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, m.id ASC"
	case "artist_desc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) DESC, m.id DESC"
	case "size_asc":
		return "COALESCE(f.byte_size, 0) ASC, m.id ASC"
	case "size_desc":
		return "COALESCE(f.byte_size, 0) DESC, m.id DESC"
	case "untagged_first":
		return untaggedExpr + " DESC, m.deleted_at DESC, m.id DESC"
	case "grouped":
		return `LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, LOWER(COALESCE(m.album, '')) ASC,
		        (m.track_number IS NULL) ASC, m.track_number ASC, m.id ASC`
	default: // deleted_desc
		return "m.deleted_at DESC, m.id DESC"
	}
}

// ListTrashedAppearancesPage returns one filtered, sorted page of the lens.
// With Limit <= 0 it returns every match (no window).
func (db *DB) ListTrashedAppearancesPage(ctx context.Context, q FileListQuery) ([]*AppearanceEntry, error) {
	where, args := appearanceTrashWhere(q.FileFilter)
	sqlText := trashAppearanceSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + appearanceTrashSort(q.Sort)
	if q.Limit > 0 {
		offset := max(q.Offset, 0)
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	return scanAppearances(db.QueryContext(ctx, sqlText, args...))
}

// ── Full Library · All Appearances (the live lens) ──────────────────────────

// liveAppearanceFrom roots the live lens on the tagset: `f` is the recording's
// ladder-best surviving rendition (bestRenditionJoin — the same play resolution
// the listening surfaces use), LEFT so a dormant recording's appearances still
// list (unplayable, Hash == ""). The rendition join is bound even for the
// count/ids queries: qFieldClause's filename term reads f.id.
var liveAppearanceFrom = `
	FROM tagsets m` + recordingJoin + bestRenditionJoin(true) + `
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

var liveAppearanceSelect = `
	SELECT` + appearanceColumns + liveAppearanceFrom

// appearanceLiveWhere is the live-lens predicate: approved and not trashed
// (drafts/submissions belong to Review, trashed rows to Trash). Same filter
// fragments as the Trash lens, so search behaves identically.
func appearanceLiveWhere(f FileFilter) (string, []any) {
	where := "m.deleted_at IS NULL AND m.review_state = 'approved'"
	var args []any
	if frag, fragArgs := qFieldClause(f.Q, f.QField); frag != "" {
		where += " AND " + frag
		args = append(args, fragArgs...)
	}
	if f.ArtistID != nil {
		where += " AND (m.album_artist_id = ? OR m.artist_id = ?)"
		args = append(args, *f.ArtistID, *f.ArtistID)
	}
	if f.AlbumID != nil {
		where += " AND m.album_id = ?"
		args = append(args, *f.AlbumID)
	}
	return where, args
}

// appearanceLiveSort maps a sort token to a safe ORDER BY — the Trash mapping
// with the deleted_* tokens swapped for created_* (the live meta column).
func appearanceLiveSort(token string) string {
	switch token {
	case "created_asc":
		return "m.created_at ASC, m.id ASC"
	case "title_asc":
		return "LOWER(COALESCE(m.title, '')) ASC, m.id ASC"
	case "title_desc":
		return "LOWER(COALESCE(m.title, '')) DESC, m.id DESC"
	case "artist_asc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, m.id ASC"
	case "artist_desc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) DESC, m.id DESC"
	case "size_asc":
		return "COALESCE(f.byte_size, 0) ASC, m.id ASC"
	case "size_desc":
		return "COALESCE(f.byte_size, 0) DESC, m.id DESC"
	case "untagged_first":
		// Untagged rows first, then newest — surfaces appearances that still
		// want tags (same needs-metadata rule as the file listing).
		return untaggedExpr + " DESC, m.created_at DESC, m.id DESC"
	case "grouped":
		return `LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, LOWER(COALESCE(m.album, '')) ASC,
		        (m.track_number IS NULL) ASC, m.track_number ASC, m.id ASC`
	default: // created_desc
		return "m.created_at DESC, m.id DESC"
	}
}

// ListAppearancesPage returns one filtered, sorted page of the live lens.
// With Limit <= 0 it returns every match (no window).
func (db *DB) ListAppearancesPage(ctx context.Context, q FileListQuery) ([]*AppearanceEntry, error) {
	where, args := appearanceLiveWhere(q.FileFilter)
	sqlText := liveAppearanceSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + appearanceLiveSort(q.Sort)
	if q.Limit > 0 {
		offset := max(q.Offset, 0)
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	return scanAppearances(db.QueryContext(ctx, sqlText, args...))
}

// CountAppearances returns how many live appearances match the filter,
// ignoring paging — the total for the list and "select all N matching".
func (db *DB) CountAppearances(ctx context.Context, f FileFilter) (int, error) {
	where, args := appearanceLiveWhere(f)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+liveAppearanceFrom+` WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count appearances: %w", err)
	}
	return n, nil
}

// AppearanceIDsByFilter returns the tagset ids of every live appearance
// matching the filter (no paging). Backs "select all N matching".
func (db *DB) AppearanceIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error) {
	where, args := appearanceLiveWhere(f)
	return scanIDs(db.QueryContext(ctx, `SELECT m.id`+liveAppearanceFrom+` WHERE `+where+` ORDER BY m.id`, args...))
}

// CountTrashedAppearances returns how many trashed appearances match the filter,
// ignoring paging — the total for the list and "select all N matching".
func (db *DB) CountTrashedAppearances(ctx context.Context, f FileFilter) (int, error) {
	where, args := appearanceTrashWhere(f)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*)`+trashAppearanceFrom+` WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count trashed appearances: %w", err)
	}
	return n, nil
}

// TrashedAppearanceIDsByFilter returns the tagset ids of every trashed
// appearance matching the filter (no paging). Backs "select all N matching".
func (db *DB) TrashedAppearanceIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error) {
	where, args := appearanceTrashWhere(f)
	return scanIDs(db.QueryContext(ctx, `SELECT m.id`+trashAppearanceFrom+` WHERE `+where+` ORDER BY m.id`, args...))
}

// BulkRestoreTagsets clears the trash mark on the given appearances in a single
// transaction (one write lock, one audit row — the SQLITE_BUSY lesson from the
// hash-addressed bulk paths). Live and unknown ids are skipped, so the count is
// the number actually restored. Each appearance re-enters its prior review
// state, exactly as the single-row RestoreTagset does.
func (db *DB) BulkRestoreTagsets(ctx context.Context, tagsetIDs []int64) (int, error) {
	if len(tagsetIDs) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("bulk restore appearances: begin: %w", err)
	}
	defer tx.Rollback()

	const chunk = 400
	affected := 0
	for i := 0; i < len(tagsetIDs); i += chunk {
		batch := tagsetIDs[i:min(i+chunk, len(tagsetIDs))]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = NULL
			  WHERE deleted_at IS NOT NULL AND id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk restore appearances: %w", err)
		}
		n, _ := res.RowsAffected()
		affected += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("bulk restore appearances: commit: %w", err)
	}
	return affected, nil
}

// BulkHardDeleteTagsets permanently removes the given **trashed** appearances
// through the shared tagset-first cascade (a non-last appearance drops only its
// tagset and keeps the blob; the last one takes the recording and all its
// files). Live and unknown ids are skipped — permanent delete is Trash-only,
// mirroring the single-row HardDeleteTrashedTagset. One transaction; the freed
// blobs come back so the caller can reclaim the bytes after commit.
func (db *DB) BulkHardDeleteTagsets(ctx context.Context, tagsetIDs []int64) (int, []DeletedBlob, error) {
	if len(tagsetIDs) == 0 {
		return 0, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("bulk delete appearances: begin: %w", err)
	}
	defer tx.Rollback()

	// Keep only the ids that are actually in the Trash.
	trashed := make([]int64, 0, len(tagsetIDs))
	const chunk = 400
	for i := 0; i < len(tagsetIDs); i += chunk {
		batch := tagsetIDs[i:min(i+chunk, len(tagsetIDs))]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		ids, err := scanIDs(tx.QueryContext(ctx,
			`SELECT id FROM tagsets WHERE deleted_at IS NOT NULL AND id IN (`+strings.Join(ph, ",")+`)`, args...))
		if err != nil {
			return 0, nil, fmt.Errorf("bulk delete appearances: filter: %w", err)
		}
		trashed = append(trashed, ids...)
	}
	if len(trashed) == 0 {
		return 0, nil, nil
	}
	blobs, err := hardDeleteTagsetsTx(ctx, tx, trashed)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("bulk delete appearances: commit: %w", err)
	}
	return len(trashed), blobs, nil
}

// BulkUpdateTagsetMetadata applies one tag patch to each appearance by tagset
// id — the Trash lens's bulk edit ("fix a tag before restoring"). Unknown ids
// are reported, not fatal. Chunked so one transaction never holds the write
// lock for long.
//
// applyMetadataPatchTagsetTx updates by primary key and reports no error for a
// row that does not exist, so the ids are checked up front rather than inferred
// from the patch — otherwise `affected` would silently count ids that matched
// nothing.
func (db *DB) BulkUpdateTagsetMetadata(ctx context.Context, tagsetIDs []int64, p MetadataPatch) (affected int, notFound []int64, err error) {
	const chunk = 500
	for i := 0; i < len(tagsetIDs); i += chunk {
		batch := tagsetIDs[i:min(i+chunk, len(tagsetIDs))]
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return affected, notFound, fmt.Errorf("bulk update appearances: begin: %w", err)
		}
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		found, err := scanIDs(tx.QueryContext(ctx,
			`SELECT id FROM tagsets WHERE id IN (`+strings.Join(ph, ",")+`)`, args...))
		if err != nil {
			tx.Rollback()
			return affected, notFound, fmt.Errorf("bulk update appearances: lookup: %w", err)
		}
		exists := make(map[int64]struct{}, len(found))
		for _, id := range found {
			exists[id] = struct{}{}
		}
		for _, id := range batch {
			if _, ok := exists[id]; !ok {
				notFound = append(notFound, id)
				continue
			}
			if e := applyMetadataPatchTagsetTx(ctx, tx, id, p); e != nil {
				tx.Rollback()
				return affected, notFound, e
			}
			affected++
		}
		if err := tx.Commit(); err != nil {
			return affected, notFound, fmt.Errorf("bulk update appearances: commit: %w", err)
		}
	}
	return affected, notFound, nil
}
