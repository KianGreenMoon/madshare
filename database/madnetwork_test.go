package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

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

	insertPeer(t, db, "f1a1", "friend-a", federation.PeerFriend)
	insertPeer(t, db, "f2b2", "friend-b", federation.PeerFriend)
	blocked := insertPeer(t, db, "f3c3", "blocked-one", federation.PeerFriend)
	friendA := insertSource(t, db, "f1a1")
	friendB := insertSource(t, db, "f2b2")
	hidden := insertSource(t, db, "f3c3")

	// friend-a and friend-b both offer the same track (same text, SAME hash →
	// one version); friend-b also offers a different album, and a second claimed
	// recording of "Crossing" with a different hash (→ two versions).
	if err := db.ReplaceSourceCatalog(ctx, friendA, "serial-a", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Shared Artist", "Shared Album", "Shared Song", "hash-shared"),
		catEntry("2", "r2", "Shared Artist", "Shared Album", "Crossing", "hash-crossing-a"),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog A: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, friendB, "serial-b", 200, []federation.CatalogEntry{
		catEntry("9", "r9", "shared artist", "shared album", "shared song", "hash-shared"), // case-folded dup
		catEntry("10", "r10", "shared artist", "shared album", "Crossing", "hash-crossing-b"),
		catEntry("11", "r11", "Only B", "B Album", "B Song", "hash-b"),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog B: %v", err)
	}
	// A BLOCKED node's cache is kept but never browsed. Since F7 item 5 that is
	// the browse's only trust condition — who may be cached at all is decided by
	// the sweep's retention walk, which SQL cannot do (it is a graph walk).
	if err := db.ReplaceSourceCatalog(ctx, hidden, "serial-p", 300, []federation.CatalogEntry{
		catEntry("50", "r50", "Ghost", "Ghost Album", "Ghost Song", "hash-ghost"),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog blocked: %v", err)
	}
	if err := db.BlockFederationPeer(ctx, blocked, federation.PeerFriend, "", 400); err != nil {
		t.Fatalf("block peer: %v", err)
	}

	// Sync state landed on the source row — not the peer row, which no longer
	// carries it.
	if s, _ := db.GetCatalogSource(ctx, "f1a1"); s.CatalogSerial != "serial-a" || s.CatalogSyncedAt != 100 {
		t.Errorf("source sync state = %q/%d, want serial-a/100", s.CatalogSerial, s.CatalogSyncedAt)
	}
	if err := db.MarkSourceCatalogChecked(ctx, friendA, "serial-a", 150); err != nil {
		t.Fatalf("MarkSourceCatalogChecked: %v", err)
	}
	if s, _ := db.GetCatalogSource(ctx, "f1a1"); s.CatalogSyncedAt != 150 {
		t.Errorf("synced_at after check = %d, want 150", s.CatalogSyncedAt)
	}

	// Artists: case-insensitive merge, no Ghost.
	artists, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 0, "")
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
	if got, _, _ := db.MadnetworkArtists(ctx, "only", MadnetworkView{}, 0, ""); len(got) != 1 || got[0].Name != "Only B" {
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
	rows, err := db.MadnetworkTracks(ctx, "Shared Artist", "Shared Album", MadnetworkView{})
	if err != nil {
		t.Fatalf("MadnetworkTracks: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("track rows = %d, want 4", len(rows))
	}
	for _, r := range rows {
		if r.SourceName == "" || len(r.Entry.Renditions) != 1 {
			t.Errorf("row missing source/renditions: %+v", r)
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
	artists, _, _ = db.MadnetworkArtists(ctx, "", MadnetworkView{}, 0, "")
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

	insertPeer(t, db, "f1a1", "friend-a", federation.PeerFriend)
	friend := insertSource(t, db, "f1a1")
	if err := db.ReplaceSourceCatalog(ctx, friend, "s", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Shared Artist", "Shared Album", "Shared Song", "self0001"),
		catEntry("2", "r2", "Zebra", "Z Album", "Z Song", "hash-z"),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	// Without self: only the friend's two artists.
	if got, _, _ := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 0, ""); len(got) != 2 {
		t.Fatalf("artists without self = %+v, want 2", got)
	}
	// With self: merged, alphabetical, unknown bucket last.
	artists, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{IncludeSelf: true}, 0, "")
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

	// The entry count beside our own name in the node list counts the same thing
	// a friend's does — distinct published tracks, not blobs — and it counts OUR
	// three, never the friend's exclusive.
	if n, err := db.MadnetworkOwnEntries(ctx, MadnetworkView{IncludeSelf: true}); err != nil || n != 3 {
		t.Errorf("own entries = %d (err %v), want our 3 published tracks", n, err)
	}

	// Own track rows: Self, local tagset key, renditions with object keys.
	own, err := db.MadnetworkOwnTracks(ctx, "Shared Artist", "Shared Album", MadnetworkView{IncludeSelf: true})
	if err != nil {
		t.Fatalf("MadnetworkOwnTracks: %v", err)
	}
	if len(own) != 1 || !own[0].Self || own[0].SourceID != 0 {
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

// TestMadnetworkBlobLookup covers the F3 lookups: which nodes advertise a hash
// (fetch order + size) and the entry text behind it. Since F7 item 5 a holder is
// any node whose catalog we cache — a member with no peer row provides exactly
// as a friend does — and a blocked node never provides.
func TestMadnetworkBlobLookup(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertPeer(t, db, "a1a1", "friend-a", federation.PeerFriend)
	friendA := insertSource(t, db, "a1a1")
	friendB := insertSource(t, db, "b2b2") // a member: cached, but no peer row
	blockedPeer := insertPeer(t, db, "c3c3", "blocked-one", federation.PeerFriend)
	blocked := insertSource(t, db, "c3c3")

	shared := catEntry("1", "r1", "Artist", "Album", "Shared Song", "hash-shared")
	if err := db.ReplaceSourceCatalog(ctx, friendA, "sa", 100, []federation.CatalogEntry{shared}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, friendB, "sb", 200, []federation.CatalogEntry{
		catEntry("9", "r9", "Artist", "Album", "Shared Song", "hash-shared"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, blocked, "sp", 300, []federation.CatalogEntry{
		catEntry("50", "r50", "Ghost", "Ghost Album", "Ghost Song", "hash-ghost"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.BlockFederationPeer(ctx, blockedPeer, federation.PeerFriend, "", 400); err != nil {
		t.Fatal(err)
	}
	// friend-b was seen more recently → first in fetch order. It is a member with
	// no peer row, so the only clock it has is its own source row's.
	if err := db.TouchCatalogSourceSeen(ctx, friendB, 9999, "friend-b"); err != nil {
		t.Fatal(err)
	}

	size, holders, err := db.MadnetworkBlobProviders(ctx, "hash-shared")
	if err != nil {
		t.Fatalf("MadnetworkBlobProviders: %v", err)
	}
	if size != 1000 {
		t.Errorf("size = %d, want the advertised 1000", size)
	}
	if len(holders) != 2 || holders[0].Display() != "friend-b" {
		t.Errorf("holders = %+v, want friend-b first (seen most recently)", holders)
	}
	if holders[0].PeerID != 0 {
		t.Errorf("the member holder carries a peer id (%d); it has no peer row", holders[0].PeerID)
	}

	entry, err := db.MadnetworkEntryForHash(ctx, "hash-shared")
	if err != nil {
		t.Fatalf("MadnetworkEntryForHash: %v", err)
	}
	if entry == nil || entry.Title != "Shared Song" || entry.Artist != "Artist" {
		t.Errorf("entry = %+v, want the advertised tagset text", entry)
	}

	// A blocked node's exclusive hash provides nothing, and its cached text never
	// surfaces — the rows are kept so an unblock restores them, not consulted.
	if _, holders, _ := db.MadnetworkBlobProviders(ctx, "hash-ghost"); len(holders) != 0 {
		t.Errorf("blocked node's hash has %d providers, want 0", len(holders))
	}
	if e, _ := db.MadnetworkEntryForHash(ctx, "hash-ghost"); e != nil {
		t.Errorf("blocked node's entry surfaced: %+v", e)
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
	if sp, _ := db.SeedingPolicy(ctx); sp.Enabled || sp.Cache {
		t.Errorf("SeedingPolicy = %+v, want Enabled and Cache false", sp)
	}
}

// TestMadnetworkAvailability covers the request-time reachability filter
// (docs/architecture/federation.md §Availability & node health): a friend seen
// before the cutoff drops out of the merged browse, while a fresh friend stays;
// cutoff 0 (fail open / filtering off) shows both, matching pre-availability.
func TestMadnetworkAvailability(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	freshPeer := insertPeer(t, db, "aaaa", "fresh", federation.PeerFriend)
	stalePeer := insertPeer(t, db, "bbbb", "stale", federation.PeerFriend)
	fresh := insertSource(t, db, "aaaa")
	stale := insertSource(t, db, "bbbb")
	// Freshness is the later of the two clocks — the friendship ping and the
	// catalog pull — so a friend pinged recently stays visible even though its
	// catalog was synced long ago, which is what these ping times assert.
	if err := db.TouchFederationPeerSeen(ctx, freshPeer, 10000); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchFederationPeerSeen(ctx, stalePeer, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, fresh, "sf", 1, []federation.CatalogEntry{
		catEntry("1", "r1", "Fresh Artist", "Fresh Album", "Fresh Song", "hash-fresh"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, stale, "ss", 1, []federation.CatalogEntry{
		catEntry("2", "r2", "Stale Artist", "Stale Album", "Stale Song", "hash-stale"),
	}); err != nil {
		t.Fatal(err)
	}

	const cutoff = 5000 // fresh (10000) reachable, stale (100) not

	// Filtered: only the fresh friend's artist shows.
	got, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{Cutoff: cutoff}, 0, "")
	if err != nil {
		t.Fatalf("MadnetworkArtists: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Fresh Artist" {
		t.Fatalf("filtered artists = %+v, want only Fresh Artist", got)
	}
	// The stale friend's album has no reachable holder → no track rows.
	if rows, _ := db.MadnetworkTracks(ctx, "Stale Artist", "Stale Album", MadnetworkView{Cutoff: cutoff}); len(rows) != 0 {
		t.Errorf("stale album track rows = %d, want 0", len(rows))
	}
	if rows, _ := db.MadnetworkTracks(ctx, "Fresh Artist", "Fresh Album", MadnetworkView{Cutoff: cutoff}); len(rows) != 1 {
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
	if got, _, _ := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 0, ""); len(got) != 2 {
		t.Errorf("unfiltered artists = %d, want 2", len(got))
	}
	if _, tracks, _ := db.MadnetworkSummary(ctx, MadnetworkView{}); tracks != 2 {
		t.Errorf("unfiltered track count = %d, want 2", tracks)
	}
}

// TestMadnetworkMemberFreshnessWindow is the regression for the bug measured
// 2026-08-01 (F7 item 10, §Availability, "Two clocks, two windows"): a MEMBER —
// a source with no peer row, reached only by the catalog rotation — was judged
// against the window sized for the one-minute friendship ping, so its tracks were
// visible for about three minutes in every fifteen.
//
// The window now follows the observer. A member nothing pings for us is judged by
// the pull window; one a friend vouches for is judged by the tight window and must
// therefore DISAPPEAR when the vouching stops and it goes quiet — which is the
// half a single wide window would get wrong in the other direction.
func TestMadnetworkMemberFreshnessWindow(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// now-relative, because reachability is judged against wall-clock cutoffs.
	now := time.Now().Unix()
	const tightWindow, pullWindow = 180, 2700
	view := MadnetworkView{
		Cutoff: now - tightWindow, PullCutoff: now - pullWindow,
		PingedSince: now - tightWindow,
	}

	// Four sources, none of them a friend: no peer rows at all.
	pulled := insertSource(t, db, "cccc")   // last pulled 10 min ago, never hinted
	vouched := insertSource(t, db, "dddd")  // hinted 1 min ago, seen 1 min ago
	departed := insertSource(t, db, "eeee") // hinted 1 min ago, but seen 10 min ago
	orphaned := insertSource(t, db, "ffff") // hinted 10 min ago, pulled 10 min ago
	for _, s := range []struct {
		id            int64
		seen, hinted  int64
		artist, album string
		entryKey      string
	}{
		{pulled, now - 600, 0, "Pulled Artist", "Pulled Album", "1"},
		{vouched, now - 60, now - 60, "Vouched Artist", "Vouched Album", "2"},
		// Its friend still reports it, carrying a frozen observation: the member
		// died, and the tight window it earned is what notices.
		{departed, now - 600, now - 60, "Departed Artist", "Departed Album", "3"},
		// The VOUCHER went quiet instead — no hint for ten minutes. The member
		// itself is fine and our own rotation still reaches it, so it must fall
		// back to the pull clock rather than be held to a window nothing feeds.
		{orphaned, now - 600, now - 600, "Orphaned Artist", "Orphaned Album", "4"},
	} {
		if err := db.TouchCatalogSourceSeen(ctx, s.id, s.seen, ""); err != nil {
			t.Fatal(err)
		}
		if s.hinted > 0 {
			if _, err := db.ApplyFreshnessHints(ctx,
				map[string]int64{sourceKey(t, db, s.id): s.seen}, s.hinted); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.ReplaceSourceCatalog(ctx, s.id, "s"+s.entryKey, 1, []federation.CatalogEntry{
			catEntry(s.entryKey, "r"+s.entryKey, s.artist, s.album, s.artist+" Song", "hash-"+s.entryKey),
		}); err != nil {
			t.Fatal(err)
		}
	}

	artists, _, err := db.MadnetworkArtists(ctx, "", view, 0, "")
	if err != nil {
		t.Fatalf("MadnetworkArtists: %v", err)
	}
	shown := map[string]bool{}
	for _, a := range artists {
		shown[a.Name] = true
	}
	if !shown["Pulled Artist"] {
		t.Error("a member last pulled 10 minutes ago was hidden — it is judged by the " +
			"pull window, not by the ping window (this is the 2026-08-01 bug)")
	}
	if !shown["Vouched Artist"] {
		t.Error("a member a friend vouched for a minute ago was hidden")
	}
	if shown["Departed Artist"] {
		t.Error("a member still being reported every minute, but last SEEN ten minutes " +
			"ago, was still shown — a hinted source must be judged by the tight window " +
			"it earned, or a dead node lingers for 45 minutes")
	}
	if !shown["Orphaned Artist"] {
		t.Error("a healthy member was hidden because the friend that vouched for it went " +
			"quiet — the class must ask who is watching NOW, so this row falls back to " +
			"the pull clock our own rotation still refreshes")
	}

	// The summary strip greys by exactly the same rule it filters by.
	friends, tracks, err := db.MadnetworkSummary(ctx, view)
	if err != nil {
		t.Fatalf("MadnetworkSummary: %v", err)
	}
	if tracks != 3 {
		t.Errorf("reachable track count = %d, want 3", tracks)
	}
	reach := map[int64]bool{}
	for _, f := range friends {
		reach[f.ID] = f.Reachable
	}
	if !reach[pulled] || !reach[vouched] || !reach[orphaned] || reach[departed] {
		t.Errorf("strip reachability = %+v; want everything but the departed member reachable", reach)
	}
}

// sourceKey reads a source's node key back, so a test can drive the key-addressed
// hint API from the id the insert helper returns.
func sourceKey(t *testing.T, db *DB, id int64) string {
	t.Helper()
	var key string
	if err := db.QueryRow(`SELECT public_key FROM federation_catalog_sources WHERE id = ?`, id).Scan(&key); err != nil {
		t.Fatalf("read source key: %v", err)
	}
	return key
}

// TestCatalogSources covers the store side of F7 item 5: a source is created
// once per node key, the rotation order is by last attempt, and dropping one
// takes everything cached from it with it.
func TestCatalogSources(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	first, err := db.EnsureCatalogSource(ctx, "AA11", 500)
	if err != nil {
		t.Fatalf("EnsureCatalogSource: %v", err)
	}
	if first.PublicKey != "aa11" {
		t.Errorf("key = %q, want it lower-cased", first.PublicKey)
	}
	// The key is the identity: asking twice must not cache a node twice.
	again, err := db.EnsureCatalogSource(ctx, "aa11", 900)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.FirstSeen != 500 {
		t.Errorf("second ensure = id %d/first_seen %d, want the original %d/500", again.ID, again.FirstSeen, first.ID)
	}

	second, err := db.EnsureCatalogSource(ctx, "bb22", 500)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCatalogSourceAttempted(ctx, first.ID, 1000); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListCatalogSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID {
		t.Errorf("rotation order = %+v, want the never-attempted source first", list)
	}

	// last_seen is monotonic — an out-of-order write from a concurrent transfer
	// must not age a node — and an empty name leaves the stored claim alone.
	if err := db.TouchCatalogSourceSeen(ctx, first.ID, 2000, "calls itself this"); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchCatalogSourceSeen(ctx, first.ID, 100, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetCatalogSource(ctx, "aa11")
	if got.LastSeen != 2000 || got.HeardName != "calls itself this" {
		t.Errorf("source after touches = %d/%q, want 2000/\"calls itself this\"", got.LastSeen, got.HeardName)
	}

	// Dropping a source drops its cache (CASCADE) — the whole reason retention
	// can be as blunt as it is.
	if err := db.ReplaceSourceCatalog(ctx, first.ID, "s", 1, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Song", "hash-x"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceHoldings(ctx, first.ID, []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DropCatalogSources(ctx, nil); err != nil {
		t.Errorf("dropping nothing should be a no-op, got %v", err)
	}
	if list, _ := db.ListCatalogSources(ctx); len(list) != 2 {
		t.Fatal("an empty drop list removed something")
	}
	if err := db.DropCatalogSources(ctx, []int64{first.ID}); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.GetCatalogSource(ctx, "aa11"); s != nil {
		t.Error("the dropped source is still there")
	}
	var rows int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM federation_catalog) + (SELECT COUNT(*) FROM federation_holdings)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d cached rows survived their source", rows)
	}
}

// TestSourceLabelPrefersAName pins the display chain a live 5-node lab caught
// getting backwards: a *friend's* self-claimed name is refreshed by the
// friendship ping onto the peer row, a *member's* by the discovery ping onto the
// source row, and reading only the second made friends — the nodes an admin
// cares most about — render as bare key prefixes while strangers rendered names.
func TestSourceLabelPrefersAName(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// A friend that named itself but that the admin never relabelled.
	friendPeer := insertPeer(t, db, "aaaa", "", federation.PeerFriend)
	if err := db.UpdateFederationPeerHeardName(ctx, friendPeer, "calls-itself-a"); err != nil {
		t.Fatal(err)
	}
	friend := insertSource(t, db, "aaaa")
	// A member with no peer row at all: its claim lands on the source.
	member := insertSource(t, db, "bbbb")
	if err := db.TouchCatalogSourceSeen(ctx, member, 100, "calls-itself-b"); err != nil {
		t.Fatal(err)
	}
	// And one that has never said anything: the short key is the last resort.
	silent := insertSource(t, db, "cccc")

	for _, tc := range []struct {
		src        int64
		album, key string
		want       string
	}{
		{friend, "A Album", "hash-a", "calls-itself-a"},
		{member, "B Album", "hash-b", "calls-itself-b"},
		{silent, "C Album", "hash-c", "cccc"},
	} {
		if err := db.ReplaceSourceCatalog(ctx, tc.src, "s", 1, []federation.CatalogEntry{
			catEntry("1", "r1", "Artist", tc.album, "Song", tc.key),
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := db.MadnetworkTracks(ctx, "Artist", tc.album, MadnetworkView{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("tracks for %s = %d rows (err %v)", tc.album, len(rows), err)
		}
		if rows[0].SourceName != tc.want {
			t.Errorf("source label = %q, want %q", rows[0].SourceName, tc.want)
		}
	}

	// An admin's own label always wins over anything a node says about itself.
	if err := db.UpdateFederationPeerName(ctx, friendPeer, "my label"); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.MadnetworkTracks(ctx, "Artist", "A Album", MadnetworkView{})
	if len(rows) != 1 || rows[0].SourceName != "my label" {
		t.Errorf("after rename label = %+v, want \"my label\"", rows)
	}
}

// TestMadnetworkSourceByKey: a node is addressed by its public key, because the
// source ID is a local row number the discovery rotation recycles. Blocked nodes
// are not addressable, on the same terms as every browse query — blocking is
// decided by the query, since it has to be instant.
func TestMadnetworkSourceByKey(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertPeer(t, db, "aaaa", "vinylcellar", federation.PeerFriend)
	friend := insertSource(t, db, "aaaa")
	if err := db.ReplaceSourceCatalog(ctx, friend, "s", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Song", "hash-a"),
		catEntry("2", "r2", "Artist", "Album", "Other", "hash-b"),
	}); err != nil {
		t.Fatal(err)
	}
	blockedPeer := insertPeer(t, db, "bbbb", "shunned", federation.PeerFriend)
	blocked := insertSource(t, db, "bbbb")
	if err := db.ReplaceSourceCatalog(ctx, blocked, "s", 100, []federation.CatalogEntry{
		catEntry("3", "r3", "Artist", "Album", "Ghost", "hash-c"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.BlockFederationPeer(ctx, blockedPeer, federation.PeerFriend, "", 200); err != nil {
		t.Fatal(err)
	}

	node, found, err := db.MadnetworkSourceByKey(ctx, "aaaa", MadnetworkView{})
	if err != nil || !found {
		t.Fatalf("by key = %v, found %v, err %v", node, found, err)
	}
	if node.ID != friend || node.Key != "aaaa" || node.Name != "vinylcellar" || !node.Friend {
		t.Errorf("node = %+v, want the friend's source row", node)
	}
	if node.Entries != 2 {
		t.Errorf("entries = %d, want the 2 cached entries", node.Entries)
	}
	if _, found, _ := db.MadnetworkSourceByKey(ctx, "bbbb", MadnetworkView{}); found {
		t.Error("a blocked node is addressable")
	}
	if _, found, _ := db.MadnetworkSourceByKey(ctx, "ffff", MadnetworkView{}); found {
		t.Error("an unknown key resolved to something")
	}

	// The summary carries the key, since that is what the node surfaces address
	// a node by.
	nodes, _, err := db.MadnetworkSummary(ctx, MadnetworkView{})
	if err != nil || len(nodes) != 1 || nodes[0].Key != "aaaa" {
		t.Errorf("summary = %+v (err %v), want the one unblocked source, keyed", nodes, err)
	}
}

// TestMadnetworkNoSourceShelf: asking for the shelf of a node we hold nothing
// from answers EMPTY, never the merged catalog. A key is an explicit request for
// one node, so widening it would file other nodes' content under that node's
// name.
func TestMadnetworkNoSourceShelf(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertPeer(t, db, "aaaa", "friend-a", federation.PeerFriend)
	friend := insertSource(t, db, "aaaa")
	if err := db.ReplaceSourceCatalog(ctx, friend, "s", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Song", "hash-a"),
	}); err != nil {
		t.Fatal(err)
	}

	merged := MadnetworkView{IncludeSelf: true}
	if artists, _, err := db.MadnetworkArtists(ctx, "", merged, 10, ""); err != nil || len(artists) != 1 {
		t.Fatalf("merged view = %+v (err %v), want the friend's artist", artists, err)
	}
	empty := MadnetworkView{IncludeSelf: true, SourceID: NoSourceID}
	artists, _, err := db.MadnetworkArtists(ctx, "", empty, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 0 {
		t.Errorf("unheld node's shelf = %+v, want nothing", artists)
	}
	rows, err := db.MadnetworkTracks(ctx, "Artist", "Album", empty)
	if err != nil || len(rows) != 0 {
		t.Errorf("unheld node's tracks = %d rows (err %v), want none", len(rows), err)
	}
	if _, tracks, err := db.MadnetworkSummary(ctx, empty); err != nil || tracks != 0 {
		t.Errorf("unheld node's track count = %d (err %v), want 0", tracks, err)
	}
}
