//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/config"
)

// The trust graph is a GRAPH, not a tree, and the pairing path says why when it
// does not converge. Both properties are here because they were found together:
// the second is what an admin needs when the first appears not to hold.

// startNodeChain wires three nodes A—B—C on the *underlay*: A listens, B dials A
// and listens itself, C dials B. A and C share no link and reach each other only
// through B's routing — the shape a real deployment has when two home servers
// both peer with the same public one.
func startNodeChain(t *testing.T, sA, sB, sC *memStore) (a, b, c *Node) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	u1, u2 := reserveUnderlay(t), reserveUnderlay(t)

	a = startChaosNode(t, "a", dir, sA, logger, config.FederationConfig{
		Listen: []string{"tcp://" + u1},
	}, chaosOpts())
	b = startChaosNode(t, "b", dir, sB, logger, config.FederationConfig{
		Peers:  []string{"tcp://" + u1},
		Listen: []string{"tcp://" + u2},
	}, chaosOpts())
	c = startChaosNode(t, "c", dir, sC, logger, config.FederationConfig{
		Peers: []string{"tcp://" + u2},
	}, chaosOpts())
	return a, b, c
}

// TestFriendOfAFriend: two nodes that are both friends of the same third node
// must be able to friend each other directly. Nothing in the design forbids it —
// friendship is a graph, and a network that could only branch would be a tree
// with a single point of failure at every fork — so this pins the property
// against the state machine, over two underlay hops with no direct link.
func TestFriendOfAFriend(t *testing.T) {
	ctx := context.Background()
	sA, sB, sC := newMemStore(), newMemStore(), newMemStore()
	a, b, c := startNodeChain(t, sA, sB, sC)

	makeFriends(t, a, b, sA, sB) // A—B
	makeFriends(t, b, c, sB, sC) // B—C

	// A's admin imports C's card and C's admin imports A's — the ordinary
	// friend-of-a-friend case. Both sides having imported is the *symmetric*
	// order, which no accept can resolve: it converges only because a pair
	// request arriving at a pending_outgoing peer proves the mutual intent.
	if _, err := a.ImportCard(ctx, c.Info().Card); err != nil {
		t.Fatalf("A imports C's card: %v", err)
	}
	if _, err := c.ImportCard(ctx, a.Info().Card); err != nil {
		t.Fatalf("C imports A's card: %v", err)
	}
	waitFor(t, "A—C to converge on friend", func() bool {
		a.Nudge()
		c.Nudge()
		pa, errA := sA.GetFederationPeerByKey(ctx, c.PublicKeyHex())
		pc, errC := sC.GetFederationPeerByKey(ctx, a.PublicKeyHex())
		return errA == nil && errC == nil && pa.State == PeerFriend && pc.State == PeerFriend
	})

	// The friendship each node already had is untouched: a new edge must not
	// disturb the one it was discovered through.
	for _, tc := range []struct {
		store *memStore
		other *Node
		what  string
	}{{sA, b, "A—B"}, {sC, b, "C—B"}} {
		p, err := tc.store.GetFederationPeerByKey(ctx, tc.other.PublicKeyHex())
		if err != nil || p.State != PeerFriend {
			t.Errorf("%s after the new friendship = %v (%v), want friend", tc.what, p, err)
		}
	}
}

// TestPairAttemptDelivered: a pairing waiting on the other admin says so. This is
// the difference between "your request is sitting in their queue" and "your
// request never arrived", which the peer list otherwise renders identically —
// both are `pending_outgoing`, forever.
func TestPairAttemptDelivered(t *testing.T) {
	ctx := context.Background()
	sA, sB := newMemStore(), newMemStore()
	dir, logger := t.TempDir(), log.New(io.Discard, "", 0)
	underlay := reserveUnderlay(t)
	a := startChaosNode(t, "a", dir, sA, logger, config.FederationConfig{Listen: []string{"tcp://" + underlay}}, chaosOpts())
	b := startChaosNode(t, "b", dir, sB, logger, config.FederationConfig{Peers: []string{"tcp://" + underlay}}, chaosOpts())

	// B asks A to be friends; A's admin never acts.
	if _, err := b.ImportCard(ctx, a.Info().Card); err != nil {
		t.Fatalf("import card on B: %v", err)
	}
	waitFor(t, "B to record a delivered pairing attempt", func() bool {
		b.Nudge()
		at, ok := b.lastAttempt(a.PublicKeyHex())
		return ok && at.Delivered() && at.Result == "pending"
	})

	// The peer list carries it, which is how the admin page shows it.
	peers, err := b.Peers(ctx)
	if err != nil || len(peers) != 1 {
		t.Fatalf("B's peers = %v (%v), want one", peers, err)
	}
	if at := peers[0].LastAttempt; at == nil || at.Result != "pending" || at.At == 0 {
		t.Fatalf("peer row's last attempt = %+v, want a delivered pending one with a timestamp", at)
	}

	// And once A accepts, the same reporting shows the far side flipped.
	pa, err := sA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
	if err != nil {
		t.Fatalf("A never recorded the request: %v", err)
	}
	if err := a.AcceptPeer(ctx, pa.ID); err != nil {
		t.Fatalf("accept on A: %v", err)
	}
	waitFor(t, "B's attempt to report friend", func() bool {
		b.Nudge()
		at, ok := b.lastAttempt(a.PublicKeyHex())
		return ok && at.Result == "friend"
	})
}

