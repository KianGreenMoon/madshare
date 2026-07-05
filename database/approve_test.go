package database

import (
	"context"
	"database/sql"
	"testing"
)

// Per-piece approve (recording-tagsets P4): the moderator publishes an appearance
// and independently decides the submitted blob's fate and the recording it lands
// on.

func reviewStateOfTagset(t *testing.T, db *DB, tagsetID int64) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT review_state FROM tagsets WHERE id=?`, tagsetID).Scan(&s); err != nil {
		t.Fatalf("state of tagset %d: %v", tagsetID, err)
	}
	return s
}

// TestApproveSubmission_DropBytes: a case-B submission (new rendition on a held
// recording, different release) approved with drop_bytes → the appearance
// publishes and the submitted blob is soft-removed, the held rendition survives.
func TestApproveSubmission_DropBytes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	pub := insertTaggedFile(t, db, hash64("ap1"), "studio.flac", "M83", "Studio Album")
	sub := insertStagedTaggedFile(t, db, hash64("ap2"), "comp.mp3", "M83", "Compilation", ReviewSubmitted)
	groupIntoRecording(t, db, pub.ID, sub.ID)
	subTid := pendingTagsetIDOf(t, db, sub.ID)

	found, err := db.ApproveSubmission(ctx, subTid, true /*dropBytes*/, false)
	if err != nil || !found {
		t.Fatalf("approve: found=%v err=%v", found, err)
	}
	if s := reviewStateOfTagset(t, db, subTid); s != ReviewApproved {
		t.Errorf("appearance state = %q, want approved", s)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NOT NULL`, sub.ID); n != 1 {
		t.Error("submitted rendition not dropped")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NULL`, pub.ID); n != 1 {
		t.Error("held rendition was removed")
	}
	// Both appearances remain library-visible, served by the surviving FLAC.
	if got := visibleTagsetCount(t, db, pub.RecordingID); got != 2 {
		t.Errorf("visible appearances = %d, want 2", got)
	}
	assertInvariants(t, db)
}

// TestApproveSubmission_KeepBytes: a case-A submission approved plainly → the
// appearance publishes and the blob stays as the recording's rendition.
func TestApproveSubmission_KeepBytes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertStagedTaggedFile(t, db, hash64("ap3"), "new.flac", "Kavinsky", "OutRun", ReviewSubmitted)
	tid := pendingTagsetIDOf(t, db, f.ID)

	found, err := db.ApproveSubmission(ctx, tid, false, false)
	if err != nil || !found {
		t.Fatalf("approve: found=%v err=%v", found, err)
	}
	if s := reviewStateOfTagset(t, db, tid); s != ReviewApproved {
		t.Errorf("state = %q, want approved", s)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NULL`, f.ID); n != 1 {
		t.Error("rendition was removed on a keep-bytes approve")
	}
	assertInvariants(t, db)
}

// TestApproveSubmission_ForceNew: a submission wrongly grouped onto a held
// recording, approved with force_new → its blob splits into a new pinned
// recording and publishes there.
func TestApproveSubmission_ForceNew(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	pub := insertTaggedFile(t, db, hash64("ap4"), "held.flac", "Artist A", "Album A")
	sub := insertStagedTaggedFile(t, db, hash64("ap5"), "actually-new.flac", "Artist B", "Album B", ReviewSubmitted)
	groupIntoRecording(t, db, pub.ID, sub.ID)
	subTid := pendingTagsetIDOf(t, db, sub.ID)
	oldRec := pub.RecordingID

	found, err := db.ApproveSubmission(ctx, subTid, false, true /*forceNew*/)
	if err != nil || !found {
		t.Fatalf("approve: found=%v err=%v", found, err)
	}
	var newRec int64
	var pinned int
	if err := db.QueryRow(`SELECT recording_id, recording_pinned FROM files WHERE id=?`, sub.ID).Scan(&newRec, &pinned); err != nil {
		t.Fatalf("load file: %v", err)
	}
	if newRec == oldRec {
		t.Error("force-new did not split the blob into a new recording")
	}
	if pinned != 1 {
		t.Error("force-new did not pin the recording (resolver would re-merge)")
	}
	if s := reviewStateOfTagset(t, db, subTid); s != ReviewApproved {
		t.Errorf("state = %q, want approved", s)
	}
	assertInvariants(t, db)
}

// TestApproveSubmission_ForceNewIgnoredOnSharedBlob: force-new on a byte-dup
// appearance (a submitted 2nd tagset on an already-published blob) is ignored —
// splitting the shared blob would strand the other appearance's recording.
func TestApproveSubmission_ForceNewIgnoredOnSharedBlob(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")

	f := insertTaggedFile(t, db, hash64("ap6"), "song.flac", "Artist", "Album A")
	oldRec := f.RecordingID
	// A byte-dup draft appearance on the same blob (different release), then submit.
	tid, created, err := db.AttachDraftTagset(ctx, f.ID, sql.NullInt64{Int64: uid, Valid: true},
		&MediaMetadata{Title: "song", Album: nullStr("Album B")}, "song.flac")
	if err != nil || !created {
		t.Fatalf("attach: created=%v err=%v", created, err)
	}
	if _, err := db.Exec(`UPDATE tagsets SET review_state='submitted' WHERE id=?`, tid); err != nil {
		t.Fatalf("submit: %v", err)
	}

	found, err := db.ApproveSubmission(ctx, tid, false, true /*forceNew*/)
	if err != nil || !found {
		t.Fatalf("approve: found=%v err=%v", found, err)
	}
	var rec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, f.ID).Scan(&rec); err != nil {
		t.Fatalf("load file: %v", err)
	}
	if rec != oldRec {
		t.Error("shared blob was split by force-new — would strand the other appearance")
	}
	if s := reviewStateOfTagset(t, db, tid); s != ReviewApproved {
		t.Errorf("state = %q, want approved", s)
	}
	assertInvariants(t, db)
}
