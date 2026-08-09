//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
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

	// Catalog half (F2): published is what this node offers; caches holds the
	// pulled copies, keyed by SOURCE id (F7 item 5 — a cached catalog hangs off
	// a node we pull from, which need not be a peer). holdings (F4) is the
	// per-source cached list of cache-held hashes; seedEnabled/seedCache back
	// SeedingPolicy.
	published   []CatalogEntry
	sources     map[int64]*CatalogSource
	nextSource  int64
	caches      map[int64][]CatalogEntry
	holdings    map[int64][]string
	seedEnable  bool
	seedCache   bool
	serveGuests bool

	// Sharing scope (F5): depths overrides one published entry's share depth by
	// entry key (absent = the node default, ∞ here); audiences overrides what a
	// peer resolves to (absent = FriendAudience, the unmapped default).
	depths    map[string]int
	audiences map[int64]Audience

	// Gossiped graph (F6, methods in gossip_store_test.go): signed records by
	// origin, for friend lists and distrust lists respectively. silent turns
	// PublishFriendList off — the node still relays, it just publishes nothing
	// of its own.
	graph map[string]*memRecord
	marks map[string]*memRecord
	// digests counts GraphDigest calls that reached the store, so a test can
	// assert the node's memo absorbed the repeats.
	digests int
	silent  bool

	// homes are the servers this node signed in to (§"The household"). Empty for
	// every node that is not a listener one, which is nearly all of them.
	homes []HomeNode
}

func newMemStore() *memStore {
	return &memStore{
		peers:      map[int64]*Peer{},
		sources:    map[int64]*CatalogSource{},
		caches:     map[int64][]CatalogEntry{},
		holdings:   map[int64][]string{},
		seedEnable: true, // seed by default, mirroring the DB defaults
		seedCache:  true,
		depths:     map[string]int{},
		audiences:  map[int64]Audience{},
		graph:      map[string]*memRecord{},
		marks:      map[string]*memRecord{},
	}
}

// inScope mirrors the DB's audience predicate over one published entry: its
// effective depth must reach the audience, and a guest-only audience sees only
// guest-playable entries. Callers hold m.mu.
func (m *memStore) inScope(e CatalogEntry, aud Audience) bool {
	if !aud.Serves() {
		return false // the fail-closed zero value, mirrored from audienceClause
	}
	depth, ok := m.depths[e.Key]
	if !ok {
		depth = DepthUnlimited
	}
	if depth < aud.Distance {
		return false
	}
	return !aud.GuestOnly || e.GuestPlayable
}

func (m *memStore) PublishedCatalog(_ context.Context, aud Audience) ([]CatalogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []CatalogEntry{}
	for _, e := range m.published {
		if m.inScope(e, aud) {
			out = append(out, e)
		}
	}
	return out, nil
}

// PeerAudience returns the peer's configured audience, defaulting to the
// unmapped friend (the whole published set).
func (m *memStore) PeerAudience(_ context.Context, peerID int64) (Audience, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if aud, ok := m.audiences[peerID]; ok {
		return aud, nil
	}
	return FriendAudience, nil
}

func (m *memStore) ReplaceSourceCatalog(_ context.Context, sourceID int64, serial string, syncedAt int64, entries []CatalogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sources[sourceID]
	if !ok {
		return ErrPeerNotFound
	}
	m.caches[sourceID] = append([]CatalogEntry(nil), entries...)
	s.CatalogSerial, s.CatalogSyncedAt = serial, syncedAt
	if syncedAt > s.LastSeen {
		s.LastSeen = syncedAt
	}
	return nil
}

func (m *memStore) MarkSourceCatalogChecked(_ context.Context, sourceID int64, serial string, syncedAt int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sources[sourceID]
	if !ok {
		return ErrPeerNotFound
	}
	s.CatalogSerial, s.CatalogSyncedAt = serial, syncedAt
	if syncedAt > s.LastSeen {
		s.LastSeen = syncedAt
	}
	return nil
}

// ── Catalog sources (F7 item 5) ──────────────────────────────────────────────

