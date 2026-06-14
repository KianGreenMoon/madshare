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
