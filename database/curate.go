package database

// Recording curation operations (recording-tagsets P5) — the primitives behind
// /admin/library#recordings: the paged recording listing with both-arms detail, merge,
// appearance move / set-primary, whole-recording trash + hard delete, and the
// recording-level access edit. Each mutation is a single transaction; audit
// rows are written by the api layer. Design:
// docs/architecture/recording-tagsets.md (Admin surfaces, P5).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// ── Listing ───────────────────────────────────────────────────────────────────

// RecordingListOptions filters/pages the recordings admin listing. Newest first
// (id DESC — the rows an admin edits first). Filter is one of "",
// "multi_rendition", "multi_appearance", "dormant", "pinned"; an unknown value
// matches nothing rather than silently listing everything.
type RecordingListOptions struct {
	Search string
	Filter string
	Limit  int // <= 0 means no limit
	Offset int // < 0 clamps to 0
}

// RecordingRow is one collapsed card on /admin/library#recordings: identity, the
// primary appearance's display fields, and the count/state chips.
type RecordingRow struct {
	ID             int64
	Title          string // primary appearance's title
	DisplayArtist  string // primary's album_artist, falling back to artist
	LiveRenditions int    // non-removed files
	RemovedFiles   int    // soft-removed blobs (absorbed / removed renditions)
	Appearances    int    // non-trashed tagsets
	TrashedTagsets int
	BestFormat     string // ladder-best live rendition's codec (MIME fallback)
	Dormant        bool   // no live rendition — hidden from the library
	Pinned         bool   // holds a recording_pinned file (split/force-new/merge)
	License        string
	GuestPlayable  bool
	CreatedAt      int64
}

// recordingFilterClause maps a RecordingListOptions.Filter token onto its SQL
// predicate over the recordings row aliased `r`. Unknown non-empty tokens
// return a never-true predicate (a typo must not widen a listing).
func recordingFilterClause(filter string) string {
	switch filter {
	case "":
		return ""
	case "multi_rendition":
		return `(SELECT COUNT(*) FROM files f WHERE f.recording_id = r.id AND f.deleted_at IS NULL) > 1`
	case "multi_appearance":
		return `(SELECT COUNT(*) FROM tagsets t WHERE t.recording_id = r.id AND t.deleted_at IS NULL) > 1`
	case "dormant":
		return `NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id = r.id AND f.deleted_at IS NULL)`
	case "pinned":
		return `EXISTS (SELECT 1 FROM files f WHERE f.recording_id = r.id AND f.recording_pinned = 1)`
	case "trashed":
		// Recordings perspective of Trash (gc-model.md): wholly out of the
		// library — once had an approved appearance, but none is visible now (all
		// trashed and/or the recording is dormant). Reuses visibleTagset (alias
		// m) so "visible" means exactly what the library shows. Pure-draft
		// recordings (never approved) are excluded — those are moderation, not
		// trash.
		return `EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = r.id AND t.review_state = 'approved')
			AND NOT EXISTS (SELECT 1 FROM tagsets m WHERE m.recording_id = r.id AND ` + visibleTagset + `)`
	default:
		return `0`
	}
}

// recordingSearchClause builds the search predicate + binds for the listing.
// "#123" (or a bare integer) addresses a recording id exactly; anything else is
// a case-insensitive substring match over any appearance's title / artist /
// album-artist / album (any tagset, not just the primary — the admin searches
// for what they remember, whichever release it named).
func recordingSearchClause(q string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	idPart := strings.TrimPrefix(q, "#")
	if idPart != "" && strings.Trim(idPart, "0123456789") == "" {
		return "r.id = ?", []any{idPart}
	}
	like := likeEscaped(q)
	return `EXISTS (SELECT 1 FROM tagsets ts WHERE ts.recording_id = r.id AND (
		unicode_lower(COALESCE(ts.title,'')) LIKE unicode_lower(?) ESCAPE '\'
		OR unicode_lower(COALESCE(ts.artist,'')) LIKE unicode_lower(?) ESCAPE '\'
		OR unicode_lower(COALESCE(ts.album_artist,'')) LIKE unicode_lower(?) ESCAPE '\'
		OR unicode_lower(COALESCE(ts.album,'')) LIKE unicode_lower(?) ESCAPE '\'))`,
		[]any{like, like, like, like}
}

