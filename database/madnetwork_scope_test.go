package database

import (
	"context"
	"database/sql"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// Federation F5 — the audience predicate at the storage layer
// (docs/architecture/federation-access.md §Sharing scope). The load-bearing claim is
// that PublishedCatalog and BlobVisibleTo answer from ONE rule, so a hash that
// falls out of an audience's catalog also stops being fetchable by it.

// seedScopeFile inserts one live approved file and returns its recording id.
func seedScopeFile(t *testing.T, db *DB, hash, title string) int64 {
	t.Helper()
	ctx := context.Background()
	meta := &MediaMetadata{
		Title:       title,
		Artist:      sql.NullString{String: "Scope Artist", Valid: true},
		AlbumArtist: sql.NullString{String: "Scope Artist", Valid: true},
		Album:       sql.NullString{String: "Scope Album", Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(ctx, newFile(hash), newUpload(hash+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile %s: %v", hash, err)
	}
	var rec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE hash = ?`, hash).Scan(&rec); err != nil {
		t.Fatalf("recording of %s: %v", hash, err)
	}
	return rec
}

// catalogTitles reduces a catalog to its titles, which is what these assertions
// care about.
func catalogTitles(t *testing.T, db *DB, aud federation.Audience) []string {
	t.Helper()
	entries, err := db.PublishedCatalog(context.Background(), aud)
	if err != nil {
		t.Fatalf("PublishedCatalog: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Title)
	}
	return out
}

func visible(t *testing.T, db *DB, hash string, aud federation.Audience) bool {
	t.Helper()
	vis, found, err := db.BlobVisibleTo(context.Background(), hash, aud)
	if err != nil {
		t.Fatalf("BlobVisibleTo(%s): %v", hash, err)
	}
	if !found {
		t.Fatalf("BlobVisibleTo(%s): hash not found", hash)
	}
	return vis
}

// TestShareDepthFiltersCatalogAndBytes: a private recording leaves the catalog
// and stops serving; the pair must move together.
func TestShareDepthFiltersCatalogAndBytes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	keepRec := seedScopeFile(t, db, "scope001", "Kept")
	hideRec := seedScopeFile(t, db, "scope002", "Hidden")
	_ = keepRec

	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 2 {
		t.Fatalf("baseline catalog = %v, want both entries", got)
	}
	if !visible(t, db, "scope002", federation.FriendAudience) {
		t.Fatal("baseline: the blob should be visible to a friend")
	}

	private := federation.DepthPrivate
	if ok, err := db.SetRecordingAccess(ctx, hideRec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: private}); err != nil || !ok {
		t.Fatalf("set private: ok=%v err=%v", ok, err)
	}

	got := catalogTitles(t, db, federation.FriendAudience)
	if len(got) != 1 || got[0] != "Kept" {
		t.Errorf("catalog after going private = %v, want [Kept]", got)
	}
	if visible(t, db, "scope002", federation.FriendAudience) {
		t.Error("a private recording's blob is still served — catalog and bytes disagree")
	}
	if !visible(t, db, "scope001", federation.FriendAudience) {
		t.Error("the untouched recording stopped serving")
	}
}

// TestShareDepthInheritsNodeDefault: a NULL share_depth follows the node
// setting, and an explicit depth overrides it in both directions.
func TestShareDepthInheritsNodeDefault(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedScopeFile(t, db, "scope003", "Inheriting")
	pinnedRec := seedScopeFile(t, db, "scope004", "Pinned open")

	// Node goes private: everything inheriting drops out.
	if err := db.SetMadnetworkPolicy(ctx, MadnetworkPolicy{
		SeedEnabled: true, SeedCache: true, HideUnavailable: true,
		DefaultShareDepth: federation.DepthPrivate,
	}); err != nil {
		t.Fatalf("SetMadnetworkPolicy: %v", err)
	}
	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 0 {
		t.Errorf("catalog on a private node = %v, want empty", got)
	}

	// An explicit depth overrides the node default upward.
	if ok, err := db.SetRecordingAccess(ctx, pinnedRec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: federation.DepthFriends}); err != nil || !ok {
		t.Fatalf("pin depth: ok=%v err=%v", ok, err)
	}
	got := catalogTitles(t, db, federation.FriendAudience)
	if len(got) != 1 || got[0] != "Pinned open" {
		t.Errorf("catalog with one pinned recording = %v, want [Pinned open]", got)
	}
	if !visible(t, db, "scope004", federation.FriendAudience) {
		t.Error("the pinned recording's blob should serve on a private node")
	}
	if visible(t, db, "scope003", federation.FriendAudience) {
		t.Error("an inheriting recording served on a private node")
	}

	// Clearing the override puts it back under the node default.
	if ok, err := db.SetRecordingAccess(ctx, pinnedRec, nil, nil,
		ShareDepthUpdate{Set: true, Inherit: true}); err != nil || !ok {
		t.Fatalf("clear depth: ok=%v err=%v", ok, err)
	}
	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 0 {
		t.Errorf("catalog after clearing the override = %v, want empty", got)
	}
}

// TestScopeGatesFriendsAndMembers: the three scopes against the mesh classes
// (F7). Direct friends is the value that discriminates — it reaches a friend and
// stops at a member — while Madnetwork reaches both, which is what makes the
// community the default audience rather than an extra tier.
func TestScopeGatesFriendsAndMembers(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	rec := seedScopeFile(t, db, "scope005", "Friends only")

	if ok, err := db.SetRecordingAccess(ctx, rec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: federation.DepthFriends}); err != nil || !ok {
		t.Fatalf("set scope to direct friends: ok=%v err=%v", ok, err)
	}
	if !visible(t, db, "scope005", federation.FriendAudience) {
		t.Error("Direct friends should reach a direct friend")
	}
	if visible(t, db, "scope005", federation.MemberAudience) {
		t.Error("Direct friends must not reach a member of the wider community")
	}

	if ok, err := db.SetRecordingAccess(ctx, rec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: federation.DepthUnlimited}); err != nil || !ok {
		t.Fatalf("set scope to madnetwork: ok=%v err=%v", ok, err)
	}
	if !visible(t, db, "scope005", federation.MemberAudience) {
		t.Error("Madnetwork should reach a member")
	}
	if !visible(t, db, "scope005", federation.FriendAudience) {
		t.Error("Madnetwork should reach a direct friend too — a friend is in the community")
	}

	// The in-between values the ladder used to offer are refused outright rather
	// than rounded: rounding a sharing decision is the quiet widening the
	// three-value vocabulary exists to prevent (migration 035).
	if ok, err := db.SetRecordingAccess(ctx, rec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: 1}); err == nil || ok {
		t.Errorf("a hop count should be refused, got ok=%v err=%v", ok, err)
	}
}

// TestOutsiderAudienceIsServedNothing pins the fail-closed zero value: an
// Audience nobody filled in is an outsider. Before F7 gave it a Class it read as
// distance 0 — a *direct friend*, the widest audience there is — so this is the
// storage half of that fix.
func TestOutsiderAudienceIsServedNothing(t *testing.T) {
	db := openMem(t)
	seedScopeFile(t, db, "scope008", "Everything, by default")

	if visible(t, db, "scope008", federation.Audience{}) {
		t.Error("the zero audience must be served nothing")
	}
	if got := catalogTitles(t, db, federation.Audience{}); len(got) != 0 {
		t.Errorf("catalog for the zero audience = %v, want empty", got)
	}
	// And the node default really is Madnetwork, so this did not pass merely by
	// everything being private.
	if !visible(t, db, "scope008", federation.MemberAudience) {
		t.Error("a member should reach a recording under the node default")
	}
}

// TestGuestOnlyAudienceSeesGuestContentOnly: the per-friend half of the
// audience, resolved from the user mapping, filters both surfaces.
func TestGuestOnlyAudienceSeesGuestContentOnly(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedScopeFile(t, db, "scope006", "Members only")
	seedScopeFile(t, db, "scope007", "Open door")

	if found, err := db.SetGuestPlayable(ctx, "scope007", true); err != nil || !found {
		t.Fatalf("SetGuestPlayable: found=%v err=%v", found, err)
	}

	got := catalogTitles(t, db, federation.GuestAudience)
	if len(got) != 1 || got[0] != "Open door" {
		t.Errorf("guest-only catalog = %v, want [Open door]", got)
	}
	if !visible(t, db, "scope007", federation.GuestAudience) {
		t.Error("a guest-playable blob should serve a guest-only audience")
	}
	if visible(t, db, "scope006", federation.GuestAudience) {
		t.Error("an ordinary blob served a guest-only audience")
	}
	// The full friend audience is unaffected.
	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 2 {
		t.Errorf("full friend catalog = %v, want both entries", got)
	}
}

// TestPeerAudienceFromUserMapping: unmapped is the wide default, a mapping to an
// account without content.access is the narrow one, and a disabled account
// narrows too.
func TestPeerAudienceFromUserMapping(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	peerID, err := db.InsertFederationPeer(ctx, &federation.Peer{
		PublicKey: "aa11", Name: "peer", State: federation.PeerFriend, CreatedAt: 1700000000,
	})
	if err != nil {
		t.Fatalf("InsertFederationPeer: %v", err)
	}

	aud, err := db.PeerAudience(ctx, peerID)
	if err != nil {
		t.Fatalf("PeerAudience: %v", err)
	}
	if aud != federation.FriendAudience {
		t.Errorf("unmapped peer audience = %+v, want the full friend audience", aud)
	}

	// Map it to a listener (holds content.access, role 4 from migration 003/011).
	full := createScopeUser(t, db, "mapped-full", 4)
	if err := db.SetFederationPeerUser(ctx, peerID, &full); err != nil {
		t.Fatalf("SetFederationPeerUser: %v", err)
	}
	if aud, _ := db.PeerAudience(ctx, peerID); aud.GuestOnly {
		t.Error("a peer mapped to a content.access holder should keep the full audience")
	}

	// Map it to an account with no roles at all.
	none := createScopeUser(t, db, "mapped-none", 0)
	if err := db.SetFederationPeerUser(ctx, peerID, &none); err != nil {
		t.Fatalf("SetFederationPeerUser: %v", err)
	}
	if aud, _ := db.PeerAudience(ctx, peerID); !aud.GuestOnly {
		t.Error("a peer mapped to an account without content.access should be guest-only")
	}

	// Disabling the mapped account narrows it the same way.
	if _, err := db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, full); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if err := db.SetFederationPeerUser(ctx, peerID, &full); err != nil {
		t.Fatalf("SetFederationPeerUser: %v", err)
	}
	if aud, _ := db.PeerAudience(ctx, peerID); !aud.GuestOnly {
		t.Error("a peer mapped to a disabled account should be guest-only")
	}
}

// createScopeUser inserts a user, optionally granting one role id (0 = none).
func createScopeUser(t *testing.T, db *DB, name string, roleID int64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, 'x', 1700000000)`, name)
	if err != nil {
		t.Fatalf("insert user %s: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	if roleID != 0 {
		if _, err := db.Exec(`INSERT INTO user_roles (user_id, role_id) VALUES (?, ?)`, id, roleID); err != nil {
			t.Fatalf("grant role: %v", err)
		}
	}
	return id
}

// TestBulkSetShareDepthByTagsets: the bulk arm writes through appearances onto
// their recordings, matching the license/guest bulk setters.
func TestBulkSetShareDepthByTagsets(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedScopeFile(t, db, "scope008", "Bulk one")
	seedScopeFile(t, db, "scope009", "Bulk two")

	var ids []int64
	rows, err := db.Query(`SELECT id FROM tagsets ORDER BY id`)
	if err != nil {
		t.Fatalf("list tagsets: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("seeded %d appearances, want 2", len(ids))
	}

	n, err := db.BulkSetShareDepthByTagsets(ctx, ids,
		ShareDepthUpdate{Set: true, Depth: federation.DepthPrivate})
	if err != nil {
		t.Fatalf("BulkSetShareDepthByTagsets: %v", err)
	}
	if n != 2 {
		t.Errorf("affected = %d, want 2", n)
	}
	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 0 {
		t.Errorf("catalog after a bulk private = %v, want empty", got)
	}

	// An unset update is a no-op, not a silent clear.
	if n, err := db.BulkSetShareDepthByTagsets(ctx, ids, ShareDepthUpdate{}); err != nil || n != 0 {
		t.Errorf("no-op bulk = (%d, %v), want (0, nil)", n, err)
	}
	if got := catalogTitles(t, db, federation.FriendAudience); len(got) != 0 {
		t.Errorf("a no-op bulk changed the catalog: %v", got)
	}
}
