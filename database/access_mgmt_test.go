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

func TestFilteredListings_RespectAccess(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	guestHash := hash64("flguest")
	privHash := hash64("flpriv")
	insertAccessFile(t, db, guestHash) // both share artist/album ("An Artist"/"An Album")
	insertAccessFile(t, db, privHash)
	setGuest(t, db, guestHash, true)

	// Anonymous sees only the guest file across every listing surface.
	files, err := db.ListFilesFiltered(ctx, anon())
	if err != nil {
		t.Fatalf("ListFilesFiltered: %v", err)
	}
	if len(files) != 1 || files[0].Hash != guestHash {
		t.Fatalf("anon files = %d (%v), want 1 guest file", len(files), files)
	}
	if !files[0].GuestPlayable {
		t.Error("guest file should report GuestPlayable=true")
	}

	artists, err := db.ListArtistsFiltered(ctx, anon())
	if err != nil {
		t.Fatalf("ListArtistsFiltered: %v", err)
	}
	if len(artists) != 1 || artists[0].TrackCount != 1 {
		t.Fatalf("anon artists = %v, want one artist with track_count 1", artists)
	}

	tracks, err := db.ListTracksByAlbumArtistFiltered(ctx, "An Artist", "An Album", anon())
	if err != nil {
		t.Fatalf("ListTracksByAlbumArtistFiltered: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("anon tracks = %d, want 1", len(tracks))
	}

	// A user granted the whole library sees both files.
	u := mkUser(t, db, "u")
	g, _ := db.CreateAccessGroup(ctx, "all")
	if err := db.AddGroupMember(ctx, g, u); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if _, err := db.AddContentGrant(ctx, g, ScopeAll, "", "", anon()); err != nil {
		t.Fatalf("AddContentGrant: %v", err)
	}
	files, err = db.ListFilesFiltered(ctx, uid(u))
	if err != nil {
		t.Fatalf("ListFilesFiltered(user): %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("granted user files = %d, want 2", len(files))
	}
}

func TestManagementQueries_GroupLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	u := mkUser(t, db, "alice")
	g, err := db.CreateAccessGroup(ctx, "friends")
	if err != nil {
		t.Fatalf("CreateAccessGroup: %v", err)
	}
	if err := db.AddGroupMember(ctx, g, u); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	members, err := db.ListGroupMembers(ctx, g)
	if err != nil || len(members) != 1 || members[0].Username != "alice" {
		t.Fatalf("ListGroupMembers = %v err=%v, want [alice]", members, err)
	}

	gid, err := db.AddContentGrant(ctx, g, ScopeArtist, "An Artist", "", anon())
	if err != nil {
		t.Fatalf("AddContentGrant: %v", err)
	}
	grants, err := db.ListContentGrants(ctx, g)
	if err != nil || len(grants) != 1 || grants[0].ScopeType != ScopeArtist {
		t.Fatalf("ListContentGrants = %v err=%v", grants, err)
	}
	if found, err := db.DeleteContentGrant(ctx, gid); err != nil || !found {
		t.Fatalf("DeleteContentGrant: found=%v err=%v", found, err)
	}

	if err := db.RemoveGroupMember(ctx, g, u); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	members, _ = db.ListGroupMembers(ctx, g)
	if len(members) != 0 {
		t.Fatalf("members after remove = %d, want 0", len(members))
	}

	if found, err := db.DeleteAccessGroup(ctx, g); err != nil || !found {
		t.Fatalf("DeleteAccessGroup: found=%v err=%v", found, err)
	}
	groups, _ := db.ListAccessGroups(ctx)
	if len(groups) != 0 {
		t.Fatalf("groups after delete = %d, want 0", len(groups))
	}
}

func TestAutoDerive_GrantsAndRespectsManual(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	autoHash := hash64("autoderive")
	manualHash := hash64("manual")
	insertAccessFile(t, db, autoHash)
	insertAccessFile(t, db, manualHash)

	// Manually mark the second file private — auto-derive must never override it.
	setGuest(t, db, manualHash, false)

	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: true, Licenses: []string{"CC0-1.0"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy: %v", err)
	}
	p, err := db.GetAutoDerivePolicy(ctx)
	if err != nil || !p.Enabled || len(p.Licenses) != 1 || p.Licenses[0] != "CC0-1.0" {
		t.Fatalf("GetAutoDerivePolicy = %+v err=%v", p, err)
	}

	// Setting a matching license on the non-manual file grants guest access.
	if _, err := db.SetLicense(ctx, autoHash, "CC0-1.0"); err != nil {
		t.Fatalf("SetLicense(auto): %v", err)
	}
	if !accessible(t, db, autoHash, anon()) {
		t.Error("auto-derived file should be guest-playable after license set")
	}

	// The manually-private file is not granted even with a matching license.
	if _, err := db.SetLicense(ctx, manualHash, "CC0-1.0"); err != nil {
		t.Fatalf("SetLicense(manual): %v", err)
	}
	if accessible(t, db, manualHash, anon()) {
		t.Error("manual override must survive auto-derivation")
	}

	// A non-allow-listed license never grants.
	otherHash := hash64("other")
	insertAccessFile(t, db, otherHash)
	if _, err := db.SetLicense(ctx, otherHash, "all-rights-reserved"); err != nil {
		t.Fatalf("SetLicense(other): %v", err)
	}
	if accessible(t, db, otherHash, anon()) {
		t.Error("non-free license must not be auto-derived")
	}

	// ApplyAutoDerive is a no-op the second time (already granted).
	if n, err := db.ApplyAutoDerive(ctx); err != nil || n != 0 {
		t.Fatalf("ApplyAutoDerive re-run = %d err=%v, want 0", n, err)
	}
}

func TestApplyAutoDerive_SweepsExisting(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)

	h := hash64("sweep")
	insertAccessFile(t, db, h)
	// License set before the policy exists: no grant yet.
	if _, err := db.SetLicense(ctx, h, "public-domain"); err != nil {
		t.Fatalf("SetLicense: %v", err)
	}
	if accessible(t, db, h, anon()) {
		t.Fatal("file should not be guest before policy enabled")
	}

	if err := db.SetAutoDerivePolicy(ctx, AutoDerivePolicy{Enabled: true, Licenses: []string{"public-domain"}}); err != nil {
		t.Fatalf("SetAutoDerivePolicy: %v", err)
	}
	n, err := db.ApplyAutoDerive(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ApplyAutoDerive = %d err=%v, want 1", n, err)
	}
	if !accessible(t, db, h, anon()) {
		t.Error("sweep should have granted the public-domain file")
	}
}
