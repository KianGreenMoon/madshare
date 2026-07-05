package database

import (
	"context"
	"database/sql"
	"testing"
)

// Submission classification (recording-tagsets P4). The cases are derived from
// the current DB state (the resolver has already grouped same-audio files), so
// these tests build the state each case describes and assert the derived class.

// insertStagedTaggedFile inserts a live file whose offered appearance is in the
// given review state (draft/submitted/returned) resolving to artist/album.
func insertStagedTaggedFile(t *testing.T, db *DB, hash, filename, artist, album, state string) *File {
	t.Helper()
	f := newFile(hash)
	f.ReviewState = state
	m := &MediaMetadata{
		Title:       filename,
		Artist:      sql.NullString{String: artist, Valid: artist != ""},
		Album:       sql.NullString{String: album, Valid: album != ""},
		TagFormat:   sql.NullString{String: "ID3v2.4", Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(context.Background(), f, newUpload(filename), m); err != nil {
		t.Fatalf("InsertFile %s: %v", hash, err)
	}
	return f
}

// tagsetIDOf returns a file's first (representative) tagset id.
func tagsetIDOf(t *testing.T, db *DB, fileID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=? ORDER BY id LIMIT 1`, fileID).Scan(&id); err != nil {
		t.Fatalf("tagset of file %d: %v", fileID, err)
	}
	return id
}

// pendingTagsetIDOf returns a file's pending (non-approved) appearance id — the
// submission a classify call addresses.
func pendingTagsetIDOf(t *testing.T, db *DB, fileID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=? AND review_state<>'approved' ORDER BY id LIMIT 1`, fileID).Scan(&id); err != nil {
		t.Fatalf("pending tagset of file %d: %v", fileID, err)
	}
	return id
}

// TestClassify_NewRecording (case A): a submission whose audio is new to the
// library — no matched recording, no ladder compare.
func TestClassify_NewRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertStagedTaggedFile(t, db, hash64("ca1"), "song.flac", "Kavinsky", "OutRun", ReviewSubmitted)

	sc, ok, err := db.ClassifySubmission(ctx, pendingTagsetIDOf(t, db, f.ID))
	if err != nil || !ok {
		t.Fatalf("classify: ok=%v err=%v", ok, err)
	}
	if sc.Case != SubmissionNewRecording {
		t.Errorf("case = %q, want %q", sc.Case, SubmissionNewRecording)
	}
	if sc.MatchedExisting || sc.CollidesAppearance {
		t.Errorf("matched/collides = %v/%v, want false/false", sc.MatchedExisting, sc.CollidesAppearance)
	}
	if sc.CurrentBest != nil {
		t.Errorf("current best = %+v, want nil (only rendition)", sc.CurrentBest)
	}
}

// TestClassify_NewAppearanceWorseBlob (case B): same audio as a published
// recording, a distinct new blob that ranks below the current best, on a
// different release → new appearance, not the new best.
func TestClassify_NewAppearanceWorseBlob(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	pub := insertTaggedFile(t, db, hash64("cb1"), "studio.flac", "M83", "Hurry Up")
	setCodec(t, db, pub.ID, "flac", 44100, 16, 0)
	sub := insertStagedTaggedFile(t, db, hash64("cb2"), "comp.mp3", "M83", "Late Night Tales", ReviewSubmitted)
	setCodec(t, db, sub.ID, "mp3", 0, 0, 320000)
	groupIntoRecording(t, db, pub.ID, sub.ID)

	sc, ok, err := db.ClassifySubmission(ctx, pendingTagsetIDOf(t, db, sub.ID))
	if err != nil || !ok {
		t.Fatalf("classify: ok=%v err=%v", ok, err)
	}
	if sc.Case != SubmissionNewAppearance || !sc.MatchedExisting {
		t.Errorf("case/matched = %q/%v, want %q/true", sc.Case, sc.MatchedExisting, SubmissionNewAppearance)
	}
	if sc.CollidesAppearance {
		t.Error("collides = true, want false (different release)")
	}
	if sc.CurrentBest == nil || sc.CurrentBest.FileID != pub.ID {
		t.Errorf("current best = %+v, want published FLAC (file %d)", sc.CurrentBest, pub.ID)
	}
	if sc.SubmittedIsNewBest {
		t.Error("submitted-is-new-best = true, want false (MP3 below FLAC)")
	}
}

