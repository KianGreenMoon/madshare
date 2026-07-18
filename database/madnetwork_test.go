package database

import (
	"context"
	"database/sql"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

func TestPublishedCatalog(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := func(hash, artist, album, title string) {
		t.Helper()
		meta := &MediaMetadata{Title: title, ExtractedAt: 1700000000}
		if artist != "" {
			meta.Artist = sql.NullString{String: artist, Valid: true}
			meta.AlbumArtist = sql.NullString{String: artist, Valid: true}
		}
		if album != "" {
			meta.Album = sql.NullString{String: album, Valid: true}
		}
		if err := db.InsertFile(ctx, newFile(hash), newUpload(hash+".mp3"), meta); err != nil {
			t.Fatalf("InsertFile %s: %v", hash, err)
		}
	}
	seed("cat00001", "Artist A", "Album One", "Song One")
	seed("cat00002", "Artist A", "Album One", "Song Two")
	seed("cat00003", "Artist B", "Album Two", "Song Three")

	entries, err := db.PublishedCatalog(ctx)
	if err != nil {
		t.Fatalf("PublishedCatalog: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	byTitle := map[string]federation.CatalogEntry{}
	for _, e := range entries {
		byTitle[e.Title] = e
	}
	one := byTitle["Song One"]
	if one.Artist != "Artist A" || one.AlbumArtist != "Artist A" || one.Album != "Album One" {
		t.Errorf("entry display = %q/%q/%q, want resolved entity names", one.Artist, one.AlbumArtist, one.Album)
	}
	if one.Key == "" || one.RecordingKey == "" {
		t.Errorf("entry keys empty: %+v", one)
	}
	if len(one.Renditions) != 1 || one.Renditions[0].Hash != "cat00001" {
		t.Errorf("renditions = %+v, want the seeded blob's hash", one.Renditions)
	}

	// The serial is deterministic and moves when the catalog changes.
	s1 := federation.CatalogSerial(entries)
	again, _ := db.PublishedCatalog(ctx)
	if federation.CatalogSerial(again) != s1 {
		t.Error("serial not deterministic across identical builds")
	}

	// A trashed appearance leaves the catalog.
	var tagsetID int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE title = 'Song Three'`).Scan(&tagsetID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tagsets SET deleted_at = 1 WHERE id = ?`, tagsetID); err != nil {
		t.Fatal(err)
	}
	entries, _ = db.PublishedCatalog(ctx)
	if len(entries) != 2 {
		t.Errorf("after trash len = %d, want 2", len(entries))
	}
	if federation.CatalogSerial(entries) == s1 {
		t.Error("serial unchanged after a catalog change")
	}
}

func ptr(v int64) *int64 { return &v }

// catEntry builds a minimal remote catalog entry.
func catEntry(key, recording, artist, album, title, hash string) federation.CatalogEntry {
	return federation.CatalogEntry{
		Key: key, RecordingKey: recording,
		Title: title, Artist: artist, AlbumArtist: artist, Album: album,
		TrackNumber: ptr(1),
		Renditions:  []federation.CatalogRendition{{Hash: hash, Size: 1000, Codec: "flac"}},
	}
}

func TestMadnetworkCacheAndBrowse(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	friendA := insertPeer(t, db, "f1a1", "friend-a", federation.PeerFriend)
	friendB := insertPeer(t, db, "f2b2", "friend-b", federation.PeerFriend)
	pending := insertPeer(t, db, "f3c3", "pending-one", federation.PeerPendingIncoming)

	// friend-a and friend-b both offer the same track (same text, SAME hash →
	// one version); friend-b also offers a different album, and a second claimed
	// recording of "Crossing" with a different hash (→ two versions).
	if err := db.ReplacePeerCatalog(ctx, friendA, "serial-a", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Shared Artist", "Shared Album", "Shared Song", "hash-shared"),
		catEntry("2", "r2", "Shared Artist", "Shared Album", "Crossing", "hash-crossing-a"),
	}); err != nil {
		t.Fatalf("ReplacePeerCatalog A: %v", err)
	}
	if err := db.ReplacePeerCatalog(ctx, friendB, "serial-b", 200, []federation.CatalogEntry{
		catEntry("9", "r9", "shared artist", "shared album", "shared song", "hash-shared"), // case-folded dup
		catEntry("10", "r10", "shared artist", "shared album", "Crossing", "hash-crossing-b"),
		catEntry("11", "r11", "Only B", "B Album", "B Song", "hash-b"),
	}); err != nil {
		t.Fatalf("ReplacePeerCatalog B: %v", err)
	}
	// A pending (non-friend) peer's cache must stay invisible.
	if err := db.ReplacePeerCatalog(ctx, pending, "serial-p", 300, []federation.CatalogEntry{
		catEntry("50", "r50", "Ghost", "Ghost Album", "Ghost Song", "hash-ghost"),
	}); err != nil {
		t.Fatalf("ReplacePeerCatalog pending: %v", err)
	}

	// Sync state landed on the peer row.
	if p, _ := db.GetFederationPeer(ctx, friendA); p.CatalogSerial != "serial-a" || p.CatalogSyncedAt != 100 {
		t.Errorf("peer sync state = %q/%d, want serial-a/100", p.CatalogSerial, p.CatalogSyncedAt)
	}
	if err := db.MarkPeerCatalogChecked(ctx, friendA, "serial-a", 150); err != nil {
		t.Fatalf("MarkPeerCatalogChecked: %v", err)
	}
	if p, _ := db.GetFederationPeer(ctx, friendA); p.CatalogSyncedAt != 150 {
		t.Errorf("synced_at after check = %d, want 150", p.CatalogSyncedAt)
	}

	// Artists: case-insensitive merge, no Ghost.
	artists, err := db.MadnetworkArtists(ctx, "")
	if err != nil {
		t.Fatalf("MadnetworkArtists: %v", err)
	}
	if len(artists) != 2 {
		t.Fatalf("artists = %+v, want 2 (merged + Only B)", artists)
	}
	if artists[0].Name != "Only B" || artists[1].Name != "Shared Artist" && artists[1].Name != "shared artist" {
		t.Errorf("artist rows = %+v", artists)
	}
	shared := artists[1]
	if shared.Albums != 1 || shared.Tracks != 2 {
		t.Errorf("shared artist counts = %d albums / %d tracks, want 1/2 (merged)", shared.Albums, shared.Tracks)
	}

	// Filter hits only matching artists.
	if got, _ := db.MadnetworkArtists(ctx, "only"); len(got) != 1 || got[0].Name != "Only B" {
		t.Errorf("filtered artists = %+v, want Only B", got)
	}

	// Albums of the merged artist.
	albums, err := db.MadnetworkAlbums(ctx, "SHARED ARTIST")
	if err != nil {
		t.Fatalf("MadnetworkAlbums: %v", err)
	}
	if len(albums) != 1 || albums[0].Tracks != 2 {
		t.Fatalf("albums = %+v, want one with 2 merged tracks", albums)
	}

	// Raw track rows for the handler's merge: 4 rows (2 peers × 2 tracks).
	rows, err := db.MadnetworkTracks(ctx, "Shared Artist", "Shared Album")
	if err != nil {
		t.Fatalf("MadnetworkTracks: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("track rows = %d, want 4", len(rows))
	}
	for _, r := range rows {
		if r.PeerName == "" || len(r.Entry.Renditions) != 1 {
			t.Errorf("row missing peer/renditions: %+v", r)
		}
	}

	// Summary: two friends (the pending peer is absent), merged track count 3.
	friends, tracks, err := db.MadnetworkSummary(ctx)
	if err != nil {
		t.Fatalf("MadnetworkSummary: %v", err)
	}
	if len(friends) != 2 {
		t.Fatalf("summary friends = %+v, want 2", friends)
	}
	if tracks != 3 {
		t.Errorf("merged track count = %d, want 3", tracks)
	}
	for _, f := range friends {
		if f.Entries == 0 {
			t.Errorf("friend %q reports 0 entries", f.Name)
		}
	}

	// Blocking a friend hides its rows everywhere; a re-replace on sync is not
	// needed after unblock (the cache is kept).
	if err := db.SetFederationPeerState(ctx, friendB, federation.PeerBlocked, federation.PeerFriend); err != nil {
		t.Fatal(err)
	}
	artists, _ = db.MadnetworkArtists(ctx, "")
	if len(artists) != 1 {
		t.Errorf("artists after block = %+v, want only friend-a's", artists)
	}
	if _, tracks, _ = db.MadnetworkSummary(ctx); tracks != 2 {
		t.Errorf("track count after block = %d, want 2", tracks)
	}
}

// TestMadnetworkBlobLookup covers the F3 lookups: which friends advertise a
// hash (fetch order + size) and the entry text behind it — friends only, a
// pending or blocked peer's cache never provides.
func TestMadnetworkBlobLookup(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	friendA := insertPeer(t, db, "a1a1", "friend-a", federation.PeerFriend)
	friendB := insertPeer(t, db, "b2b2", "friend-b", federation.PeerFriend)
	pending := insertPeer(t, db, "c3c3", "pending-one", federation.PeerPendingIncoming)

	shared := catEntry("1", "r1", "Artist", "Album", "Shared Song", "hash-shared")
	if err := db.ReplacePeerCatalog(ctx, friendA, "sa", 100, []federation.CatalogEntry{shared}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePeerCatalog(ctx, friendB, "sb", 200, []federation.CatalogEntry{
		catEntry("9", "r9", "Artist", "Album", "Shared Song", "hash-shared"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePeerCatalog(ctx, pending, "sp", 300, []federation.CatalogEntry{
		catEntry("50", "r50", "Ghost", "Ghost Album", "Ghost Song", "hash-ghost"),
	}); err != nil {
		t.Fatal(err)
	}
	// friend-b was seen more recently → first in fetch order.
	if err := db.TouchFederationPeerSeen(ctx, friendB, 9999); err != nil {
		t.Fatal(err)
	}

	size, holders, err := db.MadnetworkBlobProviders(ctx, "hash-shared")
	if err != nil {
		t.Fatalf("MadnetworkBlobProviders: %v", err)
	}
	if size != 1000 {
		t.Errorf("size = %d, want the advertised 1000", size)
	}
	if len(holders) != 2 || holders[0].Name != "friend-b" {
		t.Errorf("holders = %+v, want friend-b first (seen most recently)", holders)
	}

	entry, err := db.MadnetworkEntryForHash(ctx, "hash-shared")
	if err != nil {
		t.Fatalf("MadnetworkEntryForHash: %v", err)
	}
	if entry == nil || entry.Title != "Shared Song" || entry.Artist != "Artist" {
		t.Errorf("entry = %+v, want the advertised tagset text", entry)
	}

	// A non-friend's exclusive hash provides nothing.
	if _, holders, _ := db.MadnetworkBlobProviders(ctx, "hash-ghost"); len(holders) != 0 {
		t.Errorf("pending peer's hash has %d providers, want 0", len(holders))
	}
	if e, _ := db.MadnetworkEntryForHash(ctx, "hash-ghost"); e != nil {
		t.Errorf("pending peer's entry surfaced: %+v", e)
	}
}

// TestMadnetworkPolicy: the autoapprove_downloads setting round-trips and
// defaults to off (downloads go through the review bucket).
func TestMadnetworkPolicy(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if p, err := db.GetMadnetworkPolicy(ctx); err != nil || p.AutoapproveDownloads {
		t.Fatalf("default policy = %+v (err %v), want autoapprove off", p, err)
	}
	if err := db.SetMadnetworkPolicy(ctx, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetMadnetworkPolicy(ctx); !p.AutoapproveDownloads {
		t.Error("policy not persisted")
	}
}
