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
// apply: a track is publicly visible only when its file (aliased f) is not
// removed and its tagset (aliased m — join via tagsetJoin or equivalent) is
// neither trashed nor pending review (docs/architecture/moderation.md,
// docs/architecture/recording-tagsets.md). Trash and review/staging queries
// intentionally do not use it.
const visibleFile = "f.deleted_at IS NULL AND m.deleted_at IS NULL AND m.review_state = 'approved'"

// visibleFileOrRemoved is visibleFile without the file-liveness leg: it
// additionally admits soft-removed blobs (absorbed / removed renditions) whose
// representative appearance is still live — the All-files "Show removed"
// toggle (recording-tagsets P5). Trashed appearances stay excluded (Trash
// owns those).
const visibleFileOrRemoved = "m.deleted_at IS NULL AND m.review_state = 'approved'"

// reprTagset selects a file's *representative* appearance — the single tagset
// the files-rooted surfaces display, so those surfaces stay 1:1 with the file.
//
// It searches the file's *recording* (`files.recording_id`), not just the
// appearances read from this blob (recording-tagsets P7). A file is a rendition
// of a recording; the recording carries the appearances. `origin_file_id` is
// only provenance, and it does not cover every file: appearance dedup — merge,
// absorb — deliberately drops a redundant appearance while keeping its blob, so
// a live rendition with no tagset of its own is a normal, by-design state.
// Rooting solely on the provenance column made such renditions vanish from
// every files-rooted surface.
//
// The blob's **own** offered appearance still wins when it has one, because the
// per-blob lifecycle lives there: a rendition awaiting review must not borrow
// the recording's approved primary and read as live (it would leak into the
// All-files listing and the Library byte bucket). Only when the blob has no
// appearance of its own does it fall back to the recording's — live over
// trashed, then primary, then oldest. Requires the files row aliased `f`.
const reprTagset = `(SELECT rt.id FROM tagsets rt WHERE rt.recording_id = f.recording_id
		ORDER BY COALESCE(rt.origin_file_id = f.id, 0) DESC,
		         (rt.deleted_at IS NULL) DESC, rt.is_primary DESC, rt.id ASC LIMIT 1)`

// originTagset selects the appearance whose tags were read from this very blob
// — the provenance link. It is *not* a cover of the files table (see
// reprTagset). Only two kinds of surface may use it: the Trash Appearances
// listing, which is addressed by the origin blob's hash, and the
// file-addressed metadata edit. Requires the files row aliased `f`.
const originTagset = `(SELECT rt.id FROM tagsets rt WHERE rt.origin_file_id = f.id
		ORDER BY rt.is_primary DESC, rt.id ASC LIMIT 1)`

// tagsetJoin binds the aliases the shared predicates (visibleFile,
// accessClause, qFieldClause) expect around a files row aliased `f`: `m` is the
// appearance the file's recording is displayed under (reprTagset) and `r` its
// recording (access/license). INNER joins on purpose: a recording always has at
// least one tagset and every file has a recording (both invariants, enforced by
// the cascade ops and healed by ReconcileTagsets), so every file is covered.
const tagsetJoin = `
	JOIN tagsets m ON m.id = ` + reprTagset + `
	JOIN recordings r ON r.id = f.recording_id`

// originTagsetJoin is tagsetJoin for the hash-addressed Trash listing, where
// the row *is* the appearance offered from that blob and its `deleted_at` is
// the Trash mark being filtered on — so it must not fall back to a sibling
// appearance the way reprTagset does. Superseded once the Trash Appearances
// lens is re-rooted `FROM tagsets` (P7c).
const originTagsetJoin = `
	JOIN tagsets m ON m.id = ` + originTagset + `
	JOIN recordings r ON r.id = f.recording_id`

