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
	// live = not trashed and approved. Dedup reasons about live appearances
	// only, in both directions — see loadAppearances for why.
	live bool
	key  appearanceKey
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
	// Pass 1: non-absorbed appearances survive unconditionally, but only a LIVE
	// one seeds a kept key — a trashed or pending row is not "the appearance we
	// are keeping", and treating it as one made pass 2 drop the live approved
	// twin (see loadAppearances).
	for _, a := range appearances {
		if a.originFileID.Valid {
			if _, absorbed := absorbSet[a.originFileID.Int64]; absorbed {
				continue
			}
		}
		if !a.live {
			continue
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
		if !a.live {
			continue // Trash / the review queue owns this row, not dedup.
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
// meaningful flag resolved (nameless = reserved Unknown-artist + Other bucket),
// the identity key materialized for the in-Go dedup, and whether the appearance
// is LIVE — not trashed and approved.
//
// Liveness is what dedup is allowed to reason about, in both directions, and
// both callers must honour it (.issues/open-issues.md, "Appearance dedup treats
// trashed/pending appearances as kept keys"):
//
//   - A non-live appearance must not SEED a kept key. Reading a trashed row as
//     "the one we are keeping" made absorb drop the LIVE approved appearance as
//     its duplicate, leaving the recording with nothing but the Trash copy — it
//     left the library outright.
//   - A non-live appearance must not BE DROPPED. A draft or submitted row
//     belongs to its uploader and the review queue; hard-deleting it removes an
//     entry from moderation with no approve/return/deny, silently.
//
// This is the rule `AttachDraftTagset` and `MoveTagset` already use for their
// collision checks, which is why dedup disagreeing with them was a bug rather
// than a policy. Only the tagset's own two marks count — deliberately NOT
// visibleTagset's third leg (a surviving rendition), because absorb is in the
// middle of removing renditions and would be reading its own uncommitted work.
func loadAppearances(ctx context.Context, tx *sql.Tx, recordingID int64) ([]appearance, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.origin_file_id, t.album_id, t.album_artist_id, t.disc_number, t.track_number,
		       CASE WHEN COALESCE(aa.name,'') = ? AND COALESCE(ar.name,'') = ? AND COALESCE(al.title,'') = ?
		            THEN 0 ELSE 1 END AS meaningful,
		       CASE WHEN t.deleted_at IS NULL AND t.review_state = ? THEN 1 ELSE 0 END AS live
		  FROM tagsets t
		  LEFT JOIN artists aa ON aa.id = t.album_artist_id
		  LEFT JOIN artists ar ON ar.id = t.artist_id
		  LEFT JOIN albums  al ON al.id = t.album_id
		 WHERE t.recording_id = ?
		 ORDER BY t.id`,
		DefaultArtistName, DefaultArtistName, DefaultAlbumTitle, ReviewApproved, recordingID)
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
			meaningful, live   int
		)
		if err := rows.Scan(&a.id, &a.originFileID, &album, &albumArtist, &disc, &track, &meaningful, &live); err != nil {
			return nil, fmt.Errorf("absorb: scan appearance: %w", err)
		}
		a.meaningful = meaningful == 1
		a.live = live == 1
		a.key = keyOf(album, albumArtist, disc, track)
		out = append(out, a)
	}
	return out, rows.Err()
}

// deleteTagsetIDsTx hard-deletes the given tagset rows (the absorb drop-set).
// These are redundant/nameless appearances, so no recording cascade is intended
// — the caller keeps the recording alive via the surviving kept rendition.
func deleteTagsetIDsTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	// Save the users' references before the rows go: playlist_items.tagset_id is
	// ON DELETE CASCADE (migration 029), so deleting a deduped appearance took
	// the track out of every playlist and favorites list holding it, silently and
	// unrecoverably — while an identical appearance of the same audio survived
	// right there to point at (.issues/open-issues.md, "Appearance dedup silently
	// deletes playlist/favorites entries").
	if err := repointPlaylistItemsTx(ctx, tx, ids); err != nil {
		return err
	}
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

// repointPlaylistItemsTx moves every playlist/favorites entry that points at one
// of the dying tagsets onto a surviving appearance of the SAME RECORDING, so a
// curation dedup never costs a user a saved track.
//
// Re-pointing is sound because every appearance of a recording describes the
// same audio — that is what a recording is. The survivor is chosen to keep the
// user's row looking unchanged where possible: same identity key first (so the
// album/disc/track the user saved still reads the same), then the primary, then
// the oldest. Only live approved appearances qualify, and never one that is
// itself about to be deleted.
//
// Two cases have no re-point:
//   - no survivor (the last appearance of the recording is going): the CASCADE
//     stands. There is nothing to point at, and inventing a row would be worse
//     than the entry disappearing.
//   - the playlist already holds the survivor: the entry is dropped rather than
//     duplicated, mirroring RepointRemotePlaylistItems, whose shape this follows.
//
// Favorites need no separate handling — a favorites list IS a playlist row
// (kind='favorites', migration 015), so it comes through playlist_items too.
func repointPlaylistItemsTx(ctx context.Context, tx *sql.Tx, dying []int64) error {
	if len(dying) == 0 {
		return nil
	}
	dyingSet := make(map[int64]struct{}, len(dying))
	for _, id := range dying {
		dyingSet[id] = struct{}{}
	}

	type item struct{ itemID, playlistID, tagsetID int64 }
	var items []item
	const chunk = 400
	for i := 0; i < len(dying); i += chunk {
		batch := dying[i:min(i+chunk, len(dying))]
		in, args := inClause(batch)
		rows, err := tx.QueryContext(ctx,
			`SELECT id, playlist_id, tagset_id FROM playlist_items WHERE tagset_id IN `+in, args...)
		if err != nil {
			return fmt.Errorf("absorb: find playlist items: %w", err)
		}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.itemID, &it.playlistID, &it.tagsetID); err != nil {
				rows.Close()
				return fmt.Errorf("absorb: scan playlist item: %w", err)
			}
			items = append(items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("absorb: find playlist items: %w", err)
		}
	}
	if len(items) == 0 {
		return nil
	}

	// One survivor lookup per distinct dying tagset, memoized: the drop set is
	// normally one or two rows, and a playlist entry per row is rarer still.
	survivors := make(map[int64]int64, len(items))
	for _, it := range items {
		if _, done := survivors[it.tagsetID]; done {
			continue
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT s.id FROM tagsets s, tagsets d
			 WHERE d.id = ?
			   AND s.recording_id = d.recording_id AND s.id <> d.id
			   AND s.deleted_at IS NULL AND s.review_state = ?
			 ORDER BY (s.album_id IS d.album_id AND s.album_artist_id IS d.album_artist_id
			           AND s.disc_number IS d.disc_number AND s.track_number IS d.track_number) DESC,
			          s.is_primary DESC, s.id`,
			it.tagsetID, ReviewApproved)
		if err != nil {
			return fmt.Errorf("absorb: find surviving appearance: %w", err)
		}
		var pick int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("absorb: scan surviving appearance: %w", err)
			}
			// Skip anything dying in this same call — it would take the entry
			// with it a moment later.
			if _, doomed := dyingSet[id]; doomed {
				continue
			}
			pick = id
			break
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("absorb: find surviving appearance: %w", err)
		}
		survivors[it.tagsetID] = pick
	}

	for _, it := range items {
		survivor := survivors[it.tagsetID]
		if survivor == 0 {
			continue // nothing to point at; the cascade takes it
		}
		var dup bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM playlist_items WHERE playlist_id = ? AND tagset_id = ?)`,
			it.playlistID, survivor).Scan(&dup); err != nil {
			return fmt.Errorf("absorb: check playlist duplicate: %w", err)
		}
		if dup {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM playlist_items WHERE id = ?`, it.itemID); err != nil {
				return fmt.Errorf("absorb: drop duplicate playlist item: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_items SET tagset_id = ? WHERE id = ?`, survivor, it.itemID); err != nil {
			return fmt.Errorf("absorb: repoint playlist item: %w", err)
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
