//go:build !nofederation

package federation

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

// Friendship (federation F1): the pairing handshake, the mesh-side peer
// identity check, and the refresh loop that retries pending pairings and keeps
// friends' last_seen fresh. Design: docs/architecture/federation-trust.md §Trust
// graph and §Build plan F1.
//
// The handshake needs no signatures: a yggdrasil mesh address is derived from
// the node key, so the source address of a mesh connection *is* proof of key
// possession (self-certifying channel). A pair request additionally carries the
// full public key, which must derive to exactly the request's source address.
//
// Both directions converge through retries: whichever side is behind flips to
// friend the next time either node's pair call reaches the other. Deliberate
// friending is preserved — a node becomes a friend only after BOTH admins acted
// (imported the other's card, or explicitly accepted an incoming request).

// AddrForKeyHex derives the mesh IPv6 address (200::/7) from a lowercase-hex
// ed25519 public key.
func AddrForKeyHex(hexKey string) (net.IP, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("federation: bad public key: %w", err)
	}
	addr := address.AddrForKey(raw)
	if addr == nil {
		return nil, fmt.Errorf("federation: key derives no mesh address")
	}
	return net.IP(addr[:]), nil
}

// Info returns the node's own identity: name, mesh address, public key, and
// the node card an admin exports to friends.
func (n *Node) Info() NodeInfo {
	key := n.PublicKeyHex()
	return NodeInfo{
		Name:      n.name,
		Address:   n.Address().String(),
		PublicKey: key,
		Card:      Card{Version: ProtocolVersion, Name: n.name, PublicKey: key},
	}
}

// ── Mesh-side identity ───────────────────────────────────────────────────────

// peerFromRemote resolves the request's source mesh address to a known peer by
// deriving each stored key's address (derivation is one-way, so matching walks
// the table — fine at friend-list scale). nil = unknown node.
func (n *Node) peerFromRemote(r *http.Request) *Peer {
	if n.store == nil {
		return nil
	}
	ip := remoteIP(r)
	if ip == nil {
		return nil
	}
	peers, err := n.store.ListFederationPeers(r.Context())
	if err != nil {
		n.logger.Printf("federation: resolve peer: %v", err)
		return nil
	}
	return matchPeerAddr(peers, ip)
}

// remoteIP is a mesh request's source address — self-certifying, since a
// yggdrasil address derives from the node key.
func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// matchPeerAddr finds the peer whose key derives to ip. Split out of
// peerFromRemote so a handler that has already read the peer table can match
// against the list it holds instead of reading it again.
func matchPeerAddr(peers []*Peer, ip net.IP) *Peer {
	if ip == nil {
		return nil
	}
	for _, p := range peers {
		if addr, err := AddrForKeyHex(p.PublicKey); err == nil && addr.Equal(ip) {
			return p
		}
	}
	return nil
}

