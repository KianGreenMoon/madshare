//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// memStore is an in-memory PeerStore for handshake/sync tests (the real one is
// *database.DB, exercised in database/federation_test.go and
// database/madnetwork_test.go).
type memStore struct {
	mu    sync.Mutex
	next  int64
	peers map[int64]*Peer

	// Catalog half (F2): published is what this node offers friends; caches
	// holds the per-peer pulled copies.
	published []CatalogEntry
	caches    map[int64][]CatalogEntry
}

func newMemStore() *memStore {
	return &memStore{peers: map[int64]*Peer{}, caches: map[int64][]CatalogEntry{}}
}

func (m *memStore) PublishedCatalog(context.Context) ([]CatalogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CatalogEntry(nil), m.published...), nil
}

func (m *memStore) ReplacePeerCatalog(_ context.Context, peerID int64, serial string, syncedAt int64, entries []CatalogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[peerID]
	if !ok {
		return ErrPeerNotFound
	}
	m.caches[peerID] = append([]CatalogEntry(nil), entries...)
	p.CatalogSerial, p.CatalogSyncedAt = serial, syncedAt
	return nil
}

func (m *memStore) MarkPeerCatalogChecked(_ context.Context, peerID int64, serial string, syncedAt int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[peerID]
	if !ok {
		return ErrPeerNotFound
	}
	p.CatalogSerial, p.CatalogSyncedAt = serial, syncedAt
	return nil
}

func (m *memStore) cachedCatalog(peerID int64) []CatalogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CatalogEntry(nil), m.caches[peerID]...)
}

func (m *memStore) setPublished(entries []CatalogEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = entries
}

func (m *memStore) ListFederationPeers(context.Context) ([]*Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memStore) GetFederationPeer(_ context.Context, id int64) (*Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return nil, ErrPeerNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) GetFederationPeerByKey(_ context.Context, key string) (*Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.peers {
		if p.PublicKey == key {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrPeerNotFound
}

func (m *memStore) InsertFederationPeer(_ context.Context, p *Peer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	cp := *p
	cp.ID = m.next
	m.peers[cp.ID] = &cp
	return cp.ID, nil
}

func (m *memStore) SetFederationPeerState(_ context.Context, id int64, state, prev string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return ErrPeerNotFound
	}
	p.State, p.PrevState = state, prev
	return nil
}

func (m *memStore) UpdateFederationPeerName(_ context.Context, id int64, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return ErrPeerNotFound
	}
	p.Name = name
	return nil
}

func (m *memStore) SetFederationPeerUser(_ context.Context, id int64, userID *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return ErrPeerNotFound
	}
	p.UserID = userID
	return nil
}

func (m *memStore) TouchFederationPeerSeen(_ context.Context, id int64, when int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[id]; ok && p.LastSeen < when {
		p.LastSeen = when
	}
	return nil
}

func (m *memStore) DeleteFederationPeer(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.peers[id]; !ok {
		return ErrPeerNotFound
	}
	delete(m.peers, id)
	return nil
}

