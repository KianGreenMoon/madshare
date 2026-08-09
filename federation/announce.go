//go:build !nofederation

package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// Holdings announce (F9 item 2, docs/architecture/federation.md §Distribution,
// "Making it a swarm").
//
// Holdings were discoverable only by being pulled from, on the catalog cadence —
// fifteen minutes. A swarm's peer set is ephemeral by nature and that made our
// tracker structurally too slow for it: fetch a track, go offline in ten minutes,
// and nobody ever learned you held it. Worse for item 1, whose whole point is
// seeding DURING a fetch: a partial holder discovered a quarter of an hour later
// has long since finished.
//
// This is the push half. Three things about its shape are deliberate:
//
//   - **It is not gossip, and it is never relayed.** The design sketch called it
//     gossip by analogy with the freshness hints, but those ride the ping and
//     these must not travel second-hand at all (see the minting rule below). What
//     is left when a record may not be relayed is a direct push, so that is what
//     this is — and it rides the same one-minute refresh round the pings do,
//     rather than opening a cadence of its own.
//   - **An announce may MINT a holdings row, where a freshness hint may not.**
//     The rule looks inconsistent and is not: a hint is about a THIRD party, so
//     accepting one would let hearsay claim something only first-hand contact
//     may, while an announce is a node speaking about ITSELF. That is also
//     exactly why it must never be relayed — relayed, it becomes hearsay and the
//     permission has to flip back. The receiver enforces this by attributing the
//     announce to the connection's own key, never to anything in the body.
//   - **It carries additions only; the fifteen-minute pull stays as the
//     correcting sweep.** An increment cannot express a removal, so a blob
//     evicted from a peer's cache lingers in our index until the next full sync.
//     That asymmetry is the right way round: a stale positive costs one fast 404
//     from a live node, while a holder we never heard of costs the swarm a source
//     entirely.
const (
	// maxAnnounceHashes bounds one announce, in both directions. A node fetching
	// hard still only completes so many blobs a minute, so this is a guard
	// against a peer that talks nonsense rather than a limit real traffic meets.
	maxAnnounceHashes = 512
	// maxAnnounceBytes bounds the body we will decode.
	maxAnnounceBytes = 1 << 20
)

// announceMessage is the body of POST /madnetwork/v0/announce: what the sender
// has newly acquired. Shaped like holdingsMessage on purpose — the same two
// promises, complete blobs and partial ones — but the semantics differ and the
// types are kept apart for it: holdings is a COMPLETE statement, this is an
// INCREMENT.
type announceMessage struct {
	Protocol int      `json:"protocol"`
	Hashes   []string `json:"hashes,omitempty"`
	Partial  []string `json:"partial,omitempty"`
}

func (m *announceMessage) empty() bool { return len(m.Hashes) == 0 && len(m.Partial) == 0 }

// noteAcquired records that a fetch has just landed a whole blob, for the next
// refresh round to announce. Only completions need recording: what we hold
// PARTIALLY is read live from the transfer table at announce time, and what we
// hold whole is already the whole cache directory — announcing all of that every
// minute is the traffic this exists to avoid.
func (n *Node) noteAcquired(hash string) {
	if !isBlobHash(hash) {
		return
	}
	n.announceMu.Lock()
	defer n.announceMu.Unlock()
	if n.announceNew == nil {
		n.announceNew = map[string]bool{}
	}
	if len(n.announceNew) < maxAnnounceHashes {
		n.announceNew[hash] = true
	}
}

// drainAnnounce takes the pending completions and pairs them with whatever this
// node currently holds partially.
//
// The pending set is CLEARED whether or not the sends that follow succeed. An
// announce is an optimisation over a sync that is still running, so losing one to
// an offline moment costs at most the fifteen minutes it would have saved; a
// retry queue would be state to age, bound and reason about for that.
func (n *Node) drainAnnounce() announceMessage {
	n.announceMu.Lock()
	fresh := make([]string, 0, len(n.announceNew))
	for h := range n.announceNew {
		fresh = append(fresh, h)
	}
	n.announceNew = nil
	n.announceMu.Unlock()

	sort.Strings(fresh) // stable, so a repeated announce reads the same
	return announceMessage{
		Protocol: ProtocolVersion,
		Hashes:   fresh,
		Partial:  n.partialHoldings(fresh),
	}
}