// meshAuth wraps the whole protocol surface: a blocked peer is refused
// everything (the block cuts all application-layer service), and any request
// from a known peer refreshes its last_seen.
func (n *Node) meshAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := n.peerFromRemote(r); p != nil {
			if p.State == PeerBlocked {
				http.Error(w, "blocked", http.StatusForbidden)
				return
			}
			if err := n.store.TouchFederationPeerSeen(r.Context(), p.ID, time.Now().Unix()); err != nil {
				n.logger.Printf("federation: touch peer %d: %v", p.ID, err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ── The pairing handshake ────────────────────────────────────────────────────

// pairMessage is both the request a node sends to introduce itself and (with
// Result set) the response. Result: "friend" when the receiving side considers
// the friendship mutual, "pending" while its admin has not yet acted.
type pairMessage struct {
	Protocol  int    `json:"protocol"`
	Name      string `json:"name,omitempty"`
	PublicKey string `json:"public_key"`
	Result    string `json:"result,omitempty"`
}

// handlePair serves POST /madnetwork/v0/pair. State transitions on the
// receiving side:
//
//	unknown key      → insert pending_incoming (awaits admin accept), "pending"
//	pending_incoming → still awaiting our admin, "pending"
//	pending_outgoing → mutual intent proven (we imported their card and they
//	                   contacted us) → friend, "friend"
//	friend           → idempotent, "friend"
//
// (blocked never reaches here — meshAuth refuses it first.)
func (n *Node) handlePair(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "friendship not configured", http.StatusServiceUnavailable)
		return
	}
	var req pairMessage
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid pair request", http.StatusBadRequest)
		return
	}
	if req.Protocol != ProtocolVersion {
		http.Error(w, fmt.Sprintf("protocol %d unsupported (this node speaks %d)", req.Protocol, ProtocolVersion), http.StatusConflict)
		return
	}
	key, err := NormalizeKey(req.PublicKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if key == n.PublicKeyHex() {
		http.Error(w, "cannot pair with self", http.StatusBadRequest)
		return
	}
	// The self-certifying check: the claimed key must derive to exactly the
	// address this request arrived from, otherwise anyone on the mesh could
	// impersonate a key they don't hold.
	claimed, err := AddrForKeyHex(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if remote := net.ParseIP(host); remote == nil || !claimed.Equal(remote) {
		http.Error(w, "source address does not match claimed key", http.StatusForbidden)
		return
	}

	result := ""
	p, err := n.store.GetFederationPeerByKey(r.Context(), key)
	switch {
	case err == ErrPeerNotFound:
		_, err := n.store.InsertFederationPeer(r.Context(), &Peer{
			PublicKey: key,
			HeardName: CleanPeerName(req.Name),
			State:     PeerPendingIncoming,
			CreatedAt: time.Now().Unix(),
			LastSeen:  time.Now().Unix(),
		})
		if err != nil {
			n.logger.Printf("federation: record pair request from %s: %v", key, err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		n.logger.Printf("federation: pairing request from %q (%s) — awaiting accept on /admin/network", CleanPeerName(req.Name), key)
		result = "pending"
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	case p.State == PeerPendingOutgoing:
		if err := n.store.SetFederationPeerState(r.Context(), p.ID, PeerFriend, ""); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		n.logger.Printf("federation: friendship with %q (%s) established", p.Label(), p.PublicKey)
		n.Nudge() // start the first catalog sync right away
		result = "friend"
	case p.State == PeerFriend:
		result = "friend"
	default: // pending_incoming — their retry while our admin hasn't acted
		result = "pending"
	}
	// The request carries the peer's own name. For a peer we already know that is
	// a refreshed claim (the insert above stored it for a new one) — a node that
	// renames itself is heard on its next contact rather than never.
	if p != nil {
		n.refreshHeardName(r.Context(), p, req.Name)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pairMessage{
		Protocol:  ProtocolVersion,
		Name:      n.name,
		PublicKey: n.PublicKeyHex(),
		Result:    result,
	})
}

// pairWith performs one outbound pairing attempt toward a pending_outgoing (or
// just-accepted) peer and applies the response to our side of the state machine.
//
// Every exit records a [PairAttempt]: a pairing that does not converge is the
// one federation failure an admin cannot debug from the outside, since both
// halves of it look identical from the peer list (`pending_outgoing`, forever).
func (n *Node) pairWith(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		n.logger.Printf("federation: peer %d has an invalid key: %v", p.ID, err)
		n.recordAttempt(p, PairAttempt{Error: "this node's stored key is not a valid ed25519 key"})
		return
	}
	body, _ := json.Marshal(pairMessage{
		Protocol:  ProtocolVersion,
		Name:      n.name,
		PublicKey: n.PublicKeyHex(),
	})
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/pair", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// A cancelled sweep is not a failed attempt — it is no attempt.
		if ctx.Err() == nil {
			n.recordAttempt(p, PairAttempt{Error: "could not reach this node on the mesh: " + dialReason(err)})
		}
		return // unreachable — the refresh loop retries
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		n.recordAttempt(p, PairAttempt{Error: refusalReason(resp)})
		return
	}
	var msg pairMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		n.recordAttempt(p, PairAttempt{Error: "this node answered something that is not a pairing reply"})
		return
	}
	if msg.Result == "" {
		n.recordAttempt(p, PairAttempt{Error: "this node answered without saying where the pairing stands"})
	} else {
		n.recordAttempt(p, PairAttempt{Result: msg.Result})
	}
	_ = n.store.TouchFederationPeerSeen(ctx, p.ID, time.Now().Unix())
	if msg.Result == "friend" && p.State == PeerPendingOutgoing {
		if err := n.store.SetFederationPeerState(ctx, p.ID, PeerFriend, ""); err != nil {
			n.logger.Printf("federation: record friendship with %s: %v", p.PublicKey, err)
			return
		}
		n.logger.Printf("federation: friendship with %q (%s) established", p.Label(), p.PublicKey)
		n.Nudge() // start the first catalog sync on the next sweep
	}
	n.refreshHeardName(ctx, p, msg.Name)
}

// recordAttempt stores the outcome of one outbound pairing attempt and logs it
// **when it changes**. The sweep retries every minute per pending peer, so a
// node that is merely switched off would otherwise write a log line a minute
// for as long as its admin leaves the pairing in place.
func (n *Node) recordAttempt(p *Peer, a PairAttempt) {
	a.At = time.Now().Unix()
	n.attemptMu.Lock()
	prev, had := n.attempts[p.PublicKey]
	n.attempts[p.PublicKey] = a
	n.attemptMu.Unlock()
	if had && prev.Result == a.Result && prev.Error == a.Error {
		return
	}
	switch {
	case a.Error != "":
		n.logger.Printf("federation: pairing with %q (%s): %s", p.Display(), p.PublicKey, a.Error)
	case a.Result == "pending":
		// The single most useful line in this file for someone whose pairing
		// "does not work": it did work, and the ball is on the other side.
		n.logger.Printf("federation: pairing request delivered to %q (%s) — waiting for their admin to accept it",
			p.Display(), p.PublicKey)
	}
}

// lastAttempt returns the recorded outcome of the last pairing attempt toward a
// key, if this process made one.
func (n *Node) lastAttempt(key string) (PairAttempt, bool) {
	n.attemptMu.Lock()
	defer n.attemptMu.Unlock()
	a, ok := n.attempts[key]
	return a, ok
}

// dialReason turns the transport error into something an admin can act on.
//
// The timeout case is named rather than reported, because it is both the
// commonest outcome and the least readable one: `net/http` renders it as
// `Post "http://[200:…]:1314/…": context deadline exceeded (Client.Timeout
// exceeded while awaiting headers)`, which says nothing a person can do
// anything with. What it actually means here is that nothing answered on the
// mesh — the node is off, or has federation disabled.
func dialReason(err error) string {
	if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return "nothing answered (the node is offline, or its madnetwork node is not running)"
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		err = uerr.Err
	}
	return err.Error()
}

