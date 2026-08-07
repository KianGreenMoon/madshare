//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// gossipIntervals collapses the production cadences so a sweep does gossip work
// every round. GraphRepublish stays long: these tests assert propagation, and a
// heartbeat firing mid-test would churn sequences under the assertions.
var gossipIntervals = Intervals{
	Refresh:        50 * time.Millisecond,
	CatalogSync:    time.Millisecond,
	SnapshotTTL:    time.Millisecond,
	GraphRepublish: time.Hour,
	GraphTTL:       time.Hour,
	GraphAccept:    time.Millisecond,
	// These tests assert propagation, not caching. The digest memo invalidates
	// on every store change anyway, but a TTL of one millisecond keeps a stale
	// serial from ever being the reason a convergence assertion flakes.
	GraphDigestTTL: time.Millisecond,
}

// mustFriend pairs two running nodes and waits for both sides to agree, which
// is what every gossip test needs before it can assert anything.
func mustFriend(t *testing.T, from, to *Node, fromStore, toStore *memStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := from.ImportCard(ctx, to.Info().Card); err != nil {
		t.Fatalf("import card: %v", err)
	}
	var incoming int64
	waitFor(t, "the pairing request to arrive", func() bool {
		from.Nudge()
		p, err := toStore.GetFederationPeerByKey(ctx, from.PublicKeyHex())
		if err != nil {
			return false
		}
		incoming = p.ID
		return p.State == PeerPendingIncoming
	})
	if err := to.AcceptPeer(ctx, incoming); err != nil {
		t.Fatalf("accept peer: %v", err)
	}
	waitFor(t, "both sides to agree on the friendship", func() bool {
		from.Nudge()
		to.Nudge()
		p, err := fromStore.GetFederationPeerByKey(ctx, to.PublicKeyHex())
		return err == nil && p.State == PeerFriend
	})
}

// heldRecord returns the record a store holds for one origin, or nil.
func heldRecord(t *testing.T, s *memStore, origin string) *GraphRecord {
	t.Helper()
	payloads, err := s.GraphPayloads(context.Background(), []string{origin}, 0)
	if err != nil {
		t.Fatalf("GraphPayloads: %v", err)
	}
	raw, ok := payloads[origin]
	if !ok {
		return nil
	}
	// Parsing verifies the signature, so every assertion below is also an
	// assertion that what we stored is still authentic.
	rec, err := ParseGraphRecord(raw)
	if err != nil {
		t.Fatalf("stored record for %s does not verify: %v", origin[:8], err)
	}
	return rec
}

// startGossipNode brings up one node on the shared underlay.
func startGossipNode(t *testing.T, dir, name, underlay string, listen bool, store *memStore) *Node {
	t.Helper()
	cfg := config.FederationConfig{Name: name, KeyFile: filepath.Join(dir, name+".key")}
	if listen {
		cfg.Listen = []string{underlay}
	} else {
		cfg.Peers = []string{underlay}
	}
	n, err := Start(cfg, store, log.New(io.Discard, "", 0), WithIntervals(gossipIntervals))
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(n.Stop)
	return n
}

func freeUnderlay(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return fmt.Sprintf("tcp://%s", probe.Addr())
}

// TestGossipRelaysBeyondOwnFriends is the claim the whole F6 design rests on: A
// learns about C, whom it has never friended and never dials, because B relays
// C's own signed record.
//
// Friendship graph: A—B—C. A and C are strangers to each other.
func TestGossipRelaysBeyondOwnFriends(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)

	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)
	b := startGossipNode(t, dir, "node-b", underlay, false, storeB)
	c := startGossipNode(t, dir, "node-c", underlay, false, storeC)

	mustFriend(t, b, a, storeB, storeA) // A—B
	mustFriend(t, c, b, storeC, storeB) // B—C

	// A holds C's record, signed by C, without ever having friended it.
	waitFor(t, "C's record to reach A through B", func() bool {
		a.Nudge()
		b.Nudge()
		c.Nudge()
		return heldRecord(t, storeA, c.PublicKeyHex()) != nil
	})

	rec := heldRecord(t, storeA, c.PublicKeyHex())
	if rec.Origin != c.PublicKeyHex() {
		t.Fatalf("relayed record origin = %s, want C", rec.Origin[:8])
	}
	if len(rec.Friends) != 1 || rec.Friends[0].Key != b.PublicKeyHex() {
		t.Errorf("C's record names %+v, want exactly B", rec.Friends)
	}

	// And A never friended C — the record travelled, the trust did not.
	if p, err := storeA.GetFederationPeerByKey(context.Background(), c.PublicKeyHex()); err == nil {
		t.Errorf("A has a peer row for C (state %q); gossip must not create friendships", p.State)
	}
}

