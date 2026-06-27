package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// visibleFile is the predicate every user-facing listing / access query must
// apply (aliased table f): a file is publicly visible only when it is neither
// trashed nor pending review (docs/architecture/moderation.md). Trash
// and review/staging queries intentionally do not use it.
const visibleFile = "f.deleted_at IS NULL AND f.review_state = 'approved'"

// StorageByteBreakdown partitions the logical byte size of stored blobs by
// state. Files are content-addressed (one row per hash), so each bucket is a
// deduplicated blob total. The three buckets are mutually exclusive — Trash
// (any soft-deleted row) takes precedence over review state — and together they
// equal SUM(byte_size) over the whole files table. Sizes are logical (the
// byte_size column), matching the rest of the storage panel; see
// docs/architecture/storage.md. It backs the audio/review/trash disk-usage
// categories (files hold audio in v0); one indexed sum, so it is instant and
// needs no caching, unlike the image walk.
type StorageByteBreakdown struct {
	Library int64 // approved & not soft-deleted — the live library
	Review  int64 // not deleted, review_state <> 'approved' — staged uploads
	Trash   int64 // soft-deleted, awaiting prune
}

// StorageByteBreakdown computes the per-state byte totals in a single query. It
// counts only on-disk (owned) blobs: links rows reference external originals that
// live outside data_dir, so their byte_size is NOT part of the on-disk footprint
// (it is reported separately as external bytes — see storageStats and
// docs/architecture/data-sources.md). Importing in place adds 0 here.
func (db *DB) StorageByteBreakdown(ctx context.Context) (StorageByteBreakdown, error) {
	var b StorageByteBreakdown
	err := db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN deleted_at IS NULL AND review_state =  'approved' THEN byte_size END), 0),
		  COALESCE(SUM(CASE WHEN deleted_at IS NULL AND review_state <> 'approved' THEN byte_size END), 0),
		  COALESCE(SUM(CASE WHEN deleted_at IS NOT NULL                            THEN byte_size END), 0)
		FROM files
		WHERE storage_backend <> 'links'`,
	).Scan(&b.Library, &b.Review, &b.Trash)
	if err != nil {
		return b, fmt.Errorf("storage byte breakdown: %w", err)
	}
	return b, nil
}

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload". Soft-deleted files are
// returned with DeletedAt set so the upload handler can restore them.
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT id, hash, byte_size, mime_type, storage_backend, object_key, link_target, created_at, deleted_at,
		       review_state, review_note, submitted_at
		FROM files
		WHERE hash = ?`

	var f File
	err := db.QueryRowContext(ctx, q, hash).Scan(
		&f.ID, &f.Hash, &f.ByteSize, &f.MimeType, &f.StorageBackend, &f.ObjectKey, &f.LinkTarget, &f.CreatedAt, &f.DeletedAt,
		&f.ReviewState, &f.ReviewNote, &f.SubmittedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query file by hash: %w", err)
	}
	return &f, nil
}

