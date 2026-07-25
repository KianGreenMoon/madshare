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

	entries, err := db.PublishedCatalog(ctx, federation.FriendAudience)
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
	again, _ := db.PublishedCatalog(ctx, federation.FriendAudience)
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
	entries, _ = db.PublishedCatalog(ctx, federation.FriendAudience)
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
	artists, err := db.MadnetworkArtists(ctx, "", MadnetworkView{})
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
	if got, _ := db.MadnetworkArtists(ctx, "only", MadnetworkView{}); len(got) != 1 || got[0].Name != "Only B" {
		t.Errorf("filtered artists = %+v, want Only B", got)
	}

	// Albums of the merged artist.
	albums, err := db.MadnetworkAlbums(ctx, "SHARED ARTIST", MadnetworkView{})
	if err != nil {
		t.Fatalf("MadnetworkAlbums: %v", err)
	}
	if len(albums) != 1 || albums[0].Tracks != 2 {
		t.Fatalf("albums = %+v, want one with 2 merged tracks", albums)
	}

	// Raw track rows for the handler's merge: 4 rows (2 peers × 2 tracks).
	rows, err := db.MadnetworkTracks(ctx, "Shared Artist", "Shared Album", 0)
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
	friends, tracks, err := db.MadnetworkSummary(ctx, MadnetworkView{})
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
	artists, _ = db.MadnetworkArtists(ctx, "", MadnetworkView{})
	if len(artists) != 1 {
		t.Errorf("artists after block = %+v, want only friend-a's", artists)
	}
	if _, tracks, _ = db.MadnetworkSummary(ctx, MadnetworkView{}); tracks != 2 {
		t.Errorf("track count after block = %d, want 2", tracks)
	}
}