func (m *memStore) EnsureCatalogSource(_ context.Context, publicKey string, now int64) (*CatalogSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sourceByKeyLocked(publicKey); s != nil {
		cp := *s
		return &cp, nil
	}
	m.nextSource++
	s := &CatalogSource{ID: m.nextSource, PublicKey: publicKey, FirstSeen: now}
	m.sources[s.ID] = s
	cp := *s
	return &cp, nil
}

func (m *memStore) ListCatalogSources(context.Context) ([]*CatalogSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*CatalogSource, 0, len(m.sources))
	for _, s := range m.sources {
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AttemptedAt != out[j].AttemptedAt {
			return out[i].AttemptedAt < out[j].AttemptedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memStore) MarkCatalogSourceAttempted(_ context.Context, id int64, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sources[id]; ok {
		s.AttemptedAt = at
	}
	return nil
}

// ApplyFreshnessHints mirrors the real store: only sources we already hold are
// moved, last_seen and hinted_at both monotonic (F7 item 10).
func (m *memStore) ApplyFreshnessHints(_ context.Context, seen map[string]int64, at int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	moved := 0
	for key, when := range seen {
		for _, s := range m.sources {
			if s.PublicKey != key {
				continue
			}
			if when > s.LastSeen {
				s.LastSeen = when
			}
			if at > s.HintedAt {
				s.HintedAt = at
			}
			moved++
		}
	}
	return moved, nil
}

func (m *memStore) TouchCatalogSourceSeen(_ context.Context, id int64, at int64, heardName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sources[id]
	if !ok {
		return nil
	}
	if at > s.LastSeen {
		s.LastSeen = at
	}
	if heardName != "" {
		s.HeardName = heardName
	}
	return nil
}

func (m *memStore) DropCatalogSources(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.sources, id)
		delete(m.caches, id)
		delete(m.holdings, id)
	}
	return nil
}

// sourceByKeyLocked finds a source row by node key; caller holds m.mu.
func (m *memStore) sourceByKeyLocked(key string) *CatalogSource {
	for _, s := range m.sources {
		if s.PublicKey == key {
			return s
		}
	}
	return nil
}

// BlobVisibleTo mirrors the DB predicate over the published set: a hash
// advertised by a published entry is visible when that entry is in the
// audience's scope. found stays true for a known-but-out-of-scope hash, so the
// fake reproduces the real store's "exists, may not have it" answer.
func (m *memStore) BlobVisibleTo(_ context.Context, hash string, aud Audience) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.published {
		for _, rd := range e.Renditions {
			if rd.Hash == hash {
				return m.inScope(e, aud), true, nil
			}
		}
	}
	return false, false, nil
}

// MadnetworkBlobProviders scans the cached catalogs and holdings for the hash,
// like the DB does — the union, deduped per source, minus blocked nodes.
func (m *memStore) MadnetworkBlobProviders(_ context.Context, hash string) (int64, []*BlobProvider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var size int64
	holders := map[int64]*BlobProvider{}
	note := func(sourceID int64) {
		if _, seen := holders[sourceID]; seen {
			return
		}
		s, ok := m.sources[sourceID]
		if !ok {
			return
		}
		prov := &BlobProvider{SourceID: s.ID, PublicKey: s.PublicKey,
			HeardName: s.HeardName, LastSeen: s.LastSeen}
		if p := m.peerByKeyLocked(s.PublicKey); p != nil {
			if p.State == PeerBlocked {
				return
			}
			prov.PeerID, prov.Name = p.ID, p.Name
			if p.LastSeen > prov.LastSeen {
				prov.LastSeen = p.LastSeen
			}
		}
		holders[sourceID] = prov
	}
	for sourceID, entries := range m.caches {
		for _, e := range entries {
			for _, rd := range e.Renditions {
				if rd.Hash == hash {
					if size == 0 {
						size = rd.Size
					}
					note(sourceID)
				}
			}
		}
	}
	for sourceID, hashes := range m.holdings {
		for _, h := range hashes {
			if h == hash {
				note(sourceID)
			}
		}
	}
	out := make([]*BlobProvider, 0, len(holders))
	for _, p := range holders {
		out = append(out, p)
	}
	return size, out, nil
}