// InsertFile writes the files, file_uploads, and media_metadata rows in a
// single transaction. f.ID is populated on success. The upload and meta
// arguments may share f.ID once it is assigned.
func (db *DB) InsertFile(ctx context.Context, f *File, upload *FileUpload, meta *MediaMetadata) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// An unset ReviewState collapses to approved — the pre-moderation behavior
	// kept by auth-less embedding and existing tests.
	if f.ReviewState == "" {
		f.ReviewState = ReviewApproved
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, link_target, created_at, uploaded_by, review_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Hash, f.ByteSize, f.MimeType, f.StorageBackend, f.ObjectKey, f.LinkTarget, f.CreatedAt, f.UploadedBy, f.ReviewState,
	)
	if err != nil {
		return fmt.Errorf("insert files: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	f.ID = id

	if upload != nil {
		upload.FileID = id
		if upload.UploadedAt == 0 {
			upload.UploadedAt = time.Now().Unix()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO file_uploads (file_id, filename, uploaded_at)
			VALUES (?, ?, ?)`,
			upload.FileID, upload.Filename, upload.UploadedAt,
		); err != nil {
			return fmt.Errorf("insert file_uploads: %w", err)
		}
	}

	if meta != nil {
		meta.FileID = id
		if meta.ExtractedAt == 0 {
			meta.ExtractedAt = time.Now().Unix()
		}
		// title is required non-empty (migration 016): default it to the upload
		// filename with its extension stripped when no title tag was supplied, so
		// the CHECK is satisfied for every caller.
		if strings.TrimSpace(meta.Title) == "" {
			fname := ""
			if upload != nil {
				fname = upload.Filename
			}
			meta.Title = titleFromFilename(fname)
		}
		// Resolve the artist/album entities inline so a fresh upload carries its
		// FKs immediately, without waiting for the startup backfill. Both the
		// album-grouping artist (album_artist_id) and the track performer (artist_id)
		// are resolved; for a normal release they are the same entity.
		albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, tagsFromMeta(meta))
		if err != nil {
			return fmt.Errorf("resolve entities: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO media_metadata (
				file_id, title, artist, album, album_artist, genre, year,
				track_number, track_total, disc_number, composer, comment,
				duration_seconds, bitrate, sample_rate, channels, codec,
				tag_format, extracted_at, album_artist_id, artist_id, album_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			meta.FileID, meta.Title, meta.Artist, meta.Album, meta.AlbumArtist,
			meta.Genre, meta.Year, meta.TrackNumber, meta.TrackTotal, meta.DiscNumber,
			meta.Composer, meta.Comment, meta.DurationSeconds, meta.Bitrate,
			meta.SampleRate, meta.Channels, meta.Codec, meta.TagFormat, meta.ExtractedAt,
			albumArtistID, trackArtistID, albumID,
		); err != nil {
			return fmt.Errorf("insert media_metadata: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListFiles returns all files ordered by created_at DESC, joined with
// the first recorded filename and media_metadata tags.
func (db *DB) ListFiles(ctx context.Context) ([]*FileListEntry, error) {
	return db.listFiles(ctx, "")
}

// ListFilesGuest returns only the files an anonymous / capability-less request
// may play/download (the guest-playable / license policy). Callers holding the
// content.access permission should use the unfiltered ListFiles instead.
func (db *DB) ListFilesGuest(ctx context.Context) ([]*FileListEntry, error) {
	return db.listFiles(ctx, accessClause)
}

// fileListSelect is the column list + joins shared by every file listing (full,
// guest, and paginated). Callers append their own WHERE / ORDER BY / LIMIT and
// scan the result with scanFileList — so the row shape is defined in one place.
// (A var, not a const, because it embeds the guestAccessibleExpr var.)
var fileListSelect = `
	SELECT
		f.id, f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
		COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
		COALESCE(m.title,  '') AS title,
		COALESCE(m.artist, '') AS artist,
		m.album_artist,
		COALESCE(m.album,  '') AS album,
		m.track_number,
		m.disc_number,
		COALESCE(m.year,    0) AS year,
		m.duration_seconds,
		` + guestAccessibleExpr + ` AS guest_playable,
		f.license,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image
	FROM files f
	LEFT JOIN media_metadata m ON m.file_id = f.id
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

// scanFileList drains a fileListSelect result into FileListEntry values.
func scanFileList(rows *sql.Rows) ([]*FileListEntry, error) {
	out := make([]*FileListEntry, 0)
	for rows.Next() {
		var e FileListEntry
		var guest, artistImg, albumImg int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.DiscNumber, &e.Year, &e.DurationSeconds,
			&guest, &e.License, &artistImg, &albumImg,
		); err != nil {
			return nil, fmt.Errorf("scan file list entry: %w", err)
		}
		e.GuestPlayable = guest == 1
		e.ArtistHasImage = artistImg == 1
		e.AlbumHasImage = albumImg == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list files rows: %w", err)
	}
	return out, nil
}

// listFiles is the shared query. When where is non-empty it is appended as an
// access predicate (with its bind args), restricting the result to reachable
// files.
func (db *DB) listFiles(ctx context.Context, where string, args ...any) ([]*FileListEntry, error) {
	q := fileListSelect
	if where != "" {
		q += "\n\t\tWHERE " + visibleFile + " AND " + where
	} else {
		q += "\n\t\tWHERE " + visibleFile
	}
	q += "\n\t\tORDER BY f.created_at DESC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	return scanFileList(rows)
}

// FileFilter narrows a file listing or bulk operation. The zero value matches
// every approved, non-trashed file (the visibleFile predicate is always
// applied). Guest additionally restricts to guest-reachable files (the listing
// path); the admin bulk path leaves it false. Q is a case-insensitive substring
// over the fields a row shows (title / artist / album-artist / album / filename);
// QField scopes that substring to one column group ("artist" / "album" /
// "title") — empty (or unknown) searches every field, as before. ArtistID /
// AlbumID pin an entity — an artist matches in EITHER role (album-artist OR
// performer), mirroring the entity browse.
// See docs/architecture/file-list-scaling.md.
type FileFilter struct {
	Guest    bool
	Q        string
	QField   string
	ArtistID *int64
	AlbumID  *int64
}

// FileListQuery is a FileFilter plus presentation: a sort token (allow-listed in
// fileSortOrder) and a page window. Limit <= 0 means "no limit" (every match);
// Offset < 0 clamps to 0.
type FileListQuery struct {
	FileFilter
	Sort   string
	Limit  int
	Offset int
}

// likeEscaped wraps q as a LIKE pattern with % and _ neutralised (used with
// ESCAPE '\'), mirroring database/library.go search so a literal underscore in
// a title can't act as a wildcard.
func likeEscaped(q string) string {
	return "%" + strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(q) + "%"
}

// qFieldClause builds the case-insensitive search predicate for a filter term,
// scoped to a field group (the UI's filter-type dropdown). It is the single
// definition of "what the search box matches" — title / artist / album-artist /
// album / filename — shared by the files filter, the trash filter, and the
// review/upload filters, so every list matches identically. It returns a
// parenthesised OR fragment (no leading " AND ") and its binds, or ("", nil)
// when the term is blank. Callers must have the media_metadata join aliased `m`
// and the files row aliased `f`.
func qFieldClause(q, field string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	like := likeEscaped(q)
	// col() is a case-insensitive LIKE over one media_metadata column; fnameExists
	// matches the upload filename. field scopes which set the term searches; an
	// empty/unknown field keeps the original "every field" behaviour. Each cond
	// carries exactly one '?', so one `like` bind per cond, in order.
	col := func(c string) string {
		return "unicode_lower(COALESCE(m." + c + ", '')) LIKE unicode_lower(?) ESCAPE '\\'"
	}
	const fnameExists = `EXISTS (SELECT 1 FROM file_uploads u WHERE u.file_id = f.id AND unicode_lower(u.filename) LIKE unicode_lower(?) ESCAPE '\')`
	var conds []string
	switch field {
	case "artist":
		conds = []string{col("artist"), col("album_artist")}
	case "album":
		conds = []string{col("album")}
	case "title":
		conds = []string{col("title"), fnameExists}
	default: // "" / unknown → search every field, as before
		conds = []string{col("title"), col("artist"), col("album_artist"), col("album"), fnameExists}
	}
	args := make([]any, len(conds))
	for i := range conds {
		args[i] = like
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

// fileFilterWhere builds the WHERE predicate (always including visibleFile) and
// its bind args for a FileFilter. It is the single definition of "what matches",
// shared by the page query, the count, and the bulk hash resolver.
func fileFilterWhere(f FileFilter) (string, []any) {
	where := visibleFile
	var args []any
	if f.Guest {
		where += " AND " + accessClause
	}
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

// fileSortOrder maps a sort token to a safe ORDER BY fragment (allow-listed
// columns only — never interpolate caller input). Every order ends with f.id so
// paging is stable across ties. Unknown tokens fall back to newest-first.
// untaggedExpr is 1 for a file with neither an artist nor an album-artist tag —
// the "needs metadata" rows (mirrors file-list.js needsMeta). Used by the
// untagged_first sort to surface rows that still want tagging.
const untaggedExpr = `(CASE WHEN TRIM(COALESCE(m.artist, '')) = '' AND TRIM(COALESCE(m.album_artist, '')) = '' THEN 1 ELSE 0 END)`

// groupedSortOrder is the "By artist / album" view order, reproduced server-side
// so the grouped list can stream (infinite scroll) instead of loading every row
// to sort in the browser. Mirrors file-list.js buildArtistGroups: album-artist
// (falling back to performer, empty bucket last), then album within the artist by
// its earliest year (a window MIN so a per-track year can't reorder the album)
// then album name (empty "Other" bucket last), then disc (untagged last), track
// (untagged last), title. f.id keeps paging stable across ties. The client only
// inserts separators where these keys change between rows. Shared by the files,
// trash, and review listings so their grouped views order identically. Requires
// the media_metadata join aliased `m`, a `filename` select alias, and files `f`.
const groupedSortOrder = `(CASE WHEN COALESCE(m.album_artist, m.artist, '') = '' THEN 1 ELSE 0 END) ASC,
	LOWER(COALESCE(m.album_artist, m.artist, '')) ASC,
	(CASE WHEN COALESCE(m.album, '') = '' THEN 1 ELSE 0 END) ASC,
	COALESCE(MIN(NULLIF(m.year, 0)) OVER (
		PARTITION BY LOWER(COALESCE(m.album_artist, m.artist, '')), LOWER(COALESCE(m.album, ''))
	), 9999) ASC,
	LOWER(COALESCE(m.album, '')) ASC,
	(CASE WHEN m.disc_number IS NULL THEN 1 ELSE 0 END) ASC, m.disc_number ASC,
	(CASE WHEN m.track_number IS NULL THEN 1 ELSE 0 END) ASC, m.track_number ASC,
	LOWER(COALESCE(NULLIF(m.title, ''), filename, '')) ASC, f.id ASC`

func fileSortOrder(token string) string {
	switch token {
	case "created_asc":
		return "f.created_at ASC, f.id ASC"
	case "title_asc":
		return "LOWER(COALESCE(m.title, '')) ASC, f.id ASC"
	case "title_desc":
		return "LOWER(COALESCE(m.title, '')) DESC, f.id DESC"
	case "artist_asc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) ASC, f.id ASC"
	case "artist_desc":
		return "LOWER(COALESCE(m.album_artist, m.artist, '')) DESC, f.id DESC"
	case "size_asc":
		return "f.byte_size ASC, f.id ASC"
	case "size_desc":
		return "f.byte_size DESC, f.id DESC"
	case "untagged_first":
		// Untagged rows first, then newest — surfaces files that still need tags.
		return untaggedExpr + " DESC, f.created_at DESC, f.id DESC"
	case "grouped":
		return groupedSortOrder
	default: // created_desc
		return "f.created_at DESC, f.id DESC"
	}
}

// ListFilesPage returns one filtered, sorted page of the file listing — the
// paginated counterpart of ListFiles. With Limit <= 0 it returns every match
// (no window). See docs/architecture/file-list-scaling.md.
func (db *DB) ListFilesPage(ctx context.Context, q FileListQuery) ([]*FileListEntry, error) {
	where, args := fileFilterWhere(q.FileFilter)
	sqlText := fileListSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + fileSortOrder(q.Sort)
	if q.Limit > 0 {
		offset := q.Offset
		if offset < 0 {
			offset = 0
		}
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list files page: %w", err)
	}
	defer rows.Close()
	return scanFileList(rows)
}

// CountFiles returns how many files match the filter, ignoring paging — the
// total for "page N of M" and "select all N matching".
func (db *DB) CountFiles(ctx context.Context, f FileFilter) (int, error) {
	where, args := fileFilterWhere(f)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files f LEFT JOIN media_metadata m ON m.file_id = f.id WHERE `+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count files: %w", err)
	}
	return n, nil
}

// FileHashesByFilter returns the content hashes of every file matching the
// filter (no paging), ordered by id. Backs "select all N matching" bulk actions:
// the handler resolves the set here, then acts on the hashes.
func (db *DB) FileHashesByFilter(ctx context.Context, f FileFilter) ([]string, error) {
	where, args := fileFilterWhere(f)
	rows, err := db.QueryContext(ctx,
		`SELECT f.hash FROM files f LEFT JOIN media_metadata m ON m.file_id = f.id WHERE `+where+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("file hashes by filter: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan file hash: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// BulkSoftDeleteByHashes soft-deletes (moves to Trash) the given hashes in a
// single transaction, chunked to stay within SQLite's bound-parameter limit. It
// mirrors SoftDeleteFileByHash's deleted_at-only guard, so already-trashed and
// unknown hashes are simply skipped; the returned count is how many rows were
// actually trashed. Blobs and DB rows are preserved for the Trash flow.
func (db *DB) BulkSoftDeleteByHashes(ctx context.Context, hashes []string) (int, error) {
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
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, now)
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET deleted_at = ? WHERE deleted_at IS NULL AND hash IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk soft delete: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

// uploadFilenamesInTx returns the recorded filenames for a file (ordered by
// upload id) within an open transaction. It is a shared helper for the two
// delete variants that need to report filenames before mutating the row.
func uploadFilenamesInTx(ctx context.Context, tx *sql.Tx, fileID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT filename FROM file_uploads WHERE file_id = ? ORDER BY id`, fileID)
	if err != nil {
		return nil, fmt.Errorf("select filenames: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan filename: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// SoftDeleteFileByHash marks the file as trashed by setting deleted_at. The
// blob is left on disk. file_uploads and media_metadata rows are preserved.
// Returns found=false (no error) when no live (non-trashed) row matches.
func (db *DB) SoftDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ? AND deleted_at IS NULL`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select file id: %w", err)
	}

	filenames, err := uploadFilenamesInTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE files SET deleted_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return nil, false, fmt.Errorf("soft delete file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// HardDeleteFileByHash permanently removes any files row identified by hash,
// regardless of trash state. Used by PruneDangling, which must be able to
// clean up both live and trashed rows whose blobs are gone.
// Returns found=false (no error) when no row matches.
func (db *DB) HardDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	return db.hardDelete(ctx, `SELECT id FROM files WHERE hash = ?`, hash)
}

// HardDeleteTrashedFileByHash permanently removes a trashed files row. Live
// (non-trashed) files return found=false, making the check and delete atomic
// within the same transaction so a concurrent restore cannot race the delete.
// Returns found=false (no error) when no trashed row matches.
func (db *DB) HardDeleteTrashedFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	return db.hardDelete(ctx, `SELECT id FROM files WHERE hash = ? AND deleted_at IS NOT NULL`, hash)
}

// hardDelete is the shared implementation for the two hard-delete variants.
func (db *DB) hardDelete(ctx context.Context, selectQ, hash string) ([]string, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, selectQ, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select file id: %w", err)
	}

	filenames, err := uploadFilenamesInTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id); err != nil {
		return nil, false, fmt.Errorf("delete file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// RestoreFileByHash clears deleted_at on a trashed file, returning it to the
// live library. Returns found=false (no error) when no trashed row matches.
func (db *DB) RestoreFileByHash(ctx context.Context, hash string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE files SET deleted_at = NULL WHERE hash = ? AND deleted_at IS NOT NULL`, hash)
	if err != nil {
		return false, fmt.Errorf("restore file: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// trashListSelect is the shared SELECT for the Trash listings (the f.deleted_at /
// f.review_state columns the flat list and its restore badge need; no
// disc_number, matching the historical trash payload). Keep in sync with
// scanTrashList.
const trashListSelect = `
	SELECT
		f.id, f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
		COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
		COALESCE(m.title,  '') AS title,
		COALESCE(m.artist, '') AS artist,
		m.album_artist,
		COALESCE(m.album,  '') AS album,
		m.track_number,
		COALESCE(m.year,    0) AS year,
		m.duration_seconds,
		f.guest_playable,
		f.license,
		f.deleted_at,
		f.review_state,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image
	FROM files f
	LEFT JOIN media_metadata m ON m.file_id = f.id
	LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
	LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`

func scanTrashList(rows *sql.Rows) ([]*FileListEntry, error) {
	out := make([]*FileListEntry, 0)
	for rows.Next() {
		var e FileListEntry
		var guest, artistImg, albumImg int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.Year, &e.DurationSeconds,
			&guest, &e.License, &e.DeletedAt, &e.ReviewState, &artistImg, &albumImg,
		); err != nil {
			return nil, fmt.Errorf("scan trashed file: %w", err)
		}
		e.GuestPlayable = guest == 1
		e.ArtistHasImage = artistImg == 1
		e.AlbumHasImage = albumImg == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trashed files rows: %w", err)
	}
	return out, nil
}

// trashFilterWhere builds the WHERE predicate (always the trashed base
// f.deleted_at IS NOT NULL) and binds for a FileFilter over the Trash bucket. It
// reuses qFieldClause + the artist/album pins so Trash matches identically to the
// live library, just over soft-deleted rows. Guest is never applied (Trash is
// admin-only).
func trashFilterWhere(f FileFilter) (string, []any) {
	where := "f.deleted_at IS NOT NULL"
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

// trashSortOrder maps a sort token to a safe ORDER BY for the Trash list. It adds
// deletion-time orders (the Trash default) and otherwise delegates to the shared
// flat / grouped orders. Unknown tokens fall back to newest-deleted-first.
func trashSortOrder(token string) string {
	switch token {
	case "deleted_asc":
		return "f.deleted_at ASC, f.id ASC"
	case "deleted_desc":
		return "f.deleted_at DESC, f.id DESC"
	case "created_asc", "created_desc", "title_asc", "title_desc",
		"artist_asc", "artist_desc", "size_asc", "size_desc", "untagged_first", "grouped":
		return fileSortOrder(token)
	default:
		return "f.deleted_at DESC, f.id DESC"
	}
}

// ListTrashedFiles returns all soft-deleted files ordered by deletion time
// descending — the non-paged wrapper over ListTrashedFilesPage (Limit <= 0).
func (db *DB) ListTrashedFiles(ctx context.Context) ([]*FileListEntry, error) {
	return db.ListTrashedFilesPage(ctx, FileListQuery{})
}

// ListTrashedFilesPage returns one filtered, sorted page of the Trash listing.
// With Limit <= 0 it returns every match (no window).
func (db *DB) ListTrashedFilesPage(ctx context.Context, q FileListQuery) ([]*FileListEntry, error) {
	where, args := trashFilterWhere(q.FileFilter)
	sqlText := trashListSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + trashSortOrder(q.Sort)
	if q.Limit > 0 {
		offset := q.Offset
		if offset < 0 {
			offset = 0
		}
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list trashed files page: %w", err)
	}
	defer rows.Close()
	return scanTrashList(rows)
}

// CountTrashedFiles returns how many trashed files match the filter, ignoring
// paging — the total for the Trash list and "select all N matching".
func (db *DB) CountTrashedFiles(ctx context.Context, f FileFilter) (int, error) {
	where, args := trashFilterWhere(f)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files f LEFT JOIN media_metadata m ON m.file_id = f.id WHERE `+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count trashed files: %w", err)
	}
	return n, nil
}

// TrashedFileHashesByFilter returns the content hashes of every trashed file
// matching the filter (no paging), ordered by id. Backs the Trash "select all N
// matching" bulk restore / delete / edit.
func (db *DB) TrashedFileHashesByFilter(ctx context.Context, f FileFilter) ([]string, error) {
	where, args := trashFilterWhere(f)
	rows, err := db.QueryContext(ctx,
		`SELECT f.hash FROM files f LEFT JOIN media_metadata m ON m.file_id = f.id WHERE `+where+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("trashed file hashes by filter: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan trashed file hash: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListFileRefs returns one FileRef per live (non-trashed) files row with the
// filenames recorded for it, ordered by file id. Files with no upload rows
// carry an empty slice. Trashed files are excluded so PruneDangling does not
// permanently delete files the admin placed in the trash intentionally.
func (db *DB) ListFileRefs(ctx context.Context) ([]FileRef, error) {
	const q = `
		SELECT f.hash, f.storage_backend, COALESCE(f.link_target, ''),
		       COALESCE(GROUP_CONCAT(u.filename, char(10)), '')
		FROM files f
		LEFT JOIN file_uploads u ON u.file_id = f.id
		WHERE f.deleted_at IS NULL
		GROUP BY f.id
		ORDER BY f.id`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}
	defer rows.Close()

	out := make([]FileRef, 0)
	for rows.Next() {
		var hash, backend, linkTarget, joined string
		if err := rows.Scan(&hash, &backend, &linkTarget, &joined); err != nil {
			return nil, fmt.Errorf("scan file ref: %w", err)
		}
		ref := FileRef{Hash: hash, StorageBackend: backend, LinkTarget: linkTarget}
		if joined != "" {
			ref.Filenames = strings.Split(joined, "\n")
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file ref rows: %w", err)
	}
	return out, nil
}

// RecordUpload appends an upload row for an existing file. UNIQUE conflicts
// on (file_id, filename) are silently ignored — the same file uploaded with
// the same name twice is not an error.
func (db *DB) RecordUpload(ctx context.Context, fileID int64, filename string) error {
	_, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO file_uploads (file_id, filename, uploaded_at)
		VALUES (?, ?, ?)`,
		fileID, filename, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record upload: %w", err)
	}
	return nil
}
