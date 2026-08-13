package database

import (
	"context"
	"errors"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

func insertPeer(t *testing.T, db *DB, key, name, state string) int64 {
	t.Helper()
	id, err := db.InsertFederationPeer(context.Background(), &federation.Peer{
		PublicKey: key,
		Name:      name,
		State:     state,
		CreatedAt: 1000,
	})
	if err != nil {
		t.Fatalf("insert peer %s: %v", key, err)
	}
	return id
}

// insertSource is insertPeer's twin for the cache side (F7 item 5): the source
// row a cached catalog hangs off. Most tests want both — a node an admin decided
// something about *and* whose library we hold — but they are separate rows now,
// and a source with no peer row is exactly the member case.
func insertSource(t *testing.T, db *DB, key string) int64 {
	t.Helper()
	src, err := db.EnsureCatalogSource(context.Background(), key, 1000)
	if err != nil {
		t.Fatalf("ensure source %s: %v", key, err)
	}
	return src.ID
}

func TestFederationPeers_CRUDAndOrdering(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	blockedID := insertPeer(t, db, "aa11", "blocked-one", federation.PeerBlocked)
	pendingID := insertPeer(t, db, "bb22", "pending-one", federation.PeerPendingIncoming)
	friendID := insertPeer(t, db, "cc33", "friend-one", federation.PeerFriend)

	// A duplicate key must trip the UNIQUE constraint.
	if _, err := db.InsertFederationPeer(ctx, &federation.Peer{PublicKey: "cc33", State: federation.PeerFriend}); err == nil {
		t.Error("duplicate public_key accepted; want UNIQUE violation")
	}

	peers, err := db.ListFederationPeers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(peers) != 3 {
		t.Fatalf("len(peers) = %d, want 3", len(peers))
	}
	// Friends first, then pending, blocked last.
	if peers[0].ID != friendID || peers[1].ID != pendingID || peers[2].ID != blockedID {
		t.Errorf("order = %v/%v/%v, want friend, pending, blocked", peers[0].State, peers[1].State, peers[2].State)
	}

	p, err := db.GetFederationPeerByKey(ctx, "bb22")
	if err != nil || p.ID != pendingID {
		t.Fatalf("GetFederationPeerByKey = %v, %v", p, err)
	}
	if _, err := db.GetFederationPeerByKey(ctx, "zz99"); !errors.Is(err, federation.ErrPeerNotFound) {
		t.Errorf("unknown key error = %v, want ErrPeerNotFound", err)
	}

	// State transition with prev_state (block/unblock round trip data).
	if err := db.SetFederationPeerState(ctx, friendID, federation.PeerBlocked, federation.PeerFriend); err != nil {
		t.Fatalf("set state: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, friendID)
	if p.State != federation.PeerBlocked || p.PrevState != federation.PeerFriend {
		t.Errorf("state/prev = %s/%s, want blocked/friend", p.State, p.PrevState)
	}
	if err := db.SetFederationPeerState(ctx, 9999, federation.PeerFriend, ""); !errors.Is(err, federation.ErrPeerNotFound) {
		t.Errorf("set state on missing peer = %v, want ErrPeerNotFound", err)
	}

	// last_seen is monotonic: an older touch never rewinds it.
	if err := db.TouchFederationPeerSeen(ctx, pendingID, 500); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := db.TouchFederationPeerSeen(ctx, pendingID, 300); err != nil {
		t.Fatalf("touch older: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, pendingID)
	if p.LastSeen != 500 {
		t.Errorf("last_seen = %d, want 500 (monotonic)", p.LastSeen)
	}

	if err := db.UpdateFederationPeerName(ctx, pendingID, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, pendingID)
	if p.Name != "renamed" {
		t.Errorf("name = %q, want renamed", p.Name)
	}

	if err := db.DeleteFederationPeer(ctx, blockedID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetFederationPeer(ctx, blockedID); !errors.Is(err, federation.ErrPeerNotFound) {
		t.Errorf("deleted peer lookup = %v, want ErrPeerNotFound", err)
	}
}

func TestFederationPeers_GuestOnly(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	peerID := insertPeer(t, db, "dd44", "madplayer", federation.PeerFriend)

	// Fresh peers are full friends.
	p, _ := db.GetFederationPeer(ctx, peerID)
	if p.GuestOnly {
		t.Error("fresh peer is guest-only, want the full audience by default")
	}

	if err := db.SetFederationPeerGuestOnly(ctx, peerID, true); err != nil {
		t.Fatalf("demote peer: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, peerID)
	if !p.GuestOnly {
		t.Error("demoted peer not guest-only")
	}

	if err := db.SetFederationPeerGuestOnly(ctx, peerID, false); err != nil {
		t.Fatalf("restore peer: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, peerID)
	if p.GuestOnly {
		t.Error("restored peer still guest-only")
	}

	// A peer id without a trust row is refused, as every trust-group setter is.
	if err := db.SetFederationPeerGuestOnly(ctx, peerID+1000, true); !errors.Is(err, federation.ErrPeerNotFound) {
		t.Errorf("demote unknown peer = %v, want ErrPeerNotFound", err)
	}

	// Removing the peer clears the demotion with the rest of the trust group,
	// so a later re-friending starts from the full default.
	if err := db.SetFederationPeerGuestOnly(ctx, peerID, true); err != nil {
		t.Fatalf("re-demote peer: %v", err)
	}
	if err := db.DeleteFederationPeer(ctx, peerID); err != nil {
		t.Fatalf("delete peer: %v", err)
	}
	refriended, err := db.InsertFederationPeer(ctx, &federation.Peer{
		PublicKey: "dd44", Name: "madplayer", State: federation.PeerFriend, CreatedAt: 2000,
	})
	if err != nil {
		t.Fatalf("re-friend peer: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, refriended)
	if p.GuestOnly {
		t.Error("re-friended peer inherited a cleared demotion")
	}
}