// peerByKeyLocked finds a peer row by node key; caller holds m.mu.
func (m *memStore) peerByKeyLocked(key string) *Peer {
	for _, p := range m.peers {
		if p.PublicKey == key {
			return p
		}
	}
	return nil
}

// AddSourceHoldings mirrors the DB: add to one source's cache-holdings list
// without removing anything (the F9 item 2 announce path).
func (m *memStore) AddSourceHoldings(_ context.Context, sourceID int64, hashes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[sourceID]; !ok {
		return ErrPeerNotFound
	}
	have := make(map[string]bool, len(m.holdings[sourceID]))
	for _, h := range m.holdings[sourceID] {
		have[h] = true
	}
	for _, h := range hashes {
		if !have[h] {
			have[h] = true
			m.holdings[sourceID] = append(m.holdings[sourceID], h)
		}
	}
	return nil
}

// ReplaceSourceHoldings mirrors the DB: replace one source's cache-holdings list.
func (m *memStore) ReplaceSourceHoldings(_ context.Context, sourceID int64, hashes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[sourceID]; !ok {
		return ErrPeerNotFound
	}
	m.holdings[sourceID] = append([]string(nil), hashes...)
	return nil
}

// SeedingPolicy returns the configured seed flags (both default on; guests off,
// the production default — a test that wants the guest switch sets it).
func (m *memStore) SeedingPolicy(context.Context) (SeedPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return SeedPolicy{Enabled: m.seedEnable, Cache: m.seedCache, Guests: m.serveGuests}, nil
}

// setSeeding toggles the seed flags for a test.
func (m *memStore) setSeeding(enabled, cache bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seedEnable, m.seedCache = enabled, cache
}

// cachedCatalog returns what we hold from one node, addressed by its PEER id —
// the handle the older tests have. It resolves through the key to the source row
// that actually owns the rows now.
func (m *memStore) cachedCatalog(peerID int64) []CatalogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[peerID]
	if !ok {
		return nil
	}
	s := m.sourceByKeyLocked(p.PublicKey)
	if s == nil {
		return nil
	}
	return append([]CatalogEntry(nil), m.caches[s.ID]...)
}

// cachedFrom returns what we hold from one node addressed by its KEY, creating
// nothing — the read a discovery test needs, since the source row's existence is
// itself the thing under test.
func (m *memStore) cachedFrom(key string) []CatalogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sourceByKeyLocked(key)
	if s == nil {
		return nil
	}
	return append([]CatalogEntry(nil), m.caches[s.ID]...)
}

// cachedHoldings is cachedCatalog's twin for the holdings tracker.
func (m *memStore) cachedHoldings(peerID int64) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[peerID]
	if !ok {
		return nil
	}
	s := m.sourceByKeyLocked(p.PublicKey)
	if s == nil {
		return nil
	}
	return append([]string(nil), m.holdings[s.ID]...)
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

func (m *memStore) ListHomeNodes(context.Context) ([]HomeNode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]HomeNode(nil), m.homes...), nil
}

// addHome records a home server, as a client does after signing in.
func (m *memStore) addHome(publicKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.homes = append(m.homes, HomeNode{PublicKey: publicKey, AddedAt: time.Now().Unix()})
}

// forgetHomes signs out of every home server.
func (m *memStore) forgetHomes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.homes = nil
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

func (m *memStore) UpdateFederationPeerHeardName(_ context.Context, id int64, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return ErrPeerNotFound
	}
	p.HeardName = name
	return nil
}

// CheckSourceClaims: the store fake finds nothing — the contradiction checks are
// SQL and are tested against a real database (database/madnetwork_claims_test.go).
func (m *memStore) CheckSourceClaims(context.Context, int64) (int, error) { return 0, nil }

// ScanSourceUpgrades / SweepUpgrades: same story for the F8 quality scan — the
// join is SQL and the fingerprint arithmetic is measured against a real database
// (database/upgrades_test.go). Here it only has to exist.
func (m *memStore) ScanSourceUpgrades(context.Context, int64, int64) (int, error) { return 0, nil }
func (m *memStore) SweepUpgrades(context.Context) error                           { return nil }