// refusalReason turns a non-200 pairing reply into a sentence. The body is the
// far node's own words (handlePair answers in plain text), so it carries the
// specifics — a protocol mismatch, or the self-certifying check having failed —
// and is worth far more than the status alone.
func refusalReason(resp *http.Response) string {
	msg := fmt.Sprintf("this node refused the pairing request (HTTP %d)", resp.StatusCode)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if detail := strings.TrimSpace(string(body)); detail != "" {
		msg += ": " + CleanMarkReason(detail)
	}
	return msg
}

// refreshHeardName records what a peer just called itself, if that differs from
// what we last heard. Writes nothing when it is unchanged, which is the normal
// case — this runs on every ping, once a minute per friend.
//
// It cannot touch the local label (migration 033): a peer renaming itself must
// never overwrite the admin's choice, and an admin renaming a peer must never
// hide what that peer calls itself.
func (n *Node) refreshHeardName(ctx context.Context, p *Peer, heard string) {
	heard = CleanPeerName(heard)
	if heard == "" || heard == p.HeardName {
		return
	}
	if err := n.store.UpdateFederationPeerHeardName(ctx, p.ID, heard); err != nil {
		n.logger.Printf("federation: record heard name for %s: %v", p.PublicKey, err)
		return
	}
	p.HeardName = heard
}

