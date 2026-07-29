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
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

// Friendship (federation F1): the pairing handshake, the mesh-side peer
// identity check, and the refresh loop that retries pending pairings and keeps
// friends' last_seen fresh. Design: docs/architecture/federation.md §Trust
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
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	peers, err := n.store.ListFederationPeers(r.Context())
	if err != nil {
		n.logger.Printf("federation: resolve peer: %v", err)
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
func (n *Node) pairWith(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		n.logger.Printf("federation: peer %d has an invalid key: %v", p.ID, err)
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
		return // unreachable — the refresh loop retries
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var msg pairMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return
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

// pingPeer refreshes a friend's last_seen with a protocol ping, and takes the
// peer's own name from the reply while it is there — the contact that keeps the
// heard name current, once a minute per friend.
func (n *Node) pingPeer(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", addr, MeshPort)
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
	// The name is additive on the ping (a pre-033 peer omits it), so a missing
	// one simply leaves the last claim standing.
	var reply struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&reply); err != nil {
		return
	}
	n.refreshHeardName(ctx, p, reply.Name)
}

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

// sweep runs one round: pair toward every pending_outgoing peer; ping every
// friend and, when its catalog is due (Intervals.CatalogSync, or never synced),
// pull it. Sequential — friend lists are small and each call is bounded by the
// client timeout.
func (n *Node) sweep(ctx context.Context) {
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		n.logger.Printf("federation: refresh: %v", err)
		return
	}
	// Gossip (F6) is maintenance of our own view, not per-peer work: publish our
	// record when the friend list moved or the heartbeat came due, and drop what
	// aged out. Both run before the loop so a friendship accepted this round is
	// already in the record we serve during it.
	n.publishOwnRecord(ctx, peers)
	n.publishOwnMarkRecord(ctx, peers)
	n.expireGraph(ctx)

	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		switch p.State {
		case PeerPendingOutgoing:
			n.pairWith(ctx, p)
		case PeerFriend:
			n.pingPeer(ctx, p)
			if time.Since(time.Unix(p.CatalogSyncedAt, 0)) >= n.intervals.CatalogSync {
				n.syncCatalog(ctx, p)
				n.syncHoldings(ctx, p) // F4: refresh what they seed from cache
				n.syncGraph(ctx, p)    // F6: gossip rides the catalog cadence
			}
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
	}
	return peers, nil
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
// docs/architecture/federation.md §Friend-list gossip. It is capped and
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
	return n.store.DeleteFederationPeer(ctx, id)
}

// RenamePeer sets the local display label.
func (n *Node) RenamePeer(ctx context.Context, id int64, name string) error {
	return n.store.UpdateFederationPeerName(ctx, id, CleanPeerName(name))
}

// MapPeerUser maps the peer node to a local user account (nil clears). All
// existing local ACLs then apply to that node's owner — federation adds no
// parallel permission system.
func (n *Node) MapPeerUser(ctx context.Context, id int64, userID *int64) error {
	return n.store.SetFederationPeerUser(ctx, id, userID)
}