// StorageByteBreakdown partitions the logical byte size of stored blobs by
// state. Files are content-addressed (one row per hash), so each bucket is a
// deduplicated blob total. The three buckets are mutually exclusive — Trash
// (any soft-deleted row) takes precedence over review state — and together they
// equal SUM(byte_size) over the whole files table. Sizes are logical (the
// byte_size column), matching the rest of the storage panel; see
// docs/architecture/storage.md. It backs the audio/review/trash disk-usage
// categories (files hold audio in v0); one indexed sum, so it is instant and
// needs no caching, unlike the image walk.
// The state is read off the blob's recording (reprTagset): a rendition whose
// recording still has a live approved appearance counts as Library even when no
// tagset was read from that particular blob (recording-tagsets P7) — previously
// such orphaned renditions were misfiled as Trash bytes.
type StorageByteBreakdown struct {
	Library int64 // recording's appearance approved & not trashed — the live library
	Review  int64 // appearance not trashed, review_state <> 'approved' — staged uploads
	Trash   int64 // every appearance of the recording trashed, awaiting prune
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
		  COALESCE(SUM(CASE WHEN t.deleted_at IS NULL AND t.review_state =  'approved' THEN f.byte_size END), 0),
		  COALESCE(SUM(CASE WHEN t.deleted_at IS NULL AND t.review_state <> 'approved' THEN f.byte_size END), 0),
		  COALESCE(SUM(CASE WHEN t.id IS NULL OR t.deleted_at IS NOT NULL               THEN f.byte_size END), 0)
		FROM files f
		LEFT JOIN tagsets t ON t.id = ` + reprTagset + `
		WHERE f.storage_backend <> 'links'`,
	).Scan(&b.Library, &b.Review, &b.Trash)
	if err != nil {
		return b, fmt.Errorf("storage byte breakdown: %w", err)
	}
	return b, nil
}

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload". The lifecycle fields
// (DeletedAt, ReviewState, …) are derived from the file's offered tagset, so a
// trashed tagset reads back as a trashed file for the restore/dedup flows. A
// file somehow missing its tagset (invariant violation, repaired at startup)
// reads as approved so the dedup path still short-circuits.
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT f.id, f.hash, f.byte_size, f.mime_type, f.storage_backend, f.object_key, f.link_target,
		       f.created_at, f.recording_id, t.deleted_at,
		       COALESCE(t.review_state, 'approved'), t.review_note, t.submitted_at
		FROM files f
		LEFT JOIN tagsets t ON t.origin_file_id = f.id
		WHERE f.hash = ?
		ORDER BY t.id
		LIMIT 1`

	var f File
	err := db.QueryRowContext(ctx, q, hash).Scan(
		&f.ID, &f.Hash, &f.ByteSize, &f.MimeType, &f.StorageBackend, &f.ObjectKey, &f.LinkTarget,
		&f.CreatedAt, &f.RecordingID, &f.DeletedAt,
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

// InsertFile creates the file with everything the tagset invariant demands, in
// a single transaction: its recording (a fresh singleton — the fingerprint
// resolver may later merge it into a matched one), the files row, the
// file_uploads row, the offered tagset (descriptive tags, seeded with
// f.ReviewState and owned by f.UploadedBy, primary appearance of the new
// recording), and the tech media_metadata row. f.ID and f.RecordingID are
// populated on success.
func (db *DB) InsertFile(ctx context.Context, f *File, upload *FileUpload, meta *MediaMetadata) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	// An unset ReviewState collapses to approved — the pre-moderation behavior
	// kept by auth-less embedding and existing tests.
	if f.ReviewState == "" {
		f.ReviewState = ReviewApproved
	}

	// Every file belongs to a recording — creation is atomic with the first
	// file and first tagset (docs/architecture/recording-tagsets.md).
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, f.CreatedAt,
	).Scan(&f.RecordingID); err != nil {
		return fmt.Errorf("insert recording: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, link_target, created_at, uploaded_by, recording_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Hash, f.ByteSize, f.MimeType, f.StorageBackend, f.ObjectKey, f.LinkTarget, f.CreatedAt, f.UploadedBy, f.RecordingID,
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
			upload.UploadedAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO file_uploads (file_id, filename, uploaded_at)
			VALUES (?, ?, ?)`,
			upload.FileID, upload.Filename, upload.UploadedAt,
		); err != nil {
			return fmt.Errorf("insert file_uploads: %w", err)
		}
	}

	// The offered tagset is created even for a nil meta (an untagged caller):
	// a recording must never exist without an appearance. "A null tagset is
	// just a tagset" — it resolves to the Unknown artist / Other buckets.
	if meta == nil {
		meta = &MediaMetadata{}
	}
	meta.FileID = id
	if meta.ExtractedAt == 0 {
		meta.ExtractedAt = now
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
		INSERT INTO tagsets (
			recording_id, title, artist, album_artist, album, genre, year,
			track_number, track_total, disc_number, composer, comment,
			artist_id, album_artist_id, album_id,
			review_state, created_by, origin_file_id, is_primary, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		f.RecordingID, meta.Title, meta.Artist, meta.AlbumArtist, meta.Album,
		meta.Genre, meta.Year, meta.TrackNumber, meta.TrackTotal, meta.DiscNumber,
		meta.Composer, meta.Comment, trackArtistID, albumArtistID, albumID,
		f.ReviewState, f.UploadedBy, id, f.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert tagset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO media_metadata (
			file_id, duration_seconds, bitrate, sample_rate, channels, codec,
			tag_format, extracted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.FileID, meta.DurationSeconds, meta.Bitrate, meta.SampleRate,
		meta.Channels, meta.Codec, meta.TagFormat, meta.ExtractedAt,
	); err != nil {
		return fmt.Errorf("insert media_metadata: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// AttachDraftTagset offers a new appearance on the recording of an existing blob
// (recording-tagsets P4, byte-dup upload → draft tagset): a content-hash dedup
// re-upload adds no new file, but its tags land as a draft appearance the
// uploader reviews. It resolves the tags to entities and, unless a live
// appearance with the same identity (album / album-artist / disc / track)
// already exists on the recording, inserts a draft tagset owned by ownerID with
// origin_file_id = fileID. Returns the tagset id and whether it was created
// (created=false when an identical live appearance already exists — nothing new
// to offer, so the re-upload is a plain no-op). A trashed or unknown file yields
// (0, false, nil). Atomic.
func (db *DB) AttachDraftTagset(ctx context.Context, fileID int64, ownerID sql.NullInt64, meta *MediaMetadata, filename string) (tagsetID int64, created bool, err error) {
	if meta == nil {
		meta = &MediaMetadata{}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("attach draft tagset: begin: %w", err)
	}
	defer tx.Rollback()

	var recID int64
	err = tx.QueryRowContext(ctx,
		`SELECT recording_id FROM files WHERE id = ? AND deleted_at IS NULL`, fileID).Scan(&recID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil // trashed or unknown blob — nothing to attach to
	}
	if err != nil {
		return 0, false, fmt.Errorf("attach draft tagset: load file: %w", err)
	}

	albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, tagsFromMeta(meta))
	if err != nil {
		return 0, false, fmt.Errorf("attach draft tagset: resolve entities: %w", err)
	}

	// Identity dedup: a live appearance with the same key already offered? (NULL-safe
	// via SQLite IS, so an untagged disc/track matches another untagged one.)
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM tagsets
		 WHERE recording_id = ? AND deleted_at IS NULL
		   AND album_id IS ? AND album_artist_id IS ? AND disc_number IS ? AND track_number IS ?
		 ORDER BY id LIMIT 1`,
		recID, albumID, albumArtistID, meta.DiscNumber, meta.TrackNumber,
	).Scan(&tagsetID)
	if err == nil {
		if cerr := tx.Commit(); cerr != nil {
			return 0, false, fmt.Errorf("attach draft tagset: commit: %w", cerr)
		}
		return tagsetID, false, nil // identical appearance already present
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("attach draft tagset: dedup: %w", err)
	}

	title := meta.Title
	if strings.TrimSpace(title) == "" {
		title = titleFromFilename(filename)
	}
	now := time.Now().Unix()
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO tagsets (
			recording_id, title, artist, album_artist, album, genre, year,
			track_number, track_total, disc_number, composer, comment,
			artist_id, album_artist_id, album_id,
			review_state, created_by, origin_file_id, is_primary, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?, 0, ?)
		RETURNING id`,
		recID, title, meta.Artist, meta.AlbumArtist, meta.Album, meta.Genre, meta.Year,
		meta.TrackNumber, meta.TrackTotal, meta.DiscNumber, meta.Composer, meta.Comment,
		trackArtistID, albumArtistID, albumID, ownerID, fileID, now,
	).Scan(&tagsetID); err != nil {
		return 0, false, fmt.Errorf("attach draft tagset: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("attach draft tagset: commit: %w", err)
	}
	return tagsetID, true, nil
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
		mm.duration_seconds,
		` + guestAccessibleExpr + ` AS guest_playable,
		r.license,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image,
		f.storage_backend, f.recording_id, f.deleted_at
	FROM files f` + tagsetJoin + `
	LEFT JOIN media_metadata mm ON mm.file_id = f.id
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
			&e.StorageBackend, &e.RecordingID, &e.DeletedAt,
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
	// ShowRemoved additionally lists soft-removed blobs whose representative
	// appearance is live (the All-files physical view, recording-tagsets P5).
	// Never set on guest/bulk paths — the API gates it on a moderation
	// capability.
	ShowRemoved bool
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
// when the term is blank. Callers must have the tagsets join aliased `m`
// and the files row aliased `f`.
func qFieldClause(q, field string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	like := likeEscaped(q)
	// col() is a case-insensitive LIKE over one tagset column; fnameExists
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
	if f.ShowRemoved {
		where = visibleFileOrRemoved
	}
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
// the tagsets join aliased `m`, a `filename` select alias, and files `f`.
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
		`SELECT COUNT(*) FROM files f`+tagsetJoin+` WHERE `+where, args...).Scan(&n)
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
		`SELECT f.hash FROM files f`+tagsetJoin+` WHERE `+where+` ORDER BY f.id`, args...)
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
// single transaction, chunked to stay within SQLite's bound-parameter limit.
// The Trash lives on the tagset (tagsets.deleted_at); the guard mirrors
// SoftDeleteFileByHash, so already-trashed and unknown hashes are simply
// skipped and the returned count is how many were actually trashed. Blobs and
// DB rows are preserved for the Trash flow.
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
			`UPDATE tagsets SET deleted_at = ? WHERE deleted_at IS NULL AND origin_file_id IN
			   (SELECT id FROM files WHERE hash IN (`+strings.Join(placeholders, ",")+`))`, args...)
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

// BulkRestoreByHashes clears deleted_at on the given hashes in a single
// transaction, chunked to stay within SQLite's bound-parameter limit. It mirrors
// RestoreFileByHash's deleted_at-only guard, so live and unknown hashes are
// simply skipped; the returned count is how many trashed rows were restored.
// This is the restore counterpart to BulkSoftDeleteByHashes — one transaction
// instead of one autocommit write per hash, which is what made bulk restore both
// slow and a SQLITE_BUSY source under concurrent write pressure.
func (db *DB) BulkRestoreByHashes(ctx context.Context, hashes []string) (int, error) {
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
		args := make([]any, 0, len(batch))
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = NULL WHERE deleted_at IS NOT NULL AND origin_file_id IN
			   (SELECT id FROM files WHERE hash IN (`+strings.Join(placeholders, ",")+`))`, args...)
		if err != nil {
			return 0, fmt.Errorf("bulk restore: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

// DeletedBlob identifies one hard-deleted file's physical bytes for post-commit
// reclamation: Hash locates the blob and StorageBackend selects the reclaim path
// (local DeleteAll of a hash dir vs unlinking a links symlink). It is the batch
// counterpart to the (GetFileByHash → storage_backend) lookup the single-hash
// delete does before the row is gone.
type DeletedBlob struct {
	Hash           string
	StorageBackend string
}

// BulkHardDeleteTrashedByHashes permanently removes the trashed appearances of
// the given hashes in a single transaction (recording-tagsets P2), returning the
// count of tagsets removed and the blobs of every file a last-appearance cascade
// took down, so the caller can reclaim those bytes after commit (the filesystem
// unlink stays out of the transaction). Live (non-trashed) and unknown hashes
// are skipped — the trashed-tagset guard mirrors HardDeleteTrashedFileByHash, so
// a concurrent restore cannot be hard-deleted out from under the user. It is the
// batch counterpart to HardDeleteTrashedFileByHash: one transaction + the shared
// tagset cascade instead of one autocommit write (plus a per-row audit) per hash,
// which is what made bulk permanent delete slow and a SQLITE_BUSY source under
// write pressure (mirroring the BulkRestoreByHashes fix).
func (db *DB) BulkHardDeleteTrashedByHashes(ctx context.Context, hashes []string) (int, []DeletedBlob, error) {
	if len(hashes) == 0 {
		return 0, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	const chunk = 400
	tagsetIDs := make([]int64, 0, len(hashes))
	// Resolve the trashed appearances first; the cascade then decides, per
	// recording, whether any files (and their blobs) go with them.
	for i := 0; i < len(hashes); i += chunk {
		end := min(i+chunk, len(hashes))
		batch := hashes[i:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch))
		for j, h := range batch {
			placeholders[j] = "?"
			args = append(args, h)
		}
		ids, err := scanIDs(tx.QueryContext(ctx,
			`SELECT t.id FROM tagsets t
			   JOIN files f ON f.id = t.origin_file_id
			  WHERE t.deleted_at IS NOT NULL AND f.hash IN (`+strings.Join(placeholders, ",")+`)`, args...))
		if err != nil {
			return 0, nil, fmt.Errorf("select trashed tagsets: %w", err)
		}
		tagsetIDs = append(tagsetIDs, ids...)
	}

	blobs, err := hardDeleteTagsetsTx(ctx, tx, tagsetIDs)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit: %w", err)
	}
	return len(tagsetIDs), blobs, nil
}

// hardDeleteFilesTx permanently removes the given file ids inside tx, keeping
// the tagset invariant in one transaction (the shared cascade every hard-delete
// entry point must go through — a second code path would reintroduce the orphan
// risk): each file's offered tagsets go with it, a recording left with no files
// is deleted (its remaining tagsets cascade via FK), and a surviving recording
// that lost its primary appearance promotes its oldest remaining tagset. FK
// cascade drops file_uploads, media_metadata, source_files, etc. as before.
func hardDeleteFilesTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const chunk = 400
	recIDs := make(map[int64]struct{})
	for i := 0; i < len(ids); i += chunk {
		end := min(i+chunk, len(ids))
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = "?"
			args = append(args, id)
		}
		in := "(" + strings.Join(placeholders, ",") + ")"

		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT recording_id FROM files WHERE id IN `+in, args...)
		if err != nil {
			return fmt.Errorf("hard delete: recordings: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("hard delete: scan recording: %w", err)
			}
			recIDs[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("hard delete: recording rows: %w", err)
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tagsets WHERE origin_file_id IN `+in, args...); err != nil {
			return fmt.Errorf("hard delete: tagsets: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM files WHERE id IN `+in, args...); err != nil {
			return fmt.Errorf("hard delete: files: %w", err)
		}
	}
	for recID := range recIDs {
		if err := repairRecordingTx(ctx, tx, recID); err != nil {
			return err
		}
	}
	return nil
}

