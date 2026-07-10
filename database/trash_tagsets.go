package database

// Trash · Appearances lens — tagset-rooted (recording-tagsets P7c).
//
// The lens lists **appearances**, so it is rooted `FROM tagsets`. The previous
// implementation was rooted `FROM files` and joined each file's representative
// tagset, which made it one row per *file*: a trashed appearance that was not
// its blob's representative (a byte-dup draft, any second appearance) was
// listed nowhere, and the hash-addressed row actions acted on **every** trashed
// tagset of that blob while the UI showed a single row.
//
// `origin_file_id` is provenance, so the blob join is a LEFT JOIN: an
// appearance whose origin blob was absorbed or purged still belongs in Trash,
// just without preview/size. Everything here is addressed by tagset id.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TrashEntry is one trashed appearance. Blob-derived fields are zero when the
// appearance has no surviving origin blob (Hash == "" ⇒ no preview, no size).
type TrashEntry struct {
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
	Year        int64
	DeletedAt   sql.NullInt64
	ReviewState string
	RecordingID int64
	// ArtistHasImage / AlbumHasImage drive the grouped view's "Add cover" hint.
	ArtistHasImage bool
	AlbumHasImage  bool
}

// trashAppearanceFrom roots the lens on the tagset. `m` is the appearance (the
// row), `r` its recording, `f` the origin blob when one survives. The alias
// names match the shared predicates (qFieldClause reads `m.*` and `f.id`), so
// filtering behaves exactly as on the other file surfaces.
const trashAppearanceFrom = `
	FROM tagsets m
	JOIN recordings r ON r.id = m.recording_id
	LEFT JOIN files f ON f.id = m.origin_file_id
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

const trashAppearanceSelect = `
	SELECT
		m.id, m.origin_file_id,
		COALESCE(f.hash, ''), COALESCE(f.byte_size, 0), COALESCE(f.object_key, ''),
		COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), COALESCE(f.hash, '')) AS filename,
		COALESCE(m.title,  '') AS title,
		COALESCE(m.artist, '') AS artist,
		m.album_artist,
		COALESCE(m.album,  '') AS album,
		m.track_number,
		COALESCE(m.year, 0) AS year,
		m.deleted_at,
		m.review_state,
		m.recording_id,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image` + trashAppearanceFrom

func scanTrashAppearances(rows *sql.Rows, err error) ([]*TrashEntry, error) {
	if err != nil {
		return nil, fmt.Errorf("list trashed appearances: %w", err)
	}
	defer rows.Close()
	out := make([]*TrashEntry, 0)
	for rows.Next() {
		var e TrashEntry
		if err := rows.Scan(&e.TagsetID, &e.FileID, &e.Hash, &e.ByteSize, &e.ObjectKey, &e.Filename,
			&e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.Year,
			&e.DeletedAt, &e.ReviewState, &e.RecordingID, &e.ArtistHasImage, &e.AlbumHasImage); err != nil {
			return nil, fmt.Errorf("scan trashed appearance: %w", err)
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
	case "grouped":
		return `LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, LOWER(COALESCE(m.album, '')) ASC,
		        (m.track_number IS NULL) ASC, m.track_number ASC, m.id ASC`
	default: // deleted_desc
		return "m.deleted_at DESC, m.id DESC"
	}
}

// ListTrashedAppearancesPage returns one filtered, sorted page of the lens.
// With Limit <= 0 it returns every match (no window).
func (db *DB) ListTrashedAppearancesPage(ctx context.Context, q FileListQuery) ([]*TrashEntry, error) {
	where, args := appearanceTrashWhere(q.FileFilter)
	sqlText := trashAppearanceSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + appearanceTrashSort(q.Sort)
	if q.Limit > 0 {
		offset := max(q.Offset, 0)
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	return scanTrashAppearances(db.QueryContext(ctx, sqlText, args...))
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