func (m *memStore) ListClaimReports(context.Context) ([]*ClaimReport, error) { return nil, nil }

func (m *memStore) SetClaimReportDisposition(context.Context, int64, string) error { return nil }

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
	deadline := time.Now().Add(meshDeadline)
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
		// The requester's name is its own claim, so it lands in the heard name and
		// leaves the local label — which only an admin writes — empty.
		if p.HeardName != "node-b" || p.Name != "" {
			t.Fatalf("A recorded requester as name=%q heard=%q, want heard node-b and no label", p.Name, p.HeardName)
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

	// The naming split (migration 033). Before it, both names shared one column
	// and the backfill only ever ran while that column was empty, so a node that
	// renamed itself kept its old name here forever — and an admin's rename
	// destroyed the claim. Simulate the stale claim, add a local label on top, and
	// let one round of contact happen: the claim must refresh, the label must not
	// move, and the label must be what shows.
	pb2, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
	if err := storeA.UpdateFederationPeerHeardName(ctx, pb2.ID, "the name B used to use"); err != nil {
		t.Fatalf("seed stale heard name: %v", err)
	}
	if err := a.RenamePeer(ctx, pb2.ID, "B, my studio node"); err != nil {
		t.Fatalf("rename on A: %v", err)
	}
	waitFor(t, "A to refresh B's heard name from contact", func() bool {
		a.Nudge() // A pings its friends; B's reply carries B's own name
		p, err := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
		return err == nil && p.HeardName == "node-b"
	})
	if p, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex()); p.Name != "B, my studio node" {
		t.Errorf("local label after a heard-name refresh = %q, want it untouched", p.Name)
	} else if p.Label() != "B, my studio node" {
		t.Errorf("Label() = %q, want the local label to win", p.Label())
	}
	// Clearing the label falls back to what the peer calls itself, rather than to
	// nothing — which is why the two are stored apart.
	if err := a.RenamePeer(ctx, pb2.ID, ""); err != nil {
		t.Fatalf("clear label on A: %v", err)
	}
	if p, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex()); p.Label() != "node-b" {
		t.Errorf("Label() after clearing the label = %q, want the heard name node-b", p.Label())
	}

	// Block: A refuses B all service — even the ping.
	pa, _ := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
	if err := a.BlockPeer(ctx, pa.ID, "test block"); err != nil {
		t.Fatalf("block on A: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   meshClientTimeout,
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

	// A block also cuts the UNDERLAY link where that link is ours. B configured A
	// as a peer, so B is the side that can de-peer — and because the removal
	// cancels yggdrasil's retry, the link must stay gone rather than come back on
	// the next dial. (Left last: it takes the transport down.)
	hasUnderlay := func(n *Node, key string) bool {
		for _, info := range n.mesh.core.GetPeers() {
			if hex.EncodeToString(info.Key) == key {
				return true
			}
		}
		return false
	}
	if !hasUnderlay(b, a.PublicKeyHex()) {
		t.Fatal("B has no underlay link to A to begin with; the de-peer assertion would be vacuous")
	}
	pbOnB, _ := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex())
	if err := b.BlockPeer(ctx, pbOnB.ID, "underlay de-peer test"); err != nil {
		t.Fatalf("block on B: %v", err)
	}
	waitFor(t, "B to drop the underlay link to a blocked A", func() bool {
		b.Nudge()
		return !hasUnderlay(b, a.PublicKeyHex())
	})
	// Still gone a moment later: the retry was cancelled, not just interrupted.
	time.Sleep(500 * time.Millisecond)
	if hasUnderlay(b, a.PublicKeyHex()) {
		t.Error("the underlay link to a blocked node came back; RemovePeer must stop the retry")
	}
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
		Timeout:   meshClientTimeout,
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/pair", a.Address(), MeshPort)
	deadline := time.Now().Add(meshDeadline)
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
