package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Submission classification (recording-tagsets P4, docs/architecture/recording-tagsets.md).
// A staged submission is classified against the current library so the review
// queue can name the concrete outcome of an approve. The resolver has already
// grouped same-audio files onto one recording at upload time, so the case is
// readable off the file's recording — no fingerprint pass needed here.
const (
	// SubmissionNewRecording (case A): net-new audio — the file's recording holds
	// no other approved appearance, so approving it publishes a fresh recording.
	SubmissionNewRecording = "new_recording"
	// SubmissionNewAppearance (case B): same audio as a recording already in the
	// library, arriving as a distinct new blob (a candidate new rendition).
	SubmissionNewAppearance = "new_appearance"
	// SubmissionNoNewBytes (case C): the submitted blob is itself an
	// already-published rendition (a byte-dup upload attached its tags as a draft
	// appearance) — only the offered appearance is potentially new.
	SubmissionNoNewBytes = "no_new_bytes"
)

// SubmissionClass is the review-queue classification of one staged submission.
// The Case is a suggestion the moderator validates; CollidesAppearance and the
// ladder compare (CurrentBest vs Submitted) let the queue state what an approve
// would actually change.
type SubmissionClass struct {
	Case        string
	RecordingID int64
	// MatchedExisting is true for cases B and C (the audio is already published);
	// false for case A.
	MatchedExisting bool
	// CollidesAppearance is true when the offered tagset's identity key
	// (album_id, album_artist_id, disc, track) equals an existing approved,
	// non-trashed appearance on the recording — nothing new on the metadata side.
	CollidesAppearance bool
	// CurrentBest is the recording's current ladder-best rendition among the files
	// *other* than the submitted one (nil for case A, or when the submission is the
	// recording's only live rendition).
	CurrentBest *Rendition
	// Submitted is the submitted file viewed as a rendition.
	Submitted Rendition
	// SubmittedIsNewBest reports whether the submitted blob would become the
	// recording's ladder-best (the case-B "better rendition" hint).
	SubmittedIsNewBest bool
}

// ClassifySubmission classifies the staged submission identified by tagset id —
// a pending (non-approved), non-trashed appearance on a live blob. found is
// false (no error) when the id is unknown, its blob is trashed, or it is already
// approved. Read-only.
func (db *DB) ClassifySubmission(ctx context.Context, tagsetID int64) (SubmissionClass, bool, error) {
	var (
		fileID, recID, subTagsetID      int64
		album, albumArtist, disc, track sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `
		SELECT f.id, f.recording_id, t.id, t.album_id, t.album_artist_id, t.disc_number, t.track_number
		  FROM tagsets t
		  JOIN files f ON f.id = t.origin_file_id
		 WHERE t.id = ? AND t.deleted_at IS NULL AND t.review_state <> 'approved' AND f.deleted_at IS NULL`, tagsetID,
	).Scan(&fileID, &recID, &subTagsetID, &album, &albumArtist, &disc, &track)
	if errors.Is(err, sql.ErrNoRows) {
		return SubmissionClass{}, false, nil
	}
	if err != nil {
		return SubmissionClass{}, false, fmt.Errorf("classify submission: load: %w", err)
	}

	sc := SubmissionClass{RecordingID: recID}

	// blobPublished: this exact blob already carries an approved appearance
	// (byte-dup upload / an already-published rendition) → case C.
	var blobPublished bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM tagsets t
		  WHERE t.origin_file_id = ? AND t.id <> ? AND t.deleted_at IS NULL AND t.review_state = 'approved')`,
		fileID, subTagsetID).Scan(&blobPublished); err != nil {
		return SubmissionClass{}, false, fmt.Errorf("classify submission: blob published: %w", err)
	}
	// recordingPublished: the recording holds an approved appearance from any
	// file → the audio is already in the library (case B, unless the blob itself
	// is published → C).
	var recordingPublished bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM tagsets t
		  WHERE t.recording_id = ? AND t.id <> ? AND t.deleted_at IS NULL AND t.review_state = 'approved')`,
		recID, subTagsetID).Scan(&recordingPublished); err != nil {
		return SubmissionClass{}, false, fmt.Errorf("classify submission: recording published: %w", err)
	}

	switch {
	case blobPublished:
		sc.Case, sc.MatchedExisting = SubmissionNoNewBytes, true
	case recordingPublished:
		sc.Case, sc.MatchedExisting = SubmissionNewAppearance, true
	default:
		sc.Case = SubmissionNewRecording
	}

	// Appearance collision: the offered identity equals an existing approved
	// appearance on the recording (NULL-safe via SQLite's IS operator, so an
	// untagged disc/track matches another untagged one — never a tagged 0/N).
	if sc.MatchedExisting {
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM tagsets t
			  WHERE t.recording_id = ? AND t.id <> ? AND t.deleted_at IS NULL AND t.review_state = 'approved'
			    AND t.album_id IS ? AND t.album_artist_id IS ? AND t.disc_number IS ? AND t.track_number IS ?)`,
			recID, subTagsetID, album, albumArtist, disc, track,
		).Scan(&sc.CollidesAppearance); err != nil {
			return SubmissionClass{}, false, fmt.Errorf("classify submission: collision: %w", err)
		}
	}

	// Ladder compare: the submitted rendition against the recording's current best
	// *other* live rendition (degrades to format/size when ffprobe is absent, via
	// RankRenditions).
	rends, err := db.recordingRenditions(ctx, recID)
	if err != nil {
		return SubmissionClass{}, false, err
	}
	var others []Rendition
	for _, r := range rends {
		if r.FileID == fileID {
			sc.Submitted = r
		} else {
			others = append(others, r)
		}
	}
	if len(others) > 0 {
		best := RankRenditions(others)[0]
		sc.CurrentBest = &best
		if all := RankRenditions(rends); len(all) > 0 {
			sc.SubmittedIsNewBest = all[0].FileID == fileID
		}
	}
	return sc, true, nil
}

// recordingRenditions loads a recording's live (non-removed) files as Renditions
// with the tech fields the quality ladder ranks on. Ordered by file id.
func (db *DB) recordingRenditions(ctx context.Context, recordingID int64) ([]Rendition, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.id, f.hash, COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0), f.byte_size
		  FROM files f
		  LEFT JOIN media_metadata mm ON mm.file_id = f.id
		 WHERE f.recording_id = ? AND f.deleted_at IS NULL
		 ORDER BY f.id`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("recording renditions: %w", err)
	}
	defer rows.Close()
	var out []Rendition
	for rows.Next() {
		var r Rendition
		if err := rows.Scan(&r.FileID, &r.Hash, &r.Codec, &r.Bitrate, &r.SampleRate, &r.BitDepth, &r.ByteSize); err != nil {
			return nil, fmt.Errorf("recording renditions: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
