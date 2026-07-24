//go:build !nofederation

package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Catalog exchange (federation F2): pull-and-cache between direct friends.
// The server side serves its full published catalog as a snapshot with a
// deterministic serial; the client sends the serial it already has and gets a
// tiny "unchanged" reply when nothing moved. True since-serial deltas are a
// later optimization — the wire format already carries the serial, so they can
// arrive without a protocol break (decision 2026-07-18, supersedes the
// original per-row-delta idea; see docs/architecture/federation.md §Catalog).

// The two cadences this file lives by are node fields (defaultIntervals in
// node.go, overridable via WithIntervals): Intervals.CatalogSync is how often
// the refresh loop re-pulls each friend's catalog — most rounds a cheap
// not-modified check — and Intervals.SnapshotTTL bounds how long a built
// snapshot is served before the store is consulted again, so back-to-back
// friend syncs don't rebuild the catalog per request.

// catalogMessage is the catalog reply (and the shape cached in memory):
// either Unchanged (the caller's serial still matches) or the full snapshot.
type catalogMessage struct {
	Protocol  int            `json:"protocol"`
	Serial    string         `json:"serial"`
	Unchanged bool           `json:"unchanged,omitempty"`
	Entries   []CatalogEntry `json:"entries,omitempty"`
}

// snapshot is the memoized own-catalog build.
type snapshot struct {
	serial  string
	entries []CatalogEntry
	built   time.Time
}

// ownSnapshot returns the current published catalog + serial, rebuilding at
// most every Intervals.SnapshotTTL.
func (n *Node) ownSnapshot(ctx context.Context) (*snapshot, error) {
	n.snapMu.Lock()
	defer n.snapMu.Unlock()
	if n.snap != nil && time.Since(n.snap.built) < n.intervals.SnapshotTTL {
		return n.snap, nil
	}
	entries, err := n.store.PublishedCatalog(ctx)
	if err != nil {
		return nil, err
	}
	n.snap = &snapshot{serial: CatalogSerial(entries), entries: entries, built: time.Now()}
	return n.snap, nil
}

// handleCatalog serves GET /madnetwork/v0/catalog?since=<serial> — friends
// only. meshAuth has already refused blocked peers; everyone else must resolve
// to a known friend (the catalog is the library listing — default-deny toward
// non-friends).
func (n *Node) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "catalog not configured", http.StatusServiceUnavailable)
		return
	}
	p := n.peerFromRemote(r)
	if p == nil || p.State != PeerFriend {
		http.Error(w, "catalog is served to friends only", http.StatusForbidden)
		return
	}
	snap, err := n.ownSnapshot(r.Context())
	if err != nil {
		n.logger.Printf("federation: build catalog snapshot: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	msg := catalogMessage{Protocol: ProtocolVersion, Serial: snap.serial}
	if r.URL.Query().Get("since") == snap.serial {
		msg.Unchanged = true
	} else {
		msg.Entries = snap.entries
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

// syncCatalog pulls one friend's catalog: a not-modified check most rounds, a
// full snapshot replace when their serial moved.
func (n *Node) syncCatalog(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/catalog?since=%s", addr, MeshPort, p.CatalogSerial)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return // unreachable — the refresh loop retries
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return // e.g. an older peer without the endpoint, or not-yet-friend on their side
	}
	var msg catalogMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Serial == "" {
		return
	}
	now := time.Now().Unix()
	if msg.Unchanged {
		if err := n.store.MarkPeerCatalogChecked(ctx, p.ID, msg.Serial, now); err != nil {
			n.logger.Printf("federation: mark catalog checked for %q: %v", p.Name, err)
		}
		return
	}
	if err := n.store.ReplacePeerCatalog(ctx, p.ID, msg.Serial, now, msg.Entries); err != nil {
		n.logger.Printf("federation: store catalog of %q: %v", p.Name, err)
		return
	}
	n.logger.Printf("federation: synced catalog of %q (%s) — %d entries", p.Name, p.PublicKey, len(msg.Entries))
}