// A stranger cannot read the graph: it names third parties, so it is
// friends-only exactly like the catalog.
func TestGraphEndpointIsFriendsOnly(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)

	storeA, storeB := newMemStore(), newMemStore()
	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)
	b := startGossipNode(t, dir, "node-b", underlay, false, storeB)

	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   meshClientTimeout,
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/graph", a.Address(), MeshPort)

	waitFor(t, "the pre-friendship refusal", func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false // mesh still converging
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("graph without friendship = %d, want 403", resp.StatusCode)
		}
		return true
	})

	mustFriend(t, b, a, storeB, storeA)
	waitFor(t, "the graph to open to a friend", func() bool {
		b.Nudge()
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// Turning publish_friend_list off makes a node a DEAD END in the graph, not
// merely a quiet participant — a consequence of the admission rule worth
// pinning down, since it is the real cost of the setting.
//
// B still collects and would still serve its friends' records. But it publishes
// no edges, so nothing in A's store ever names C, and the admission rule
// refuses C's record as unattributed. The wall is symmetric: C cannot learn A
// either. This is why the setting defaults to on, and why any UI for it has to
// say more than "hide my friends".
func TestSilentNodeWallsOffItsFriends(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)

	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	storeB.silent = true // B publishes no record of its own

	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)
	b := startGossipNode(t, dir, "node-b", underlay, false, storeB)
	c := startGossipNode(t, dir, "node-c", underlay, false, storeC)

	mustFriend(t, b, a, storeB, storeA)
	mustFriend(t, c, b, storeC, storeB)

	// B participates fully in everything except publishing: it collects both
	// friends' records, so its silence is not a withdrawal from the network.
	waitFor(t, "B to collect both friends' records", func() bool {
		a.Nudge()
		b.Nudge()
		c.Nudge()
		return heldRecord(t, storeB, a.PublicKeyHex()) != nil &&
			heldRecord(t, storeB, c.PublicKeyHex()) != nil
	})
	if rec := heldRecord(t, storeB, b.PublicKeyHex()); rec != nil {
		t.Error("a silent node published a record of its own")
	}

	// Give the graph every chance to converge, then confirm it did not: with
	// nobody vouching for C, A refuses the record however often it is offered.
	for i := 0; i < 20; i++ {
		a.Nudge()
		b.Nudge()
		c.Nudge()
		time.Sleep(50 * time.Millisecond)
	}
	if rec := heldRecord(t, storeA, c.PublicKeyHex()); rec != nil {
		t.Errorf("A admitted C's record with nobody naming C: %+v", rec)
	}
	if rec := heldRecord(t, storeC, a.PublicKeyHex()); rec != nil {
		t.Errorf("C admitted A's record with nobody naming A: %+v", rec)
	}
}

// A node republishes when its friend list changes, and the new record carries a
// higher sequence so it replaces the old one everywhere.
func TestOwnRecordBumpsSequenceOnNewFriendship(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)

	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)
	b := startGossipNode(t, dir, "node-b", underlay, false, storeB)
	c := startGossipNode(t, dir, "node-c", underlay, false, storeC)

	mustFriend(t, b, a, storeB, storeA)
	var first *GraphRecord
	waitFor(t, "B's first record", func() bool {
		b.Nudge()
		first = heldRecord(t, storeB, b.PublicKeyHex())
		return first != nil && len(first.Friends) == 1
	})

	mustFriend(t, c, b, storeC, storeB)
	var second *GraphRecord
	waitFor(t, "B to republish with both friendships", func() bool {
		b.Nudge()
		second = heldRecord(t, storeB, b.PublicKeyHex())
		return second != nil && len(second.Friends) == 2
	})
	if second.Seq <= first.Seq {
		t.Errorf("sequence did not advance: %d → %d", first.Seq, second.Seq)
	}
}

// heldMarks returns the distrust record a store holds for one origin, or nil.
func heldMarks(t *testing.T, s *memStore, origin string) *MarkRecord {
	t.Helper()
	payloads, err := s.MarkPayloads(context.Background(), []string{origin}, 0)
	if err != nil {
		t.Fatalf("MarkPayloads: %v", err)
	}
	raw, ok := payloads[origin]
	if !ok {
		return nil
	}
	rec, err := ParseMarkRecord(raw)
	if err != nil {
		t.Fatalf("stored distrust record for %s does not verify: %v", origin[:8], err)
	}
	return rec
}