// repairRecordingTx re-establishes the recording invariant after members moved
// or died inside tx: a recording with no files left is removed (remaining
// tagsets cascade via FK); one that kept files and tagsets but lost its primary
// appearance promotes the oldest remaining tagset.
func repairRecordingTx(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM recordings WHERE id = ?
		   AND NOT EXISTS (SELECT 1 FROM files WHERE recording_id = ?)`,
		recordingID, recordingID)
	if err != nil {
		return fmt.Errorf("repair recording %d: delete: %w", recordingID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary = 1
		  WHERE id = (SELECT MIN(id) FROM tagsets WHERE recording_id = ?)
		    AND NOT EXISTS (SELECT 1 FROM tagsets WHERE recording_id = ? AND is_primary = 1)`,
		recordingID, recordingID); err != nil {
		return fmt.Errorf("repair recording %d: promote primary: %w", recordingID, err)
	}
	return nil
}

// hardDeleteTagsetsTx is the tagset-first hard-delete cascade (recording-tagsets
// P2) — the Trash permanent-delete op, symmetric to hardDeleteFilesTx. It
// removes the given tagset rows and enforces the hardlink invariant per
// recording: a recording that loses its LAST tagset is invalid (no appearance
// left) and is garbage-collected together with all of its files (rows + blobs +
// FK children), the deleted blobs returned so the caller can reclaim the bytes
// after commit; a recording that keeps tagsets but lost its primary promotes the
// oldest survivor. It deletes by recording membership (never origin_file_id), so
// it stays correct once appearances decouple from their origin file (absorb,
// P3). One tx — the single path every tagset hard-delete entry point goes
// through, so no second code path can strand a recording or a blob.
func hardDeleteTagsetsTx(ctx context.Context, tx *sql.Tx, tagsetIDs []int64) ([]DeletedBlob, error) {
	const chunk = 400
	recIDs := make(map[int64]struct{})
	// 1. Collect the affected recordings, then drop the tagset rows.
	for i := 0; i < len(tagsetIDs); i += chunk {
		end := min(i+chunk, len(tagsetIDs))
		batch := tagsetIDs[i:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = "?"
			args = append(args, id)
		}
		in := "(" + strings.Join(placeholders, ",") + ")"

		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT recording_id FROM tagsets WHERE id IN `+in, args...)
		if err != nil {
			return nil, fmt.Errorf("hard delete tagsets: recordings: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("hard delete tagsets: scan recording: %w", err)
			}
			recIDs[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("hard delete tagsets: recording rows: %w", err)
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tagsets WHERE id IN `+in, args...); err != nil {
			return nil, fmt.Errorf("hard delete tagsets: delete: %w", err)
		}
	}

	// 2. Per affected recording: GC it (with all its files) if it lost its last
	//    tagset, else re-promote a primary. GC collects blobs for reclamation.
	var blobs []DeletedBlob
	for recID := range recIDs {
		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tagsets WHERE recording_id = ?`, recID).Scan(&remaining); err != nil {
			return nil, fmt.Errorf("hard delete tagsets: count remaining: %w", err)
		}
		if remaining > 0 {
			if err := repairRecordingTx(ctx, tx, recID); err != nil {
				return nil, err
			}
			continue
		}
		// Last tagset gone → the recording is invalid: reclaim every file's blob,
		// then drop the recording (files first — files.recording_id has no
		// cascade; the recording delete then cascades any stray tagsets).
		recBlobs, err := deleteRecordingFilesTx(ctx, tx, recID)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, recBlobs...)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recordings WHERE id = ?`, recID); err != nil {
			return nil, fmt.Errorf("hard delete tagsets: delete recording %d: %w", recID, err)
		}
	}
	return blobs, nil
}

