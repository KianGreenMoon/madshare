package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Absorb (recording-tagsets P3) — the library-side dedup the whole feature was
// motivated by: on /admin/duplicates a moderator keeps one rendition's blob as
// the master and *absorbs* the others — their bytes are soft-removed but their
// distinct appearances are preserved, with redundant (duplicate identity or
// nameless) appearances dropped. Nothing is auto-absorbed; the selection is
// always the human's. See docs/architecture/recording-tagsets.md (Absorb).

// AbsorbOutcome reports what an absorb did. Found is false (no error) when the
// kept file or any absorbed file is not a live rendition of the recording (a
// stale selection — the caller 404s / reloads).
type AbsorbOutcome struct {
	Found              bool
	RenditionsRemoved  int // absorbed blobs soft-removed
	AppearancesDropped int // redundant/nameless tagsets hard-deleted
}

// appearance is one tagset of a recording, with the fields the absorb dedup /
// meaningful rules read. Meaningful is false only for the reserved
// Unknown-artist + Other bucket (the "nameless" appearance).
type appearance struct {
	id           int64
	originFileID sql.NullInt64
	meaningful   bool
	key          appearanceKey
}

// appearanceKey is the identity of an appearance on a recording (recording is
// fixed): resolved album + album-artist + disc + track. NULL disc/track are
// normalized so two untagged appearances compare equal as a Go map key
// (IS NOT DISTINCT FROM), which SQLite UNIQUE could not express.
type appearanceKey struct {
	album       int64
	albumArtist int64
	disc        int64
	track       int64
	albumNull   bool
	aArtistNull bool
	discNull    bool
	trackNull   bool
}

func keyOf(album, albumArtist, disc, track sql.NullInt64) appearanceKey {
	return appearanceKey{
		album: album.Int64, albumNull: !album.Valid,
		albumArtist: albumArtist.Int64, aArtistNull: !albumArtist.Valid,
		disc: disc.Int64, discNull: !disc.Valid,
		track: track.Int64, trackNull: !track.Valid,
	}
}

// AbsorbRenditions keeps keepFileID's blob and absorbs absorbFileIDs into the
// recording: their appearances are deduped (nameless or duplicate-identity ones
// dropped, the rest preserved) and their blobs soft-removed. One transaction +
// one caller-issued audit row. The kept rendition and its appearance are never
// touched; the primary is re-promoted if a dropped tagset held it.
func (db *DB) AbsorbRenditions(ctx context.Context, recordingID, keepFileID int64, absorbFileIDs []int64) (AbsorbOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AbsorbOutcome{}, fmt.Errorf("absorb: begin: %w", err)
	}
	defer tx.Rollback()

	out, err := absorbRenditionsTx(ctx, tx, recordingID, keepFileID, absorbFileIDs)
	if err != nil {
		return AbsorbOutcome{}, err
	}
	if !out.Found {
		return out, nil
	}
	if err := tx.Commit(); err != nil {
		return AbsorbOutcome{}, fmt.Errorf("absorb: commit: %w", err)
	}
	return out, nil
}