// announceHoldings pushes what we have just acquired to every direct friend,
// once per refresh round.
//
// Friends only. A member learns from the holdings pull as before: pushing to the
// wider community would mean dialling nodes the frontier rotation deliberately
// budgets, and the reach a push buys past the first hop is somebody else's
// announce to make, not ours to relay.
//
// Nothing is announced when seeding is off, because the endpoint would refuse to
// serve any of it. Advertising what we will not hand over is the one rule
// §Sharing scope holds above the others.
func (n *Node) announceHoldings(ctx context.Context, peers []*Peer) {
	if n.store == nil {
		return
	}
	friends := make([]*Peer, 0, len(peers))
	for _, p := range peers {
		if p.State == PeerFriend {
			friends = append(friends, p)
		}
	}
	if len(friends) == 0 {
		return // nobody to tell; keep the pending set for a round where there is
	}
	policy, err := n.store.SeedingPolicy(ctx)
	if err != nil || !policy.Enabled || !policy.Cache {
		return
	}
	msg := n.drainAnnounce()
	if msg.empty() {
		return
	}
	for _, p := range friends {
		if ctx.Err() != nil {
			return
		}
		n.announceTo(ctx, p, msg)
	}
}

// announceTo delivers one announce. Failures are silent: the friend will pull our
// holdings on its own cadence regardless, so a missed push costs latency, never
// correctness.
func (n *Node) announceTo(ctx context.Context, p *Peer, msg announceMessage) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/announce", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return // unreachable, or too old to know the endpoint
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
}

// handleAnnounce serves POST /madnetwork/v0/announce.
//
// The sender is identified by its CONNECTION, never by anything in the body —
// that is what makes an announce first-hand and therefore allowed to mint a
// holdings row. A requester we cannot name (a guest, a capability-token bearer)
// is refused for the same reason: an unattributable announce is a claim about
// nobody. A listener node's holdings have their own path, the household tracker
// at POST /api/madnetwork/holdings, which is scoped to the device's own account.
func (n *Node) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	aud, key, ok := n.serveAudienceKey(r)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// Same audience as the holdings listing this feeds: our community, and a plain
	// 403 rather than a 404, because the request names no hash of ours to confirm.
	if !aud.InCommunity() || key == "" {
		http.Error(w, "announcements are accepted inside the madnetwork only", http.StatusForbidden)
		return
	}
	var msg announceMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAnnounceBytes)).Decode(&msg); err != nil {
		http.Error(w, "malformed announce", http.StatusBadRequest)
		return
	}
	valid := make([]string, 0, len(msg.Hashes)+len(msg.Partial))
	seen := make(map[string]bool, cap(valid))
	for _, h := range append(append([]string(nil), msg.Hashes...), msg.Partial...) {
		if len(valid) >= maxAnnounceHashes {
			break
		}
		if isBlobHash(h) && !seen[h] {
			seen[h] = true
			valid = append(valid, h)
		}
	}
	if len(valid) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	now := time.Now().Unix()
	src, err := n.store.EnsureCatalogSource(r.Context(), key, now)
	if err != nil {
		n.logger.Printf("federation: announce from %s: %v", key[:8], err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// EnsureCatalogSource sets first_seen and NOT last_seen, so a source minted
	// here would be stale on arrival and filtered straight back out by
	// StaleHolderWindow — the holder would be recorded and never used. Touching it
	// is what makes the announce mean anything.
	if err := n.store.TouchCatalogSourceSeen(r.Context(), src.ID, now, ""); err != nil {
		n.logger.Printf("federation: touch announcing source %s: %v", key[:8], err)
	}
	if err := n.store.AddSourceHoldings(r.Context(), src.ID, valid); err != nil {
		n.logger.Printf("federation: store announce from %s: %v", key[:8], err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