// Blocking publishes a distrust mark, and lifting the block clears it — the
// property that makes a network-wide accusation ledger recoverable rather than
// permanent. Without the supersede-with-empty step, a lifted block would stand
// on every node until the record expired days later.
func TestBlockPublishesMarkAndUnblockClearsIt(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)
	ctx := context.Background()

	storeA, storeB := newMemStore(), newMemStore()
	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)
	b := startGossipNode(t, dir, "node-b", underlay, false, storeB)
	mustFriend(t, b, a, storeB, storeA)

	// Nobody blocked: no distrust record at all, rather than an empty one.
	if rec := heldMarks(t, storeA, a.PublicKeyHex()); rec != nil {
		t.Fatalf("published a distrust record with nothing blocked: %+v", rec)
	}

	peerB, err := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
	if err != nil {
		t.Fatalf("look up B on A: %v", err)
	}
	if err := a.BlockPeer(ctx, peerB.ID, "advertised a hash with a contradicting fingerprint"); err != nil {
		t.Fatalf("BlockPeer: %v", err)
	}

	var marked *MarkRecord
	waitFor(t, "A to publish the block as a mark", func() bool {
		a.Nudge()
		marked = heldMarks(t, storeA, a.PublicKeyHex())
		return marked != nil && len(marked.Marks) == 1
	})
	if marked.Marks[0].Key != b.PublicKeyHex() {
		t.Errorf("mark names %s, want B", marked.Marks[0].Key[:8])
	}
	if marked.Marks[0].Reason != "advertised a hash with a contradicting fingerprint" {
		t.Errorf("mark reason = %q", marked.Marks[0].Reason)
	}
	if marked.Marks[0].At == 0 {
		t.Error("mark carries no timestamp")
	}

	// The blocked peer also leaves the friend-list record: a block is not a
	// friendship, and publishing it as one would misstate the graph.
	waitFor(t, "the blocked edge to leave A's friend list", func() bool {
		a.Nudge()
		rec := heldRecord(t, storeA, a.PublicKeyHex())
		return rec != nil && len(rec.Friends) == 0
	})

	if err := a.UnblockPeer(ctx, peerB.ID); err != nil {
		t.Fatalf("UnblockPeer: %v", err)
	}
	var cleared *MarkRecord
	waitFor(t, "the mark to be superseded by an empty record", func() bool {
		a.Nudge()
		cleared = heldMarks(t, storeA, a.PublicKeyHex())
		return cleared != nil && len(cleared.Marks) == 0
	})
	if cleared.Seq <= marked.Seq {
		t.Errorf("clearing record did not supersede: seq %d → %d", marked.Seq, cleared.Seq)
	}
}

// A node can block a key it has no relationship with — someone seen only on the
// gossiped graph. Without this the map would be a read-only curiosity.
func TestBlockKeyMarksAStranger(t *testing.T) {
	dir := t.TempDir()
	underlay := freeUnderlay(t)
	ctx := context.Background()

	storeA := newMemStore()
	a := startGossipNode(t, dir, "node-a", underlay, true, storeA)

	stranger := "00000000000000000000000000000000000000000000000000000000000000ab"
	if err := a.BlockKey(ctx, stranger, "seen on the map", "claims audio it does not have"); err != nil {
		t.Fatalf("BlockKey: %v", err)
	}
	p, err := storeA.GetFederationPeerByKey(ctx, stranger)
	if err != nil {
		t.Fatalf("blocked stranger has no peer row: %v", err)
	}
	if p.State != PeerBlocked {
		t.Errorf("state = %q, want blocked", p.State)
	}

	var rec *MarkRecord
	waitFor(t, "the stranger's mark to be published", func() bool {
		a.Nudge()
		rec = heldMarks(t, storeA, a.PublicKeyHex())
		// Wait for a mark that is actually written, not merely counted: a publish
		// can be observed with the list sized but the fields still empty, and a
		// len()-only predicate passes on that half-written record — the assertion
		// below then reads the blanks and fails a test that had nothing wrong with
		// it. Checking wholeness here rather than the values keeps the assertion
		// live, so a genuinely wrong mark still fails loudly.
		return rec != nil && len(rec.Marks) == 1 && rec.Marks[0].Key != ""
	})
	if rec.Marks[0].Key != stranger || rec.Marks[0].Reason != "claims audio it does not have" {
		t.Errorf("mark = %+v", rec.Marks[0])
	}
}
