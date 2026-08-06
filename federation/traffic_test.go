//go:build !nofederation

package federation

import (
	"errors"
	"net/http/httptest"
	"testing"
)

// The traffic table's contract (docs/architecture/swarm-admin.md): draining is
// for the database, the session view is for the page, and the two must not be
// the same counter.

func TestTrafficDrainLeavesTheSessionView(t *testing.T) {
	tt := newTrafficTable()
	tt.note("aa", TrafficCounters{Up: 100})
	tt.note("aa", TrafficCounters{Down: 40})
	tt.note("bb", TrafficCounters{Up: 7})

	deltas := tt.drain()
	if len(deltas) != 2 {
		t.Fatalf("drain returned %d deltas, want 2", len(deltas))
	}
	if deltas[0].Hash != "aa" || deltas[0].Up != 100 || deltas[0].Down != 40 {
		t.Errorf("delta[0] = %+v, want aa up 100 down 40", deltas[0])
	}

	// Draining is what the flusher does every 30 s. If it reset the view, the
	// page's "this session" figure would mean "since the last flush" instead.
	snap := tt.snapshot()
	if snap.Up != 107 || snap.Down != 40 {
		t.Errorf("session totals after drain = up %d down %d, want 107/40", snap.Up, snap.Down)
	}
	if got := snap.Hashes["aa"]; got.Up != 100 || got.Down != 40 {
		t.Errorf("session per-hash after drain = %+v, want up 100 down 40", got)
	}

	// A second drain must be empty, or a retrying flusher would double-count.
	if again := tt.drain(); len(again) != 0 {
		t.Errorf("second drain returned %d deltas, want none", len(again))
	}
}

func TestTrafficPeersAreKeyedByIdentity(t *testing.T) {
	tt := newTrafficTable()
	tt.notePeer("keyA", "200::1", 10, 0) // an inbound serve to a placeable peer
	tt.notePeer("keyA", "", 0, 5)        // an outbound fetch from the same node
	tt.notePeer("", "200::9", 3, 0)      // a requester we could not place

	peers := tt.snapshot().Peers
	if len(peers) != 2 {
		t.Fatalf("peers = %d, want 2 (one identified, one anonymous)", len(peers))
	}
	byKey := map[string]PeerTraffic{}
	for _, p := range peers {
		byKey[p.Key+p.Addr] = p
	}
	known, ok := byKey["keyA200::1"]
	if !ok {
		t.Fatalf("no merged row for keyA: %+v", peers)
	}
	if known.Up != 10 || known.Down != 5 {
		t.Errorf("keyA = up %d down %d, want 10/5 — both directions belong to one row",
			known.Up, known.Down)
	}
	if _, ok := byKey["200::9"]; !ok {
		t.Errorf("an unplaceable requester should still be accounted by address: %+v", peers)
	}
}

// Waste is received minus delivered, computed once at the end. That is what
// makes it cover every way bytes get thrown away without any discard site having
// to remember to report one.
func TestTransferWasteIsReceivedMinusDelivered(t *testing.T) {
	cases := []struct {
		name              string
		received, kept    int64
		err               error
		wantWasted        int64
		wantDownUnchanged bool
	}{
		{name: "clean fetch wastes nothing", received: 100, kept: 100, wantWasted: 0},
		{name: "a re-fetched chunk is waste", received: 130, kept: 100, wantWasted: 30},
		{name: "a failed fetch wastes everything", received: 70, err: errors.New("no holder"), wantWasted: 70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &Node{traffic: newTrafficTable()}
			tr := newTransfer("hh", "", "")
			if tc.kept > 0 {
				tr.addProgress(tc.kept) // finish(nil) freezes size at the watermark
			}
			tr.addReceived(tc.received)
			tr.finish(tc.err)

			n.noteTransferEnd(tr)
			got := n.Traffic().Hashes["hh"].Wasted
			if got != tc.wantWasted {
				t.Errorf("wasted = %d, want %d (received %d, delivered %d)",
					got, tc.wantWasted, tc.received, tc.kept)
			}
		})
	}
}