// recordingWhere combines the filter + search predicates into a WHERE clause
// (possibly empty) and its binds — shared by ListRecordings and CountRecordings
// so the page total always matches the rows.
func recordingWhere(opts RecordingListOptions) (string, []any) {
	var conds []string
	var args []any
	if c := recordingFilterClause(opts.Filter); c != "" {
		conds = append(conds, c)
	}
	if c, a := recordingSearchClause(opts.Search); c != "" {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListRecordings returns one page of the recordings admin listing, newest
// first. The derived representative appearance (live first, then the manual
// is_primary override, then oldest) provides the display fields; count
// subqueries fill the chips.
func (db *DB) ListRecordings(ctx context.Context, opts RecordingListOptions) ([]RecordingRow, error) {
	where, args := recordingWhere(opts)
	query := `
		SELECT r.id, r.created_at, COALESCE(r.license, ''), r.guest_playable,
		       COALESCE(pt.title, ''),
		       COALESCE(NULLIF(pt.album_artist, ''), NULLIF(pt.artist, ''), ''),
		       (SELECT COUNT(*) FROM files f WHERE f.recording_id = r.id AND f.deleted_at IS NULL),
		       (SELECT COUNT(*) FROM files f WHERE f.recording_id = r.id AND f.deleted_at IS NOT NULL),
		       (SELECT COUNT(*) FROM tagsets t WHERE t.recording_id = r.id AND t.deleted_at IS NULL),
		       (SELECT COUNT(*) FROM tagsets t WHERE t.recording_id = r.id AND t.deleted_at IS NOT NULL),
		       EXISTS (SELECT 1 FROM files f WHERE f.recording_id = r.id AND f.recording_pinned = 1),
		       COALESCE((SELECT COALESCE(NULLIF(mm2.codec, ''), f2.mime_type)
		          FROM files f2
		          LEFT JOIN media_metadata mm2 ON mm2.file_id = f2.id
		         WHERE f2.recording_id = r.id AND f2.deleted_at IS NULL
		         ORDER BY ` + renditionLadderOrder("f2", "mm2", "r.preferred_file_id") + `
		         LIMIT 1), '')
		  FROM recordings r
		  LEFT JOIN tagsets pt ON pt.id = (
		       SELECT t2.id FROM tagsets t2 WHERE t2.recording_id = r.id
		        ORDER BY (t2.deleted_at IS NULL) DESC, t2.is_primary DESC, t2.id ASC LIMIT 1)` +
		where + `
		 ORDER BY r.id DESC`
	if opts.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, opts.Limit, max(opts.Offset, 0))
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()
	var out []RecordingRow
	for rows.Next() {
		var rec RecordingRow
		var guest, pinned int
		if err := rows.Scan(&rec.ID, &rec.CreatedAt, &rec.License, &guest,
			&rec.Title, &rec.DisplayArtist,
			&rec.LiveRenditions, &rec.RemovedFiles, &rec.Appearances, &rec.TrashedTagsets,
			&pinned, &rec.BestFormat); err != nil {
			return nil, fmt.Errorf("list recordings: scan: %w", err)
		}
		rec.GuestPlayable = guest != 0
		rec.Pinned = pinned != 0
		rec.Dormant = rec.LiveRenditions == 0
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountRecordings returns the total matching the listing's filter + search —
// the page header count and the windowed scroller's extent.
func (db *DB) CountRecordings(ctx context.Context, opts RecordingListOptions) (int, error) {
	where, args := recordingWhere(opts)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recordings r`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count recordings: %w", err)
	}
	return n, nil
}

// ── Detail (both arms) ────────────────────────────────────────────────────────

// RecordingFile is one rendition row in the detail view — tech + state,
// including soft-removed blobs (the recordings page is where they are found
// and restored).
type RecordingFile struct {
	FileID          int64
	Hash            string
	ObjectKey       string
	MimeType        string
	Codec           string
	Bitrate         int
	SampleRate      int
	BitDepth        int
	ByteSize        int64
	DurationSeconds float64
	Removed         bool
	Pinned          bool
}

// RecordingAppearance is one tagset row in the detail view, including trashed
// appearances (state shown, restorable via Trash).
type RecordingAppearance struct {
	TagsetID    int64
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	DiscNumber  sql.NullInt64
	TrackNumber sql.NullInt64
	Year        sql.NullInt64
	ReviewState string
	Trashed     bool
	IsPrimary   bool
	CreatedAt   int64
}

// RecordingDetail is the expanded card: the recording's own fields plus both
// arms. Renditions are ladder-ordered (live best-first, then removed ones);
// appearances primary-first then oldest-first.
type RecordingDetail struct {
	ID              int64
	CreatedAt       int64
	License         string
	GuestPlayable   bool
	GuestManual     bool
	Pinned          bool
	PreferredFileID int64 // 0 = no manual preference
	Renditions      []RecordingFile
	Appearances     []RecordingAppearance
}

// GetRecordingDetail loads one recording with both arms; nil (no error) when
// the id is unknown.
func (db *DB) GetRecordingDetail(ctx context.Context, recordingID int64) (*RecordingDetail, error) {
	d := &RecordingDetail{ID: recordingID}
	var guest, manual int
	var preferred sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT r.created_at, COALESCE(r.license, ''), r.guest_playable,
		        r.guest_playable_manual, r.preferred_file_id
		   FROM recordings r WHERE r.id = ?`, recordingID,
	).Scan(&d.CreatedAt, &d.License, &guest, &manual, &preferred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recording detail: %w", err)
	}
	d.GuestPlayable = guest != 0
	d.GuestManual = manual != 0
	d.PreferredFileID = preferred.Int64

	rows, err := db.QueryContext(ctx,
		`SELECT f.id, f.hash, f.object_key, f.mime_type,
		        COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		        COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		        f.byte_size, COALESCE(mm.duration_seconds, 0),
		        f.deleted_at IS NOT NULL, f.recording_pinned
		   FROM files f
		   LEFT JOIN media_metadata mm ON mm.file_id = f.id
		  WHERE f.recording_id = ?
		  ORDER BY `+renditionLadderOrder("f", "mm",
			"(SELECT preferred_file_id FROM recordings WHERE id = f.recording_id)"),
		recordingID)
	if err != nil {
		return nil, fmt.Errorf("recording detail: files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f RecordingFile
		var removed, pinned int
		if err := rows.Scan(&f.FileID, &f.Hash, &f.ObjectKey, &f.MimeType,
			&f.Codec, &f.Bitrate, &f.SampleRate, &f.BitDepth,
			&f.ByteSize, &f.DurationSeconds, &removed, &pinned); err != nil {
			return nil, fmt.Errorf("recording detail: scan file: %w", err)
		}
		f.Removed = removed != 0
		f.Pinned = pinned != 0
		d.Renditions = append(d.Renditions, f)
		if f.Pinned {
			d.Pinned = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The ladder ORDER BY interleaves removed blobs by their tech; the page wants
	// live ones (playable) first, keeping ladder order within each group.
	sort.SliceStable(d.Renditions, func(i, j int) bool {
		return !d.Renditions[i].Removed && d.Renditions[j].Removed
	})

	tRows, err := db.QueryContext(ctx,
		`SELECT t.id, t.title, COALESCE(t.artist, ''), COALESCE(t.album_artist, ''),
		        COALESCE(t.album, ''), t.disc_number, t.track_number, t.year,
		        t.review_state, t.deleted_at IS NOT NULL, t.is_primary, t.created_at
		   FROM tagsets t
		  WHERE t.recording_id = ?
		  ORDER BY t.is_primary DESC, t.id ASC`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("recording detail: tagsets: %w", err)
	}
	defer tRows.Close()
	var flagged []bool
	for tRows.Next() {
		var a RecordingAppearance
		var trashed, primary int
		if err := tRows.Scan(&a.TagsetID, &a.Title, &a.Artist, &a.AlbumArtist,
			&a.Album, &a.DiscNumber, &a.TrackNumber, &a.Year,
			&a.ReviewState, &trashed, &primary, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("recording detail: scan tagset: %w", err)
		}
		a.Trashed = trashed != 0
		flagged = append(flagged, primary != 0)
		d.Appearances = append(d.Appearances, a)
	}
	if err := tRows.Err(); err != nil {
		return nil, err
	}
	// The reported primary is DERIVED (GC model): live first, then the manual
	// is_primary override, then oldest. If the flagged appearance is trashed,
	// the derived default silently takes over — no repair, no enforcement.
	best := -1
	for i := range d.Appearances {
		if best < 0 {
			best = i
			continue
		}
		a, b := d.Appearances[i], d.Appearances[best]
		switch {
		case a.Trashed != b.Trashed:
			if !a.Trashed {
				best = i
			}
		case flagged[i] != flagged[best]:
			if flagged[i] {
				best = i
			}
		case a.TagsetID < b.TagsetID:
			best = i
		}
	}
	if best >= 0 {
		d.Appearances[best].IsPrimary = true
	}
	return d, nil
}

// ── Merge ─────────────────────────────────────────────────────────────────────

// MergeOutcome reports what a merge did. Found is false (no error) when the
// target or any source recording is unknown, a source equals the target, or no
// sources were given — a stale selection the caller 404s.
type MergeOutcome struct {
	Found              bool
	SourcesMerged      int
	RenditionsMoved    int
	AppearancesMoved   int
	AppearancesDropped int // duplicate-identity / nameless source appearances
}

// MergeRecordings folds the source recordings into the target (the
// selection-based merge on /admin/library#recordings): renditions move over and are
// pinned (a manual merge is a human identity decision — the resolver must never
// regroup them), appearances move with identity dedup (the target's copy wins;
// nameless source appearances are dropped, same rules as absorb), the target
// keeps its primary and its license/guest values, and the emptied source rows
// are removed. One transaction; deliberately no single undo (reverse piecewise
// via Split off / Move).
func (db *DB) MergeRecordings(ctx context.Context, targetID int64, sourceIDs []int64) (MergeOutcome, error) {
	srcSet := make(map[int64]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		if id == targetID {
			return MergeOutcome{}, nil // target ticked as source → stale selection
		}
		srcSet[id] = struct{}{}
	}
	if len(srcSet) == 0 {
		return MergeOutcome{}, nil
	}
	// Deterministic source order: appearance dedup across sources must not
	// depend on map iteration (oldest recording's copy wins among the sources).
	sources := make([]int64, 0, len(srcSet))
	for id := range srcSet {
		sources = append(sources, id)
	}
	slices.Sort(sources)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MergeOutcome{}, fmt.Errorf("merge: begin: %w", err)
	}
	defer tx.Rollback()

	for _, id := range append([]int64{targetID}, sources...) {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, id).Scan(&exists); err != nil {
			return MergeOutcome{}, fmt.Errorf("merge: check recording %d: %w", id, err)
		}
		if !exists {
			return MergeOutcome{}, nil
		}
	}

	out := MergeOutcome{Found: true}

	// The target's appearance identities always win the dedup.
	targetApps, err := loadAppearances(ctx, tx, targetID)
	if err != nil {
		return MergeOutcome{}, err
	}
	keptKeys := make(map[appearanceKey]struct{}, len(targetApps))
	for _, a := range targetApps {
		keptKeys[a.key] = struct{}{}
	}

	for _, srcID := range sources {
		apps, err := loadAppearances(ctx, tx, srcID)
		if err != nil {
			return MergeOutcome{}, err
		}
		var dropIDs, moveIDs []int64
		for _, a := range apps {
			if _, dup := keptKeys[a.key]; dup || !a.meaningful {
				dropIDs = append(dropIDs, a.id)
				continue
			}
			keptKeys[a.key] = struct{}{}
			moveIDs = append(moveIDs, a.id)
		}
		if err := deleteTagsetIDsTx(ctx, tx, dropIDs); err != nil {
			return MergeOutcome{}, err
		}
		// Surviving appearances re-home; never primary (the target keeps its own).
		const chunk = 400
		for i := 0; i < len(moveIDs); i += chunk {
			end := min(i+chunk, len(moveIDs))
			batch := moveIDs[i:end]
			ph := make([]string, len(batch))
			args := make([]any, 0, len(batch)+1)
			args = append(args, targetID)
			for j, id := range batch {
				ph[j] = "?"
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE tagsets SET recording_id = ?, is_primary = 0
				  WHERE id IN (`+strings.Join(ph, ",")+`)`, args...); err != nil {
				return MergeOutcome{}, fmt.Errorf("merge: move tagsets: %w", err)
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET recording_id = ?, recording_pinned = 1
			  WHERE recording_id = ?`, targetID, srcID)
		if err != nil {
			return MergeOutcome{}, fmt.Errorf("merge: move files: %w", err)
		}
		moved, _ := res.RowsAffected()

		// The source is empty now; any stray tagsets cascade with the row.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recordings WHERE id = ?`, srcID); err != nil {
			return MergeOutcome{}, fmt.Errorf("merge: drop source %d: %w", srcID, err)
		}
		out.SourcesMerged++
		out.RenditionsMoved += int(moved)
		out.AppearancesMoved += len(moveIDs)
		out.AppearancesDropped += len(dropIDs)
	}

	// Belt-and-braces: the target gained members, but a merge of husks could
	// still leave it empty — let the scoped reap decide.
	if err := reapRecordingsTx(ctx, tx, []int64{targetID}); err != nil {
		return MergeOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return MergeOutcome{}, fmt.Errorf("merge: commit: %w", err)
	}
	return out, nil
}

// ── Appearance move / primary ─────────────────────────────────────────────────

// MoveTagsetOutcome reports a MoveTagset attempt. Exactly one of Moved /
// SameRecording / LastAppearance / Collides is set when Found; the refusals are
// outcomes (not errors) so the API can map them to specific responses.
type MoveTagsetOutcome struct {
	Found          bool // tagset and target recording both exist
	SameRecording  bool // no-op: already on the target
	LastAppearance bool // refused: its recording's only appearance (merge instead)
	Collides       bool // refused: identical appearance already on the target
	Moved          bool
}

// MoveTagset re-homes an appearance onto another existing recording (the
// appearance-level split — a mis-attached release moves to the audio it
// belongs to). Moving clears the manual is_primary override (its context was
// the old recording); both sides fall back to their derived representative.
// Moving the last appearance is refused —
// it would leave the source invisible with no catalog entry (that shape is a
// merge). An identical non-trashed appearance on the target refuses the move
// (nothing new to say — same rule as attach/absorb dedup).
func (db *DB) MoveTagset(ctx context.Context, tagsetID, targetRecordingID int64) (MoveTagsetOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: begin: %w", err)
	}
	defer tx.Rollback()

	var srcRec int64
	var album, albumArtist, disc, track sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT recording_id, album_id, album_artist_id, disc_number, track_number
		   FROM tagsets WHERE id = ?`, tagsetID,
	).Scan(&srcRec, &album, &albumArtist, &disc, &track)
	if errors.Is(err, sql.ErrNoRows) {
		return MoveTagsetOutcome{}, nil
	}
	if err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: load: %w", err)
	}
	var targetExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, targetRecordingID).Scan(&targetExists); err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: check target: %w", err)
	}
	if !targetExists {
		return MoveTagsetOutcome{}, nil
	}
	if srcRec == targetRecordingID {
		return MoveTagsetOutcome{Found: true, SameRecording: true}, nil
	}
	var srcCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tagsets WHERE recording_id = ?`, srcRec).Scan(&srcCount); err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: count source: %w", err)
	}
	if srcCount <= 1 {
		return MoveTagsetOutcome{Found: true, LastAppearance: true}, nil
	}
	// NULL-safe identity collision on the target (SQLite IS treats NULLs equal).
	var collides bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM tagsets t
		  WHERE t.recording_id = ? AND t.deleted_at IS NULL
		    AND t.album_id IS ? AND t.album_artist_id IS ?
		    AND t.disc_number IS ? AND t.track_number IS ?)`,
		targetRecordingID, album, albumArtist, disc, track).Scan(&collides); err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: collision check: %w", err)
	}
	if collides {
		return MoveTagsetOutcome{Found: true, Collides: true}, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET recording_id = ?, is_primary = 0 WHERE id = ?`,
		targetRecordingID, tagsetID); err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: move: %w", err)
	}
	if err := reapRecordingsTx(ctx, tx, []int64{srcRec}); err != nil {
		return MoveTagsetOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return MoveTagsetOutcome{}, fmt.Errorf("move tagset: commit: %w", err)
	}
	return MoveTagsetOutcome{Found: true, Moved: true}, nil
}

