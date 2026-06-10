package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// plFixture creates a user and n live files, returning the user id and the
// files' content hashes in insertion order.
func plFixture(t *testing.T, db *DB, n int) (userID int64, hashes []string) {
	t.Helper()
	ctx := context.Background()
	userID, err := db.CreateUser(ctx, fmt.Sprintf("user-%s", t.Name()), "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for i := range n {
		hash := fmt.Sprintf("%064d", i+1)
		if err := db.InsertFile(ctx, newFile(hash), newUpload(fmt.Sprintf("t%d.mp3", i+1)), newMeta()); err != nil {
			t.Fatalf("InsertFile %d: %v", i, err)
		}
		hashes = append(hashes, hash)
	}
	return userID, hashes
}

func TestPlaylist_CreateGetRoundtrip(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Road Trip", hashes)
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
		if e.Hash != hashes[i] {
			t.Errorf("items[%d].Hash = %s, want %s", i, e.Hash, hashes[i])
		}
		if e.Trashed {
			t.Errorf("items[%d] unexpectedly trashed", i)
		}
	}
}

func TestPlaylist_OwnershipIsScoped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	owner, hashes := plFixture(t, db, 1)
	other, err := db.CreateUser(ctx, "other", "x", false)
	if err != nil {
		t.Fatalf("CreateUser other: %v", err)
	}

	p, err := db.CreatePlaylist(ctx, owner, "Mine", hashes)
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
	if _, err := db.AddPlaylistItemsByHash(ctx, other, p.ID, hashes); !errors.Is(err, ErrPlaylistNotFound) {
		t.Errorf("AddPlaylistItemsByHash as other user: err = %v, want ErrPlaylistNotFound", err)
	}
}

func TestPlaylist_AddRejectsUnknownAndTrashed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes := plFixture(t, db, 2)

	p, err := db.CreatePlaylist(ctx, userID, "Strict", nil)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	unknown := fmt.Sprintf("%064d", 999)
	if _, err := db.AddPlaylistItemsByHash(ctx, userID, p.ID, []string{hashes[0], unknown}); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("add with unknown hash: err = %v, want ErrFileNotFound", err)
	}
	// The batch is atomic: the valid first hash must not have been added.
	_, items, err := db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items after failed batch = %d, want 0 (atomic add)", len(items))
	}

	if _, found, err := db.SoftDeleteFileByHash(ctx, hashes[1]); err != nil || !found {
		t.Fatalf("SoftDeleteFileByHash: found=%v err=%v", found, err)
	}
	if _, err := db.AddPlaylistItemsByHash(ctx, userID, p.ID, []string{hashes[1]}); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("add trashed hash: err = %v, want ErrFileNotFound", err)
	}
}

func TestPlaylist_TrashedAndHardDeletedItems(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Decay", hashes)
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	// Trash one file → its item stays, flagged Trashed (grayed in the UI).
	if _, found, err := db.SoftDeleteFileByHash(ctx, hashes[1]); err != nil || !found {
		t.Fatalf("SoftDeleteFileByHash: found=%v err=%v", found, err)
	}
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

	// Hard-delete it → the item disappears via FK cascade.
	if _, found, err := db.HardDeleteFileByHash(ctx, hashes[1]); err != nil || !found {
		t.Fatalf("HardDeleteFileByHash: found=%v err=%v", found, err)
	}
	_, items, err = db.GetPlaylist(ctx, userID, p.ID)
	if err != nil {
		t.Fatalf("GetPlaylist after hard delete: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("items after hard delete = %d, want 2 (cascade)", len(items))
	}
}

func TestFavorites_ToggleAndDedupe(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, hashes := plFixture(t, db, 2)

	favA, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil {
		t.Fatalf("EnsureFavoritesPlaylist: %v", err)
	}
	favB, err := db.EnsureFavoritesPlaylist(ctx, userID)
	if err != nil || favA != favB {
		t.Fatalf("EnsureFavoritesPlaylist not idempotent: %d vs %d (err %v)", favA, favB, err)
	}

	if liked, err := db.ToggleFavorite(ctx, userID, hashes[0]); err != nil || !liked {
		t.Fatalf("first toggle: liked=%v err=%v, want liked", liked, err)
	}
	// Adding the same file through the batch path dedupes on favorites.
	if added, err := db.AddPlaylistItemsByHash(ctx, userID, favA, []string{hashes[0], hashes[1]}); err != nil || added != 1 {
		t.Fatalf("batch add to favorites: added=%d err=%v, want 1 (dedupe)", added, err)
	}
	got, err := db.ListFavoriteHashes(ctx, userID)
	if err != nil {
		t.Fatalf("ListFavoriteHashes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("favorites = %v, want 2 entries", got)
	}

	// Un-like removes; trashed favorites drop out of the listed hashes.
	if liked, err := db.ToggleFavorite(ctx, userID, hashes[0]); err != nil || liked {
		t.Fatalf("second toggle: liked=%v err=%v, want un-liked", liked, err)
	}
	if _, found, err := db.SoftDeleteFileByHash(ctx, hashes[1]); err != nil || !found {
		t.Fatalf("SoftDeleteFileByHash: found=%v err=%v", found, err)
	}
	if got, err = db.ListFavoriteHashes(ctx, userID); err != nil || len(got) != 0 {
		t.Errorf("favorites after unlike+trash = %v (err %v), want empty", got, err)
	}

	if _, err := db.ToggleFavorite(ctx, userID, fmt.Sprintf("%064d", 999)); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("toggle unknown hash: err = %v, want ErrFileNotFound", err)
	}
}

func TestFavorites_SystemPlaylistGuards(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	userID, _ := plFixture(t, db, 0)

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
	userID, hashes := plFixture(t, db, 3)

	p, err := db.CreatePlaylist(ctx, userID, "Order", hashes)
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
	if items[0].Hash != hashes[2] || items[2].Hash != hashes[0] {
		t.Errorf("order after reorder = [%s %s %s], want reversed", items[0].Hash, items[1].Hash, items[2].Hash)
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
	userID, hashes := plFixture(t, db, 2)

	if _, err := db.ToggleFavorite(ctx, userID, hashes[0]); err != nil {
		t.Fatalf("ToggleFavorite: %v", err)
	}
	if _, err := db.CreatePlaylist(ctx, userID, "Beta", hashes); err != nil {
		t.Fatalf("CreatePlaylist Beta: %v", err)
	}
	if _, err := db.CreatePlaylist(ctx, userID, "alpha", nil); err != nil {
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
