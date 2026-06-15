package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"daemonlord.ygg/madshare/media"
)

const (
	// maxBitErrorRate is the fingerprint match threshold: two files are the same
	// recording when their positional bit-error rate is at or below this. Chosen
	// conservatively — false negatives (a missed grouping) are preferred over a
	// wrong merge, and grouping is only ever a suggestion to a human (P2/P3),
	// never an auto-delete. See docs/architecture/recordings.md (Identity).
	maxBitErrorRate = 0.10
	// recordingDurationTolerance bounds the candidate shortlist: same-recording
	// renditions share a near-identical duration, so this both shrinks the scan
	// and avoids comparing structurally different audio.
	recordingDurationTolerance = 7.0 // seconds
)

// ResolveRecording assigns the file a recording_id: the recording of the closest
// matching fingerprint within threshold, or a brand-new recording when nothing
// matches. Idempotent and safe to re-run. Returns the assigned recording id, or
// 0 (no-op) when the file has no fingerprint (it stays its own implicit
// recording) or is pinned (a human split it — the resolver must never re-merge).
func (db *DB) ResolveRecording(ctx context.Context, fileID int64) (int64, error) {
	raw, dur, found, err := db.fileFingerprint(ctx, fileID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil // no fingerprint → its own implicit recording
	}
	pinned, err := db.fileRecordingPinned(ctx, fileID)
	if err != nil {
		return 0, err
	}
	if pinned {
		return 0, nil // human-pinned split; never re-merge
	}

	candidates, err := db.recordingShortlist(ctx, fileID, dur)
	if err != nil {
		return 0, err
	}
	bestRec, bestBER := int64(0), math.MaxFloat64
	for _, c := range candidates {
		ber := media.BitErrorRate(raw, media.DecodeFingerprint(c.fingerprint))
		if ber <= maxBitErrorRate && ber < bestBER {
			bestBER, bestRec = ber, c.recordingID
		}
	}

	if bestRec == 0 {
		if bestRec, err = db.createRecording(ctx, time.Now().Unix()); err != nil {
			return 0, err
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE files SET recording_id=? WHERE id=?`, bestRec, fileID,
	); err != nil {
		return 0, fmt.Errorf("assign recording: %w", err)
	}
	return bestRec, nil
}

// BackfillRecordings resolves every non-trashed, unpinned, fingerprinted file
// that has no recording_id yet — the idempotent startup pass for blobs whose
// fingerprints predate the overlay. Processed sequentially so each resolved file
// becomes a candidate for the next (the inline resolver in mediaproc covers new
// uploads). Returns the number of files processed.
func (db *DB) BackfillRecordings(ctx context.Context) (int, error) {
	ids, err := db.FilesNeedingRecording(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := db.ResolveRecording(ctx, id); err != nil {
			return 0, fmt.Errorf("resolve recording file=%d: %w", id, err)
		}
	}
	return len(ids), nil
}

// FilesNeedingRecording returns ids of non-trashed, unpinned files that have a
// fingerprint but no recording_id yet. A file without a fingerprint is skipped
// (nothing to resolve — it is its own implicit recording).
func (db *DB) FilesNeedingRecording(ctx context.Context) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.id
		   FROM files f
		   JOIN audio_fingerprints af ON af.file_id = f.id
		  WHERE f.deleted_at IS NULL
		    AND f.recording_pinned = 0
		    AND f.recording_id IS NULL
		  ORDER BY f.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("files needing recording: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("files needing recording: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) fileFingerprint(ctx context.Context, fileID int64) (raw []uint32, dur float64, found bool, err error) {
	var (
		blob []byte
		d    sql.NullFloat64
	)
	err = db.QueryRowContext(ctx,
		`SELECT fingerprint, duration FROM audio_fingerprints WHERE file_id=?`, fileID,
	).Scan(&blob, &d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("read fingerprint: %w", err)
	}
	return media.DecodeFingerprint(blob), d.Float64, true, nil
}

func (db *DB) fileRecordingPinned(ctx context.Context, fileID int64) (bool, error) {
	var pinned int
	if err := db.QueryRowContext(ctx,
		`SELECT recording_pinned FROM files WHERE id=?`, fileID,
	).Scan(&pinned); err != nil {
		return false, fmt.Errorf("read recording pin: %w", err)
	}
	return pinned == 1, nil
}

type recordingCandidate struct {
	recordingID int64
	fingerprint []byte
}

// recordingShortlist returns the fingerprints of other established recordings
// (recording_id set) within the duration tolerance — the set the new file is
// bit-compared against. A zero/unknown dur disables the duration filter.
func (db *DB) recordingShortlist(ctx context.Context, fileID int64, dur float64) ([]recordingCandidate, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.recording_id, af.fingerprint
		   FROM audio_fingerprints af
		   JOIN files f ON f.id = af.file_id
		  WHERE af.file_id != ?
		    AND f.deleted_at IS NULL
		    AND f.recording_id IS NOT NULL
		    AND (? = 0 OR af.duration IS NULL OR ABS(af.duration - ?) <= ?)`,
		fileID, dur, dur, recordingDurationTolerance,
	)
	if err != nil {
		return nil, fmt.Errorf("recording shortlist: %w", err)
	}
	defer rows.Close()
	var out []recordingCandidate
	for rows.Next() {
		var c recordingCandidate
		if err := rows.Scan(&c.recordingID, &c.fingerprint); err != nil {
			return nil, fmt.Errorf("recording shortlist: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) createRecording(ctx context.Context, now int64) (int64, error) {
	var id int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, now,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("create recording: %w", err)
	}
	return id, nil
}

// DuplicateRendition is a single rendition of a duplicate recording, enriched
// with the display + URL fields the duplicates admin page needs.
type DuplicateRendition struct {
	FileID    int64
	Hash      string
	ObjectKey string // "<hash>/<filename>"; the play URL is "/files/" + this
	Title     string
	// Raw tag fields (not coalesced) so the edit-tags modal prefills correctly;
	// the page derives the display artist as AlbumArtist || Artist.
	Artist          string
	AlbumArtist     string
	Album           string
	Codec           string
	MimeType        string
	Bitrate         int
	SampleRate      int
	BitDepth        int
	ByteSize        int64
	DurationSeconds float64
}

// DuplicateRecording is a recording with more than one live (non-trashed)
// rendition — the rows the duplicates admin page lists.
type DuplicateRecording struct {
	RecordingID int64
	Renditions  []DuplicateRendition
}

// ListDuplicateRecordings returns every recording with >1 non-trashed rendition,
// each with its renditions (tech info + display fields), ordered by recording id
// then file id. Single-rendition recordings (the norm) are excluded.
func (db *DB) ListDuplicateRecordings(ctx context.Context) ([]DuplicateRecording, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.recording_id, f.id, f.hash, f.object_key, f.byte_size, f.mime_type,
		        mm.title, COALESCE(mm.artist, ''), COALESCE(mm.album_artist, ''),
		        COALESCE(mm.album, ''),
		        COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		        COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		        COALESCE(mm.duration_seconds, 0)
		   FROM files f
		   JOIN media_metadata mm ON mm.file_id = f.id
		  WHERE f.recording_id IS NOT NULL
		    AND f.deleted_at IS NULL
		    AND f.recording_id IN (
		        SELECT recording_id FROM files
		         WHERE recording_id IS NOT NULL AND deleted_at IS NULL
		         GROUP BY recording_id HAVING COUNT(*) > 1)
		  ORDER BY f.recording_id, f.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list duplicate recordings: %w", err)
	}
	defer rows.Close()

	var (
		out    []DuplicateRecording
		curID  int64
		curIdx = -1
	)
	for rows.Next() {
		var (
			recID int64
			r     DuplicateRendition
		)
		if err := rows.Scan(&recID, &r.FileID, &r.Hash, &r.ObjectKey, &r.ByteSize,
			&r.MimeType, &r.Title, &r.Artist, &r.AlbumArtist, &r.Album, &r.Codec, &r.Bitrate,
			&r.SampleRate, &r.BitDepth, &r.DurationSeconds); err != nil {
			return nil, fmt.Errorf("list duplicate recordings: scan: %w", err)
		}
		if curIdx < 0 || recID != curID {
			out = append(out, DuplicateRecording{RecordingID: recID})
			curIdx++
			curID = recID
		}
		out[curIdx].Renditions = append(out[curIdx].Renditions, r)
	}
	return out, rows.Err()
}

// RecordingRenditionsByHash returns the approved, non-trashed renditions of the
// recording that the file with the given hash belongs to — the data the player's
// Auto/High/Low quality control walks (recordings P4). A file with no
// recording_id (no fingerprint / its own recording) yields just itself. An
// unknown / non-approved / trashed hash yields nil (the caller 404s).
func (db *DB) RecordingRenditionsByHash(ctx context.Context, hash string) ([]DuplicateRendition, error) {
	var (
		fileID int64
		recID  sql.NullInt64
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, recording_id FROM files
		  WHERE hash=? AND deleted_at IS NULL AND review_state='approved'`, hash,
	).Scan(&fileID, &recID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("renditions: load file: %w", err)
	}
	if recID.Valid {
		return db.renditionsWhere(ctx, "f.recording_id = ?", recID.Int64)
	}
	return db.renditionsWhere(ctx, "f.id = ?", fileID)
}

// renditionsWhere selects approved, non-trashed renditions matching cond (a
// single-arg WHERE fragment), ordered by file id. Shared by the by-hash lookup.
func (db *DB) renditionsWhere(ctx context.Context, cond string, arg int64) ([]DuplicateRendition, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.id, f.hash, f.object_key, f.byte_size, f.mime_type,
		        mm.title, COALESCE(mm.artist, ''), COALESCE(mm.album_artist, ''),
		        COALESCE(mm.album, ''),
		        COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		        COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		        COALESCE(mm.duration_seconds, 0)
		   FROM files f
		   JOIN media_metadata mm ON mm.file_id = f.id
		  WHERE f.deleted_at IS NULL AND f.review_state='approved' AND `+cond+`
		  ORDER BY f.id`,
		arg,
	)
	if err != nil {
		return nil, fmt.Errorf("renditions: query: %w", err)
	}
	defer rows.Close()
	var out []DuplicateRendition
	for rows.Next() {
		var r DuplicateRendition
		if err := rows.Scan(&r.FileID, &r.Hash, &r.ObjectKey, &r.ByteSize, &r.MimeType,
			&r.Title, &r.Artist, &r.AlbumArtist, &r.Album, &r.Codec, &r.Bitrate, &r.SampleRate,
			&r.BitDepth, &r.DurationSeconds); err != nil {
			return nil, fmt.Errorf("renditions: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SplitRendition detaches a file into its own brand-new recording and pins it so
// the resolver never re-merges it (the "save as another composition" action).
// found is false (no error) when no live file matches the id. Atomic.
func (db *DB) SplitRendition(ctx context.Context, fileID int64) (newRecordingID int64, found bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("split rendition: begin: %w", err)
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx,
		`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, time.Now().Unix(),
	).Scan(&newRecordingID); err != nil {
		return 0, false, fmt.Errorf("split rendition: create recording: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE files SET recording_id=?, recording_pinned=1 WHERE id=? AND deleted_at IS NULL`,
		newRecordingID, fileID,
	)
	if err != nil {
		return 0, false, fmt.Errorf("split rendition: reassign: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("split rendition: rows affected: %w", err)
	}
	if n == 0 {
		return 0, false, nil // no live file with that id; the new recording rolls back
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("split rendition: commit: %w", err)
	}
	return newRecordingID, true, nil
}

// IsDuplicateSubmission reports whether the file with the given hash duplicates
// content already approved in the library — the derived flag that suppresses a
// moderator's self-approve and highlights the moderation queue (recordings P3,
// docs/architecture/recordings.md). It is computed, never stored.
//
// When the file has an acoustic fingerprint, identity is its recording: flagged
// iff another approved, non-trashed file shares the same recording_id. When it
// has no fingerprint (fpcalc absent), it falls back to a tag collision on a
// non-default artist+album+title — untagged files, whose artist/album tag
// columns are NULL/empty, never collide. Returns false for an unknown/trashed
// hash.
func (db *DB) IsDuplicateSubmission(ctx context.Context, hash string) (bool, error) {
	var (
		fileID int64
		recID  sql.NullInt64
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, recording_id FROM files WHERE hash=? AND deleted_at IS NULL`, hash,
	).Scan(&fileID, &recID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("duplicate check: load file: %w", err)
	}

	var hasFP bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM audio_fingerprints WHERE file_id=?)`, fileID,
	).Scan(&hasFP); err != nil {
		return false, fmt.Errorf("duplicate check: fingerprint exists: %w", err)
	}

	if hasFP {
		// Fingerprint identity: another approved rendition in the same recording.
		if !recID.Valid {
			return false, nil
		}
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM files
			  WHERE recording_id=? AND id<>? AND deleted_at IS NULL AND review_state='approved'`,
			recID.Int64, fileID,
		).Scan(&n); err != nil {
			return false, fmt.Errorf("duplicate check: recording siblings: %w", err)
		}
		return n > 0, nil
	}

	// Tag-collision fallback (fpcalc absent). Only real, non-default tags
	// participate — a file missing its artist or album is never flagged.
	var (
		title         string
		artist, album sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT title, artist, album FROM media_metadata WHERE file_id=?`, fileID,
	).Scan(&title, &artist, &album); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("duplicate check: load tags: %w", err)
	}
	if title == "" || !artist.Valid || artist.String == "" || !album.Valid || album.String == "" {
		return false, nil
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files f
		   JOIN media_metadata mm ON mm.file_id=f.id
		  WHERE f.id<>? AND f.deleted_at IS NULL AND f.review_state='approved'
		    AND mm.title=? AND mm.artist=? AND mm.album=?`,
		fileID, title, artist.String, album.String,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("duplicate check: tag collision: %w", err)
	}
	return n > 0, nil
}

// Rendition is one file viewed as a member of a recording, with the tech fields
// the quality ladder ranks on. Zero-valued tech fields mean "unknown" (ffprobe
// absent or didn't report), in which case the ladder degrades to format + size.
type Rendition struct {
	FileID     int64
	Hash       string
	Codec      string // canonical codec (or format inferred from MIME, degraded)
	Bitrate    int
	SampleRate int
	BitDepth   int
	ByteSize   int64
}

// RankRenditions returns a copy of rs sorted best-first by the quality ladder
// (docs/architecture/recordings.md): codec class (lossless > lossy > unknown),
// then — branching on class — bitrate for lossy or sample-rate+bit-depth for
// lossless, with file size as the final tiebreak (and the only axis when tech
// info is unknown, the ffprobe-absent degraded path). Deterministic; no human
// picks the default.
func RankRenditions(rs []Rendition) []Rendition {
	out := make([]Rendition, len(rs))
	copy(out, rs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ca, cb := codecClass(a.Codec), codecClass(b.Codec)
		if ca != cb {
			return ca < cb // lossless(0) before lossy(1) before unknown(2)
		}
		switch ca {
		case classLossless:
			if a.SampleRate != b.SampleRate {
				return a.SampleRate > b.SampleRate
			}
			if a.BitDepth != b.BitDepth {
				return a.BitDepth > b.BitDepth
			}
		case classLossy:
			if a.Bitrate != b.Bitrate {
				return a.Bitrate > b.Bitrate
			}
		}
		return a.ByteSize > b.ByteSize // final tiebreak / unknown-class ordering
	})
	return out
}

const (
	classLossless = 0
	classLossy    = 1
	classUnknown  = 2
)

// codecClass maps a codec (or MIME-derived format token) to its ladder class.
// Unknown/empty codecs sort last so a probed rendition always outranks an
// unprobed one of otherwise-equal standing.
func codecClass(codec string) int {
	switch strings.ToLower(codec) {
	case "flac", "alac":
		return classLossless
	case "mp3", "aac", "vorbis", "opus", "wmav2", "ac3", "mp2":
		return classLossy
	default:
		return classUnknown
	}
}
