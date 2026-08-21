package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// Covers over the madnetwork, M3 (docs/plans/covers-federation.md), through the
// caller that actually meets it: POST /api/madnetwork/download for bytes the
// library already holds, whose remote entry claims a cover. The claim must be
// redeemed — original fetched by hash, album cover claimed, variant job queued
// — without the download's own answer waiting for any of it.

// pngBytes encodes a 1×1 PNG — a real decodable image, since the attach path
// sniffs the header before trusting the claim.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadAttachesRemoteCover(t *testing.T) {
	cover := pngBytes(t)
	sum := sha256.Sum256(cover)
	coverHash := hex.EncodeToString(sum[:])
	coverTr := newFakeTransfer(t, coverHash, "original.png", int64(len(cover)))
	coverTr.append(t, cover)
	coverTr.finish()

	fed := &fakeBlobFederation{blob: coverTr}
	srv, db := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	audioHash, _ := uploadStaged(t, up, srv.URL, "held.mp3")

	ctx := context.Background()
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "aa22", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa22", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Held", Artist: "Band",
		AlbumArtist: "Band", Album: "Album X",
		CoverHash: coverHash, CoverExt: ".png",
		Renditions: []federation.CatalogRendition{{Hash: audioHash, Size: 1000, Codec: "mp3"}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	var body struct {
		Existed bool `json:"existed"`
	}
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/madnetwork/download",
		map[string]any{"hash": audioHash}, &body); code != http.StatusOK {
		t.Fatalf("download = %d, want 200 (bytes already held)", code)
	}
	if !body.Existed {
		t.Fatal("handler did not take the already-held path")
	}

	// The attach runs behind the answered request; wait for the claim to land.
	albumID := int64(0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id, found, err := db.LookupAlbumID(ctx, "Band", "Album X"); err == nil && found {
			if has, err := db.HasAlbumCover(ctx, id); err == nil && has {
				albumID = id
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the remote cover to attach")
		}
		time.Sleep(20 * time.Millisecond)
	}

	imageHash, sourceExt, ready, found, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || !found {
		t.Fatalf("cover status: found=%v err=%v", found, err)
	}
	if imageHash != coverHash || sourceExt != ".png" {
		t.Errorf("cover row = (%q,%q), want (%q,.png)", imageHash, sourceExt, coverHash)
	}
	if ready {
		t.Error("variants_ready is set before any worker ran")
	}

	// The cover row exists only if the original's disk write succeeded (the
	// claim runs strictly after it), so the row above already proves the seed
	// file landed. What remains is the derive job — the same downstream every
	// other cover ingress feeds.
	var jobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_processing_jobs WHERE image_hash = ?`,
		coverHash).Scan(&jobs); err != nil || jobs != 1 {
		t.Errorf("image jobs for the cover = %d err=%v, want 1", jobs, err)
	}
}

// ── M4: the election and the relay ───────────────────────────────────────────

// TestCoverBallotVoicesRule: the election counts voices, not keys — a farm of
// holders behind one friendship is one voice, so a cover corroborated by two
// independent voices beats one echoed by any number of sybils.
func TestCoverBallotVoicesRule(t *testing.T) {
	branches := database.BranchMap{
		"sybil-1": {"branch-x"},
		"sybil-2": {"branch-x"},
	}
	var b coverBallot
	b.add("aaaa", ".jpg", "sybil-1", false)
	b.add("aaaa", ".jpg", "sybil-2", false) // two keys, one branch: one voice
	b.add("bbbb", ".png", "stranger", false)
	b.add("bbbb", ".png", "", true) // unplaceable + self: two voices
	if hash, ext := b.winner(branches); hash != "bbbb" || ext != ".png" {
		t.Errorf("winner = (%q,%q), want the two-voice bbbb", hash, ext)
	}

	// A perfect tie falls through holders and self to the lexically smallest
	// hash, so two requests always paint the same art.
	var tie coverBallot
	tie.add("cccc", ".jpg", "n1", false)
	tie.add("dddd", ".jpg", "n2", false)
	if hash, _ := tie.winner(nil); hash != "cccc" {
		t.Errorf("tie winner = %q, want the deterministic cccc", hash)
	}

	// No claims at all elects nothing.
	var empty coverBallot
	if hash, ext := empty.winner(nil); hash != "" || ext != "" {
		t.Errorf("empty ballot elected (%q,%q)", hash, ext)
	}
}

// TestMergeTracksElectCover: the merged track row carries the elected cover —
// the majority claim among its contributing sources.
func TestMergeTracksElectCover(t *testing.T) {
	row := func(source, cover string) *database.MadnetworkTrackRow {
		r := &database.MadnetworkTrackRow{SourceKey: source}
		r.Entry.Title = "One Song"
		r.Entry.CoverHash, r.Entry.CoverExt = cover, ".jpg"
		r.Entry.Renditions = []federation.CatalogRendition{{Hash: strings.Repeat("ee", 32), Size: 5}}
		return r
	}
	tracks := mergeMadnetworkTracks([]*database.MadnetworkTrackRow{
		row("n1", "aaaa"), row("n2", "bbbb"), row("n3", "bbbb"),
	}, mergeOpts{})
	if len(tracks) != 1 {
		t.Fatalf("merged = %d tracks, want 1", len(tracks))
	}
	if tracks[0].CoverHash != "bbbb" || tracks[0].CoverExt != ".jpg" {
		t.Errorf("track cover = (%q,%q), want the majority bbbb",
			tracks[0].CoverHash, tracks[0].CoverExt)
	}
}

// TestMadnetworkCoverRelay: the relay serves a fetched cover with the type its
// BYTES carry, immutable cache headers, and a 404 for a hash nobody holds.
func TestMadnetworkCoverRelay(t *testing.T) {
	cover := pngBytes(t)
	sum := sha256.Sum256(cover)
	hash := hex.EncodeToString(sum[:])
	tr := newFakeTransfer(t, hash, "original.png", int64(len(cover)))
	tr.append(t, cover)
	tr.finish()

	srv := streamServer(t, tr)
	resp, err := http.Get(srv.URL + "/api/madnetwork/cover/" + hash)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cover relay = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (sniffed from bytes)", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want an immutable hash-addressed answer", cc)
	}
	if !bytes.Equal(body, cover) {
		t.Errorf("relayed %d bytes, want the cover verbatim", len(body))
	}

	// Nobody holds it: 404, indistinguishable from any other unknown hash.
	none := streamServer(t, nil)
	resp, err = http.Get(none.URL + "/api/madnetwork/cover/" + strings.Repeat("12", 32))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown cover = %d, want 404", resp.StatusCode)
	}
}

// TestSearchAlbumHitsCarryTheElectedCover: the search page's album hits get
// the same election as the album list — through the real endpoint, so the JSON
// field the webui reads is what is pinned.
func TestSearchAlbumHitsCarryTheElectedCover(t *testing.T) {
	fed := &fakeFederation{}
	srv, db := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	ctx := context.Background()
	coverHash := strings.Repeat("fa", 32)
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "aa33", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa33", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	// Fresh, or the availability window hides the source from the browse view.
	if err := db.TouchCatalogSourceSeen(ctx, src.ID, time.Now().Unix(), ""); err != nil {
		t.Fatalf("touch source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Findable Song", Artist: "Search Band",
		AlbumArtist: "Search Band", Album: "Findable Album",
		CoverHash: coverHash, CoverExt: ".jpg",
		Renditions: []federation.CatalogRendition{{Hash: strings.Repeat("dc", 32), Size: 5}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	resp, err := admin.Get(srv.URL + "/api/madnetwork/search?q=Findable")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Albums []struct {
			Title     string `json:"title"`
			CoverHash string `json:"cover_hash"`
		} `json:"albums"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Albums) != 1 {
		t.Fatalf("album hits = %+v, want the one album", body.Albums)
	}
	if body.Albums[0].CoverHash != coverHash {
		t.Errorf("hit cover = %q, want the claimant's %q", body.Albums[0].CoverHash, coverHash)
	}
}

