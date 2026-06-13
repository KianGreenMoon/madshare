package database

import (
	"context"
	"testing"
)

// setGuest is a helper that marks a file guest-playable (manual) and fails the
// test on error.
func setGuest(t *testing.T, db *DB, hash string, guest bool) {
	t.Helper()
	if _, err := db.SetGuestPlayable(context.Background(), hash, guest); err != nil {
		t.Fatalf("SetGuestPlayable: %v", err)
	}
}

func TestGuestListings_RespectAccess(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	guestHash := hash64("flguest")
	privHash := hash64("flpriv")
	insertAccessFile(t, db, guestHash) // both share artist/album ("An Artist"/"An Album")
	insertAccessFile(t, db, privHash)
	setGuest(t, db, guestHash, true)

	// The guest listings expose only the guest-playable file across every surface.
	files, err := db.ListFilesGuest(ctx)
	if err != nil {
		t.Fatalf("ListFilesGuest: %v", err)
	}
	if len(files) != 1 || files[0].Hash != guestHash {
		t.Fatalf("guest files = %d (%v), want 1 guest file", len(files), files)
	}
	if !files[0].GuestPlayable {
		t.Error("guest file should report GuestPlayable=true")
	}

	artists, err := db.ListArtistsGuest(ctx)
	if err != nil {
		t.Fatalf("ListArtistsGuest: %v", err)
	}
	if len(artists) != 1 || artists[0].TrackCount != 1 {
		t.Fatalf("guest artists = %v, want one artist with track_count 1", artists)
	}

	albumID, found, err := db.LookupAlbumID(ctx, "An Artist", "An Album")
	if err != nil || !found {
		t.Fatalf("LookupAlbumID: found=%v err=%v", found, err)
	}
	tracks, err := db.ListTracksByAlbumIDGuest(ctx, albumID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumIDGuest: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("guest tracks = %d, want 1", len(tracks))
	}
}

func TestAutoDerive_GrantsAndRespectsManual(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	autoHash := hash64("autoderive")
	manualHash := hash64("manual")
	insertAccessFile(t, db, autoHash)
	insertAccessFile(t, db, manualHash)

	// Manually mark the second file private — license policy must never override it.
	setGuest(t, db, manualHash, false)

	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: true, Licenses: []string{"CC0-1.0"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy: %v", err)
	}
	p, err := db.GetAutoDerivePolicy(ctx)
	if err != nil || !p.Enabled || len(p.Licenses) != 1 || p.Licenses[0] != "CC0-1.0" {
		t.Fatalf("GetAutoDerivePolicy = %+v err=%v", p, err)
	}

	// Setting a matching license grants guest access via the live policy check.
	if _, err := db.SetLicense(ctx, autoHash, "CC0-1.0"); err != nil {
		t.Fatalf("SetLicense(auto): %v", err)
	}
	if !accessible(t, db, autoHash) {
		t.Error("license-match file should be guest-accessible when policy is enabled")
	}

	// The manually-private file is not accessible even with a matching license.
	if _, err := db.SetLicense(ctx, manualHash, "CC0-1.0"); err != nil {
		t.Fatalf("SetLicense(manual): %v", err)
	}
	if accessible(t, db, manualHash) {
		t.Error("explicit manual override must win over license policy")
	}

	// A non-allow-listed license never grants.
	otherHash := hash64("other")
	insertAccessFile(t, db, otherHash)
	if _, err := db.SetLicense(ctx, otherHash, "all-rights-reserved"); err != nil {
		t.Fatalf("SetLicense(other): %v", err)
	}
	if accessible(t, db, otherHash) {
		t.Error("non-free license must not grant guest access")
	}

	// Disabling the policy immediately revokes license-based access.
	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: false, Licenses: []string{"CC0-1.0"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy(disable): %v", err)
	}
	if accessible(t, db, autoHash) {
		t.Error("disabling policy must revoke license-based access immediately")
	}
}

func TestAutoDerive_LivePolicyCheck(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	h := hash64("sweep")
	insertAccessFile(t, db, h)
	// License set before the policy is enabled: not yet accessible.
	if _, err := db.SetLicense(ctx, h, "public-domain"); err != nil {
		t.Fatalf("SetLicense: %v", err)
	}
	if accessible(t, db, h) {
		t.Fatal("file should not be guest-accessible before policy is enabled")
	}

	// Enabling the policy makes it accessible immediately — no flush step.
	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: true, Licenses: []string{"public-domain"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy: %v", err)
	}
	if !accessible(t, db, h) {
		t.Error("enabling policy should immediately grant access to matching file")
	}

	// Removing the license from the allow-list revokes access immediately.
	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: true, Licenses: []string{"CC0-1.0"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy(change allowlist): %v", err)
	}
	if accessible(t, db, h) {
		t.Error("removing license from allowlist should immediately revoke access")
	}
}
