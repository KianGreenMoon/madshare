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

// TestMigration047_GuestOnlyBackfill: the user mapping becomes the plain
// guest-only flag, freezing each mapped peer's effective audience at upgrade
// time — mapped to an active content.access holder stays full, everything
// else a mapping could express (no permission, disabled account) lands
// demoted, and unmapped peers are untouched. The user_id column is gone after.
func TestMigration047_GuestOnlyBackfill(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL"} {
		if _, err := sqlDB.Exec(p); err != nil {
			t.Fatalf("pragma %q: %v", p, err)
		}
	}
	db := &DB{DB: sqlDB}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}
	var m047 migration
	for _, m := range migs {
		switch {
		case m.version <= 46:
			if err := db.applyMigration(m); err != nil {
				t.Fatalf("apply migration %d: %v", m.version, err)
			}
		case m.version == 47:
			m047 = m
		}
	}
	if m047.version != 47 {
		t.Fatal("migration 047 not found")
	}

	// Pre-047 state: role 4 (migration 003/011) holds content.access.
	seedUser := func(name string, roleID int64, disabled int) int64 {
		res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at, disabled)
			VALUES (?, 'x', 1000, ?)`, name, disabled)
		if err != nil {
			t.Fatalf("insert user %s: %v", name, err)
		}
		id, _ := res.LastInsertId()
		if roleID != 0 {
			mustExec(t, db, `INSERT INTO user_roles (user_id, role_id) VALUES (?, ?)`, id, roleID)
		}
		return id
	}
	full := seedUser("mapped-full", 4, 0)
	none := seedUser("mapped-none", 0, 0)
	off := seedUser("mapped-disabled", 4, 1)
	seedPeer := func(key string, userID any) {
		mustExec(t, db, `INSERT INTO federation_nodes (public_key, trust_state, user_id, trusted_at)
			VALUES (?, 'friend', ?, 1000)`, key, userID)
	}
	seedPeer("aa01", full)
	seedPeer("aa02", none)
	seedPeer("aa03", off)
	seedPeer("aa04", nil)

	if err := db.applyMigration(m047); err != nil {
		t.Fatalf("apply migration 047: %v", err)
	}

	for _, tc := range []struct {
		key  string
		want int
		why  string
	}{
		{"aa01", 0, "mapped to an active content.access holder stays full"},
		{"aa02", 1, "mapped without content.access is demoted"},
		{"aa03", 1, "mapped to a disabled account is demoted"},
		{"aa04", 0, "unmapped stays the full default"},
	} {
		var got int
		if err := db.QueryRow(`SELECT guest_only FROM federation_nodes WHERE public_key = ?`, tc.key).Scan(&got); err != nil {
			t.Fatalf("read guest_only of %s: %v", tc.key, err)
		}
		if got != tc.want {
			t.Errorf("%s: guest_only = %d, want %d (%s)", tc.key, got, tc.want, tc.why)
		}
	}

	// The mapping column itself is gone.
	var cols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('federation_nodes') WHERE name = 'user_id'`).Scan(&cols); err != nil {
		t.Fatalf("table_info: %v", err)
	}
	if cols != 0 {
		t.Error("user_id column still present after 047")
	}
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
