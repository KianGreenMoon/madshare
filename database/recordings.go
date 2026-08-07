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

// ResolveRecording re-homes the file onto the recording of the closest matching
// fingerprint within threshold. Every file already owns a (usually singleton)
// recording from insert time, so a match *moves* the file — together with its
// offered tagsets — into the matched recording and garbage-collects the emptied
// one; no match leaves the file where it is. Idempotent and safe to re-run.
// Returns the file's (possibly new) recording id, or 0 (no-op) when the file
// has no fingerprint or is pinned (a human split it — the resolver must never
// re-merge).
func (db *DB) ResolveRecording(ctx context.Context, fileID int64) (int64, error) {
	raw, dur, found, err := db.fileFingerprint(ctx, fileID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil // no fingerprint → stays on its own singleton recording
	}
	pinned, currentRec, err := db.fileRecordingState(ctx, fileID)
	if err != nil {
		return 0, err
	}
	if pinned {
		return 0, nil // human-pinned split; never re-merge
	}

	candidates, err := db.recordingShortlist(ctx, fileID, currentRec, dur)
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
	if bestRec == 0 || bestRec == currentRec {
		return currentRec, nil // no match (or already grouped): nothing to move
	}

	// Move the file and its offered tagsets into the matched recording, then
	// repair the one it left (usually: delete the emptied singleton). One tx.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("resolve recording: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET recording_id=? WHERE id=?`, bestRec, fileID,
	); err != nil {
		return 0, fmt.Errorf("assign recording: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET recording_id = ? WHERE origin_file_id = ?`, bestRec, fileID,
	); err != nil {
		return 0, fmt.Errorf("resolve recording: move tagsets: %w", err)
	}
	// The audio has left. Anything still on the old recording — a hand-authored
	// appearance (origin NULL), or one MoveTagset re-homed here while its origin
	// blob stayed elsewhere — describes audio that is now over there, so it goes
	// too. `origin_file_id` is provenance, not structure (recording-tagsets P7);
	// this was the last place still moving appearances by it.
	//
	// Without this the rows are left on a recording with no file rows, where
	// reaper P2 trashes them — and restoring is futile, because the next reaper
	// run trashes them again. That bounce is why quarantining them is not an
	// acceptable outcome for an UNATTENDED path: the background analysis worker
	// runs this, so nobody is watching, and the startup backfill can regroup a
	// whole library at once the first time fpcalc is installed.
	//
	// Identity collisions on the target are allowed rather than deduped: this
	// path has no human to refuse to, and destroying a curated row to keep the
	// identity set tidy is the trade the wrong way round. /admin/duplicates is
	// the cleanup surface (see the standing note that resolver moves do not
	// enforce identity dedup). is_primary is cleared — the target keeps its own.
	var filesLeft int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE recording_id = ?`, currentRec,
	).Scan(&filesLeft); err != nil {
		return 0, fmt.Errorf("resolve recording: count renditions: %w", err)
	}
	if filesLeft == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET recording_id = ?, is_primary = 0 WHERE recording_id = ?`,
			bestRec, currentRec,
		); err != nil {
			return 0, fmt.Errorf("resolve recording: move stranded appearances: %w", err)
		}
	}
	if err := reapRecordingsTx(ctx, tx, []int64{currentRec}); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("resolve recording: commit: %w", err)
	}
	return bestRec, nil
}