// absorbRenditionsTx is the core absorb, shared by the single and bulk entry
// points so the dedup/meaningful rules live in exactly one place.
func absorbRenditionsTx(ctx context.Context, tx *sql.Tx, recordingID, keepFileID int64, absorbFileIDs []int64) (AbsorbOutcome, error) {
	// Validate: keep and every absorbed id must be a live rendition of *this*
	// recording, and disjoint from each other.
	absorbSet := make(map[int64]struct{}, len(absorbFileIDs))
	for _, id := range absorbFileIDs {
		if id == keepFileID {
			return AbsorbOutcome{}, nil // keep cannot also be absorbed → stale selection
		}
		absorbSet[id] = struct{}{}
	}
	if len(absorbSet) == 0 {
		return AbsorbOutcome{}, nil
	}
	live, err := liveRenditionIDs(ctx, tx, recordingID)
	if err != nil {
		return AbsorbOutcome{}, err
	}
	if _, ok := live[keepFileID]; !ok {
		return AbsorbOutcome{}, nil
	}
	for id := range absorbSet {
		if _, ok := live[id]; !ok {
			return AbsorbOutcome{}, nil
		}
	}

	// Load the recording's appearances and decide survivors. Non-absorbed
	// appearances (the kept rendition's, and any unselected rendition's) always
	// survive and seed the kept-key set; an absorbed appearance is dropped when it
	// is nameless or duplicates an already-kept key — "keep the existing one".
	appearances, err := loadAppearances(ctx, tx, recordingID)
	if err != nil {
		return AbsorbOutcome{}, err
	}
	keptKeys := make(map[appearanceKey]struct{})
	var dropIDs []int64
	// Pass 1: non-absorbed appearances survive unconditionally.
	for _, a := range appearances {
		if a.originFileID.Valid {
			if _, absorbed := absorbSet[a.originFileID.Int64]; absorbed {
				continue
			}
		}
		keptKeys[a.key] = struct{}{}
	}
	// Pass 2: absorbed appearances, in id order (loadAppearances is ordered).
	for _, a := range appearances {
		if !a.originFileID.Valid {
			continue
		}
		if _, absorbed := absorbSet[a.originFileID.Int64]; !absorbed {
			continue
		}
		if _, dup := keptKeys[a.key]; dup || !a.meaningful {
			dropIDs = append(dropIDs, a.id)
			continue
		}
		keptKeys[a.key] = struct{}{}
	}

	if err := deleteTagsetIDsTx(ctx, tx, dropIDs); err != nil {
		return AbsorbOutcome{}, err
	}

	// Soft-remove the absorbed blobs (bytes kept, restorable as renditions).
	removed, err := softRemoveFileIDsTx(ctx, tx, absorbFileIDs)
	if err != nil {
		return AbsorbOutcome{}, err
	}

	// A dropped tagset may have been the primary; re-promote deterministically.
	if err := reapRecordingsTx(ctx, tx, []int64{recordingID}); err != nil {
		return AbsorbOutcome{}, err
	}
	return AbsorbOutcome{Found: true, RenditionsRemoved: removed, AppearancesDropped: len(dropIDs)}, nil
}

// liveRenditionIDs returns the set of a recording's non-removed file ids.
func liveRenditionIDs(ctx context.Context, tx *sql.Tx, recordingID int64) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM files WHERE recording_id = ? AND deleted_at IS NULL`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("absorb: live renditions: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("absorb: scan rendition: %w", err)
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// loadAppearances returns a recording's tagsets (ordered by id) with the
// meaningful flag resolved (nameless = reserved Unknown-artist + Other bucket)
// and the identity key materialized for the in-Go dedup.
func loadAppearances(ctx context.Context, tx *sql.Tx, recordingID int64) ([]appearance, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.origin_file_id, t.album_id, t.album_artist_id, t.disc_number, t.track_number,
		       CASE WHEN COALESCE(aa.name,'') = ? AND COALESCE(ar.name,'') = ? AND COALESCE(al.title,'') = ?
		            THEN 0 ELSE 1 END AS meaningful
		  FROM tagsets t
		  LEFT JOIN artists aa ON aa.id = t.album_artist_id
		  LEFT JOIN artists ar ON ar.id = t.artist_id
		  LEFT JOIN albums  al ON al.id = t.album_id
		 WHERE t.recording_id = ?
		 ORDER BY t.id`,
		DefaultArtistName, DefaultArtistName, DefaultAlbumTitle, recordingID)
	if err != nil {
		return nil, fmt.Errorf("absorb: load appearances: %w", err)
	}
	defer rows.Close()
	var out []appearance
	for rows.Next() {
		var (
			a                  appearance
			album, albumArtist sql.NullInt64
			disc, track        sql.NullInt64
			meaningful         int
		)
		if err := rows.Scan(&a.id, &a.originFileID, &album, &albumArtist, &disc, &track, &meaningful); err != nil {
			return nil, fmt.Errorf("absorb: scan appearance: %w", err)
		}
		a.meaningful = meaningful == 1
		a.key = keyOf(album, albumArtist, disc, track)
		out = append(out, a)
	}
	return out, rows.Err()
}

