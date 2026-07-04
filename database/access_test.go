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

// TestBulkSetLicenseAndGuest sets one license + one guest flag across a set in a
// single guarded UPDATE each: live files change, an unknown/trashed hash is
// skipped, and the count reflects what was actually written.
func TestBulkSetLicenseAndGuest(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	h1 := hash64("bacc1")
	h2 := hash64("bacc2")
	trashed := hash64("bacc-trashed")
	insertAccessFile(t, db, h1)
	insertAccessFile(t, db, h2)
	insertAccessFile(t, db, trashed)
	if _, _, err := db.SoftDeleteFileByHash(ctx, trashed); err != nil {
		t.Fatalf("trash setup: %v", err)
	}
	missing := hash64("bacc-missing")

	n, err := db.BulkSetLicense(ctx, []string{h1, h2, trashed, missing}, "CC0-1.0")
	if err != nil {
		t.Fatalf("BulkSetLicense: %v", err)
	}
	if n != 2 {
		t.Fatalf("license affected = %d, want 2 (trashed + missing skipped)", n)
	}

	g, err := db.BulkSetGuestPlayable(ctx, []string{h1, h2}, true)
	if err != nil {
		t.Fatalf("BulkSetGuestPlayable: %v", err)
	}
	if g != 2 {
		t.Fatalf("guest affected = %d, want 2", g)
	}

	for _, h := range []string{h1, h2} {
		var lic string
		var guest, manual int
		if err := db.QueryRow(`SELECT COALESCE(r.license,''), r.guest_playable, r.guest_playable_manual FROM recordings r JOIN files f ON f.recording_id = r.id WHERE f.hash=?`, h).
			Scan(&lic, &guest, &manual); err != nil {
			t.Fatalf("read %s: %v", h, err)
		}
		if lic != "CC0-1.0" || guest != 1 || manual != 1 {
			t.Errorf("%s license=%q guest=%d manual=%d, want CC0-1.0/1/1", h, lic, guest, manual)
		}
	}
}
