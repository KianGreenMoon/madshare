package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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
	// The target keeps its primary appearance; a moving tagset only stays
	// primary when the target has none yet.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary = 0
		  WHERE origin_file_id = ?
		    AND EXISTS (SELECT 1 FROM tagsets p WHERE p.recording_id = ? AND p.is_primary = 1)`,
		fileID, bestRec,
	); err != nil {
		return 0, fmt.Errorf("resolve recording: demote primary: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET recording_id = ? WHERE origin_file_id = ?`, bestRec, fileID,
	); err != nil {
		return 0, fmt.Errorf("resolve recording: move tagsets: %w", err)
	}
	if err := repairRecordingTx(ctx, tx, currentRec); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("resolve recording: commit: %w", err)
	}
	return bestRec, nil
}

// ReconcileTagsets is the startup invariant sweep (recording-tagsets P0): it
// repairs whatever a crash or bug left behind so nothing rots silently.
//   - a file with no recording gets a fresh singleton (belt for rows that
//     somehow bypassed the trigger),
//   - a file with no tagset gets one derived from its first upload filename
//     (approved — such a row predates the staging flow by construction),
//   - a recording with no files is deleted (remaining tagsets cascade),
//   - a recording without a primary appearance promotes its oldest tagset.
//
// Returns the number of repairs applied. Idempotent; a healthy library is a
// fast no-op.
func (db *DB) ReconcileTagsets(ctx context.Context) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reconcile tagsets: begin: %w", err)
	}
	defer tx.Rollback()

	repairs := 0
	now := time.Now().Unix()

	// 1. Files without a recording → fresh singletons (one by one; violating
	// rows are rare to nonexistent).
	orphanFiles, err := scanIDs(tx.QueryContext(ctx,
		`SELECT id FROM files WHERE recording_id IS NULL ORDER BY id`))
	if err != nil {
		return 0, fmt.Errorf("reconcile tagsets: orphan files: %w", err)
	}
	for _, id := range orphanFiles {
		var recID int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, now,
		).Scan(&recID); err != nil {
			return 0, fmt.Errorf("reconcile tagsets: create recording: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE files SET recording_id = ? WHERE id = ?`, recID, id); err != nil {
			return 0, fmt.Errorf("reconcile tagsets: assign recording: %w", err)
		}
		log.Printf("reconcile tagsets: file %d had no recording; created singleton %d", id, recID)
		repairs++
	}

	// 2. Recordings without any tagset → a filename-derived approved appearance,
	// read from the recording's oldest file.
	//
	// The grain is the *recording*, not the file (recording-tagsets P7). A file
	// with no tagset of its own is not a violation: appearance dedup (merge,
	// absorb) drops a redundant appearance and keeps its blob, so a rendition
	// that no tagset was read from is a normal state. Healing at file grain
	// undid every dedup on the next restart, and did it by manufacturing a
	// nameless Unknown-artist appearance — exactly what the "meaningful tagset"
	// rule forbids. A recording with no tagset at all *is* a violation: it has
	// no catalog entry and nothing can reach it.
	bare, err := scanIDs(tx.QueryContext(ctx,
		`SELECT r.id FROM recordings r
		  WHERE NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = r.id)
		  ORDER BY r.id`))
	if err != nil {
		return 0, fmt.Errorf("reconcile tagsets: bare recordings: %w", err)
	}
	for _, recID := range bare {
		// Fileless recordings are step 3's job (they are removed, not repaired).
		var fileID, uploadedBy sql.NullInt64
		var fname sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT f.id, f.uploaded_by,
			        (SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1)
			   FROM files f WHERE f.recording_id = ? ORDER BY f.id LIMIT 1`, recID,
		).Scan(&fileID, &uploadedBy, &fname); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return 0, fmt.Errorf("reconcile tagsets: recording %d origin file: %w", recID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tagsets (recording_id, title, review_state, created_by, origin_file_id, is_primary, created_at)
			 VALUES (?, ?, 'approved', ?, ?, 1, ?)`,
			recID, titleFromFilename(fname.String), uploadedBy, fileID, now); err != nil {
			return 0, fmt.Errorf("reconcile tagsets: create tagset: %w", err)
		}
		log.Printf("reconcile tagsets: recording %d had no appearance; created one from file %d", recID, fileID.Int64)
		repairs++
	}

	// 3. Recordings with no files → invalid, remove (tagsets cascade via FK).
	res, err := tx.ExecContext(ctx,
		`DELETE FROM recordings WHERE NOT EXISTS
		   (SELECT 1 FROM files f WHERE f.recording_id = recordings.id)`)
	if err != nil {
		return 0, fmt.Errorf("reconcile tagsets: invalid recordings: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("reconcile tagsets: removed %d fileless recording(s)", n)
		repairs += int(n)
	}

	// 4. Recordings without a primary appearance → promote the oldest tagset.
	res, err = tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary = 1 WHERE id IN (
		   SELECT MIN(t.id) FROM tagsets t
		    WHERE NOT EXISTS (SELECT 1 FROM tagsets p
		                       WHERE p.recording_id = t.recording_id AND p.is_primary = 1)
		    GROUP BY t.recording_id)`)
	if err != nil {
		return 0, fmt.Errorf("reconcile tagsets: promote primaries: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("reconcile tagsets: promoted %d primary tagset(s)", n)
		repairs += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reconcile tagsets: commit: %w", err)
	}
	return repairs, nil
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

// SplitRendition detaches a file into its own brand-new recording and pins it so
// the resolver never re-merges it (the "save as another composition" action).
// The file's offered tagsets move with it (its appearance follows the audio),
// becoming the new recording's primary; the recording it left is repaired
// (primary re-promoted; removed if the split emptied it). When the file has no
// tagset of its own (an absorbed rendition — recording-tagsets P3), the new
// recording instead takes a *copy* of the source recording's primary appearance
// so it stays browsable (the moderator fixes its tags afterward). found is false
// (no error) when no live file matches the id. Atomic.
func (db *DB) SplitRendition(ctx context.Context, fileID int64) (newRecordingID int64, found bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("split rendition: begin: %w", err)
	}
	defer tx.Rollback()

	var oldRecordingID int64
	err = tx.QueryRowContext(ctx,
		`SELECT f.recording_id FROM files f WHERE f.id = ? AND f.deleted_at IS NULL`,
		fileID,
	).Scan(&oldRecordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil // no live file with that id
	}
	if err != nil {
		return 0, false, fmt.Errorf("split rendition: load file: %w", err)
	}

	if err := tx.QueryRowContext(ctx,
		`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, time.Now().Unix(),
	).Scan(&newRecordingID); err != nil {
		return 0, false, fmt.Errorf("split rendition: create recording: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET recording_id=?, recording_pinned=1 WHERE id=?`,
		newRecordingID, fileID,
	); err != nil {
		return 0, false, fmt.Errorf("split rendition: reassign: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET recording_id=?, is_primary=1 WHERE origin_file_id=?`,
		newRecordingID, fileID,
	); err != nil {
		return 0, false, fmt.Errorf("split rendition: move tagsets: %w", err)
	}
	// Multiple moved tagsets can't all be primary; keep only the oldest.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET is_primary=0
		  WHERE recording_id=? AND id <> (SELECT MIN(id) FROM tagsets WHERE recording_id=?)`,
		newRecordingID, newRecordingID,
	); err != nil {
		return 0, false, fmt.Errorf("split rendition: primary: %w", err)
	}
	// Tagset-less split (the file carried no appearance of its own): copy the
	// source recording's primary so the new recording is browsable, not invalid.
	var newTagsetCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, newRecordingID,
	).Scan(&newTagsetCount); err != nil {
		return 0, false, fmt.Errorf("split rendition: count tagsets: %w", err)
	}
	if newTagsetCount == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tagsets (recording_id, title, artist, album_artist, album, genre, year,
				track_number, track_total, disc_number, composer, comment,
				artist_id, album_artist_id, album_id, review_state, created_by, origin_file_id, is_primary, created_at)
			SELECT ?, title, artist, album_artist, album, genre, year,
				track_number, track_total, disc_number, composer, comment,
				artist_id, album_artist_id, album_id, review_state, created_by, ?, 1, ?
			  FROM tagsets WHERE recording_id=? AND is_primary=1 LIMIT 1`,
			newRecordingID, fileID, time.Now().Unix(), oldRecordingID,
		); err != nil {
			return 0, false, fmt.Errorf("split rendition: copy primary: %w", err)
		}
	}
	if err := repairRecordingTx(ctx, tx, oldRecordingID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("split rendition: commit: %w", err)
	}
	return newRecordingID, true, nil
}

// SweepInvalidRecordings garbage-collects recordings that violate the hardlink
// invariant by having no files left to play (recording-tagsets P2 — the prune
// backstop). Their tagsets cascade via FK. This is the standing sweep that keeps
// a bug or crash from leaving a fileless recording (and its orphaned appearances)
// to rot silently; the per-row prune cascade already removes a recording when it
// prunes that recording's last file, so on a healthy library this is a fast
// no-op. Returns the number of recordings removed. (A recording that still has
// files but lost all its tagsets is a *heal* case, not a GC — startup
// ReconcileTagsets re-creates an appearance rather than destroying the blob.)
func (db *DB) SweepInvalidRecordings(ctx context.Context) (int, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM recordings WHERE NOT EXISTS
		   (SELECT 1 FROM files f WHERE f.recording_id = recordings.id)`)
	if err != nil {
		return 0, fmt.Errorf("sweep invalid recordings: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
