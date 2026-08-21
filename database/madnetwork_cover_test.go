package database

import (
	"context"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// Covers over the madnetwork, M1/M2 (docs/plans/covers-federation.md): the
// catalog advertises an album's ready cover, the cached foreign copy carries
// it, and the cover's ORIGINAL answers BlobVisibleTo by the album's own scope —
// the same "catalog and bytes read one rule" claim the audience tests pin for
// audio, extended to the second kind of blob a node now serves.

// coverHash is a well-formed stand-in for a cover original's sha256.
var coverHash = strings.Repeat("ab", 32)

// claimScopeCover claims the "Scope Artist — Scope Album" cover the way every
// ingress path does, and returns the album id.
func claimScopeCover(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	albumID, err := db.ResolveAlbumID(ctx, "Scope Artist", "Scope Album")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	inserted, err := db.SetAlbumCoverIfAbsent(ctx, albumID, coverHash, ".jpg",
		coverHash+"/original.jpg", "image/jpeg", 1000)
	if err != nil || !inserted {
		t.Fatalf("claim cover: inserted=%v err=%v", inserted, err)
	}
	return albumID
}

// TestPublishedCatalogAdvertisesReadyCover: an entry carries its album's cover
// only once the variants are ready — before that (and for a legacy row with no
// full image hash) a peer is told nothing, because there is nothing it could
// fetch and decode the way the field promises.
func TestPublishedCatalogAdvertisesReadyCover(t *testing.T) {
	db := openMem(t)
	seedScopeFile(t, db, "cover001", "Tracked")

	entryCover := func(stage string) (string, string) {
		t.Helper()
		entries, err := db.PublishedCatalog(context.Background(), federation.FriendAudience)
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: catalog = %d entries err=%v, want 1", stage, len(entries), err)
		}
		return entries[0].CoverHash, entries[0].CoverExt
	}

	if h, _ := entryCover("no cover yet"); h != "" {
		t.Errorf("catalog advertises %q before any cover exists", h)
	}
	claimScopeCover(t, db)
	if h, e := entryCover("claimed, variants pending"); h != "" || e != "" {
		t.Errorf("catalog advertises (%q,%q) before variants are ready", h, e)
	}
	if _, err := db.Exec(`UPDATE album_images SET variants_ready = 1`); err != nil {
		t.Fatal(err)
	}
	if h, e := entryCover("ready"); h != coverHash || e != ".jpg" {
		t.Errorf("ready cover published as (%q,%q), want (%q,.jpg)", h, e, coverHash)
	}
	// A legacy row (pre-variants: no full-hash key) publishes neither field —
	// an ext without a fetchable hash would be a dangling half-claim.
	if _, err := db.Exec(`UPDATE album_images SET image_hash = ''`); err != nil {
		t.Fatal(err)
	}
	if h, e := entryCover("legacy"); h != "" || e != "" {
		t.Errorf("legacy cover row published as (%q,%q), want nothing", h, e)
	}
}

// TestCachedCatalogRoundTripsCover: what a peer advertises survives the cached
// copy — including through the diffing rewrite when only the cover changed,
// which is exactly the row the digest must not skip.
func TestCachedCatalogRoundTripsCover(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "cc11", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "cc11", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	audioHash := strings.Repeat("cd", 32)
	entry := federation.CatalogEntry{
		Key: "e1", RecordingKey: "r1", Title: "Far Song", Artist: "Far Band",
		AlbumArtist: "Far Band", Album: "Far Album",
		CoverHash: coverHash, CoverExt: ".jpg",
		Renditions: []federation.CatalogRendition{{Hash: audioHash, Size: 9, Codec: "mp3"}},
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}
	got, err := db.MadnetworkEntryForHash(ctx, audioHash)
	if err != nil || got == nil {
		t.Fatalf("MadnetworkEntryForHash: entry=%v err=%v", got, err)
	}
	if got.CoverHash != coverHash || got.CoverExt != ".jpg" {
		t.Errorf("cached entry cover = (%q,%q), want (%q,.jpg)", got.CoverHash, got.CoverExt, coverHash)
	}

	// The origin re-keys its cover and nothing else: the row digest must treat
	// that as a change, or the stale hash would be served until the title moved.
	entry.CoverHash = strings.Repeat("ef", 32)
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s2", 200, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog (cover change): %v", err)
	}
	got, err = db.MadnetworkEntryForHash(ctx, audioHash)
	if err != nil || got == nil {
		t.Fatalf("re-read: entry=%v err=%v", got, err)
	}
	if got.CoverHash != entry.CoverHash {
		t.Errorf("cover change did not reach the cache: got %q, want %q", got.CoverHash, entry.CoverHash)
	}
}

// TestCoverVisibleToFollowsAlbumScope: the cover original serves exactly the
// audiences the album's music serves. One recording in scope is enough; an
// album whose every appearance is private takes its cover with it.
func TestCoverVisibleToFollowsAlbumScope(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	rec := seedScopeFile(t, db, "cover002", "Only Track")
	claimScopeCover(t, db)

	vis, found, err := db.BlobVisibleTo(ctx, coverHash, federation.FriendAudience)
	if err != nil || !found || !vis {
		t.Fatalf("cover for a friend = (vis=%v, found=%v, err=%v), want visible", vis, found, err)
	}
	// variants_ready is deliberately not part of the predicate: the original
	// exists from the claim, and the fetcher derives its own variants.
	if _, err := db.Exec(`UPDATE album_images SET variants_ready = 1`); err != nil {
		t.Fatal(err)
	}

	// The album's one recording goes private; the cover must follow the music.
	if ok, err := db.SetRecordingAccess(ctx, rec, nil, nil,
		ShareDepthUpdate{Set: true, Depth: federation.DepthPrivate}); err != nil || !ok {
		t.Fatalf("set private: ok=%v err=%v", ok, err)
	}
	vis, found, err = db.BlobVisibleTo(ctx, coverHash, federation.FriendAudience)
	if err != nil || !found {
		t.Fatalf("cover after going private: found=%v err=%v", found, err)
	}
	if vis {
		t.Error("a fully-private album still serves its cover — bytes and catalog disagree")
	}

	// A hash nothing owns stays "not found" — the answer that refuses without
	// confirming anything.
	if _, found, err := db.BlobVisibleTo(ctx, strings.Repeat("09", 32), federation.FriendAudience); err != nil || found {
		t.Errorf("unknown hash: found=%v err=%v, want not found", found, err)
	}
}
