package database

import (
	"context"
	"database/sql"
	"testing"
)

// Byte-dup upload → draft tagset (recording-tagsets P4). A content-hash re-upload
// adds no blob but offers a new appearance on the held recording as a draft.

func TestAttachDraftTagset_NewAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")

	f := insertTaggedFile(t, db, hash64("at1"), "nightcall.flac", "Kavinsky", "Nightcall (single)")
	// A byte-dup upload offering a different release of the same audio.
	meta := &MediaMetadata{
		Title:       "Nightcall",
		Artist:      sql.NullString{String: "Kavinsky", Valid: true},
		AlbumArtist: sql.NullString{String: "Various Artists", Valid: true},
		Album:       sql.NullString{String: "Drive (OST)", Valid: true},
	}
	tid, created, err := db.AttachDraftTagset(ctx, f.ID, sql.NullInt64{Int64: uid, Valid: true}, meta, "nightcall.flac")
	if err != nil || !created {
		t.Fatalf("attach: created=%v err=%v", created, err)
	}

	var recID, createdBy sql.NullInt64
	var primary int
	var state string
	if err := db.QueryRow(
		`SELECT recording_id, is_primary, review_state, created_by FROM tagsets WHERE id=?`, tid,
	).Scan(&recID, &primary, &state, &createdBy); err != nil {
		t.Fatalf("load new tagset: %v", err)
	}
	if recID.Int64 != f.RecordingID {
		t.Errorf("recording = %d, want the blob's recording %d", recID.Int64, f.RecordingID)
	}
	if state != ReviewDraft || primary != 0 || createdBy.Int64 != uid {
		t.Errorf("draft tagset = state %q primary %d owner %d, want draft/0/%d", state, primary, createdBy.Int64, uid)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, f.RecordingID); n != 2 {
		t.Errorf("appearances = %d, want 2 (original + offered draft)", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, f.RecordingID); n != 1 {
		t.Errorf("files = %d, want 1 (no new blob)", n)
	}
	// The submission classifies as case C — no new bytes, a new appearance.
	sc, ok, err := db.ClassifySubmission(ctx, tid)
	if err != nil || !ok {
		t.Fatalf("classify: ok=%v err=%v", ok, err)
	}
	if sc.Case != SubmissionNoNewBytes {
		t.Errorf("class = %q, want %q", sc.Case, SubmissionNoNewBytes)
	}
}

func TestAttachDraftTagset_DedupsIdenticalAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")

	f := insertTaggedFile(t, db, hash64("at2"), "song.flac", "The Band", "The Album")
	// Same identity as the file's existing appearance → nothing new to offer.
	meta := &MediaMetadata{
		Title:  "song",
		Artist: sql.NullString{String: "The Band", Valid: true},
		Album:  sql.NullString{String: "The Album", Valid: true},
	}
	tid, created, err := db.AttachDraftTagset(ctx, f.ID, sql.NullInt64{Int64: uid, Valid: true}, meta, "song.flac")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if created {
		t.Error("created=true, want false (identical appearance already present)")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, f.RecordingID); n != 1 {
		t.Errorf("appearances = %d, want 1 (no duplicate manufactured)", n)
	}
	// It returns the existing appearance's id.
	if tid == 0 {
		t.Error("returned tagset id = 0, want the existing appearance id")
	}
}

func TestAttachDraftTagset_RemovedRenditionAndUnknownNoOp(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// A blob whose rendition has been removed (files.deleted_at) has no bytes to
	// serve — attaching a new appearance to it is a no-op.
	f := insertTaggedFile(t, db, hash64("at3"), "song.flac", "Artist", "Album")
	if _, err := db.RemoveRendition(ctx, f.ID); err != nil {
		t.Fatalf("remove rendition: %v", err)
	}
	if tid, created, err := db.AttachDraftTagset(ctx, f.ID, sql.NullInt64{}, &MediaMetadata{Title: "x"}, "song.flac"); err != nil || created || tid != 0 {
		t.Errorf("attach to removed rendition = (%d, %v, %v), want (0, false, nil)", tid, created, err)
	}
	// Unknown file id is also a clean no-op.
	if tid, created, err := db.AttachDraftTagset(ctx, 999999, sql.NullInt64{}, &MediaMetadata{Title: "x"}, "x.flac"); err != nil || created || tid != 0 {
		t.Errorf("attach to unknown = (%d, %v, %v), want (0, false, nil)", tid, created, err)
	}
}