// deleteTagsetIDsTx hard-deletes the given tagset rows (the absorb drop-set).
// These are redundant/nameless appearances, so no recording cascade is intended
// — the caller keeps the recording alive via the surviving kept rendition.
func deleteTagsetIDsTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		end := min(i+chunk, len(ids))
		batch := ids[i:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tagsets WHERE id IN (`+strings.Join(ph, ",")+`)`, args...); err != nil {
			return fmt.Errorf("absorb: drop tagsets: %w", err)
		}
	}
	return nil
}

// softRemoveFileIDsTx sets files.deleted_at on the given ids (rendition removal),
// returning how many rows it actually removed (already-removed ids are skipped).
func softRemoveFileIDsTx(ctx context.Context, tx *sql.Tx, ids []int64) (int, error) {
	now := time.Now().Unix()
	total := 0
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		end := min(i+chunk, len(ids))
		batch := ids[i:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, now)
		for j, id := range batch {
			ph[j] = "?"
			args = append(args, id)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET deleted_at = ? WHERE deleted_at IS NULL AND id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("absorb: soft-remove renditions: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// BulkAbsorbKeepBest absorbs each given recording's non-best live renditions into
// its ladder-best one (recording-tagsets P3) — the "keep best, preserve every
// appearance" one-click over a set. One transaction; recordings with a single
// live rendition are skipped. Returns how many recordings were absorbed and how
// many renditions were removed in total. The keep is picked deterministically by
// the quality ladder, so the human's action is "absorb these duplicates", never
// a silent choice of which blob to keep.
func (db *DB) BulkAbsorbKeepBest(ctx context.Context, recordingIDs []int64) (recordingsAbsorbed, renditionsRemoved int, err error) {
	if len(recordingIDs) == 0 {
		return 0, 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("bulk absorb: begin: %w", err)
	}
	defer tx.Rollback()

	for _, recID := range recordingIDs {
		keep, absorb, err := bestKeepAndRestTx(ctx, tx, recID)
		if err != nil {
			return 0, 0, err
		}
		if keep == 0 || len(absorb) == 0 {
			continue // single rendition (or gone): nothing to absorb
		}
		out, err := absorbRenditionsTx(ctx, tx, recID, keep, absorb)
		if err != nil {
			return 0, 0, err
		}
		if out.Found && out.RenditionsRemoved > 0 {
			recordingsAbsorbed++
			renditionsRemoved += out.RenditionsRemoved
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("bulk absorb: commit: %w", err)
	}
	return recordingsAbsorbed, renditionsRemoved, nil
}

// bestKeepAndRestTx returns a recording's ladder-best live rendition (keep) and
// the rest (absorb), using the same RankRenditions ladder the duplicates page
// shows. keep is 0 when the recording has no live rendition.
func bestKeepAndRestTx(ctx context.Context, tx *sql.Tx, recordingID int64) (keep int64, rest []int64, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT f.id, f.hash, COALESCE(mm.codec,''), COALESCE(mm.bitrate,0),
		       COALESCE(mm.sample_rate,0), COALESCE(mm.bit_depth,0), f.byte_size
		  FROM files f
		  LEFT JOIN media_metadata mm ON mm.file_id = f.id
		 WHERE f.recording_id = ? AND f.deleted_at IS NULL`, recordingID)
	if err != nil {
		return 0, nil, fmt.Errorf("bulk absorb: renditions: %w", err)
	}
	defer rows.Close()
	var rs []Rendition
	for rows.Next() {
		var r Rendition
		if err := rows.Scan(&r.FileID, &r.Hash, &r.Codec, &r.Bitrate, &r.SampleRate, &r.BitDepth, &r.ByteSize); err != nil {
			return 0, nil, fmt.Errorf("bulk absorb: scan rendition: %w", err)
		}
		rs = append(rs, r)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(rs) < 2 {
		return 0, nil, nil
	}
	ranked := RankRenditions(rs)
	keep = ranked[0].FileID
	for _, r := range ranked[1:] {
		rest = append(rest, r.FileID)
	}
	return keep, rest, nil
}
