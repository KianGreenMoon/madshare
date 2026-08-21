package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"net/http"
	"testing"
	"time"

	"daemonlord.ygg/madshare/auth"
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
