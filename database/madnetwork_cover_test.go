package database

import (
	"context"
	"database/sql"
	"fmt"
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

// TestMadnetworkAlbumCoverClaims (covers-federation M4): the claims query hands
// the handler everything a cover election needs — each source's claim keyed to
// the same group identity the album rows carry, plus the self claim under the
// exact ready-only rule the published catalog advertises by.
func TestMadnetworkAlbumCoverClaims(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Two sources claim different covers for one album of "Claim Artist".
	hashA, hashB := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	for i, claim := range []struct{ key, hash string }{{"f1a1", hashA}, {"f2b2", hashB}} {
		if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
			PublicKey: claim.key, Label: claim.key, TrustState: federation.PeerFriend, TrustedAt: 1000,
		}); err != nil {
			t.Fatalf("insert peer: %v", err)
		}
		src, err := db.EnsureCatalogSource(ctx, claim.key, 1000)
		if err != nil {
			t.Fatalf("ensure source: %v", err)
		}
		if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
			Key: "e1", RecordingKey: "r1", Title: "Claimed Song",
			Artist: "Claim Artist", AlbumArtist: "Claim Artist", Album: "Claim Album",
			CoverHash: claim.hash, CoverExt: ".jpg",
			Renditions: []federation.CatalogRendition{{Hash: strings.Repeat("dd", 31) + fmt.Sprintf("%02d", i), Size: 5}},
		}}); err != nil {
			t.Fatalf("ReplaceSourceCatalog: %v", err)
		}
	}

	// The self claim: a local published track in the same album with a READY cover.
	seedClaimFile(t, db, "claim001", "Claim Artist", "Claim Album", "Own Take")
	selfHash := strings.Repeat("cc", 32)
	albumID, err := db.ResolveAlbumID(ctx, "Claim Artist", "Claim Album")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAlbumCoverIfAbsent(ctx, albumID, selfHash, ".png",
		selfHash+"/original.png", "image/png", 1000); err != nil {
		t.Fatal(err)
	}

	view := MadnetworkView{IncludeSelf: true, DefaultShareDepth: federation.DepthUnlimited}
	byHash := func(claims []MadnetworkCoverClaim) map[string]MadnetworkCoverClaim {
		out := map[string]MadnetworkCoverClaim{}
		for _, c := range claims {
			out[c.CoverHash] = c
		}
		return out
	}

	// Before the local variants are ready, only the two remote claims exist.
	claims, err := db.MadnetworkAlbumCoverClaims(ctx, "claim artist", view)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims before ready = %+v, want the two remote ones", claims)
	}
	if _, err := db.Exec(`UPDATE album_images SET variants_ready = 1`); err != nil {
		t.Fatal(err)
	}
	claims, err = db.MadnetworkAlbumCoverClaims(ctx, "claim artist", view)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(claims) != 3 {
		t.Fatalf("claims = %+v, want two remote + one self", claims)
	}
	got := byHash(claims)
	if c := got[hashA]; c.SourceKey != "f1a1" || c.Self {
		t.Errorf("claim A = %+v, want source f1a1", c)
	}
	if c := got[selfHash]; !c.Self || c.SourceKey != "" || c.CoverExt != ".png" {
		t.Errorf("self claim = %+v, want Self with no source key", c)
	}

	// Every claim's AlbumKey pairs with the album row's Key — the join the
	// handler's election stands on.
	albums, err := db.MadnetworkAlbums(ctx, "Claim Artist", view)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %+v err=%v, want the one album", albums, err)
	}
	for _, c := range claims {
		if c.AlbumKey != albums[0].Key {
			t.Errorf("claim key %q does not pair with album key %q", c.AlbumKey, albums[0].Key)
		}
	}
}

// seedClaimFile inserts one live approved file under the given artist/album.
func seedClaimFile(t *testing.T, db *DB, hash, artist, album, title string) {
	t.Helper()
	meta := &MediaMetadata{
		Title:       title,
		Artist:      sql.NullString{String: artist, Valid: true},
		AlbumArtist: sql.NullString{String: artist, Valid: true},
		Album:       sql.NullString{String: album, Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(context.Background(), newFile(hash), newUpload(hash+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile %s: %v", hash, err)
	}
}