// scanIDs drains a single-int64-column query result.
func scanIDs(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
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

func (db *DB) fileRecordingState(ctx context.Context, fileID int64) (pinned bool, recordingID int64, err error) {
	var p int
	if err := db.QueryRowContext(ctx,
		`SELECT recording_pinned, recording_id FROM files WHERE id=?`, fileID,
	).Scan(&p, &recordingID); err != nil {
		return false, 0, fmt.Errorf("read recording state: %w", err)
	}
	return p == 1, recordingID, nil
}

type recordingCandidate struct {
	recordingID int64
	fingerprint []byte
}

// recordingShortlist returns the fingerprints of files on *other* recordings
// within the duration tolerance — the set the file is bit-compared against.
// A zero/unknown dur disables the duration filter.
func (db *DB) recordingShortlist(ctx context.Context, fileID, currentRec int64, dur float64) ([]recordingCandidate, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.recording_id, af.fingerprint
		   FROM audio_fingerprints af
		   JOIN files f ON f.id = af.file_id
		  WHERE af.file_id != ?
		    AND f.deleted_at IS NULL
		    AND f.recording_id <> ?
		    AND (? = 0 OR af.duration IS NULL OR ABS(af.duration - ?) <= ?)`,
		fileID, currentRec, dur, dur, recordingDurationTolerance,
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
		        t.title, COALESCE(t.artist, ''), COALESCE(t.album_artist, ''),
		        COALESCE(t.album, ''),
		        COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		        COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		        COALESCE(mm.duration_seconds, 0)
		   FROM files f
		   JOIN tagsets t ON t.origin_file_id = f.id AND t.deleted_at IS NULL
		   LEFT JOIN media_metadata mm ON mm.file_id = f.id
		  WHERE f.deleted_at IS NULL
		    AND f.recording_id IN (
		        SELECT f2.recording_id FROM files f2
		         JOIN tagsets t2 ON t2.origin_file_id = f2.id AND t2.deleted_at IS NULL
		         WHERE f2.deleted_at IS NULL
		         GROUP BY f2.recording_id HAVING COUNT(*) > 1)
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

// RecordingRenditionsByTagsetID returns the surviving renditions of the
// appearance's recording — the data the player's quality control walks. The
// display fields come from the addressed tagset (renditions are interchangeable
// audio; the appearance the user entered from names them). An unknown or
// unavailable (trashed / unapproved / dormant) tagset yields nil (the caller
// 404s).
func (db *DB) RecordingRenditionsByTagsetID(ctx context.Context, tagsetID int64) ([]DuplicateRendition, error) {
	var (
		recID                      int64
		title                      string
		artist, albumArtist, album sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT m.recording_id, m.title, m.artist, m.album_artist, m.album
		   FROM tagsets m
		  WHERE m.id = ? AND `+visibleTagset, tagsetID,
	).Scan(&recID, &title, &artist, &albumArtist, &album)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("renditions: load tagset: %w", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT f.id, f.hash, f.object_key, f.byte_size, f.mime_type,
		        COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		        COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		        COALESCE(mm.duration_seconds, 0)
		   FROM files f
		   LEFT JOIN media_metadata mm ON mm.file_id = f.id
		  WHERE f.recording_id = ? AND f.deleted_at IS NULL
		  ORDER BY f.id`,
		recID,
	)
	if err != nil {
		return nil, fmt.Errorf("renditions: query: %w", err)
	}
	defer rows.Close()
	var out []DuplicateRendition
	for rows.Next() {
		r := DuplicateRendition{Title: title, Artist: artist.String, AlbumArtist: albumArtist.String, Album: album.String}
		if err := rows.Scan(&r.FileID, &r.Hash, &r.ObjectKey, &r.ByteSize, &r.MimeType,
			&r.Codec, &r.Bitrate, &r.SampleRate, &r.BitDepth, &r.DurationSeconds); err != nil {
			return nil, fmt.Errorf("renditions: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SplitRenditionOutcome reports a SplitRendition attempt. Found is false (no
// error) when no live file matches the id; StrandedAppearances > 0 is a refusal
// (an outcome, not an error, so the API can answer it specifically) and nothing
// was changed.
type SplitRenditionOutcome struct {
	NewRecordingID int64
	Found          bool
	// StrandedAppearances counts the appearances the split would have orphaned:
	// rows this recording holds that are not read from the departing blob. See
	// SplitRendition for why that is refused rather than resolved.
	StrandedAppearances int
}

// SplitRendition detaches a file into its own brand-new recording and pins it so
// the resolver never re-merges it (the "save as another composition" action).
// The file's offered tagsets move with it (its appearance follows the audio),
// becoming the new recording's primary; the recording it left is repaired
// (primary re-promoted; removed if the split emptied it). When the file has no
// tagset of its own (an absorbed rendition — recording-tagsets P3), the new
// recording instead takes a *copy* of the source recording's primary appearance
// so it stays browsable (the moderator fixes its tags afterward). Atomic.
//
// It REFUSES when it would take the recording's last rendition away while
// appearances remain that are not read from that blob — a hand-authored
// appearance (CreateAppearance, origin NULL) or one that MoveTagset re-homed
// here while its origin blob stayed elsewhere. Those rows describe this
// recording's audio, and the split takes all of it, so they would be left on a
// recording with no file rows: reaper P2 then trashes them, and restoring is
// futile because the next reaper run trashes them again. The moderator has to
// break that loop by hand (Move… them onto a recording that still has a
// rendition), so we ask before creating it rather than after.
//
// Refusing rather than moving them along is deliberate, and it is where this
// differs from ResolveRecording, which does move them: a split ASSERTS the
// rendition is a different composition, so carrying a curator's hand-added
// appearance across would file it under the very composition the moderator just
// declared separate. The resolver has fingerprint proof of the opposite (same
// audio) and no human to ask. Owner decision, 2026-08-07.
func (db *DB) SplitRendition(ctx context.Context, fileID int64) (SplitRenditionOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: begin: %w", err)
	}
	defer tx.Rollback()

	var oldRecordingID int64
	err = tx.QueryRowContext(ctx,
		`SELECT f.recording_id FROM files f WHERE f.id = ? AND f.deleted_at IS NULL`,
		fileID,
	).Scan(&oldRecordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return SplitRenditionOutcome{}, nil // no live file with that id
	}
	if err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: load file: %w", err)
	}

	// Would this empty the source? Count file ROWS, not live ones: a recording
	// keeping a soft-removed rendition is dormant, not a husk, so reaper P2 never
	// fires on it and its appearances stay put.
	var filesLeft int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE recording_id = ? AND id <> ?`, oldRecordingID, fileID,
	).Scan(&filesLeft); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: count renditions: %w", err)
	}
	if filesLeft == 0 {
		var stranded int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tagsets
			  WHERE recording_id = ? AND deleted_at IS NULL
			    AND (origin_file_id IS NULL OR origin_file_id <> ?)`, oldRecordingID, fileID,
		).Scan(&stranded); err != nil {
			return SplitRenditionOutcome{}, fmt.Errorf("split rendition: count stranded: %w", err)
		}
		if stranded > 0 {
			return SplitRenditionOutcome{Found: true, StrandedAppearances: stranded}, nil
		}
	}

	var newRecordingID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, time.Now().Unix(),
	).Scan(&newRecordingID); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: create recording: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET recording_id=?, recording_pinned=1 WHERE id=?`,
		newRecordingID, fileID,
	); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: reassign: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET recording_id=? WHERE origin_file_id=?`,
		newRecordingID, fileID,
	); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: move tagsets: %w", err)
	}
	// Tagset-less split (the file carried no appearance of its own): copy the
	// source recording's representative appearance (derived: live first, then
	// the manual is_primary override, then oldest — GC model P3) so the new
	// recording is browsable.
	var newTagsetCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, newRecordingID,
	).Scan(&newTagsetCount); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: count tagsets: %w", err)
	}
	if newTagsetCount == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tagsets (recording_id, title, artist, album_artist, album, genre, year,
				track_number, track_total, disc_number, composer, comment,
				artist_id, album_artist_id, album_id, review_state, created_by, origin_file_id, is_primary, created_at)
			SELECT ?, title, artist, album_artist, album, genre, year,
				track_number, track_total, disc_number, composer, comment,
				artist_id, album_artist_id, album_id, review_state, created_by, ?, 0, ?
			  FROM tagsets WHERE recording_id=?
			  ORDER BY (deleted_at IS NULL) DESC, is_primary DESC, id ASC LIMIT 1`,
			newRecordingID, fileID, time.Now().Unix(), oldRecordingID,
		); err != nil {
			return SplitRenditionOutcome{}, fmt.Errorf("split rendition: copy primary: %w", err)
		}
	}
	if err := reapRecordingsTx(ctx, tx, []int64{oldRecordingID}); err != nil {
		return SplitRenditionOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return SplitRenditionOutcome{}, fmt.Errorf("split rendition: commit: %w", err)
	}
	return SplitRenditionOutcome{NewRecordingID: newRecordingID, Found: true}, nil
}