// TestMadnetworkCoverRelaySized: ?size= serves a derived crop — sized like the
// local album endpoint, derived once, and answered from the variant tree ever
// after (the second request runs with the federation's blob taken away, so a
// re-fetch would 404).
func TestMadnetworkCoverRelaySized(t *testing.T) {
	// 200×100, so the 64px thumb crop is a real resize, not a passthrough.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 200, 100))); err != nil {
		t.Fatal(err)
	}
	cover := buf.Bytes()
	sum := sha256.Sum256(cover)
	hash := hex.EncodeToString(sum[:])
	tr := newFakeTransfer(t, hash, "original.png", int64(len(cover)))
	tr.append(t, cover)
	tr.finish()

	fed := &fakeBlobFederation{blob: tr}
	srv, _ := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	get := func(size string) (*http.Response, []byte) {
		t.Helper()
		resp, err := admin.Get(srv.URL + "/api/madnetwork/cover/" + hash + "?size=" + size)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	resp, body := get("thumb")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sized cover = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want the source format kept", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Errorf("thumb = %dx%d, want the 64px square crop", b.Dx(), b.Dy())
	}

	// The original stays the no-size answer.
	resp, body = get("")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, cover) {
		t.Errorf("no-size answer changed: %d, %d bytes", resp.StatusCode, len(body))
	}

	// Every size is on disk after one derive: no second fetch may happen.
	fed.blob = nil
	if resp, _ := get("small"); resp.StatusCode != http.StatusOK {
		t.Errorf("small after derive = %d, want 200 from the variant tree", resp.StatusCode)
	}
}

// TestAlbumImageMissBackfillsFromTheNetwork: a coverless local album whose
// image is requested redeems the madnetwork's claim in the background — the
// download-attach rule extended to albums that were already here. Driven
// through the real 404, because the trigger IS the miss.
func TestAlbumImageMissBackfillsFromTheNetwork(t *testing.T) {
	cover := pngBytes(t)
	sum := sha256.Sum256(cover)
	coverHash := hex.EncodeToString(sum[:])
	tr := newFakeTransfer(t, coverHash, "original.png", int64(len(cover)))
	tr.append(t, cover)
	tr.finish()

	fed := &fakeBlobFederation{blob: tr}
	srv, db := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	ctx := context.Background()
	albumID, err := db.ResolveAlbumID(ctx, "Backfill Band", "Backfill Album")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "aa44", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa44", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := db.TouchCatalogSourceSeen(ctx, src.ID, time.Now().Unix(), ""); err != nil {
		t.Fatalf("touch source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Their Song", Artist: "Backfill Band",
		AlbumArtist: "Backfill Band", Album: "Backfill Album",
		CoverHash: coverHash, CoverExt: ".png",
		Renditions: []federation.CatalogRendition{{Hash: strings.Repeat("db", 32), Size: 5}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	// The miss that triggers it.
	resp, err := admin.Get(srv.URL + "/api/albums/" + strconv.FormatInt(albumID, 10) + "/image")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("coverless album image = %d, want 404 (the answer itself stays a miss)", resp.StatusCode)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if has, err := db.HasAlbumCover(ctx, albumID); err == nil && has {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the network's claim was never redeemed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	imageHash, sourceExt, _, _, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || imageHash != coverHash || sourceExt != ".png" {
		t.Fatalf("backfilled cover = (%q,%q) err=%v, want the claimant's", imageHash, sourceExt, err)
	}
}