// deleteRecordingFilesTx deletes every files row of a recording and returns
// their blobs (hash + storage backend) for post-commit reclamation. FK cascade
// drops media_metadata / file_uploads / audio_fingerprints / source_files;
// tagsets.origin_file_id is SET NULL, so an appearance on another recording that
// merely read its tags from one of these blobs (absorb, P3) is never destroyed.
func deleteRecordingFilesTx(ctx context.Context, tx *sql.Tx, recordingID int64) ([]DeletedBlob, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT hash, storage_backend FROM files WHERE recording_id = ?`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("recording %d files: %w", recordingID, err)
	}
	var blobs []DeletedBlob
	for rows.Next() {
		var b DeletedBlob
		if err := rows.Scan(&b.Hash, &b.StorageBackend); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recording %d file scan: %w", recordingID, err)
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("recording %d file rows: %w", recordingID, err)
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM files WHERE recording_id = ?`, recordingID); err != nil {
		return nil, fmt.Errorf("delete recording %d files: %w", recordingID, err)
	}
	return blobs, nil
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

// SoftDeleteFileByHash marks the file as trashed by setting deleted_at on its
// tagset. The blob is left on disk; every row is preserved. Returns
// found=false (no error) when no live (non-trashed) row matches.
func (db *DB) SoftDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT f.id FROM files f
		WHERE f.hash = ? AND EXISTS (SELECT 1 FROM tagsets t WHERE t.origin_file_id = f.id AND t.deleted_at IS NULL)`,
		hash).Scan(&id)
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET deleted_at = ? WHERE origin_file_id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), id); err != nil {
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

// HardDeleteTrashedFileByHash permanently removes the trashed appearance of the
// file identified by hash — the Trash "Delete Forever" op (recording-tagsets
// P2). It cascades from the *tagset*: a non-last appearance drops just the
// tagset row (the blob stays — another appearance may still play it); the last
// appearance of a recording takes the recording and all its files with it, and
// those blobs come back in the returned slice for post-commit reclamation. The
// trashed-tagset guard makes the check and cascade atomic within one
// transaction, so a concurrent restore cannot race the delete. Returns
// found=false (no error) when the file has no trashed appearance.
func (db *DB) HardDeleteTrashedFileByHash(ctx context.Context, hash string) ([]string, []DeletedBlob, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var fileID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("select file id: %w", err)
	}

	tagsetIDs, err := scanIDs(tx.QueryContext(ctx,
		`SELECT id FROM tagsets WHERE origin_file_id = ? AND deleted_at IS NOT NULL`, fileID))
	if err != nil {
		return nil, nil, false, fmt.Errorf("select trashed tagsets: %w", err)
	}
	if len(tagsetIDs) == 0 {
		return nil, nil, false, nil
	}

	filenames, err := uploadFilenamesInTx(ctx, tx, fileID)
	if err != nil {
		return nil, nil, false, err
	}

	blobs, err := hardDeleteTagsetsTx(ctx, tx, tagsetIDs)
	if err != nil {
		return nil, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, blobs, true, nil
}

// hardDelete is the shared implementation for the two hard-delete variants; the
// row removal runs through the recording cascade (hardDeleteFilesTx).
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

	if err := hardDeleteFilesTx(ctx, tx, []int64{id}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// RestoreFileByHash clears the trash mark (tagsets.deleted_at) on a trashed
// file, returning it to its prior review state. Returns found=false (no error)
// when no trashed row matches.
func (db *DB) RestoreFileByHash(ctx context.Context, hash string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE tagsets SET deleted_at = NULL
		WHERE deleted_at IS NOT NULL AND origin_file_id IN (SELECT id FROM files WHERE hash = ?)`, hash)
	if err != nil {
		return false, fmt.Errorf("restore file: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// trashListSelect is the shared SELECT for the Trash listings (the trash
// timestamp / review state now live on the tagset; no disc_number, matching the
// historical trash payload). Keep in sync with scanTrashList.
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
		mm.duration_seconds,
		r.guest_playable,
		r.license,
		m.deleted_at,
		m.review_state,
		CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
		CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image
	FROM files f` + originTagsetJoin + `
	LEFT JOIN media_metadata mm ON mm.file_id = f.id
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
// m.deleted_at IS NOT NULL — the tagset carries the Trash) and binds for a
// FileFilter over the Trash bucket. It reuses qFieldClause + the artist/album
// pins so Trash matches identically to the live library, just over soft-deleted
// rows. Guest is never applied (Trash is admin-only).
func trashFilterWhere(f FileFilter) (string, []any) {
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

// trashSortOrder maps a sort token to a safe ORDER BY for the Trash list. It adds
// deletion-time orders (the Trash default) and otherwise delegates to the shared
// flat / grouped orders. Unknown tokens fall back to newest-deleted-first.
func trashSortOrder(token string) string {
	switch token {
	case "deleted_asc":
		return "m.deleted_at ASC, f.id ASC"
	case "deleted_desc":
		return "m.deleted_at DESC, f.id DESC"
	case "created_asc", "created_desc", "title_asc", "title_desc",
		"artist_asc", "artist_desc", "size_asc", "size_desc", "untagged_first", "grouped":
		return fileSortOrder(token)
	default:
		return "m.deleted_at DESC, f.id DESC"
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
		`SELECT COUNT(*) FROM files f`+originTagsetJoin+` WHERE `+where, args...).Scan(&n)
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
		`SELECT f.hash FROM files f`+originTagsetJoin+` WHERE `+where+` ORDER BY f.id`, args...)
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
// carry an empty slice. Trashed files (tagset in the Trash) are excluded so
// PruneDangling does not permanently delete files the admin placed in the trash
// intentionally; a file with no tagset at all (invariant violation) IS listed,
// so the prune sweep can flag and clean it.
func (db *DB) ListFileRefs(ctx context.Context) ([]FileRef, error) {
	const q = `
		SELECT f.hash, f.storage_backend, COALESCE(f.link_target, ''),
		       COALESCE(GROUP_CONCAT(u.filename, char(10)), '')
		FROM files f
		LEFT JOIN file_uploads u ON u.file_id = f.id
		WHERE EXISTS (SELECT 1 FROM tagsets t WHERE t.origin_file_id = f.id AND t.deleted_at IS NULL)
		   OR NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.origin_file_id = f.id)
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