// TestMadnetworkSelfMergeAndSorting: includeSelf folds the own published set
// into the merged browse (same track text = one row, counts stay distinct),
// the unknown buckets sort last, and the search queries cover both sources
// (docs/ui/madnetwork-page.md §Own tracks / §Sorting / §Search).
func TestMadnetworkSelfMergeAndSorting(t *testing.T) {
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
	// Own library: one artist shared with the friend (same track text + hash),
	// one own-only artist, and one untagged file (the unknown buckets).
	seed("self0001", "Shared Artist", "Shared Album", "Shared Song")
	seed("self0002", "Aardvark", "First Album", "Own Song")
	seed("self0003", "", "", "Bucket Song")
	// The merged identity includes the track number (catEntry advertises 1) —
	// align the own appearance so the shared song folds across sources.
	if _, err := db.Exec(`UPDATE tagsets SET track_number = 1 WHERE title = 'Shared Song'`); err != nil {
		t.Fatal(err)
	}

	friend := insertPeer(t, db, "f1a1", "friend-a", federation.PeerFriend)
	if err := db.ReplacePeerCatalog(ctx, friend, "s", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Shared Artist", "Shared Album", "Shared Song", "self0001"),
		catEntry("2", "r2", "Zebra", "Z Album", "Z Song", "hash-z"),
	}); err != nil {
		t.Fatalf("ReplacePeerCatalog: %v", err)
	}

	// Without self: only the friend's two artists.
	if got, _ := db.MadnetworkArtists(ctx, "", MadnetworkView{}); len(got) != 2 {
		t.Fatalf("artists without self = %+v, want 2", got)
	}
	// With self: merged, alphabetical, unknown bucket last.
	artists, err := db.MadnetworkArtists(ctx, "", MadnetworkView{IncludeSelf: true})
	if err != nil {
		t.Fatalf("MadnetworkArtists(self): %v", err)
	}
	names := make([]string, len(artists))
	for i, a := range artists {
		names[i] = a.Name
	}
	want := []string{"Aardvark", "Shared Artist", "Zebra", DefaultArtistName}
	if len(names) != 4 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] || names[3] != want[3] {
		t.Fatalf("artists with self = %v, want %v (unknown last)", names, want)
	}
	// Same track offered by us and the friend stays ONE distinct track.
	if artists[1].Tracks != 1 || artists[1].Albums != 1 {
		t.Errorf("shared artist counts = %+v, want 1 album / 1 track", artists[1])
	}

	// Albums with self; the untagged bucket album sorts last for its artist.
	if albums, _ := db.MadnetworkAlbums(ctx, "Shared Artist", MadnetworkView{IncludeSelf: true}); len(albums) != 1 || albums[0].Tracks != 1 {
		t.Errorf("shared albums = %+v, want one album with 1 merged track", albums)
	}
	if albums, _ := db.MadnetworkAlbums(ctx, DefaultArtistName, MadnetworkView{IncludeSelf: true}); len(albums) != 1 || albums[0].Title != DefaultAlbumTitle {
		t.Errorf("bucket albums = %+v, want the %q bucket", albums, DefaultAlbumTitle)
	}

	// Own track rows: Self, local tagset key, renditions with object keys.
	own, err := db.MadnetworkOwnTracks(ctx, "Shared Artist", "Shared Album", MadnetworkView{IncludeSelf: true})
	if err != nil {
		t.Fatalf("MadnetworkOwnTracks: %v", err)
	}
	if len(own) != 1 || !own[0].Self || own[0].PeerID != 0 {
		t.Fatalf("own rows = %+v, want one Self row", own)
	}
	if own[0].Entry.Key == "" || len(own[0].Entry.Renditions) != 1 {
		t.Errorf("own row entry = %+v, want tagset key + 1 rendition", own[0].Entry)
	}
	if key := own[0].ObjectKeys["self0001"]; key == "" {
		t.Errorf("own row object keys = %+v, want the local files key", own[0].ObjectKeys)
	}

	// Summary counts own tracks only when included.
	if _, tracks, _ := db.MadnetworkSummary(ctx, MadnetworkView{}); tracks != 2 {
		t.Errorf("summary tracks without self = %d, want 2", tracks)
	}
	if _, tracks, _ := db.MadnetworkSummary(ctx, MadnetworkView{IncludeSelf: true}); tracks != 4 {
		t.Errorf("summary tracks with self = %d, want 4 (shared song folds)", tracks)
	}

	// Search: albums by title substring (the untagged "Other" bucket doesn't
	// match "album"; the shared album folds across sources), raw track rows
	// from both sources.
	if hits, _ := db.MadnetworkSearchAlbums(ctx, "album", 10, MadnetworkView{IncludeSelf: true}); len(hits) != 3 {
		t.Errorf("search albums = %+v, want 3 buckets", hits)
	}
	rows, err := db.MadnetworkSearchTrackRows(ctx, "song", MadnetworkView{IncludeSelf: true})
	if err != nil {
		t.Fatalf("MadnetworkSearchTrackRows: %v", err)
	}
	selfRows, remoteRows := 0, 0
	for _, r := range rows {
		if r.Self {
			selfRows++
		} else {
			remoteRows++
		}
		if r.GroupArtist == "" || r.GroupAlbum == "" {
			t.Errorf("search row missing group identity: %+v", r)
		}
	}
	if remoteRows != 2 || selfRows != 3 {
		t.Errorf("search rows = %d remote / %d self, want 2/3", remoteRows, selfRows)
	}
	// Self excluded → own rows disappear from search too.
	if rows, _ := db.MadnetworkSearchTrackRows(ctx, "song", MadnetworkView{}); len(rows) != 2 {
		t.Errorf("search rows without self = %d, want 2", len(rows))
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
	// Defaults: autoapprove off, but seeding + cache-seeding ON.
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil || p.AutoapproveDownloads || !p.SeedEnabled || !p.SeedCache {
		t.Fatalf("default policy = %+v (err %v), want autoapprove off + seeding on", p, err)
	}
	if err := db.SetMadnetworkPolicy(ctx, MadnetworkPolicy{
		AutoapproveDownloads: true, SeedEnabled: false, SeedCache: false,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMadnetworkPolicy(ctx)
	if !got.AutoapproveDownloads || got.SeedEnabled || got.SeedCache {
		t.Errorf("policy not persisted: %+v", got)
	}
	// SeedingPolicy reflects the same stored flags.
	if en, ca, _ := db.SeedingPolicy(ctx); en || ca {
		t.Errorf("SeedingPolicy = (%v,%v), want (false,false)", en, ca)
	}
}

// TestMadnetworkAvailability covers the request-time reachability filter
// (docs/architecture/federation.md §Availability & node health): a friend seen
// before the cutoff drops out of the merged browse, while a fresh friend stays;
// cutoff 0 (fail open / filtering off) shows both, matching pre-availability.
func TestMadnetworkAvailability(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	fresh := insertPeer(t, db, "aaaa", "fresh", federation.PeerFriend)
	stale := insertPeer(t, db, "bbbb", "stale", federation.PeerFriend)
	if err := db.TouchFederationPeerSeen(ctx, fresh, 10000); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchFederationPeerSeen(ctx, stale, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePeerCatalog(ctx, fresh, "sf", 1, []federation.CatalogEntry{
		catEntry("1", "r1", "Fresh Artist", "Fresh Album", "Fresh Song", "hash-fresh"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplacePeerCatalog(ctx, stale, "ss", 1, []federation.CatalogEntry{
		catEntry("2", "r2", "Stale Artist", "Stale Album", "Stale Song", "hash-stale"),
	}); err != nil {
		t.Fatal(err)
	}

	const cutoff = 5000 // fresh (10000) reachable, stale (100) not

	// Filtered: only the fresh friend's artist shows.
	got, err := db.MadnetworkArtists(ctx, "", MadnetworkView{Cutoff: cutoff})
	if err != nil {
		t.Fatalf("MadnetworkArtists: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Fresh Artist" {
		t.Fatalf("filtered artists = %+v, want only Fresh Artist", got)
	}
	// The stale friend's album has no reachable holder → no track rows.
	if rows, _ := db.MadnetworkTracks(ctx, "Stale Artist", "Stale Album", cutoff); len(rows) != 0 {
		t.Errorf("stale album track rows = %d, want 0", len(rows))
	}
	if rows, _ := db.MadnetworkTracks(ctx, "Fresh Artist", "Fresh Album", cutoff); len(rows) != 1 {
		t.Errorf("fresh album track rows = %d, want 1", len(rows))
	}
	// Search is filtered too.
	if rows, _ := db.MadnetworkSearchTrackRows(ctx, "Song", MadnetworkView{Cutoff: cutoff}); len(rows) != 1 {
		t.Errorf("filtered search rows = %d, want 1 (fresh only)", len(rows))
	}

	// Summary lists BOTH friends (the strip greys, not hides) but marks
	// reachability, and the track count reflects only the reachable set.
	friends, tracks, err := db.MadnetworkSummary(ctx, MadnetworkView{Cutoff: cutoff})
	if err != nil {
		t.Fatalf("MadnetworkSummary: %v", err)
	}
	if len(friends) != 2 {
		t.Fatalf("summary friends = %d, want 2 (both listed)", len(friends))
	}
	reach := map[string]bool{}
	for _, f := range friends {
		reach[f.Name] = f.Reachable
	}
	if !reach["fresh"] || reach["stale"] {
		t.Errorf("reachability = %+v, want fresh=true stale=false", reach)
	}
	if tracks != 1 {
		t.Errorf("reachable track count = %d, want 1", tracks)
	}

	// Cutoff 0 = fail open: both friends' tracks are visible again.
	if got, _ := db.MadnetworkArtists(ctx, "", MadnetworkView{}); len(got) != 2 {
		t.Errorf("unfiltered artists = %d, want 2", len(got))
	}
	if _, tracks, _ := db.MadnetworkSummary(ctx, MadnetworkView{}); tracks != 2 {
		t.Errorf("unfiltered track count = %d, want 2", tracks)
	}
}