// waitFor polls until cond returns true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestFriendshipHandshake walks the full F1 flow between two embedded nodes:
// B imports A's card (pending_outgoing) → B's node introduces itself → A sees
// a pending_incoming request → A accepts → both sides converge on friend.
// Then A blocks B and the mesh refuses B everything; unblock restores service.
func TestFriendshipHandshake(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	storeA, storeB := newMemStore(), newMemStore()
	a, err := Start(config.FederationConfig{
		Name:    "node-a",
		KeyFile: filepath.Join(dir, "a.key"),
		Listen:  []string{underlay},
	}, storeA, logger)
	if err != nil {
		t.Fatalf("start node A: %v", err)
	}
	defer a.Stop()

	b, err := Start(config.FederationConfig{
		Name:    "node-b",
		KeyFile: filepath.Join(dir, "b.key"),
		Peers:   []string{underlay},
	}, storeB, logger)
	if err != nil {
		t.Fatalf("start node B: %v", err)
	}
	defer b.Stop()

	ctx := context.Background()

	// B's admin imports A's card.
	cardA := a.Info().Card
	if cardA.Name != "node-a" || cardA.PublicKey != a.PublicKeyHex() {
		t.Fatalf("card A = %+v, want name node-a and A's key", cardA)
	}
	pB, err := b.ImportCard(ctx, cardA)
	if err != nil {
		t.Fatalf("import card on B: %v", err)
	}
	if pB.State != PeerPendingOutgoing {
		t.Fatalf("B's row after import = %s, want pending_outgoing", pB.State)
	}
	// Importing again is idempotent.
	if p2, err := b.ImportCard(ctx, cardA); err != nil || p2.ID != pB.ID {
		t.Fatalf("re-import = %v, %v; want the same row", p2, err)
	}

	// B's refresh loop introduces B to A: A records a pending_incoming request.
	var incomingID int64
	waitFor(t, "A to see B's pairing request", func() bool {
		b.Nudge()
		p, err := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
		if err != nil {
			return false
		}
		incomingID = p.ID
		if p.Name != "node-b" {
			t.Fatalf("A recorded requester name %q, want node-b", p.Name)
		}
		return p.State == PeerPendingIncoming
	})

	// A's admin accepts → A is friend immediately; B converges via its retries.
	if err := a.AcceptPeer(ctx, incomingID); err != nil {
		t.Fatalf("accept on A: %v", err)
	}
	if err := a.AcceptPeer(ctx, incomingID); err == nil {
		t.Error("second accept succeeded; want ErrPeerState")
	}
	waitFor(t, "both sides to reach friend", func() bool {
		b.Nudge()
		pa, errA := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
		pb, errB := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex())
		return errA == nil && errB == nil && pa.State == PeerFriend && pb.State == PeerFriend
	})

	// Contact updates last_seen on A's side (B's calls arrived over the mesh).
	if pa, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex()); pa.LastSeen == 0 {
		t.Error("A never recorded last_seen for B despite mesh contact")
	}

	// Block: A refuses B all service — even the ping.
	pa, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
	if err := a.BlockPeer(ctx, pa.ID); err != nil {
		t.Fatalf("block on A: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   5 * time.Second,
	}
	pingA := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", a.Address(), MeshPort)
	waitFor(t, "A to refuse the blocked B", func() bool {
		resp, err := client.Get(pingA)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusForbidden
	})

	// A blocked node's card cannot be imported.
	if _, err := a.ImportCard(ctx, b.Info().Card); err == nil {
		t.Error("importing a blocked node's card succeeded; want ErrPeerState")
	}

	// Unblock returns to the pre-block state (friend) and service resumes.
	if err := a.UnblockPeer(ctx, pa.ID); err != nil {
		t.Fatalf("unblock on A: %v", err)
	}
	if pa, _ = storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex()); pa.State != PeerFriend {
		t.Errorf("state after unblock = %s, want friend", pa.State)
	}
	waitFor(t, "service to resume after unblock", func() bool {
		resp, err := client.Get(pingA)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// TestPairRejectsMismatchedKey: a pair request claiming a key that does not
// derive to the caller's mesh address must be refused — otherwise anyone on
// the mesh could enroll someone else's identity.
func TestPairRejectsMismatchedKey(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	a, err := Start(config.FederationConfig{
		KeyFile: filepath.Join(dir, "a.key"),
		Listen:  []string{underlay},
	}, newMemStore(), logger)
	if err != nil {
		t.Fatalf("start node A: %v", err)
	}
	defer a.Stop()

	b, err := Start(config.FederationConfig{
		KeyFile: filepath.Join(dir, "b.key"),
		Peers:   []string{underlay},
	}, newMemStore(), logger)
	if err != nil {
		t.Fatalf("start node B: %v", err)
	}
	defer b.Stop()

	// C's key is real but the request comes from B's address.
	cCfg, err := loadOrCreateKey(filepath.Join(dir, "c.key"))
	if err != nil {
		t.Fatal(err)
	}
	cPub := ed25519.PrivateKey(cCfg.PrivateKey).Public().(ed25519.PublicKey)
	forged := fmt.Sprintf(`{"protocol":0,"name":"evil","public_key":"%x"}`, []byte(cPub))

	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   5 * time.Second,
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/pair", a.Address(), MeshPort)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Post(url, "application/json", strings.NewReader(forged))
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("pair request never reached A: %v", err)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("forged pair request = %d, want 403", resp.StatusCode)
		}
		return
	}
}