// SetPrimaryTagset makes the given appearance the one that names its recording.
// found is false when the tagset does not belong to the recording.
func (db *DB) SetPrimaryTagset(ctx context.Context, recordingID, tagsetID int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("set primary: begin: %w", err)
	}
	defer tx.Rollback()

	var belongs bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM tagsets WHERE id = ? AND recording_id = ?)`,
		tagsetID, recordingID).Scan(&belongs); err != nil {
		return false, fmt.Errorf("set primary: check: %w", err)
	}
	if !belongs {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary = 0 WHERE recording_id = ? AND is_primary = 1`,
		recordingID); err != nil {
		return false, fmt.Errorf("set primary: clear: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary = 1 WHERE id = ?`, tagsetID); err != nil {
		return false, fmt.Errorf("set primary: set: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("set primary: commit: %w", err)
	}
	return true, nil
}

// ── Whole-recording delete ────────────────────────────────────────────────────

// TrashRecording soft-deletes every non-trashed appearance of the recording —
// the whole-recording "Trash" (the recording goes dormant in the library but
// everything is restorable from Trash). Returns how many appearances were newly
// trashed; found is false when the recording is unknown. Already-fully-trashed
// recordings return (0, true, nil).
func (db *DB) TrashRecording(ctx context.Context, recordingID int64) (int, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("trash recording: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, recordingID).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("trash recording: check: %w", err)
	}
	if !exists {
		return 0, false, nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET deleted_at = ? WHERE recording_id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), recordingID)
	if err != nil {
		return 0, false, fmt.Errorf("trash recording: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("trash recording: commit: %w", err)
	}
	return int(n), true, nil
}

// BulkTrashRecordings trashes every appearance of each listed recording in one
// transaction — the bulk bar's "Trash selected". Returns recordings touched
// (unknown ids skipped) and appearances newly trashed.
func (db *DB) BulkTrashRecordings(ctx context.Context, recordingIDs []int64) (recordings, appearances int, err error) {
	if len(recordingIDs) == 0 {
		return 0, 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("bulk trash recordings: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for _, id := range recordingIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, id).Scan(&exists); err != nil {
			return 0, 0, fmt.Errorf("bulk trash recordings: check %d: %w", id, err)
		}
		if !exists {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = ? WHERE recording_id = ? AND deleted_at IS NULL`,
			now, id)
		if err != nil {
			return 0, 0, fmt.Errorf("bulk trash recordings: %d: %w", id, err)
		}
		n, _ := res.RowsAffected()
		recordings++
		appearances += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("bulk trash recordings: commit: %w", err)
	}
	return recordings, appearances, nil
}

// RecordingDeleteOutcome reports a whole-recording hard delete: what died, and
// the blobs to reclaim after commit.
type RecordingDeleteOutcome struct {
	Found       bool
	Appearances int
	Files       int
	Blobs       []DeletedBlob
}

// HardDeleteRecording permanently removes the recording with all of its
// appearances and files — the count-aware "Delete permanently" behind the
// confirm that spelled these numbers out. It routes through the purge
// composition (purgeTagsetsTx): dropping every appearance abandons the
// recording, so the reclaim takes its files and husk with it; blobs are
// returned for post-commit reclamation.
func (db *DB) HardDeleteRecording(ctx context.Context, recordingID int64) (RecordingDeleteOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RecordingDeleteOutcome{}, fmt.Errorf("delete recording: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, recordingID).Scan(&exists); err != nil {
		return RecordingDeleteOutcome{}, fmt.Errorf("delete recording: check: %w", err)
	}
	if !exists {
		return RecordingDeleteOutcome{}, nil
	}
	out := RecordingDeleteOutcome{Found: true}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE recording_id = ?`, recordingID).Scan(&out.Files); err != nil {
		return RecordingDeleteOutcome{}, fmt.Errorf("delete recording: count files: %w", err)
	}
	tagsetIDs, err := scanIDs(tx.QueryContext(ctx,
		`SELECT id FROM tagsets WHERE recording_id = ?`, recordingID))
	if err != nil {
		return RecordingDeleteOutcome{}, fmt.Errorf("delete recording: tagsets: %w", err)
	}
	out.Appearances = len(tagsetIDs)

	if len(tagsetIDs) > 0 {
		// The purge composition: dropping every appearance leaves the recording
		// abandoned, so the reclaim takes its files, blobs, and husk with it.
		out.Blobs, err = purgeTagsetsTx(ctx, tx, tagsetIDs)
		if err != nil {
			return RecordingDeleteOutcome{}, err
		}
	} else {
		// Already appearance-less (garbage): reclaim files + husk directly.
		out.Blobs, err = reclaimAbandonedRecordingsTx(ctx, tx, []int64{recordingID})
		if err != nil {
			return RecordingDeleteOutcome{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RecordingDeleteOutcome{}, fmt.Errorf("delete recording: commit: %w", err)
	}
	return out, nil
}

// ── Access ────────────────────────────────────────────────────────────────────

// SetRecordingAccess updates the recording-level access fields (the editable
// license/guest chip on /admin/library#recordings). nil leaves a field unchanged; an
// empty license clears it. Setting guest marks the manual override, so the
// license auto-derive policy never overrides an explicit admin decision — the
// same semantics as the hash-addressed setters (auth.md §5.1).
func (db *DB) SetRecordingAccess(ctx context.Context, recordingID int64, license *string, guest *bool) (bool, error) {
	var sets []string
	var args []any
	if license != nil {
		var lic sql.NullString
		if *license != "" {
			lic = sql.NullString{String: *license, Valid: true}
		}
		sets = append(sets, "license = ?")
		args = append(args, lic)
	}
	if guest != nil {
		sets = append(sets, "guest_playable = ?", "guest_playable_manual = 1")
		args = append(args, boolToInt(*guest))
	}
	if len(sets) == 0 {
		var exists bool
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, recordingID).Scan(&exists)
		return exists, err
	}
	args = append(args, recordingID)
	res, err := db.ExecContext(ctx,
		`UPDATE recordings SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return false, fmt.Errorf("set recording access: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── Trashed-appearance restore / permanent delete ──────────────────────────────

// RestoreTagset clears a single appearance's trash mark (tagsets.deleted_at),
// bringing it back into the library — the tagset-addressed inverse of the
// discard (BulkTrashTagsets) the recordings view uses for per-appearance Remove.
// review_state is left untouched — restore re-enters the prior review state
// (moderation.md). Returns found=false (no error) when no trashed tagset
// matches the id.
func (db *DB) RestoreTagset(ctx context.Context, tagsetID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE tagsets SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`,
		tagsetID)
	if err != nil {
		return false, fmt.Errorf("restore tagset: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HardDeleteTagsetOutcome reports a single-appearance permanent delete: whether
// the id exists at all, whether it was trashed (purge only touches trashed
// rows), and the blobs to reclaim after commit (non-empty only when this was
// the recording's last appearance, so the purge reclaimed the abandoned
// recording and its files).
type HardDeleteTagsetOutcome struct {
	Found   bool
	Trashed bool
	Blobs   []DeletedBlob
}

// HardDeleteTrashedTagset permanently removes one trashed appearance through
// the shared purge (purgeTagsetsTx): the recording keeps its other appearances,
// or — if this was the last one — its files are reclaimed and the husk reaped,
// the blobs returned for post-commit reclamation. It refuses a live appearance
// (Found && !Trashed): permanent delete is a Trash-only op, so the caller must
// trash it first (a live appearance is removed via the whole-recording delete
// or MoveTagset instead).
func (db *DB) HardDeleteTrashedTagset(ctx context.Context, tagsetID int64) (HardDeleteTagsetOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return HardDeleteTagsetOutcome{}, fmt.Errorf("delete tagset: begin: %w", err)
	}
	defer tx.Rollback()

	var deleted sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT deleted_at FROM tagsets WHERE id = ?`, tagsetID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return HardDeleteTagsetOutcome{}, nil
	}
	if err != nil {
		return HardDeleteTagsetOutcome{}, fmt.Errorf("delete tagset: lookup: %w", err)
	}
	out := HardDeleteTagsetOutcome{Found: true, Trashed: deleted.Valid}
	if !deleted.Valid {
		return out, nil // refuse: live appearance, trash it first
	}

	out.Blobs, err = purgeTagsetsTx(ctx, tx, []int64{tagsetID})
	if err != nil {
		return HardDeleteTagsetOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return HardDeleteTagsetOutcome{}, fmt.Errorf("delete tagset: commit: %w", err)
	}
	return out, nil
}

// ── Manual appearance creation (recording-tagsets P7d) ────────────────────────

// AppearanceInput carries the descriptive tags for a hand-authored appearance —
// the /admin/library#recordings "Add appearance" form. Blank optional fields resolve to
// the reserved Unknown-artist / Other buckets, exactly as an untagged upload
// does. Numeric fields are pointers so "unset" is distinct from 0.
type AppearanceInput struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genre       string
	Composer    string
	Comment     string
	Year        *int64
	TrackNumber *int64
	TrackTotal  *int64
	DiscNumber  *int64
}

// CreateAppearanceOutcome reports a CreateAppearance attempt. Exactly one of the
// refusals is set when !TagsetID; the API maps them to specific responses.
type CreateAppearanceOutcome struct {
	TagsetID   int64 // >0 on success
	NotFound   bool  // the recording does not exist
	Nameless   bool  // refused: resolves to Unknown artist / Other (meaningful rule)
	Collides   bool  // refused: an identical live appearance already exists here
	EmptyTitle bool  // refused: title is required non-empty (migration 016)
}

// CreateAppearance adds a hand-authored appearance to an existing recording —
// the appearance-level analogue of an upload's offered tagset, minus the file.
// It carries no blob (origin_file_id NULL: provenance is "the file these tags
// were read from", and these were typed), plays the recording's ladder-best
// rendition like any other appearance, and is born **approved**: a moderator
// curating on /admin/library#recordings is the reviewer, and a blobless draft would be
// invisible in the review queue (which INNER-joins files) — so review would
// strand it. It is never primary (SetPrimaryTagset promotes it deliberately).
//
// Refusals mirror the neighbouring curation ops: the meaningful-tagset rule
// (don't manufacture a nameless Unknown-artist appearance — that rule exists to
// stop absorb/merge growing the bucket, and applies doubly to a manual add) and
// the NULL-safe identity dedup (an identical live appearance already says this).
func (db *DB) CreateAppearance(ctx context.Context, recordingID int64, in AppearanceInput, createdBy sql.NullInt64) (CreateAppearanceOutcome, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return CreateAppearanceOutcome{EmptyTitle: true}, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, recordingID).Scan(&exists); err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: check recording: %w", err)
	}
	if !exists {
		return CreateAppearanceOutcome{NotFound: true}, nil
	}

	year := 0
	if in.Year != nil {
		year = int(*in.Year)
	}
	albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, AlbumArtistTags{
		Artist: in.Artist, AlbumArtist: in.AlbumArtist, Album: in.Album, Year: year,
	})
	if err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: resolve entities: %w", err)
	}

	// Meaningful rule: refuse an appearance that resolves to Unknown artist AND
	// Other — it says nothing. Reuses the reserved-bucket check by name (the same
	// exact test loadAppearances uses), not a heuristic.
	var nameless bool
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(aa.name,'') = ? AND COALESCE(ar.name,'') = ? AND COALESCE(al.title,'') = ?
		   FROM (SELECT 1) x
		   LEFT JOIN artists aa ON aa.id = ?
		   LEFT JOIN artists ar ON ar.id = ?
		   LEFT JOIN albums  al ON al.id = ?`,
		DefaultArtistName, DefaultArtistName, DefaultAlbumTitle,
		albumArtistID, trackArtistID, albumID).Scan(&nameless); err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: meaningful check: %w", err)
	}
	if nameless {
		return CreateAppearanceOutcome{Nameless: true}, nil
	}

	// NULL-safe identity dedup on live appearances of this recording — the same
	// (album_id, album_artist_id, disc, track) key MoveTagset/absorb use.
	disc := nullPtrInt(in.DiscNumber)
	track := nullPtrInt(in.TrackNumber)
	var collides bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM tagsets t
		  WHERE t.recording_id = ? AND t.deleted_at IS NULL
		    AND t.album_id IS ? AND t.album_artist_id IS ?
		    AND t.disc_number IS ? AND t.track_number IS ?)`,
		recordingID, sql.NullInt64{Int64: albumID, Valid: albumID != 0},
		sql.NullInt64{Int64: albumArtistID, Valid: albumArtistID != 0}, disc, track).Scan(&collides); err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: collision check: %w", err)
	}
	if collides {
		return CreateAppearanceOutcome{Collides: true}, nil
	}

	var tagsetID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO tagsets (
			recording_id, title, artist, album_artist, album, genre, year,
			track_number, track_total, disc_number, composer, comment,
			artist_id, album_artist_id, album_id,
			review_state, created_by, origin_file_id, is_primary, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'approved', ?, NULL, 0, ?)
		RETURNING id`,
		recordingID, title,
		nullStr(strings.TrimSpace(in.Artist)), nullStr(strings.TrimSpace(in.AlbumArtist)), nullStr(strings.TrimSpace(in.Album)), nullStr(strings.TrimSpace(in.Genre)),
		nullPtrInt(in.Year), track, nullPtrInt(in.TrackTotal), disc, nullStr(strings.TrimSpace(in.Composer)), nullStr(strings.TrimSpace(in.Comment)),
		nullI64(trackArtistID), nullI64(albumArtistID), nullI64(albumID),
		createdBy, time.Now().Unix(),
	).Scan(&tagsetID); err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return CreateAppearanceOutcome{}, fmt.Errorf("create appearance: commit: %w", err)
	}
	return CreateAppearanceOutcome{TagsetID: tagsetID}, nil
}

// nullPtrInt maps an optional manual-input number to its nullable column form:
// an unset (nil) value → NULL. Blank strings and 0 entity ids reuse the package
// helpers nullStr / nullI64.
func nullPtrInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
