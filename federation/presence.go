//go:build !nofederation

package federation

// Presence — the 10-second rule (docs/ui/madnetwork-page.md §Presence). A
// dedicated prober pings every friend on a short cadence; a friend counts as
// online only while it answers, with hysteresis so a flapping link doesn't
// strobe the merged browse:
//
//   - online → offline: no successful ping for presenceOfflineAfter.
//   - offline → online: the first success starts probation; the friend flips
//     online only after staying reachable for presenceOnlineAfter.
//
// Presence is in-memory node state: on startup everyone begins offline and
// earns online status through probation. The browse queries consume it via
// the database presence provider (Node.OnlinePeerIDs / CachedHashes, wired in
// madshare.go).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	presenceInterval     = 5 * time.Second  // probe cadence
	presenceTimeout      = 3 * time.Second  // per-probe deadline
	presenceOfflineAfter = 10 * time.Second // silence → offline
	presenceOnlineAfter  = 10 * time.Second // probation before flipping online
)

// presenceTracker is the pure per-peer online state machine — probes feed it
// successes, readers ask "online now?". Failures are not recorded: offline
// detection is purely the silence gap, so one dropped probe inside the window
// doesn't flap the state.
type presenceTracker struct {
	mu    sync.Mutex
	peers map[int64]*peerPresence
}

type peerPresence struct {
	lastOK  time.Time // last successful probe
	okSince time.Time // start of the current uninterrupted success streak
}

func newPresenceTracker() *presenceTracker {
	return &presenceTracker{peers: map[int64]*peerPresence{}}
}

// ObserveSuccess records a successful probe. A success after an outage gap
// (longer than presenceOfflineAfter) restarts probation.
func (t *presenceTracker) ObserveSuccess(peerID int64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.peers[peerID]
	if p == nil {
		p = &peerPresence{}
		t.peers[peerID] = p
	}
	if p.okSince.IsZero() || now.Sub(p.lastOK) > presenceOfflineAfter {
		p.okSince = now
	}
	p.lastOK = now
}

// RecentlySeen reports whether the peer produced a successful contact within
// `within` of `now` — a ping OR (via ObserveSuccess from the swarm) a delivered
// chunk. The prober uses it to skip a peer we are already exchanging bytes
// with, so presence probing never opens a connection that competes with an
// active transfer over the same mesh path.
func (t *presenceTracker) RecentlySeen(peerID int64, now time.Time, within time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.peers[peerID]
	return p != nil && !p.lastOK.IsZero() && now.Sub(p.lastOK) <= within
}

// Online reports whether the peer counts as online at `now`: last heard from
// within the offline window AND the success streak has outlived probation.
func (t *presenceTracker) Online(peerID int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.peers[peerID]
	if p == nil || p.okSince.IsZero() {
		return false
	}
	return now.Sub(p.lastOK) <= presenceOfflineAfter && now.Sub(p.okSince) >= presenceOnlineAfter
}

// OnlineIDs returns every peer currently online.
func (t *presenceTracker) OnlineIDs(now time.Time) []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []int64{}
	for id, p := range t.peers {
		if !p.okSince.IsZero() &&
			now.Sub(p.lastOK) <= presenceOfflineAfter && now.Sub(p.okSince) >= presenceOnlineAfter {
			out = append(out, id)
		}
	}
	return out
}

// Forget drops state for peers not in keep (unfriended/blocked meanwhile).
func (t *presenceTracker) Forget(keep map[int64]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.peers {
		if !keep[id] {
			delete(t.peers, id)
		}
	}
}

// presenceLoop drives the prober until the node stops.
func (n *Node) presenceLoop(ctx context.Context) {
	defer close(n.presenceDone)
	ticker := time.NewTicker(presenceInterval)
	defer ticker.Stop()
	for {
		n.probeFriends(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeFriends pings every friend in parallel (bounded by presenceTimeout —
// mesh pings are one GET inside an established session, so a small friend set
// at this cadence is negligible) and feeds the tracker. It does NOT touch
// last_seen — the minute refresh loop owns the persisted timestamp; presence
// is deliberately volatile.
func (n *Node) probeFriends(ctx context.Context) {
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		return
	}
	keep := map[int64]bool{}
	var wg sync.WaitGroup
	for _, p := range peers {
		if p.State != PeerFriend {
			continue
		}
		keep[p.ID] = true
		// An active transfer from this peer already proves it is online
		// (fetchSwarm feeds ObserveSuccess on every delivered chunk). Skip the
		// ping so presence probing never contends with the transfer for the
		// mesh path — the interference the 5 s cadence would otherwise add on
		// top of an in-flight download.
		if n.presence.RecentlySeen(p.ID, time.Now(), presenceInterval) {
			continue
		}
		wg.Add(1)
		go func(p *Peer) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, presenceTimeout)
			defer cancel()
			if n.probePeer(pctx, p) {
				n.presence.ObserveSuccess(p.ID, time.Now())
			}
		}(p)
	}
	wg.Wait()
	n.presence.Forget(keep)
}

// probePeer runs one protocol ping; true on a 200.
func (n *Node) probePeer(ctx context.Context, p *Peer) bool {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return false
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return false
	}
	// Drain before closing so net/http can return the connection to the idle
	// pool and REUSE it on the next probe. Without this the /ping JSON body is
	// left unread, the Transport discards the connection, and every probe opens
	// a fresh mesh TCP connection — at the 5 s cadence that is a heavy
	// connection-churn load on the gVisor netstack (its inbound path is a
	// single point of failure — see .issues/open-issues.md), which competes
	// with and can stall in-flight blob transfers. Keep-alive reuse makes the
	// prober ride one persistent connection per friend instead.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// OnlinePeerIDs returns the friends currently online — the presence half of
// the browse visibility rule. Empty until the prober's probation has passed.
func (n *Node) OnlinePeerIDs() []int64 {
	if n.presence == nil {
		return nil
	}
	return n.presence.OnlineIDs(time.Now())
}

// CachedHashes returns the finished blobs in the download cache — the "fully
// cached, playable regardless of who is online" exception to the visibility
// rule (same set the holdings endpoint advertises).
func (n *Node) CachedHashes() []string {
	return n.cacheHoldings()
}
