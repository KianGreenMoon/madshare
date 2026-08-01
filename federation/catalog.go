//go:build !nofederation

package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Catalog exchange (federation F2): pull-and-cache. Between direct friends when
// it was built; since F7 item 5 between any two nodes in one community.
// The server side serves its full published catalog as a snapshot with a
// deterministic serial; the client sends the serial it already has and gets a
// tiny "unchanged" reply when nothing moved. True since-serial deltas are a
// later optimization — the wire format already carries the serial, so they can
// arrive without a protocol break (decision 2026-07-18, supersedes the
// original per-row-delta idea; see docs/architecture/federation.md §Catalog).

// The two cadences this file lives by are node fields (defaultIntervals in
// node.go, overridable via WithIntervals): Intervals.CatalogSync is how often
// the refresh loop re-pulls each source's catalog — most rounds a cheap
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

// ownSnapshot returns the published catalog + serial for one audience,
// rebuilding at most every Intervals.SnapshotTTL. Each audience class carries
// its own memo and its own serial: a friend restricted to guest-playable content
// sees a different catalog, so it must not be told the full snapshot's serial —
// that would make its next not-modified check answer about a catalog it never
// received (F5, docs/architecture/federation.md §Sharing scope).
func (n *Node) ownSnapshot(ctx context.Context, aud Audience) (*snapshot, error) {
	n.snapMu.Lock()
	defer n.snapMu.Unlock()
	if snap := n.snaps[aud]; snap != nil && time.Since(snap.built) < n.intervals.SnapshotTTL {
		return snap, nil
	}
	entries, err := n.store.PublishedCatalog(ctx, aud)
	if err != nil {
		return nil, err
	}
	snap := &snapshot{serial: CatalogSerial(entries), entries: entries, built: time.Now()}
	if n.snaps == nil {
		n.snaps = map[Audience]*snapshot{}
	}
	n.snaps[aud] = snap
	return snap, nil
}

// handleCatalog serves GET /madnetwork/v0/catalog?since=<serial> — our
// community only (F7). A member pulls the Madnetwork-scoped listing, a direct
// friend its mapped one; a node outside the community is refused, and the guest
// switch does not open this — guests are answered bytes they can already name,
// never a listing of what we have.
//
// That one-line widening from friends to members is what makes other people's
// libraries visible at all: the blocker on reaching them was never authorization
// but knowing a hash exists (§Discovery beyond the friend ring). All members
// share one memoized snapshot and the same `since=` not-modified reply, so the
// cost does not scale with the community.
//
// meshAuth has already refused blocked peers.
func (n *Node) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "catalog not configured", http.StatusServiceUnavailable)
		return
	}
	aud, ok := n.serveAudience(r)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !aud.InCommunity() {
		http.Error(w, "catalog is served inside the madnetwork only", http.StatusForbidden)
		return
	}
	snap, err := n.ownSnapshot(r.Context(), aud)
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

// syncCatalog pulls one source's catalog: a not-modified check most rounds, a
// full snapshot replace when their serial moved. The source may be a friend or
// any member of our community (F7 item 5) — the wire call is the same either
// way, since the *serving* node decides what its answer contains.
func (n *Node) syncCatalog(ctx context.Context, p *CatalogSource) {
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
		// An older node without the endpoint, a friendship their side has not
		// recorded yet, or a member that does not count us as one of theirs —
		// all the same to us here: nothing to cache, try again next round.
		return
	}
	var msg catalogMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Serial == "" {
		return
	}
	now := time.Now().Unix()
	if msg.Unchanged {
		if err := n.store.MarkSourceCatalogChecked(ctx, p.ID, msg.Serial, now); err != nil {
			n.logger.Printf("federation: mark catalog checked for %q: %v", p.Display(), err)
		}
		n.checkClaims(ctx, p)
		n.scanUpgrades(ctx, p)
		return
	}
	if err := n.store.ReplaceSourceCatalog(ctx, p.ID, msg.Serial, now, msg.Entries); err != nil {
		n.logger.Printf("federation: store catalog of %q: %v", p.Display(), err)
		return
	}
	n.logger.Printf("federation: synced catalog of %q (%s) — %d entries", p.Display(), p.PublicKey, len(msg.Entries))
	n.checkClaims(ctx, p)
	n.scanUpgrades(ctx, p)
}

// checkClaims re-runs the contradiction checks over this source's cached catalog
// (F6). It runs on both sync paths, including the not-modified one: a peer's
// claims stand still while *our* library moves, and every upload or materialized
// download is a new blob those old claims can be checked against.
//
// A finding is logged once per round and otherwise waits on /admin/network.
// Nothing here blocks, scores or notifies — that is the whole point of the design
// (docs/architecture/federation.md §Trust graph).
func (n *Node) checkClaims(ctx context.Context, p *CatalogSource) {
	open, err := n.store.CheckSourceClaims(ctx, p.ID)
	if err != nil {
		n.logger.Printf("federation: check claims of %q: %v", p.Display(), err)
		return
	}
	if open > 0 {
		n.logger.Printf("federation: %d unreviewed contradicted claim(s) from %q (%s) — see /admin/network",
			open, p.Display(), p.PublicKey)
	}
}

// scanUpgrades looks for renditions this source holds that would beat ours
// (F8 item 3). It runs on both sync paths for exactly the reason checkClaims
// does — their catalog standing still says nothing about ours — and is bounded
// by a per-source watermark so the steady-state cost is the material that
// changed, not the size of either library.
//
// Like the claim checks, it decides nothing: findings wait on /admin/upgrades,
// and materializing one is an admin pressing a button.
func (n *Node) scanUpgrades(ctx context.Context, p *CatalogSource) {
	open, err := n.store.ScanSourceUpgrades(ctx, p.ID, time.Now().Unix())
	if err != nil {
		n.logger.Printf("federation: scan upgrades from %q: %v", p.Display(), err)
		return
	}
	if err := n.store.SweepUpgrades(ctx); err != nil {
		n.logger.Printf("federation: sweep upgrades: %v", err)
	}
	if open > 0 {
		n.logger.Printf("federation: %d better rendition(s) available on the madnetwork — see /admin/upgrades", open)
	}
}