// TestPairAttemptUnreachable: a node that is not there records why, rather than
// failing silently. The key is real (it derives a mesh address) but no node holds
// it, so the dial is the failure — the commonest one in the field, and the one an
// admin has no other way to distinguish.
func TestPairAttemptUnreachable(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	dir, logger := t.TempDir(), log.New(io.Discard, "", 0)
	n := startChaosNode(t, "a", dir, store, logger, config.FederationConfig{}, chaosOpts())

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ghost := hex.EncodeToString(pub)
	if _, err := n.ImportKey(ctx, ghost, "nobody home"); err != nil {
		t.Fatalf("import key: %v", err)
	}
	waitFor(t, "the failed attempt to be recorded", func() bool {
		n.Nudge()
		at, ok := n.lastAttempt(ghost)
		return ok && at.Error != "" && !at.Delivered()
	})
	at, _ := n.lastAttempt(ghost)
	if want := "could not reach this node on the mesh"; !strings.Contains(at.Error, want) {
		t.Errorf("attempt error = %q, want it to start with %q", at.Error, want)
	}
}

// Removing a peer forgets the in-memory pairing note with it (§Forgetting).
// Otherwise re-importing a key we once failed to reach greets the admin with a
// "last try" from a relationship that no longer exists.
func TestRemovePeerForgetsPairingAttempt(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	dir, logger := t.TempDir(), log.New(io.Discard, "", 0)
	n := startChaosNode(t, "a", dir, store, logger, config.FederationConfig{}, chaosOpts())

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ghost := hex.EncodeToString(pub)
	p, err := n.ImportKey(ctx, ghost, "nobody home")
	if err != nil {
		t.Fatalf("import key: %v", err)
	}
	waitFor(t, "the failed attempt to be recorded", func() bool {
		n.Nudge()
		_, ok := n.lastAttempt(ghost)
		return ok
	})

	if err := n.RemovePeer(ctx, p.ID); err != nil {
		t.Fatalf("remove peer: %v", err)
	}
	if at, ok := n.lastAttempt(ghost); ok {
		t.Errorf("the pairing note survived the removal: %+v", at)
	}
}

// TestImportKey: the map-driven path. A key is a whole identity, so importing one
// is the same act as importing a card — and the name that rides along is stored
// as the peer's own claim, never as this admin's label.
func TestImportKey(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	n := startChaosNode(t, "a", t.TempDir(), store, log.New(io.Discard, "", 0), config.FederationConfig{}, chaosOpts())

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := hex.EncodeToString(pub)
	p, err := n.ImportKey(ctx, "  "+strings.ToUpper(key)+"\n", "heard on the map")
	if err != nil {
		t.Fatalf("import key: %v", err)
	}
	if p.PublicKey != key {
		t.Errorf("stored key = %q, want the normalized %q", p.PublicKey, key)
	}
	if p.State != PeerPendingOutgoing {
		t.Errorf("state = %q, want pending_outgoing", p.State)
	}
	if p.HeardName != "heard on the map" || p.Name != "" {
		t.Errorf("names = label %q / heard %q, want the name as hearsay and no label", p.Name, p.HeardName)
	}
	if _, err := n.ImportKey(ctx, "not-a-key", ""); err == nil {
		t.Error("importing junk succeeded; want a validation error")
	}
	// Our own key is not a friend request.
	if _, err := n.ImportKey(ctx, n.PublicKeyHex(), ""); err == nil {
		t.Error("importing our own key succeeded; want ErrPeerState")
	}
}
