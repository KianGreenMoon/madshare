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

	"daemonlord.ygg/madshare/config"
)

// resetSync ages one node's cached catalog so the next sweep re-pulls it,
// addressed by node key — sync state lives on the source row since F7 item 5.
func (m *memStore) resetSync(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sourceByKeyLocked(key); s != nil {
		s.CatalogSyncedAt, s.AttemptedAt = 0, 0
	}
}

// syncState returns one node's cached serial and last confirmed-fresh time.
func (m *memStore) syncState(key string) (string, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sourceByKeyLocked(key); s != nil {
		return s.CatalogSerial, s.CatalogSyncedAt
	}
	return "", 0
}

// TestCatalogSync walks the F2 pull-and-cache flow between two embedded nodes:
// the catalog is refused before friendship, pulled automatically once the
// nodes are friends, and re-checked cheaply via the not-modified path.
func TestCatalogSync(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{
		{Key: "1", RecordingKey: "r1", Title: "Song One", Artist: "A", Album: "One",
			Renditions: []CatalogRendition{{Hash: "h1", Size: 10}}},
		{Key: "2", RecordingKey: "r2", Title: "Song Two", Artist: "A", Album: "One",
			Renditions: []CatalogRendition{{Hash: "h2", Size: 20}}},
	})

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
	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   meshClientTimeout,
	}
	catalogURL := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/catalog", a.Address(), MeshPort)

	// Before friendship, the catalog is default-deny: an unknown node gets 403.
	waitFor(t, "pre-friendship catalog refusal", func() bool {
		resp, err := client.Get(catalogURL)
		if err != nil {
			return false // mesh still converging
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("pre-friendship catalog = %d, want 403", resp.StatusCode)
		}
		return true
	})

	// Friend the nodes (B imports A's card; A accepts B's request).
	if _, err := b.ImportCard(ctx, a.Info().Card); err != nil {
		t.Fatalf("import card on B: %v", err)
	}
	var incomingID int64
	waitFor(t, "A to see B's pairing request", func() bool {
		b.Nudge()
		p, err := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
		if err != nil {
			return false
		}
		incomingID = p.ID
		return p.State == PeerPendingIncoming
	})
	if err := a.AcceptPeer(ctx, incomingID); err != nil {
		t.Fatalf("accept on A: %v", err)
	}

	// The first sync follows the friendship on its own (nudged sweeps).
	var peerAonB *Peer
	waitFor(t, "B to pull A's catalog", func() bool {
		b.Nudge()
		p, err := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex())
		if err != nil || p.State != PeerFriend {
			return false
		}
		if serial, _ := storeB.syncState(a.PublicKeyHex()); serial == "" {
			return false
		}
		peerAonB = p
		return len(storeB.cachedCatalog(p.ID)) == 2
	})
	cached := storeB.cachedCatalog(peerAonB.ID)
	if cached[0].Title != "Song One" || cached[1].Renditions[0].Hash != "h2" {
		t.Errorf("cached catalog = %+v, want A's two entries", cached)
	}
	wantSerial := CatalogSerial(mustPublished(t, storeA))
	if serial, _ := storeB.syncState(a.PublicKeyHex()); serial != wantSerial {
		t.Errorf("stored serial = %q, want the snapshot serial %q", serial, wantSerial)
	}

	// A due re-sync with an unchanged catalog takes the not-modified path:
	// synced_at moves, the cache stays.
	storeB.resetSync(a.PublicKeyHex())
	waitFor(t, "the not-modified re-check", func() bool {
		b.Nudge()
		_, syncedAt := storeB.syncState(a.PublicKeyHex())
		return syncedAt > 0
	})
	if got := storeB.cachedCatalog(peerAonB.ID); len(got) != 2 {
		t.Errorf("cache after not-modified check = %d entries, want 2 (untouched)", len(got))
	}
}

func mustPublished(t *testing.T, s *memStore) []CatalogEntry {
	t.Helper()
	entries, err := s.PublishedCatalog(context.Background(), FriendAudience)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
