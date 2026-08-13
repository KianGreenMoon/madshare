package database

import (
	"context"
	"testing"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// TestMadnetworkDownMark covers the reactive down-mark (migration 048,
// docs/architecture/federation.md §Availability, "Reactive down-mark + the ping
// floor"): a first-hand connect failure hides a node's exclusively-held tracks
// without waiting out the pull window — but only in the corridor where the pull
// window is what was holding them up.
//
// The two halves are asserted together on purpose. Dropping the tight-window
// guard would turn one failed dial against a node pinged 30 seconds ago into an
// instant disappearance, which is the 1× margin the reverted presence feature
// died of; dropping the mark leaves the 45-minute stall the feature exists to
// close.
func TestMadnetworkDownMark(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	now := time.Now().Unix()
	const tightWindow, pullWindow = 180, 2700
	view := MadnetworkView{
		Cutoff: now - tightWindow, PullCutoff: now - pullWindow,
		PingedSince: now - tightWindow,
	}

	// Three nodes, all marked unreachable a moment ago. What differs is how
	// recently each was last SEEN, which is what decides whether the mark may
	// speak at all.
	corridor := insertSource(t, db, "cccc") // member, seen 10 min ago: the corridor
	justSeen := insertSource(t, db, "dddd") // member, seen 30 s ago: knife-edge
	friendPeer := insertPeer(t, db, "eeee", "friendly", federation.PeerFriend)
	friend := insertSource(t, db, "eeee")

	for _, s := range []struct {
		id       int64
		seen     int64
		artist   string
		entryKey string
	}{
		{corridor, now - 600, "Corridor Artist", "1"},
		{justSeen, now - 30, "Just Seen Artist", "2"},
		{friend, now - 30, "Friend Artist", "3"},
	} {
		if err := db.TouchCatalogSourceSeen(ctx, s.id, s.seen, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceSourceCatalog(ctx, s.id, "s"+s.entryKey, 1, []federation.CatalogEntry{
			catEntry(s.entryKey, "r"+s.entryKey, s.artist, s.artist+" Album",
				s.artist+" Song", "hash-"+s.entryKey),
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkNodeUnreachable(ctx, sourceKey(t, db, s.id), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.TouchFederationPeerSeen(ctx, friendPeer, now-30); err != nil {
		t.Fatal(err)
	}

	shown := func() map[string]bool {
		t.Helper()
		artists, _, err := db.MadnetworkArtists(ctx, "", view, 0, "")
		if err != nil {
			t.Fatalf("MadnetworkArtists: %v", err)
		}
		out := map[string]bool{}
		for _, a := range artists {
			out[a.Name] = true
		}
		return out
	}

	got := shown()
	if got["Corridor Artist"] {
		t.Error("a member last seen 10 minutes ago was still shown after a failed " +
			"first-hand contact — the mark exists precisely to close that corridor, " +
			"where the pull window would have carried it for another 35 minutes")
	}
	if !got["Just Seen Artist"] {
		t.Error("a mark against a node seen 30 seconds ago hid it — the mark may " +
			"shorten the pull window and never the ping window, or one failed dial " +
			"is a 1x margin (the knife-edge the presence feature died of)")
	}
	if !got["Friend Artist"] {
		t.Error("a marked FRIEND pinged 30 seconds ago was hidden; friends are always " +
			"judged by the tight window, where the mark is inert by construction")
	}

	// The strip greys by the Go twin and the browse filters in SQL. They must
	// agree row for row, or one surface contradicts the other on the same screen.
	nodes, tracks, err := db.MadnetworkSummary(ctx, view)
	if err != nil {
		t.Fatalf("MadnetworkSummary: %v", err)
	}
	reach := map[int64]bool{}
	for _, n := range nodes {
		reach[n.ID] = n.Reachable
	}
	if reach[corridor] || !reach[justSeen] || !reach[friend] {
		t.Errorf("strip reachability = %+v; ReachableAt disagrees with reachClause", reach)
	}
	if tracks != 2 {
		t.Errorf("reachable track count = %d, want 2", tracks)
	}

	// No clearing column and no state machine: a later success retires the mark
	// by moving the other clock past it.
	if err := db.TouchCatalogSourceSeen(ctx, corridor, now, ""); err != nil {
		t.Fatal(err)
	}
	if !shown()["Corridor Artist"] {
		t.Error("a node that answered after being marked stayed hidden — the mark is " +
			"read against last_seen, so a fresh contact must retire it")
	}

	// Filtering off (fail open, or hide_unavailable disabled) suppresses the mark
	// exactly as it suppresses the windows.
	open, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 0, "")
	if err != nil {
		t.Fatalf("MadnetworkArtists (fail open): %v", err)
	}
	if len(open) != 3 {
		t.Errorf("fail-open artists = %d, want all 3 — marks are part of the filter", len(open))
	}
}

// TestReachableAtMatchesTheMarkRule pins the Go twin's arithmetic directly. The
// table is the predicate written out: the mark speaks only for a pull-clock node
// already outside the tight window.
func TestReachableAtMatchesTheMarkRule(t *testing.T) {
	const tight, pull = 900, 100 // "now" is 1000; tight = 100 s ago, pull = 900 s ago
	for _, tc := range []struct {
		name string
		r    SourceReach
		want bool
	}{
		{"pull-clock node in the corridor, marked", SourceReach{LastSeen: 500, UnreachableAt: 990}, false},
		{"pull-clock node in the corridor, unmarked", SourceReach{LastSeen: 500}, true},
		{"pull-clock node marked before it was last seen", SourceReach{LastSeen: 500, UnreachableAt: 400}, true},
		{"pull-clock node inside the tight window, marked", SourceReach{LastSeen: 950, UnreachableAt: 990}, true},
		{"pinged node inside its window, marked", SourceReach{LastSeen: 950, UnreachableAt: 990, Pinged: true}, true},
		{"pinged node outside its window", SourceReach{LastSeen: 500, Pinged: true}, false},
		{"pull-clock node past even the pull window", SourceReach{LastSeen: 50}, false},
	} {
		if got := ReachableAt(tc.r, tight, pull); got != tc.want {
			t.Errorf("%s: ReachableAt(%+v) = %v, want %v", tc.name, tc.r, got, tc.want)
		}
	}
	// Cutoff 0 is fail open, marks included.
	if !ReachableAt(SourceReach{LastSeen: 1, UnreachableAt: 990}, 0, pull) {
		t.Error("a marked node was called unreachable while filtering was off")
	}
}
