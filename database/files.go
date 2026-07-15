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

// tagsetJoin binds the aliases the shared predicates (visibleFile,
// accessClause, qFieldClause) expect around a files row aliased `f`: `m` is the
// appearance the file's recording is displayed under (reprTagset) and `r` its
// recording (access/license). INNER joins on purpose: every file has a
// recording (NOT NULL) and a recording without any tagset is garbage the
// reaper quarantines (GC model) — its files leave the live surfaces anyway,
// so nothing reachable is dropped by the join.
const tagsetJoin = `
	JOIN tagsets m ON m.id = ` + reprTagset + `
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
		LEFT JOIN tagsets t ON t.id = `+reprTagset+`
		WHERE f.storage_backend <> 'links'`,
	).Scan(&b.Library, &b.Review, &b.Trash)
	if err != nil {
		return b, fmt.Errorf("storage byte breakdown: %w", err)
	}
	return b, nil
}

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload". The lifecycle fields
// are derived from the blob's representative appearance (reprTagset — the
// blob's own offered appearance first, else its recording's), so a trashed
// catalog reads back as a trashed file for the restore/dedup flows. DeletedAt
// additionally reflects a soft-removed rendition (files.deleted_at): either
// mark means the bytes are not serving and a re-upload may restore them. A
// recording with no tagset at all (garbage awaiting the reaper) reads as
// approved so the dedup path still short-circuits.
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT f.id, f.hash, f.byte_size, f.mime_type, f.storage_backend, f.object_key, f.link_target,
		       f.created_at, f.recording_id, COALESCE(f.deleted_at, t.deleted_at),
		       COALESCE(t.review_state, 'approved'), t.review_note, t.submitted_at
		FROM files f
		LEFT JOIN tagsets t ON t.id = ` + reprTagset + `
		WHERE f.hash = ?
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
// f.ReviewState and owned by f.UploadedBy; is_primary stays 0 — the flag is a
// manual preference, the representative appearance is derived), and the tech
// media_metadata row. f.ID and f.RecordingID are populated on success.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
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

// DeletedBlob identifies one hard-deleted file's physical bytes for post-commit
// reclamation: Hash locates the blob and StorageBackend selects the reclaim path
// (local DeleteAll of a hash dir vs unlinking a links symlink). It is the batch
// counterpart to the (GetFileByHash → storage_backend) lookup the single-hash
// delete does before the row is gone.
type DeletedBlob struct {
	Hash           string
	StorageBackend string
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

// HardDeleteFileByHash permanently removes any files row identified by hash,
// regardless of trash state. Used by PruneDangling, which must be able to
// clean up both live and trashed rows whose blobs are gone.
// Returns found=false (no error) when no row matches.
func (db *DB) HardDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	return db.hardDelete(ctx, `SELECT id FROM files WHERE hash = ?`, hash)
}

// hardDelete is the shared implementation for the two hash-addressed
// hard-delete variants (the prune blob-loss path): purge the file row —
// regardless of trash state, its bytes are gone so the row is a lie — then
// reap the touched recordings in the same transaction (GC model: drafts died
// with their origin blob inside the purge; approved appearances keep living,
// their provenance pointer now NULL; an emptied recording's appearances are
// trashed, never destroyed).
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

	// The caller (prune) reclaims the blob itself; the returned blobs are not
	// needed here.
	_, recIDs, err := deleteFileRowsTx(ctx, tx, []int64{id})
	if err != nil {
		return nil, false, err
	}
	if err := reapRecordingsTx(ctx, tx, recIDs); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// RestoreFileByHash brings re-offered bytes back from the Trash. The entry
// point is hash-addressed (an upload only knows its bytes); the rows it acts
// on are found via the recording edge, never origin_file_id (GC model P3):
// every trashed appearance of the blob's recording is restored to its prior
// review state, and the rendition itself is revived (files.deleted_at
// cleared) so the bytes serve again. Returns found=false (no error) when
// nothing was in the Trash.
func (db *DB) RestoreFileByHash(ctx context.Context, hash string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("restore file: begin: %w", err)
	}
	defer tx.Rollback()

	resT, err := tx.ExecContext(ctx, `
		UPDATE tagsets SET deleted_at = NULL
		WHERE deleted_at IS NOT NULL
		  AND recording_id = (SELECT recording_id FROM files WHERE hash = ?)`, hash)
	if err != nil {
		return false, fmt.Errorf("restore file: appearances: %w", err)
	}
	resF, err := tx.ExecContext(ctx, `
		UPDATE files SET deleted_at = NULL
		WHERE deleted_at IS NOT NULL AND hash = ?`, hash)
	if err != nil {
		return false, fmt.Errorf("restore file: rendition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("restore file: commit: %w", err)
	}
	nT, _ := resT.RowsAffected()
	nF, _ := resF.RowsAffected()
	return nT+nF > 0, nil
}

// ListFileRefs returns one FileRef per live (non-removed) files row with the
// filenames recorded for it, ordered by file id. Files with no upload rows
// carry an empty slice. Soft-removed renditions (files.deleted_at — the
// Trash › Files quarantine) are excluded so PruneDangling never permanently
// deletes what an admin placed in the Trash; their rows are the purge path's
// business, not the sweep's (GC model P3).
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
