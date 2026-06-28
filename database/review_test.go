package database

import (
	"context"
	"database/sql"
	"testing"
)

// insertStagedFile inserts a draft (or other-state) file with the standard
// metadata, owned by userID. Returns the file id.
func insertStagedFile(t *testing.T, db *DB, hash, state string, userID int64) int64 {
	t.Helper()
	f := newFile(hash)
	f.ReviewState = state
	if userID != 0 {
		f.UploadedBy = sql.NullInt64{Int64: userID, Valid: true}
	}
	if err := db.InsertFile(context.Background(), f, newUpload("song.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	return f.ID
}

// makeReviewUser creates a bare user row and returns its id (the review
// listings join users for the uploader name).
func makeReviewUser(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at) VALUES (?, 'x', 0)`, name)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func reviewState(t *testing.T, db *DB, hash string) string {
	t.Helper()
	state, _, _, found, err := db.FileReviewInfo(context.Background(), hash)
	if err != nil || !found {
		t.Fatalf("FileReviewInfo: found=%v err=%v", found, err)
	}
	return state
}

func TestInsertFile_DefaultsToApproved(t *testing.T) {
	db := openMem(t)
	h := hash64("default")
	insertAccessFile(t, db, h) // does not set ReviewState

	if got := reviewState(t, db, h); got != ReviewApproved {
		t.Errorf("review state = %q, want approved (unset state must collapse)", got)
	}
}

func TestUpdateReviewState_Transitions(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")
	h := hash64("trans")
	insertStagedFile(t, db, h, ReviewDraft, uid)

	// Owner submit: draft -> submitted, stamps submitted_at.
	found, err := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewDraft, ReviewReturned}, To: ReviewSubmitted,
		OwnerID: uid, StampSubmittedAt: true,
	})
	if err != nil || !found {
		t.Fatalf("submit: found=%v err=%v", found, err)
	}
	f, err := db.GetFileByHash(ctx, h)
	if err != nil {
		t.Fatalf("GetFileByHash: %v", err)
	}
	if f.ReviewState != ReviewSubmitted || !f.SubmittedAt.Valid {
		t.Errorf("after submit: state=%q submitted_at.Valid=%v", f.ReviewState, f.SubmittedAt.Valid)
	}

	// Return with note.
	found, err = db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewSubmitted, ReviewReturned}, To: ReviewReturned, Note: "fix the album tag",
	})
	if err != nil || !found {
		t.Fatalf("return: found=%v err=%v", found, err)
	}
	if f, _ = db.GetFileByHash(ctx, h); f.ReviewNote.String != "fix the album tag" {
		t.Errorf("note = %q, want the return message", f.ReviewNote.String)
	}

	// Resubmit clears the note; approve clears it too and lands approved.
	if found, _ = db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewDraft, ReviewReturned}, To: ReviewSubmitted, OwnerID: uid, StampSubmittedAt: true,
	}); !found {
		t.Fatal("resubmit returned found=false")
	}
	if f, _ = db.GetFileByHash(ctx, h); f.ReviewNote.Valid {
		t.Errorf("note after resubmit = %q, want cleared", f.ReviewNote.String)
	}
	if found, _ = db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewSubmitted, ReviewReturned}, To: ReviewApproved,
	}); !found {
		t.Fatal("approve returned found=false")
	}
	if got := reviewState(t, db, h); got != ReviewApproved {
		t.Errorf("final state = %q, want approved", got)
	}
}

func TestUpdateReviewState_Guards(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")
	other := makeReviewUser(t, db, "other")
	h := hash64("guard")
	insertStagedFile(t, db, h, ReviewDraft, uid)

	// Wrong source state: approving a draft directly (moderator path) is not a
	// legal transition.
	if found, _ := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewSubmitted, ReviewReturned}, To: ReviewApproved,
	}); found {
		t.Error("approve of a draft applied, want guard rejection")
	}
	// Wrong owner.
	if found, _ := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewDraft, ReviewReturned}, To: ReviewSubmitted, OwnerID: other,
	}); found {
		t.Error("submit by non-owner applied, want guard rejection")
	}
	// Trashed files are untouchable.
	if _, found, err := db.SoftDeleteFileByHash(ctx, h); err != nil || !found {
		t.Fatalf("SoftDeleteFileByHash: found=%v err=%v", found, err)
	}
	if found, _ := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewDraft, ReviewReturned}, To: ReviewSubmitted, OwnerID: uid,
	}); found {
		t.Error("transition on a trashed file applied, want guard rejection")
	}
	// Unknown hash.
	if found, _ := db.UpdateReviewState(ctx, hash64("nope"), ReviewTransition{
		From: []string{ReviewDraft}, To: ReviewSubmitted,
	}); found {
		t.Error("transition on unknown hash applied")
	}
}

func TestReview_TrashRestoreKeepsState(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")
	h := hash64("restore")
	insertStagedFile(t, db, h, ReviewSubmitted, uid)

	if _, found, err := db.SoftDeleteFileByHash(ctx, h); err != nil || !found {
		t.Fatalf("SoftDeleteFileByHash: found=%v err=%v", found, err)
	}
	// The trash listing badges the staged state.
	trashed, err := db.ListTrashedFiles(ctx)
	if err != nil || len(trashed) != 1 {
		t.Fatalf("ListTrashedFiles: n=%d err=%v", len(trashed), err)
	}
	if trashed[0].ReviewState != ReviewSubmitted {
		t.Errorf("trash entry state = %q, want submitted", trashed[0].ReviewState)
	}
	// A discarded submission is out of the moderation queue while trashed...
	pending, err := db.ListPendingReview(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending while trashed = %d, want 0 (err=%v)", len(pending), err)
	}
	// ...and re-enters it (not the library) on restore.
	if found, err := db.RestoreFileByHash(ctx, h); err != nil || !found {
		t.Fatalf("RestoreFileByHash: found=%v err=%v", found, err)
	}
	if got := reviewState(t, db, h); got != ReviewSubmitted {
		t.Errorf("state after restore = %q, want submitted", got)
	}
	if pending, _ = db.ListPendingReview(ctx); len(pending) != 1 {
		t.Errorf("pending after restore = %d, want 1", len(pending))
	}
}

func TestReviewListings_ScopeAndUploader(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	alice := makeReviewUser(t, db, "alice")
	bob := makeReviewUser(t, db, "bob")

	insertStagedFile(t, db, hash64("a1"), ReviewDraft, alice)
	insertStagedFile(t, db, hash64("a2"), ReviewReturned, alice)
	insertStagedFile(t, db, hash64("b1"), ReviewSubmitted, bob)
	insertStagedFile(t, db, hash64("ap"), ReviewApproved, alice) // published: in neither listing

	mine, err := db.ListUploadsByUser(ctx, alice)
	if err != nil {
		t.Fatalf("ListUploadsByUser: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("alice staged = %d, want 2", len(mine))
	}
	for _, e := range mine {
		if e.ReviewState == ReviewApproved {
			t.Errorf("approved file leaked into staging list: %v", e.Hash)
		}
	}

	all, err := db.ListPendingReview(ctx)
	if err != nil {
		t.Fatalf("ListPendingReview: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("pending review = %d, want 3", len(all))
	}
	// Ordered by uploader name: alice's two first, then bob's.
	if all[0].UploaderName.String != "alice" || all[2].UploaderName.String != "bob" {
		t.Errorf("uploader order = %q,%q,%q, want alice grouped before bob",
			all[0].UploaderName.String, all[1].UploaderName.String, all[2].UploaderName.String)
	}
}

func TestReviewVisibility_StagedFilesHiddenEverywhere(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")
	h := hash64("hidden")
	insertStagedFile(t, db, h, ReviewDraft, uid)

	if files, _ := db.ListFiles(ctx); len(files) != 0 {
		t.Errorf("ListFiles = %d, want 0 (draft hidden)", len(files))
	}
	if artists, _ := db.ListArtists(ctx); len(artists) != 0 {
		t.Errorf("ListArtists = %d, want 0 (entity of a draft-only artist hidden)", len(artists))
	}
	if res, _ := db.Search(ctx, "An Artist"); len(res.Artists)+len(res.Albums)+len(res.Tracks) != 0 {
		t.Error("Search found a draft file")
	}
	// Even a guest-playable flag must not expose a staged file anonymously.
	if _, err := db.SetGuestPlayable(ctx, h, true); err != nil {
		t.Fatalf("SetGuestPlayable: %v", err)
	}
	if accessible(t, db, h) {
		t.Error("staged file accessible anonymously despite pending review")
	}
	// Playlists and favorites refuse staged files.
	if _, err := db.ToggleFavorite(ctx, uid, h); err != ErrFileNotFound {
		t.Errorf("ToggleFavorite on draft err = %v, want ErrFileNotFound", err)
	}

	// Approval flips all of it.
	if found, _ := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewDraft}, To: ReviewSubmitted, OwnerID: uid, StampSubmittedAt: true,
	}); !found {
		t.Fatal("submit failed")
	}
	if found, _ := db.UpdateReviewState(ctx, h, ReviewTransition{
		From: []string{ReviewSubmitted}, To: ReviewApproved,
	}); !found {
		t.Fatal("approve failed")
	}
	if files, _ := db.ListFiles(ctx); len(files) != 1 {
		t.Errorf("ListFiles after approve = %d, want 1", len(files))
	}
	if artists, _ := db.ListArtists(ctx); len(artists) != 1 {
		t.Errorf("ListArtists after approve = %d, want 1", len(artists))
	}
	if !accessible(t, db, h) {
		t.Error("approved guest-playable file should be reachable anonymously")
	}
	if _, err := db.ToggleFavorite(ctx, uid, h); err != nil {
		t.Errorf("ToggleFavorite after approve: %v", err)
	}
}

// TestBulkUpdateReviewState_ApproveSkipsNonMatching covers the batched moderation
// approve: only rows whose current state is in From transition; a draft (not in
// From) is left alone, and the count reflects what actually changed.
func TestBulkUpdateReviewState_ApproveSkipsNonMatching(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")

	hs := hash64("bsub1")
	hr := hash64("bsub2")
	hd := hash64("bdraft")
	insertStagedFile(t, db, hs, ReviewSubmitted, uid)
	insertStagedFile(t, db, hr, ReviewSubmitted, uid)
	insertStagedFile(t, db, hd, ReviewDraft, uid) // not in From -> skipped

	// Empty From is rejected (guards against an unscoped UPDATE).
	if _, err := db.BulkUpdateReviewState(ctx, []string{hs}, ReviewTransition{To: ReviewApproved}); err == nil {
		t.Error("empty From should error")
	}

	n, err := db.BulkUpdateReviewState(ctx, []string{hs, hr, hd}, ReviewTransition{
		From: []string{ReviewSubmitted, ReviewReturned}, To: ReviewApproved,
	})
	if err != nil {
		t.Fatalf("BulkUpdateReviewState: %v", err)
	}
	if n != 2 {
		t.Fatalf("affected = %d, want 2 (draft skipped)", n)
	}
	if reviewState(t, db, hs) != ReviewApproved || reviewState(t, db, hr) != ReviewApproved {
		t.Error("submitted files should be approved")
	}
	if reviewState(t, db, hd) != ReviewDraft {
		t.Error("draft should be untouched (not in From)")
	}
}

// TestBulkUpdateReviewState_ReturnNoteSkipsTrashed covers the batched return: the
// note lands on the live row, and a trashed row (deleted_at set) is skipped by the
// same guard the single-hash path uses.
func TestBulkUpdateReviewState_ReturnNoteSkipsTrashed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "up")
	hLive := hash64("bret-live")
	hTrash := hash64("bret-trash")
	insertStagedFile(t, db, hLive, ReviewSubmitted, uid)
	insertStagedFile(t, db, hTrash, ReviewSubmitted, uid)
	if _, found, err := db.SoftDeleteFileByHash(ctx, hTrash); err != nil || !found {
		t.Fatalf("trash setup: found=%v err=%v", found, err)
	}

	n, err := db.BulkUpdateReviewState(ctx, []string{hLive, hTrash}, ReviewTransition{
		From: []string{ReviewSubmitted, ReviewReturned}, To: ReviewReturned, Note: "fix tags",
	})
	if err != nil {
		t.Fatalf("BulkUpdateReviewState: %v", err)
	}
	if n != 1 {
		t.Fatalf("affected = %d, want 1 (trashed skipped)", n)
	}
	if f, _ := db.GetFileByHash(ctx, hLive); f.ReviewState != ReviewReturned || f.ReviewNote.String != "fix tags" {
		t.Errorf("live file = %q note=%q, want returned + note", f.ReviewState, f.ReviewNote.String)
	}
	if ft, _ := db.GetFileByHash(ctx, hTrash); ft.ReviewState != ReviewSubmitted || !ft.DeletedAt.Valid {
		t.Errorf("trashed file changed: state=%q deleted=%v", ft.ReviewState, ft.DeletedAt.Valid)
	}
}

// TestBulkDiscardOwnUploads_OwnerAndStateScoped covers the batched My-uploads
// remove: only the caller's editable (draft/returned) files are soft-deleted;
// submitted and foreign rows are left untouched.
func TestBulkDiscardOwnUploads_OwnerAndStateScoped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	uid := makeReviewUser(t, db, "owner")
	other := makeReviewUser(t, db, "other")

	hDraft := hash64("disc-draft")
	hReturned := hash64("disc-returned")
	hSubmitted := hash64("disc-submitted")
	hForeign := hash64("disc-foreign")
	insertStagedFile(t, db, hDraft, ReviewDraft, uid)
	insertStagedFile(t, db, hReturned, ReviewReturned, uid)
	insertStagedFile(t, db, hSubmitted, ReviewSubmitted, uid) // submitted: not withdrawable
	insertStagedFile(t, db, hForeign, ReviewDraft, other)     // not owned

	n, err := db.BulkDiscardOwnUploads(ctx, []string{hDraft, hReturned, hSubmitted, hForeign}, uid)
	if err != nil {
		t.Fatalf("BulkDiscardOwnUploads: %v", err)
	}
	if n != 2 {
		t.Fatalf("affected = %d, want 2 (own draft+returned only)", n)
	}
	for _, h := range []string{hDraft, hReturned} {
		if f, _ := db.GetFileByHash(ctx, h); !f.DeletedAt.Valid {
			t.Errorf("%s should be trashed", h)
		}
	}
	for _, h := range []string{hSubmitted, hForeign} {
		if f, _ := db.GetFileByHash(ctx, h); f.DeletedAt.Valid {
			t.Errorf("%s should be untouched (submitted or foreign)", h)
		}
	}
}

func TestMigration017_GrantsModeratorPermissions(t *testing.T) {
	db := openMem(t)
	for _, tc := range []struct{ role, perm string }{
		{"1", "content.moderate"}, // admin
		{"2", "content.moderate"}, // moderator
		{"2", "file.upload"},      // moderators are the trusted uploaders
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM role_permissions WHERE role_id = ? AND permission = ?`,
			tc.role, tc.perm).Scan(&n); err != nil {
			t.Fatalf("query role_permissions: %v", err)
		}
		if n != 1 {
			t.Errorf("role %s missing %s", tc.role, tc.perm)
		}
	}
}
