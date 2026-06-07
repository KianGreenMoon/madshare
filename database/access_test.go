package database

import (
	"context"
	"strings"
	"testing"
)

func hash64(seed string) string {
	return (seed + strings.Repeat("0", 64))[:64]
}

// insertAccessFile inserts a file (with the standard metadata: artist "An
// Artist", album "An Album") and returns its id.
func insertAccessFile(t *testing.T, db *DB, hash string) int64 {
	t.Helper()
	f := newFile(hash)
	if err := db.InsertFile(context.Background(), f, newUpload("song.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	return f.ID
}

// accessible reports whether an anonymous / capability-less request may reach
// the file (the guest-playable / license predicate). Callers holding
// content.access bypass this check entirely.
func accessible(t *testing.T, db *DB, hash string) bool {
	t.Helper()
	ok, err := db.FileAccessibleByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("FileAccessibleByHash: %v", err)
	}
	return ok
}

func TestAccess_DefaultDeny(t *testing.T) {
	db := openMem(t)
	h := hash64("deny")
	insertAccessFile(t, db, h)

	// No guest flag and no license policy: not reachable without content.access.
	if accessible(t, db, h) {
		t.Error("file should be denied to anonymous by default")
	}
}

func TestAccess_GuestPlayable(t *testing.T) {
	db := openMem(t)
	h := hash64("guest")
	insertAccessFile(t, db, h)

	if found, err := db.SetGuestPlayable(context.Background(), h, true); err != nil || !found {
		t.Fatalf("SetGuestPlayable: found=%v err=%v", found, err)
	}
	if !accessible(t, db, h) {
		t.Error("guest_playable file should be reachable anonymously")
	}
}

func TestAccess_UnknownHashDenied(t *testing.T) {
	db := openMem(t)
	if accessible(t, db, hash64("nope")) {
		t.Error("unknown hash should be denied")
	}
}
