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

func TestFederationPeers_UserMapping(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at) VALUES ('mapped', 'x', 1000)`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, _ := res.LastInsertId()
	peerID := insertPeer(t, db, "dd44", "madplayer", federation.PeerFriend)

	if err := db.SetFederationPeerUser(ctx, peerID, &userID); err != nil {
		t.Fatalf("map user: %v", err)
	}
	p, _ := db.GetFederationPeer(ctx, peerID)
	if p.UserID == nil || *p.UserID != userID || p.Username != "mapped" {
		t.Errorf("mapping = %v/%q, want %d/mapped", p.UserID, p.Username, userID)
	}

	// A dangling user id must trip the foreign key, not create an orphan mapping.
	bogus := userID + 1000
	if err := db.SetFederationPeerUser(ctx, peerID, &bogus); err == nil {
		t.Error("dangling user_id accepted; want FK violation")
	}

	// Clearing the mapping.
	if err := db.SetFederationPeerUser(ctx, peerID, nil); err != nil {
		t.Fatalf("clear mapping: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, peerID)
	if p.UserID != nil || p.Username != "" {
		t.Errorf("cleared mapping = %v/%q, want nil/empty", p.UserID, p.Username)
	}

	// Deleting the mapped user leaves the peer row with the mapping nulled.
	if err := db.SetFederationPeerUser(ctx, peerID, &userID); err != nil {
		t.Fatalf("re-map user: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	p, _ = db.GetFederationPeer(ctx, peerID)
	if p.UserID != nil {
		t.Errorf("user_id after user delete = %v, want nil (ON DELETE SET NULL)", p.UserID)
	}
}
