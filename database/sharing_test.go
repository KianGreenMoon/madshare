package database

import (
	"context"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// The publish-picker queries (sharing.go, full-node-mode.md W2). The stakes:
// on a player the node default is pinned Local, so these calls are the ONLY
// gate between its disk and the catalog — a pin that does not stick keeps a
// library closed, and a listing that overshoots claims sharing that is not
// happening.

// pinLocalDefault makes the store behave like an embedded player: nothing is
// published unless pinned. The same write app.Network's PublishNothing does.
func pinLocalDefault(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	p.DefaultShareDepth = federation.DepthPrivate
	if err := db.SetMadnetworkPolicy(ctx, p); err != nil {
		t.Fatalf("pin default: %v", err)
	}
}

func TestSetShareDepthByTagsets(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("sh1"), "a.mp3", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("sh2"), "b.mp3", "The Band", "Studio Album")
	t1, t2 := tagsetIDOf(t, db, f1.ID), tagsetIDOf(t, db, f2.ID)

	// A pin must be explicit: the unset update is refused, not a silent no-op.
	if _, err := db.SetShareDepthByTagsets(ctx, []int64{t1}, ShareDepthUpdate{}); err == nil {
		t.Error("an unset update was accepted")
	}
	if _, err := db.SetShareDepthByTagsets(ctx, []int64{t1}, ShareDepthUpdate{Set: true, Depth: 3}); err == nil {
		t.Error("an illegal depth (3) was accepted — ValidDepth allows exactly three values")
	}

	n, err := db.SetShareDepthByTagsets(ctx, []int64{t1, t2},
		ShareDepthUpdate{Set: true, Depth: federation.DepthUnlimited})
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if n != 2 {
		t.Errorf("pinned %d recordings, want 2", n)
	}

	depths, err := db.TagsetShareDepths(ctx, []int64{t1, t2})
	if err != nil {
		t.Fatalf("depths: %v", err)
	}
	if depths[t1] != federation.DepthUnlimited || depths[t2] != federation.DepthUnlimited {
		t.Errorf("depths = %v, want both pinned to Madnetwork", depths)
	}

	// Inherit un-pins: the id must then be ABSENT from the read, because
	// absence is the "inherits the default" answer.
	if _, err := db.SetShareDepthByTagsets(ctx, []int64{t1}, ShareDepthUpdate{Set: true, Inherit: true}); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	depths, err = db.TagsetShareDepths(ctx, []int64{t1, t2})
	if err != nil {
		t.Fatalf("depths: %v", err)
	}
	if _, pinned := depths[t1]; pinned {
		t.Error("an inherited row still reads as pinned")
	}
	if depths[t2] != federation.DepthUnlimited {
		t.Error("un-pinning one appearance touched another")
	}

	// An empty selection is a no-op, not an accidental everything.
	if n, err := db.SetShareDepthByTagsets(ctx, nil, ShareDepthUpdate{Set: true, Depth: federation.DepthFriends}); err != nil || n != 0 {
		t.Errorf("empty selection: n=%d err=%v, want 0, nil", n, err)
	}
}

func TestSharedTracksListsExactlyThePublishedSet(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	pinLocalDefault(t, db)

	f1 := insertTaggedFile(t, db, hash64("sl1"), "one.mp3", "The Band", "Studio Album")
	insertTaggedFile(t, db, hash64("sl2"), "two.mp3", "The Band", "Studio Album")
	t1 := tagsetIDOf(t, db, f1.ID)

	// The player's resting state: nothing pinned, nothing listed.
	shared, err := db.SharedTracks(ctx)
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if len(shared) != 0 {
		t.Fatalf("a closed library lists %d shared tracks, want 0", len(shared))
	}

	if _, err := db.SetShareDepthByTagsets(ctx, []int64{t1},
		ShareDepthUpdate{Set: true, Depth: federation.DepthFriends}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	shared, err = db.SharedTracks(ctx)
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if len(shared) != 1 || shared[0].TagsetID != t1 {
		t.Fatalf("shared = %+v, want exactly the pinned appearance", shared)
	}
	if shared[0].Depth != federation.DepthFriends || !shared[0].Pinned {
		t.Errorf("row = %+v, want Depth=Friends, Pinned", shared[0])
	}

	// A pinned-Local row is a pin, but not a published one.
	if _, err := db.SetShareDepthByTagsets(ctx, []int64{t1},
		ShareDepthUpdate{Set: true, Depth: federation.DepthPrivate}); err != nil {
		t.Fatalf("re-pin local: %v", err)
	}
	if shared, err = db.SharedTracks(ctx); err != nil || len(shared) != 0 {
		t.Errorf("a Local-pinned row is listed as shared: %+v, %v", shared, err)
	}
}

// On a node whose DEFAULT is open (a server), unpinned rows are published too,
// and the listing must say the default did it — un-pinning such a row would
// not withdraw it.
func TestSharedTracksUnderAnOpenDefault(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("so1"), "one.mp3", "The Band", "Studio Album")
	tid := tagsetIDOf(t, db, f.ID)

	shared, err := db.SharedTracks(ctx)
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if len(shared) != 1 || shared[0].TagsetID != tid {
		t.Fatalf("shared = %+v, want the one row (server default is open)", shared)
	}
	if shared[0].Pinned {
		t.Error("a default-published row claims a pin of its own")
	}
	if shared[0].Depth != federation.DepthUnlimited {
		t.Errorf("Depth = %d, want the inherited default", shared[0].Depth)
	}
}
