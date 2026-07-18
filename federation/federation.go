// Package federation embeds a madnetwork node: a yggdrasil mesh identity plus
// the mesh-side federation protocol listener, kept entirely in-process (gVisor
// userspace netstack, no TUN, no root). This is the F0 skeleton — identity,
// transport, and a protocol ping; friendship, catalog, and transfer arrive in
// later milestones. Design: docs/architecture/federation.md.
//
// The package compiles out with -tags nofederation (see node_stub.go), which
// also drops the yggdrasil and gVisor dependencies from the binary; main
// refuses to start such a build with federation.enabled set.
package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MeshPort is the fixed TCP port of the madnetwork protocol listener on every
// node's mesh address (the port lives inside the embedded netstack, so it can
// never collide with anything on the host). 1314 spells MAD (M=13, A=1, D=4).
const MeshPort = 1314

// ProtocolVersion is the madnetwork protocol generation, exchanged in ping so
// incompatible peers can refuse each other early.
const ProtocolVersion = 0

// Peer states (federation_peers.state, migration 026). The friendship state
// machine — see docs/architecture/federation.md §Trust graph and the pairing
// handshake in node.go:
//
//	pending_outgoing → friend   when the peer's node confirms (their admin
//	                            imported our card, or accepted our request)
//	pending_incoming → friend   when our admin accepts
//	any              → blocked  by admin action; prev_state remembers where an
//	                            unblock returns to (local effect only in F1)
const (
	PeerPendingOutgoing = "pending_outgoing"
	PeerPendingIncoming = "pending_incoming"
	PeerFriend          = "friend"
	PeerBlocked         = "blocked"
)

// ErrPeerNotFound is returned by peer lookups when no row matches.
var ErrPeerNotFound = errors.New("federation peer not found")

// ErrPeerState marks an operation refused because the peer is in the wrong
// state (accepting a non-pending peer, importing a blocked node's card, …).
// The admin API maps it to 409.
var ErrPeerState = errors.New("invalid peer state for this operation")

// Peer is a row of the trusted-peer table plus display decorations: Username is
// the mapped local account's name (joined by the store), Address the mesh IPv6
// derived from PublicKey (filled by the running node — key derivation lives in
// the !nofederation build).
type Peer struct {
	ID        int64  `json:"id"`
	PublicKey string `json:"public_key"` // lowercase hex ed25519
	Name      string `json:"name"`
	State     string `json:"state"`
	PrevState string `json:"-"`
	UserID    *int64 `json:"user_id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"` // unix seconds; 0 = never

	Username string `json:"username,omitempty"` // mapped account, display only
	Address  string `json:"address,omitempty"`  // derived mesh address, display only
}

// PeerStore is the persistence the node needs for friendship. *database.DB
// implements it (database/federation.go).
type PeerStore interface {
	ListFederationPeers(ctx context.Context) ([]*Peer, error)
	GetFederationPeer(ctx context.Context, id int64) (*Peer, error)
	GetFederationPeerByKey(ctx context.Context, publicKey string) (*Peer, error)
	InsertFederationPeer(ctx context.Context, p *Peer) (int64, error)
	SetFederationPeerState(ctx context.Context, id int64, state, prevState string) error
	UpdateFederationPeerName(ctx context.Context, id int64, name string) error
	SetFederationPeerUser(ctx context.Context, id int64, userID *int64) error
	TouchFederationPeerSeen(ctx context.Context, id int64, when int64) error
	DeleteFederationPeer(ctx context.Context, id int64) error
}

// Card is a node card — the out-of-band introduction two admins exchange to
// friend their nodes (copy-paste JSON). It deliberately carries only identity:
// underlay connectivity is [federation] config's business (public mesh or
// explicit peers/listen), not the card's. Version is the protocol generation
// (field name "madshare_node_card" doubles as the format marker).
type Card struct {
	Version   int    `json:"madshare_node_card"`
	Name      string `json:"name,omitempty"`
	PublicKey string `json:"public_key"`
}

// NodeInfo is the running node's own identity as shown on the admin page.
type NodeInfo struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
	Card      Card   `json:"card"`
}

// NormalizeKey validates a hex-encoded ed25519 public key and returns it
// lowercased, the canonical form stored and compared everywhere.
func NormalizeKey(hexKey string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(hexKey))
	raw, err := hex.DecodeString(k)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "", errors.New("public key must be 64 hex characters (ed25519)")
	}
	return k, nil
}

// ParseCard validates raw JSON as a node card. It tolerates unknown fields
// (future card versions may add some) but requires the format marker and a
// well-formed key; the name is trimmed and length-capped.
func ParseCard(raw []byte) (Card, error) {
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil {
		return Card{}, errors.New("not a node card (invalid JSON)")
	}
	if c.Version != ProtocolVersion {
		return Card{}, fmt.Errorf("unsupported node card version %d (this node speaks %d)", c.Version, ProtocolVersion)
	}
	key, err := NormalizeKey(c.PublicKey)
	if err != nil {
		return Card{}, err
	}
	c.PublicKey = key
	c.Name = CleanPeerName(c.Name)
	return c, nil
}

// CleanPeerName trims and length-caps a peer-supplied display name (cards and
// pair requests are remote input).
func CleanPeerName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}