// pingPeer refreshes a friend's last_seen with a protocol ping, and takes what
// else the reply carries while it is there: the peer's own name, and the
// freshness hints that are this loop's other job since F7 item 10 — what our
// friend saw of ITS friends, which is how a member two hops out stays inside the
// one-minute window instead of waiting fifteen for its turn in the catalog
// rotation (freshness.go).
func (n *Node) pingPeer(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping?hints=1", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	_ = n.store.TouchFederationPeerSeen(ctx, p.ID, time.Now().Unix())
	// Both extras are additive: a pre-033 peer omits the name, a peer older than
	// F7 item 10 omits the hints and simply ignored the query parameter. A
	// missing name leaves the last claim standing; missing hints leave that
	// friend's friends on the pull clock, which is what they were on before.
	var reply struct {
		Name  string           `json:"name"`
		Hints map[string]int64 `json:"hints"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPingReplyBytes)).Decode(&reply); err != nil {
		return
	}
	n.refreshHeardName(ctx, p, reply.Name)
	n.applyFreshnessHints(ctx, p, reply.Hints)
}

// maxPingReplyBytes bounds a ping reply. Generous next to the handful of fields
// it used to be, because MaxFreshnessHints keys and ages fit inside it — the
// bound is on the reply, and the hint list has its own.
const maxPingReplyBytes = 64 << 10

// refreshLoop periodically retries outbound pairings and pings friends; Nudge
// wakes it early after an import/accept so the admin sees the result promptly.
func (n *Node) refreshLoop(ctx context.Context) {
	defer close(n.loopDone)
	ticker := time.NewTicker(n.intervals.Refresh)
	defer ticker.Stop()
	for {
		n.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.nudge:
		}
	}
}

// sweep runs one round: pair toward every pending_outgoing peer, ping every
// friend, then pull catalogs — from the friends and, since F7 item 5, from a
// bounded slice of the wider community (discovery.go). Sequential: peer lists
// are small, the frontier is budgeted, and each call is bounded by the client
// timeout.
func (n *Node) sweep(ctx context.Context) {
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		n.logger.Printf("federation: refresh: %v", err)
		return
	}
	// Gossip (F6) is maintenance of our own view, not per-peer work: publish our
	// record when the friend list moved or the heartbeat came due, then age the
	// store on both its clocks — expiry, and the reachability walk that collects
	// what a block or a removal orphaned. All of it runs before the loop so a
	// friendship accepted this round is already in the record we serve during it,
	// and a branch we just cut is gone before we offer anyone a digest.
	//
	// expireGraph takes no peer list: it deletes, so it re-reads one of its own
	// rather than acting on this one, which is by then a round old (gossip_node.go).
	n.publishOwnRecord(ctx, peers)
	n.publishOwnMarkRecord(ctx, peers)
	n.expireGraph(ctx)
	n.depeerBlocked(peers)

	// The Rescan button (ResyncGraph). Consumed once per sweep, so presses during
	// a round fold into the next one instead of queueing rounds.
	forceGraph := n.forceGraph.Swap(false)

	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		switch p.State {
		case PeerPendingOutgoing:
			n.pairWith(ctx, p)
		case PeerFriend:
			n.pingPeer(ctx, p)
		}
	}

	// Tell our friends what we have just acquired (F9 item 2). Before the pulls
	// below, because it is the half that is time-critical: a blob we finished
	// seconds ago is seedable NOW, and the alternative is that nobody learns until
	// somebody's fifteen-minute holdings sync comes round.
	n.announceHoldings(ctx, peers)

	// Catalogs and holdings — friends unbudgeted, the community a few nodes per
	// round (F7 item 5). It runs after the pings so a friendship that converged
	// this round is pulled from in the same one.
	dueFriends := n.syncSources(ctx, peers)

	// Gossip rides the catalog cadence, except when an admin asked for it now —
	// and then it is the graph alone, never the catalog. Friends only: a record
	// is relayed by the nodes that vouched for us, and widening that is not what
	// discovery does.
	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		if p.State != PeerFriend {
			continue
		}
		if _, due := dueFriends[p.PublicKey]; due || forceGraph {
			n.syncGraph(ctx, p)
		}
	}
}

// Nudge wakes the refresh loop without waiting for the next tick.
func (n *Node) Nudge() {
	select {
	case n.nudge <- struct{}{}:
	default:
	}
}

// ── Admin-facing operations (the /api/admin/federation surface) ──────────────

// Peers lists the trusted-peer table with derived mesh addresses filled in.
func (n *Node) Peers(ctx context.Context) ([]*Peer, error) {
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range peers {
		if addr, err := AddrForKeyHex(p.PublicKey); err == nil {
			p.Address = addr.String()
		}
		if a, ok := n.lastAttempt(p.PublicKey); ok {
			p.LastAttempt = &a
		}
	}
	return peers, nil
}

// ImportKey is [Node.ImportCard] for a node whose *key* an admin has but whose
// card they do not — the node they just found on the network map, which carries
// every key it draws. Identity is the key, so a card adds nothing here except a
// claimed name, and name is stored as exactly that: a claim the handshake will
// refresh (it may be empty).
//
// Friending stays deliberate: this is our half of the mutual intent, and the far
// node still records a `pending_incoming` request its admin has to accept.
func (n *Node) ImportKey(ctx context.Context, publicKey, name string) (*Peer, error) {
	key, err := NormalizeKey(publicKey)
	if err != nil {
		return nil, err
	}
	return n.ImportCard(ctx, Card{Version: ProtocolVersion, Name: CleanPeerName(name), PublicKey: key})
}

// ImportCard records a friend's node card. A new key becomes pending_outgoing
// (our half of the mutual intent) and the handshake starts immediately; a card
// for a node that already asked to pair (pending_incoming) completes the
// friendship — importing the card IS the deliberate accept. Idempotent for
// already-known peers; a blocked node's card is refused.
func (n *Node) ImportCard(ctx context.Context, c Card) (*Peer, error) {
	if c.PublicKey == n.PublicKeyHex() {
		return nil, fmt.Errorf("%w: this is this node's own card", ErrPeerState)
	}
	p, err := n.store.GetFederationPeerByKey(ctx, c.PublicKey)
	switch {
	case err == ErrPeerNotFound:
		id, err := n.store.InsertFederationPeer(ctx, &Peer{
			PublicKey: c.PublicKey,
			// A card's name is what that node calls itself, not a label this
			// admin chose — so it lands in the claim, where the handshake keeps
			// it current.
			HeardName: c.Name,
			State:     PeerPendingOutgoing,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return nil, err
		}
		n.Nudge()
		return n.store.GetFederationPeer(ctx, id)
	case err != nil:
		return nil, err
	case p.State == PeerBlocked:
		return nil, fmt.Errorf("%w: this node is blocked — unblock it first", ErrPeerState)
	case p.State == PeerPendingIncoming:
		if err := n.store.SetFederationPeerState(ctx, p.ID, PeerFriend, ""); err != nil {
			return nil, err
		}
		n.logger.Printf("federation: friendship with %q (%s) established (card import accepted their request)", p.Label(), p.PublicKey)
		n.Nudge()
		return n.store.GetFederationPeer(ctx, p.ID)
	default: // pending_outgoing or friend — idempotent re-import
		return p, nil
	}
}

// AcceptPeer approves a pending_incoming pairing request. The admin is expected
// to have verified the key against the card received out-of-band (the UI shows
// it in full).
func (n *Node) AcceptPeer(ctx context.Context, id int64) error {
	p, err := n.store.GetFederationPeer(ctx, id)
	if err != nil {
		return err
	}
	if p.State != PeerPendingIncoming {
		return fmt.Errorf("%w: peer is %s, not awaiting acceptance", ErrPeerState, p.State)
	}
	if err := n.store.SetFederationPeerState(ctx, id, PeerFriend, ""); err != nil {
		return err
	}
	n.logger.Printf("federation: friendship with %q (%s) established (request accepted)", p.Label(), p.PublicKey)
	n.Nudge() // tell their node right away so its side flips too
	return nil
}

// BlockPeer refuses the node all madnetwork service and publishes the block as
// a distrust mark on the next sweep. The prior state is remembered for
// UnblockPeer. Idempotent.
//
// reason is what the mark carries to the rest of the network, so blocking is a
// public act here by construction — see the accepted risk in
// docs/architecture/federation-trust.md §Friend-list gossip. It is capped and
// sanitized on the peer-name rules; empty is allowed but makes the mark an
// anonymous downvote nobody downstream can act on.
func (n *Node) BlockPeer(ctx context.Context, id int64, reason string) error {
	p, err := n.store.GetFederationPeer(ctx, id)
	if err != nil {
		return err
	}
	if p.State == PeerBlocked {
		return nil
	}
	n.logger.Printf("federation: blocked %q (%s)", p.Label(), p.PublicKey)
	if err := n.store.BlockFederationPeer(ctx, id, p.State, CleanMarkReason(reason), time.Now().Unix()); err != nil {
		return err
	}
	n.Nudge() // republish the mark (and drop the edge) without waiting for the tick
	return nil
}

// BlockKey blocks a node we have no relationship with — someone seen only on
// the gossiped graph. It creates the peer row a block needs, since blocking is
// a judgement about a key rather than about a friendship.
//
// This is what makes the network map actionable: the whole point of seeing
// past your own friend list is being able to act on what you see.
func (n *Node) BlockKey(ctx context.Context, publicKey, name, reason string) error {
	key, err := NormalizeKey(publicKey)
	if err != nil {
		return err
	}
	p, err := n.store.GetFederationPeerByKey(ctx, key)
	switch {
	case err == nil:
		return n.BlockPeer(ctx, p.ID, reason)
	case !errors.Is(err, ErrPeerNotFound):
		return err
	}
	id, err := n.store.InsertFederationPeer(ctx, &Peer{
		PublicKey: key,
		HeardName: CleanPeerName(name),
		State:     PeerBlocked,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	if err := n.store.BlockFederationPeer(ctx, id, "", CleanMarkReason(reason), time.Now().Unix()); err != nil {
		return err
	}
	n.logger.Printf("federation: blocked %s (never a peer of ours)", key)
	n.Nudge()
	return nil
}

// UnblockPeer returns a blocked peer to its pre-block state. With no recorded
// prior state it falls back to pending_outgoing — the handshake re-proves the
// friendship on its own if both sides still want it.
func (n *Node) UnblockPeer(ctx context.Context, id int64) error {
	p, err := n.store.GetFederationPeer(ctx, id)
	if err != nil {
		return err
	}
	if p.State != PeerBlocked {
		return fmt.Errorf("%w: peer is not blocked", ErrPeerState)
	}
	prev := p.PrevState
	if prev == "" || prev == PeerBlocked {
		prev = PeerPendingOutgoing
	}
	if err := n.store.SetFederationPeerState(ctx, id, prev, ""); err != nil {
		return err
	}
	n.Nudge()
	return nil
}

// RemovePeer deletes the peer row entirely (forget the node; a fresh card
// import or incoming request starts from scratch).
func (n *Node) RemovePeer(ctx context.Context, id int64) error {
	// Forget the in-memory pairing attempt too, or re-importing this key later
	// shows a "last try" from before the removal — a diagnostic about a
	// relationship that no longer exists, which is exactly the kind of stale
	// remembering §Forgetting is about. Best-effort: a peer we cannot read is
	// one we cannot key the map by, and failing the removal over a log note
	// would be worse than the stale note.
	if p, err := n.store.GetFederationPeer(ctx, id); err == nil && p != nil {
		n.attemptMu.Lock()
		delete(n.attempts, p.PublicKey)
		n.attemptMu.Unlock()
	}
	return n.store.DeleteFederationPeer(ctx, id)
}

// RenamePeer sets the local display label.
func (n *Node) RenamePeer(ctx context.Context, id int64, name string) error {
	return n.store.UpdateFederationPeerName(ctx, id, CleanPeerName(name))
}

// depeerBlocked cuts the *underlay* link to a blocked node wherever that link is
// ours to cut — the second half of what blocking promises (§Trust graph): a block
// refuses all application-layer service, and where we peer with the node directly
// it also loses us as transit.
//
// It works from the live link list rather than from config, because a configured
// peer URI carries no key: we only learn who is behind `tcp://host:port` once the
// handshake completes. That also removes the need for a suppression list — the
// blocked set is already persisted, so re-running this every sweep re-cuts a link
// that config re-added at startup, or that the far side re-dialled. Within a
// minute, in other words, and permanently for as long as the block stands.
//
// A failure is expected and silent: on a shared public-mesh segment the link is
// not ours (`ErrLinkNotConfigured`), and transit below the app layer is
// Yggdrasil's business. The app-layer cut is the guaranteed part.
func (n *Node) depeerBlocked(peers []*Peer) {
	blocked := map[string]*Peer{}
	for _, p := range peers {
		if p.State == PeerBlocked {
			blocked[p.PublicKey] = p
		}
	}
	if len(blocked) == 0 {
		return
	}
	for _, info := range n.mesh.core.GetPeers() {
		if len(info.Key) == 0 {
			continue // a link that has not finished its handshake claims no key
		}
		// Only links WE dialled. This is not just scoping: yggdrasil v0.5.14
		// panics on RemovePeer for an inbound link — `links.remove` calls
		// `state.cancel()`, and only `links.add` ever sets that field, so an
		// incoming link's cancel func is nil (src/core/link.go:434 against :254).
		// Cutting an inbound link would be desirable and there is no API for it
		// anyway: PeerInfo carries no handle, so nothing else identifies it.
		// Reported by TestFriendshipHandshake, which blocks in both directions.
		if info.Inbound {
			continue
		}
		p, ok := blocked[hex.EncodeToString(info.Key)]
		if !ok {
			continue
		}
		u, err := url.Parse(info.URI)
		if err != nil {
			continue
		}
		// The empty source interface matches how peers are configured here
		// (core.Peer{URI} with no SourceInterface) and how listeners accept.
		if err := n.mesh.core.RemovePeer(u, ""); err != nil {
			continue
		}
		n.logger.Printf("federation: de-peered blocked node %q (%s) on the underlay — %s",
			p.Label(), p.PublicKey, info.URI)
	}
}

// ClaimReports lists the contradicted claims still waiting for an admin (F6).
// Evidence, not a verdict: the peer card shows what was compared and how each
// side was obtained, next to the Block action that was always there.
func (n *Node) ClaimReports(ctx context.Context) ([]*ClaimReport, error) {
	reports, err := n.store.ListClaimReports(ctx)
	if err != nil {
		return nil, err
	}
	if reports == nil {
		reports = []*ClaimReport{}
	}
	return reports, nil
}

// SetClaimDisposition records the admin's decision on one finding — dismissed
// (an innocent explanation, or not worth acting on) or acted (they blocked, or
// took it up with the peer). Nothing about what the peer is served changes here;
// blocking stays a separate, deliberate act.
func (n *Node) SetClaimDisposition(ctx context.Context, id int64, disposition string) error {
	return n.store.SetClaimReportDisposition(ctx, id, disposition)
}

// SetPeerGuestOnly sets or clears the admin's per-peer demotion: a guest-only
// friend keeps its standing (the swarm, the catalog) but sees only
// guest-accessible content — exactly what an anonymous local visitor may play.
// It replaced the node-key → local-account mapping, which only ever expressed
// this one bit (§Principals & access).
func (n *Node) SetPeerGuestOnly(ctx context.Context, id int64, guestOnly bool) error {
	return n.store.SetFederationPeerGuestOnly(ctx, id, guestOnly)
}