// RemoveRendition soft-removes a rendition — the file-side (blob) removal, the
// counterpart to the tagset-side Trash (recording-tagsets P2). It sets
// files.deleted_at (bytes kept on disk, restorable via RestoreRendition); the
// recording and its tagsets are untouched — soft removal never cascades.
// Removing the LAST surviving rendition is allowed and makes the recording
// dormant: it keeps its appearances but drops out of the library (visibleTagset
// requires ≥1 surviving file to play) until a rendition is restored. Returns
// found=false (no error) when no live file matches the id.
func (db *DB) RemoveRendition(ctx context.Context, fileID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE files SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().Unix(), fileID)
	if err != nil {
		return false, fmt.Errorf("remove rendition: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RestoreRendition clears a rendition's removal mark (files.deleted_at), bringing
// the blob back as a playable rendition of its recording; a recording that went
// dormant when its last rendition was removed re-enters the library. Returns
// found=false (no error) when no removed file matches the id.
func (db *DB) RestoreRendition(ctx context.Context, fileID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE files SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`,
		fileID)
	if err != nil {
		return false, fmt.Errorf("restore rendition: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
		recID  int64
	)
	err := db.QueryRowContext(ctx,
		`SELECT f.id, f.recording_id FROM files f
		  WHERE f.hash=? AND f.deleted_at IS NULL
		    AND EXISTS (SELECT 1 FROM tagsets t WHERE t.origin_file_id=f.id AND t.deleted_at IS NULL)`,
		hash,
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
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM files f
			   JOIN tagsets t ON t.origin_file_id=f.id
			  WHERE f.recording_id=? AND f.id<>? AND f.deleted_at IS NULL
			    AND t.deleted_at IS NULL AND t.review_state='approved'`,
			recID, fileID,
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
		`SELECT title, artist, album FROM tagsets WHERE origin_file_id=? ORDER BY id LIMIT 1`, fileID,
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
		   JOIN tagsets t ON t.origin_file_id=f.id
		  WHERE f.id<>? AND f.deleted_at IS NULL
		    AND t.deleted_at IS NULL AND t.review_state='approved'
		    AND t.title=? AND t.artist=? AND t.album=?`,
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
