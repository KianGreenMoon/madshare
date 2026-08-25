package app_test

import (
	"context"
	"testing"

	"daemonlord.ygg/madshare/app"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The embedder's community browse and publish picker (app/madnetwork.go). The
// row-level behaviour is pinned in database/sharing_test.go and the browse
// rules in the api package's handler tests — the same code answers both, which
// is the point of the browse core. What THESE tests pin is the wiring: when
// the surfaces exist, and that they answer through a real started instance.

// A configuration with no federation node has no community to browse, and the
// embedder should learn that at the call that hands out the surface — not from
// every browse call failing.
func TestMadnetworkIsAbsentWithoutANode(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	if _, ok := inst.Madnetwork(); ok {
		t.Error("Madnetwork() is available with federation disabled; want not ok")
	}

	// The sharing arm needs NO node — organize-then-share happens before any
	// mesh is up (full-node-mode.md: the surface hangs off the library side).
	ctx := context.Background()
	if shared, err := inst.Published(ctx); err != nil || len(shared) != 0 {
		t.Errorf("Published on a fresh library = %v, %v; want empty, nil", shared, err)
	}
	if depths, err := inst.ShareDepths(ctx, []int64{1, 2}); err != nil || len(depths) != 0 {
		t.Errorf("ShareDepths on a fresh library = %v, %v; want empty, nil", depths, err)
	}
	if err := inst.SetShareDepth(ctx, []int64{}, database.ShareDepthUpdate{Set: true, Depth: federation.DepthFriends}); err != nil {
		t.Errorf("SetShareDepth over an empty selection: %v", err)
	}
}

// With the node up the browse exists and answers — empty, on a fresh library
// with no friends, which is a working answer and not an error.
func TestMadnetworkBrowsesThroughTheNode(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	cfg := meshConfig(t, t.TempDir())
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	mn, ok := inst.Madnetwork()
	if !ok {
		t.Fatal("Madnetwork() not available with the node running")
	}
	ctx := context.Background()
	artists, cursor, err := mn.Artists(ctx, "", 0, "")
	if err != nil {
		t.Fatalf("Artists: %v", err)
	}
	if len(artists) != 0 || cursor != "" {
		t.Errorf("Artists on an empty community = %d rows, cursor %q", len(artists), cursor)
	}
	if _, err := mn.AlbumsByArtist(ctx, "Nobody"); err != nil {
		t.Errorf("AlbumsByArtist: %v", err)
	}
	if _, err := mn.TracksByAlbum(ctx, "Nobody", "Nothing"); err != nil {
		t.Errorf("TracksByAlbum: %v", err)
	}
	if res, err := mn.Search(ctx, "anything"); err != nil || res == nil {
		t.Errorf("Search = %v, %v; want empty results, nil", res, err)
	}
}