// TestClassify_BetterRenditionCollides (case B, mirror): same audio, a better
// blob, but the offered tags equal the existing appearance → new best, no new
// appearance (collision reported).
func TestClassify_BetterRenditionCollides(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	pub := insertTaggedFile(t, db, hash64("cc1"), "lossy.mp3", "Massive Attack", "Mezzanine")
	setCodec(t, db, pub.ID, "mp3", 0, 0, 245000)
	sub := insertStagedTaggedFile(t, db, hash64("cc2"), "hires.flac", "Massive Attack", "Mezzanine", ReviewSubmitted)
	setCodec(t, db, sub.ID, "flac", 96000, 24, 0)
	groupIntoRecording(t, db, pub.ID, sub.ID)

	sc, ok, err := db.ClassifySubmission(ctx, pendingTagsetIDOf(t, db, sub.ID))
	if err != nil || !ok {
		t.Fatalf("classify: ok=%v err=%v", ok, err)
	}
	if sc.Case != SubmissionNewAppearance {
		t.Errorf("case = %q, want %q", sc.Case, SubmissionNewAppearance)
	}
	if !sc.CollidesAppearance {
		t.Error("collides = false, want true (same album/artist/disc/track)")
	}
	if !sc.SubmittedIsNewBest {
		t.Error("submitted-is-new-best = false, want true (FLAC over MP3)")
	}
}

// TestClassify_NoNewBytes (case C): the submitted blob is itself an
// already-published rendition (a second pending appearance attached to it, as a
// byte-dup upload would) → no new bytes, only the appearance is new.
func TestClassify_NoNewBytes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("cd1"), "nightcall.flac", "Kavinsky", "Nightcall (single)")
	// A byte-dup upload's draft appearance on the same blob (a different release).
	albumID, err := db.ResolveAlbumID(ctx, "Various Artists", "Drive (OST)")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	artistID, err := db.ResolveArtistID(ctx, "Various Artists")
	if err != nil {
		t.Fatalf("resolve artist: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tagsets (recording_id, title, artist, album_artist, album,
			album_artist_id, album_id, review_state, created_by, origin_file_id, is_primary, created_at)
		SELECT recording_id, 'Nightcall', 'Kavinsky', 'Various Artists', 'Drive (OST)',
			?, ?, 'submitted', uploaded_by, id, 0, 1700000001
		  FROM files WHERE id = ?`, artistID, albumID, f.ID); err != nil {
		t.Fatalf("insert dup appearance: %v", err)
	}

	sc, ok, err := db.ClassifySubmission(ctx, pendingTagsetIDOf(t, db, f.ID))
	if err != nil || !ok {
		t.Fatalf("classify: ok=%v err=%v", ok, err)
	}
	if sc.Case != SubmissionNoNewBytes || !sc.MatchedExisting {
		t.Errorf("case/matched = %q/%v, want %q/true", sc.Case, sc.MatchedExisting, SubmissionNoNewBytes)
	}
	if sc.CollidesAppearance {
		t.Error("collides = true, want false (Drive OST is a new release)")
	}
}

// TestClassify_UnknownHash: an unknown / already-approved hash is not a live
// pending submission.
func TestClassify_UnknownHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if _, ok, err := db.ClassifySubmission(ctx, 999999); err != nil || ok {
		t.Errorf("unknown hash: ok=%v err=%v, want ok=false", ok, err)
	}
	// An approved file has no pending submission.
	f := insertTaggedFile(t, db, hash64("ce1"), "live.flac", "Artist", "Album")
	if _, ok, err := db.ClassifySubmission(ctx, tagsetIDOf(t, db, f.ID)); err != nil || ok {
		t.Errorf("approved file: ok=%v err=%v, want ok=false", ok, err)
	}
}
