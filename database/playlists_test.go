package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// plFixture creates a user and n live files, returning the user id, the files'
// content hashes (the trash/file operations stay hash-addressed), and their
// offered tagsets' ids (the playlist/favorites identity) in insertion order.
func plFixture(t *testing.T, db *DB, n int) (userID int64, hashes []string, tagsetIDs []int64) {
	t.Helper()
	ctx := context.Background()
	userID, err := db.CreateUser(ctx, fmt.Sprintf("user-%s", t.Name()), "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for i := range n {
		hash := fmt.Sprintf("%064d", i+1)
		f := newFile(hash)
		if err := db.InsertFile(ctx, f, newUpload(fmt.Sprintf("t%d.mp3", i+1)), newMeta()); err != nil {
			t.Fatalf("InsertFile %d: %v", i, err)
		}
		hashes = append(hashes, hash)
		var tsID int64
		if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id = ?`, f.ID).Scan(&tsID); err != nil {
			t.Fatalf("tagset id %d: %v", i, err)
		}
		tagsetIDs = append(tagsetIDs, tsID)
	}
	return userID, hashes, tagsetIDs
}

// TestPlaylist_RemoteItems — the remote madnetwork half (mig 029,
// docs/ui/madnetwork-page.md §Remote tracks): adds, favorites toggle,
// availability, and the repoint sweep.
func TestPlaylist_RemoteItems(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes, tagsetIDs := plFixture(t, db, 1)

	remoteHash := "ab" + fmt.Sprintf("%062d", 7)
	ref := RemoteTrackRef{Hash: remoteHash, Title: "Far Song", Artist: "Far Artist", Album: "Far Album"}

	// Mixed create: one local appearance + one remote ref.
	p, err := db.CreatePlaylist(ctx, userID, "Mixed", tagsetIDs, []RemoteTrackRef{ref})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if p.TrackCount != 2 {
		t.Fatalf("track count = %d, want 2", p.TrackCount)
	}
	_, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	rm := items[1]
	if rm.RemoteHash != remoteHash || rm.Title.String != "Far Song" || rm.Artist.String != "Far Artist" {
		t.Errorf("remote item = %+v, want the captured ref", rm)
	}
	if rm.Available {
		t.Error("remote item available with no source, want unavailable")
	}

	// An ONLINE friend advertising the hash in holdings makes it available
	// (presence-grade: an offline holder does not).
	friend := insertPeer(t, db, "d4d4", "friend-d", federation.PeerFriend)
	if err := db.ReplacePeerHoldings(ctx, friend, []string{remoteHash}); err != nil {
		t.Fatalf("ReplacePeerHoldings: %v", err)
	}
	if _, items, _ = db.GetPlaylist(ctx, userID, p.ID); items[1].Available {
		t.Error("remote item available while its only holder is offline")
	}
	var online []int64
	db.SetMadnetworkPresenceProvider(func() MadnetworkPresence {
		return MadnetworkPresence{OnlinePeerIDs: online}
	})
	online = []int64{friend}
	if _, items, _ = db.GetPlaylist(ctx, userID, p.ID); !items[1].Available {
		t.Error("remote item still unavailable with an online friend holding it")
	}

	// Malformed hash rejects the batch.
	if _, err := db.AddPlaylistItems(ctx, userID, p.ID, nil, []RemoteTrackRef{{Hash: "nope"}}); !errors.Is(err, ErrBadRemoteRef) {
		t.Errorf("bad hash add err = %v, want ErrBadRemoteRef", err)
	}

	// Remote favorites: toggle on, listed, deduped on re-add, toggle off.
	if liked, err := db.ToggleRemoteFavorite(ctx, userID, ref); err != nil || !liked {
		t.Fatalf("ToggleRemoteFavorite on = (%v, %v), want liked", liked, err)
	}
	if hs, _ := db.ListFavoriteRemoteHashes(ctx, userID); len(hs) != 1 || hs[0] != remoteHash {
		t.Errorf("remote favorite hashes = %v, want [%s]", hs, remoteHash)
	}
	favID, _ := db.EnsureFavoritesPlaylist(ctx, userID)
	if added, err := db.AddPlaylistItems(ctx, userID, favID, nil, []RemoteTrackRef{ref}); err != nil || added != 0 {
		t.Errorf("re-add to favorites = (%d, %v), want 0 added (deduped)", added, err)
	}
	if liked, err := db.ToggleRemoteFavorite(ctx, userID, ref); err != nil || liked {
		t.Errorf("ToggleRemoteFavorite off = (%v, %v), want unliked", liked, err)
	}
	if _, err := db.ToggleRemoteFavorite(ctx, userID, RemoteTrackRef{Hash: "zz"}); !errors.Is(err, ErrBadRemoteRef) {
		t.Errorf("bad hash toggle err = %v, want ErrBadRemoteRef", err)
	}

	// Repoint: a remote row whose hash IS a local live blob becomes the blob's
	// visible appearance; a row whose playlist already holds that appearance is
	// dropped instead of duplicated.
	localRef := RemoteTrackRef{Hash: hashes[0], Title: "Now Local"}
	if _, err := db.AddPlaylistItems(ctx, userID, p.ID, nil, []RemoteTrackRef{localRef}); err != nil {
		t.Fatalf("add repointable ref: %v", err)
	}
	if n, err := db.RepointRemotePlaylistItems(ctx); err != nil || n != 1 {
		t.Fatalf("RepointRemotePlaylistItems = (%d, %v), want 1 handled", n, err)
	}
	_, items, _ = db.GetPlaylist(ctx, userID, p.ID)
	// The playlist already held tagsetIDs[0] from the create — the remote twin
	// must be gone, not doubled.
	locals := 0
	for _, it := range items {
		if it.TagsetID == tagsetIDs[0] {
			locals++
		}
		if it.RemoteHash == hashes[0] {
			t.Errorf("repointable remote row survived: %+v", it)
		}
	}
	if locals != 1 {
		t.Errorf("local appearance rows = %d, want exactly 1 after repoint-dedupe", locals)
	}

	// And the plain repoint (no duplicate in the target playlist): a fresh
	// playlist with only the remote ref gains the local appearance.
	p2, err := db.CreatePlaylist(ctx, userID, "Repoint", nil, []RemoteTrackRef{localRef})
	if err != nil {
		t.Fatalf("CreatePlaylist repoint: %v", err)
	}
	if n, err := db.RepointRemotePlaylistItems(ctx); err != nil || n != 1 {
		t.Fatalf("second sweep = (%d, %v), want 1", n, err)
	}
	_, items2, _ := db.GetPlaylist(ctx, userID, p2.ID)
	if len(items2) != 1 || items2[0].TagsetID != tagsetIDs[0] || items2[0].RemoteHash != "" {
		t.Errorf("repointed item = %+v, want the local appearance", items2[0])
	}
	// Idempotent: nothing left to do.
	if n, _ := db.RepointRemotePlaylistItems(ctx); n != 0 {
		t.Errorf("third sweep handled %d rows, want 0", n)
	}
}

func TestPlaylist_CreateGetRoundtrip(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, _, tagsetIDs := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Road Trip", tagsetIDs, nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if p.TrackCount != 3 || p.Kind != PlaylistRegular {
		t.Errorf("created playlist = %+v, want 3 tracks, regular", p)
	}

	got, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if got.Name != "Road Trip" || got.TrackCount != 3 {
		t.Errorf("playlist = %+v, want name Road Trip, 3 tracks", got)
	}
	for i, e := range items {
		if e.TagsetID != tagsetIDs[i] {
			t.Errorf("items[%d].TagsetID = %d, want %d", i, e.TagsetID, tagsetIDs[i])
		}
		if e.Trashed {
			t.Errorf("items[%d] unexpectedly trashed", i)
		}
	}
}

func TestPlaylist_OwnershipIsScoped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	owner, _, tagsetIDs := plFixture(t, db, 1)
	other, err := db.CreateUser(ctx, "other", "x", false)
	if err != nil {
		t.Fatalf("CreateUser other: %v", err)
	}

	p, err := db.CreatePlaylist(ctx, owner, "Mine", tagsetIDs, nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	if _, _, err := db.GetPlaylist(ctx, other, p.ID); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("GetPlaylist as other user: err = %v, want ErrPlaylistNotFound", err)
	}
	if err := db.RenamePlaylist(ctx, other, p.ID, "Stolen"); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("RenamePlaylist as other user: err = %v, want ErrPlaylistNotFound", err)
	}
	if err := db.DeletePlaylist(ctx, other, p.ID); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("DeletePlaylist as other user: err = %v, want ErrPlaylistNotFound", err)
	}
	if _, err := db.AddPlaylistItems(ctx, other, p.ID, tagsetIDs, nil); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("AddPlaylistItems as other user: err = %v, want ErrPlaylistNotFound", err)
	}
}

func TestPlaylist_AddRejectsUnknownAndTrashed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes, tagsetIDs := plFixture(t, db, 2)

	p, err := db.CreatePlaylist(ctx, userID, "Strict", nil, nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	if _, err := db.AddPlaylistItems(ctx, userID, p.ID, []int64{tagsetIDs[0], 99999}, nil); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("add with unknown tagset: err = %v, want ErrFileNotFound", err)
	}
	// The batch is atomic: the valid first id must not have been added.
	_, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items after failed batch = %d, want 0 (atomic add)", len(items))
	}

	trashAppearancesByHash(t, db, hashes[1])
	if _, err := db.AddPlaylistItems(ctx, userID, p.ID, []int64{tagsetIDs[1]}, nil); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("add trashed appearance: err = %v, want ErrFileNotFound", err)
	}
}

func TestPlaylist_TrashedAndHardDeletedItems(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes, tagsetIDs := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Decay", tagsetIDs, nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	// Trash one file → its item stays, flagged Trashed (grayed in the UI).
	trashAppearancesByHash(t, db, hashes[1])
	_, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if len(items) != 3 || !items[1].Trashed || items[0].Trashed || items[2].Trashed {
		t.Fatalf("after trash: items=%d trashed=[%v %v %v], want 3 items with only #1 trashed",
			len(items), items[0].Trashed, items[1].Trashed, items[2].Trashed)
	}
	// Trashed items keep their metadata visible.
	if items[1].Title.String == "" {
		t.Errorf("trashed item lost its title metadata")
	}

	// Hard-delete the blob (prune blob-loss) → the appearance survives in
	// Trash (GC model: purge destroys bytes, never catalog entries), so the
	// item stays, still flagged Trashed. Only purging the appearance row
	// itself (Trash "Delete forever") would cascade the item away.
	if _, found, err := db.HardDeleteFileByHash(ctx, hashes[1]); err != nil || !found {
		t.Fatalf("HardDeleteFileByHash: found=%v err=%v", found, err)
	}
	_, items, err = db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist after hard delete: %v", err)
	}
	if len(items) != 3 || !items[1].Trashed {
		t.Errorf("items after blob hard delete = %d (trashed=%v), want 3 with #1 trashed", len(items), items[1].Trashed)
	}

	// Purge the trashed appearance row → now the item disappears via FK cascade.
	if n, _, err := db.BulkHardDeleteTagsets(ctx, []int64{tagsetIDs[1]}); err != nil || n != 1 {
		t.Fatalf("purge trashed appearance: n=%d err=%v", n, err)
	}
	_, items, err = db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist after purge: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("items after appearance purge = %d, want 2 (cascade)", len(items))
	}
}

func TestFavorites_ToggleAndDedupe(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes, tagsetIDs := plFixture(t, db, 2)

	favA, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		t.Fatalf("EnsureFavoritesPlaylist: %v", err)
	}
	favB, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil || favA != favB {
		t.Fatalf("EnsureFavoritesPlaylist not idempotent: %d vs %d (err %v)", favA, favB, err)
	}

	if liked, err := db.ToggleFavorite(ctx, userID, tagsetIDs[0]); err != nil || !liked {
		t.Fatalf("first toggle: liked=%v err=%v, want liked", liked, err)
	}
	// Adding the same appearance through the batch path dedupes on favorites.
	if added, err := db.AddPlaylistItems(ctx, userID, favA, []int64{tagsetIDs[0], tagsetIDs[1]}, nil); err != nil || added != 1 {
		t.Fatalf("batch add to favorites: added=%d err=%v, want 1 (dedupe)", added, err)
	}
	got, err := db.ListFavoriteTagsetIDs(ctx, userID)
	if err != nil {
		t.Fatalf("ListFavoriteTagsetIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("favorites = %v, want 2 entries", got)
	}

	// Un-like removes; trashed favorites drop out of the listed ids.
	if liked, err := db.ToggleFavorite(ctx, userID, tagsetIDs[0]); err != nil || liked {
		t.Fatalf("second toggle: liked=%v err=%v, want un-liked", liked, err)
	}
	trashAppearancesByHash(t, db, hashes[1])
	if got, err = db.ListFavoriteTagsetIDs(ctx, userID); err != nil || len(got) != 0 {
		t.Errorf("favorites after unlike+trash = %v (err %v), want empty", got, err)
	}

	if _, err := db.ToggleFavorite(ctx, userID, 99999); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("toggle unknown tagset: err = %v, want ErrFileNotFound", err)
	}
}

func TestFavorites_SystemPlaylistGuards(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, _, _ := plFixture(t, db, 0)

	fav, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		t.Fatalf("EnsureFavoritesPlaylist: %v", err)
	}
	if err := db.RenamePlaylist(ctx, userID, fav, "Not Favorites"); !errors.Is(err, ErrPlaylistSystem) {
		t.Errorf("rename favorites: err = %v, want ErrPlaylistSystem", err)
	}
	if err := db.DeletePlaylist(ctx, userID, fav); !errors.Is(err, ErrPlaylistSystem) {
		t.Errorf("delete favorites: err = %v, want ErrPlaylistSystem", err)
	}
}

func TestPlaylist_ReorderAndRemove(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, _, tagsetIDs := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Order", tagsetIDs, nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	_, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}

	// Reverse the order.
	ids := []int64{items[2].ItemID, items[1].ItemID, items[0].ItemID}
	if err := db.ReorderPlaylist(ctx, userID, p.ID, ids); err != nil {
		t.Fatalf("ReorderPlaylist: %v", err)
	}
	_, items, err = db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist after reorder: %v", err)
	}
	if items[0].TagsetID != tagsetIDs[2] || items[2].TagsetID != tagsetIDs[0] {
		t.Errorf("order after reorder = [%d %d %d], want reversed", items[0].TagsetID, items[1].TagsetID, items[2].TagsetID)
	}

	// Non-permutations are rejected.
	if err := db.ReorderPlaylist(ctx, userID, p.ID, ids[:2]); !errors.Is(err, ErrBadReorder) {
		t.Errorf("short reorder: err = %v, want ErrBadReorder", err)
	}
	if err := db.ReorderPlaylist(ctx, userID, p.ID, []int64{ids[0], ids[1], 99999}); !errors.Is(err, ErrBadReorder) {
		t.Errorf("foreign id reorder: err = %v, want ErrBadReorder", err)
	}
	if err := db.ReorderPlaylist(ctx, userID, p.ID, []int64{ids[0], ids[0], ids[1]}); !errors.Is(err, ErrBadReorder) {
		t.Errorf("duplicate id reorder: err = %v, want ErrBadReorder", err)
	}

	// Remove the middle item.
	found, err := db.RemovePlaylistItem(ctx, userID, p.ID, items[1].ItemID)
	if err != nil || !found {
		t.Fatalf("RemovePlaylistItem: found=%v err=%v", found, err)
	}
	if found, err = db.RemovePlaylistItem(ctx, userID, p.ID, items[1].ItemID); err != nil || found {
		t.Errorf("second remove: found=%v err=%v, want not found", found, err)
	}
	_, items, err = db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist after remove: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("items after remove = %d, want 2", len(items))
	}
}

func TestPlaylist_ListIncludesCountsAndFavoritesFirst(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, _, tagsetIDs := plFixture(t, db, 2)

	if _, err := db.ToggleFavorite(ctx, userID, tagsetIDs[0]); err != nil {
		t.Fatalf("ToggleFavorite: %v", err)
	}
	if _, err := db.CreatePlaylist(ctx, userID, "Beta", tagsetIDs, nil); err != nil {
		t.Fatalf("CreatePlaylist Beta: %v", err)
	}
	if _, err := db.CreatePlaylist(ctx, userID, "alpha", nil, nil); err != nil {
		t.Fatalf("CreatePlaylist alpha: %v", err)
	}

	lists, err := db.ListPlaylists(ctx, userID)
	if err != nil {
		t.Fatalf("ListPlaylists: %v", err)
	}
	if len(lists) != 3 {
		t.Fatalf("playlists = %d, want 3", len(lists))
	}
	if lists[0].Kind != PlaylistFavorites || lists[0].TrackCount != 1 {
		t.Errorf("lists[0] = %+v, want favorites with 1 track", lists[0])
	}
	// Regular playlists follow, case-insensitively by name.
	if lists[1].Name != "alpha" || lists[2].Name != "Beta" {
		t.Errorf("regular order = [%s %s], want [alpha Beta]", lists[1].Name, lists[2].Name)
	}
	if lists[2].TrackCount != 2 {
		t.Errorf("Beta track count = %d, want 2", lists[2].TrackCount)
	}
}