// The seeding meter is always present, unlike the throttle, which the shipped
// (unlimited) default leaves off entirely — an unmeasured default would leave
// every node's contribution unknown.
func TestMeteredResponseWriterCountsWhatItWrites(t *testing.T) {
	var counted int64
	rec := httptest.NewRecorder()
	w := metered(rec, func(b int64) { counted += b })

	if _, err := w.Write([]byte("twelve bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	if counted != 13 {
		t.Errorf("counted %d bytes, want 13", counted)
	}
	if rec.Body.String() != "twelve bytes!" {
		t.Errorf("body = %q — the meter must pass bytes through untouched", rec.Body.String())
	}
}

// Nil-safety: a Node built without a traffic table (older test constructions)
// must count nothing rather than panic.
func TestTrafficTableIsNilSafe(t *testing.T) {
	var tt *trafficTable
	tt.note("aa", TrafficCounters{Up: 1})
	tt.notePeer("k", "a", 1, 1)
	if got := tt.drain(); got != nil {
		t.Errorf("drain on a nil table = %v, want nil", got)
	}
	if snap := tt.snapshot(); snap.Up != 0 || snap.Hashes == nil {
		t.Errorf("snapshot on a nil table = %+v, want zeroed with a usable map", snap)
	}
}

// The peer ledger drains like the blob one: the flusher takes what has not been
// written, the panel keeps what this session has moved. Two counters, because
// "this session" means since the process started, not since the last flush.
func TestPeerTrafficDrainLeavesTheSessionView(t *testing.T) {
	tt := newTrafficTable()
	tt.notePeer("keyA", "200::1", 100, 0)
	tt.notePeer("keyA", "", 0, 40)
	tt.notePeer("", "200::9", 7, 0) // could not be placed

	deltas := tt.drainPeers()
	if len(deltas) != 2 {
		t.Fatalf("drainPeers returned %d deltas, want 2", len(deltas))
	}
	byKey := map[string]PeerTrafficDelta{}
	for _, d := range deltas {
		byKey[d.Key] = d
	}
	if got := byKey["keyA"]; got.Up != 100 || got.Down != 40 {
		t.Errorf("keyA delta = %+v, want both directions on one row", got)
	}
	// An address is NOT an identity to file history under: the unplaceable
	// requester travels with an empty key, and the store folds every one of them
	// into a single bucket rather than letting a stranger size the table.
	got, ok := byKey[""]
	if !ok || got.Up != 7 {
		t.Errorf("unplaced delta = %+v (present %v), want 7 bytes under the empty key", got, ok)
	}

	snap := tt.snapshot()
	if len(snap.Peers) != 2 {
		t.Errorf("draining emptied the session view: %+v", snap.Peers)
	}
	for _, p := range snap.Peers {
		if p.Key == "keyA" && (p.Up != 100 || p.Down != 40) {
			t.Errorf("session row for keyA = %+v, want its bytes intact", p)
		}
	}

	if again := tt.drainPeers(); len(again) != 0 {
		t.Errorf("second drain returned %d deltas, want none", len(again))
	}

	// And it keeps counting after a drain, from the drained baseline.
	tt.notePeer("keyA", "", 5, 0)
	next := tt.drainPeers()
	if len(next) != 1 || next[0].Up != 5 {
		t.Errorf("post-drain delta = %+v, want only the 5 new bytes", next)
	}
	if snap := tt.snapshot(); snap.Peers[0].Up != 105 {
		t.Errorf("session row = %+v, want the running 105", snap.Peers[0])
	}
}

// Both drains are nil-safe on a node built without a traffic table.
func TestPeerTrafficDrainIsNilSafe(t *testing.T) {
	var tt *trafficTable
	if got := tt.drainPeers(); got != nil {
		t.Errorf("nil table drained %v", got)
	}
}
