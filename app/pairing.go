package app

import (
	"context"

	"daemonlord.ygg/madshare/federation"
)

// Pairing is the friendship surface — the same acts the /admin/network page
// performs, for an embedder that wants its node to take part in the graph as
// an ordinary member: export its own card, import somebody else's (or a bare
// key), and manage the resulting peer rows.
//
// Nothing here limits the embedder's node: a device paired through this
// surface is a full member of the graph, exactly like a server — a gossiped
// edge, a place on the network map, peers of its own. The listener-node path
// (federation-access.md §"The household") remains what a device gets when it
// does NOT pair; this surface is how an embedder chooses membership instead.
// EXPERIMENTAL (2026-08-17): added for madplayer's befriending test, and the
// method set may still change with what that test finds.
//
// Sharing is a separate axis and is untouched: PublishNothing still pins the
// scope, so what a paired device serves is decided exactly as before.
type Pairing interface {
	// Info is this node's own identity: name, mesh address, public key, and
	// the card an admin hands to the other side.
	Info() federation.NodeInfo
	// Peers is the trusted-peer table, friends first.
	Peers(ctx context.Context) ([]*federation.ExternalNode, error)
	// ImportCard parses a node card (the copy-paste JSON) and imports it: a
	// new node becomes pending_outgoing and is contacted immediately; for a
	// node that already asked to pair, importing completes the friendship.
	ImportCard(ctx context.Context, raw []byte) (*federation.ExternalNode, error)
	// ImportKey is the same act from a bare public key.
	ImportKey(ctx context.Context, publicKey, name string) (*federation.ExternalNode, error)
	// AcceptPeer answers a pending_incoming request.
	AcceptPeer(ctx context.Context, id int64) error
	// RemovePeer forgets a peer row entirely.
	RemovePeer(ctx context.Context, id int64) error
}

// Pairing returns the friendship surface, and whether there is one — absent
// for exactly the reasons Network is.
func (i *Instance) Pairing() (Pairing, bool) {
	if i == nil || i.node == nil {
		return nil, false
	}
	return pairing{i}, true
}

type pairing struct{ inst *Instance }

func (p pairing) Info() federation.NodeInfo { return p.inst.node.Info() }

func (p pairing) Peers(ctx context.Context) ([]*federation.ExternalNode, error) {
	return p.inst.node.Peers(ctx)
}

func (p pairing) ImportCard(ctx context.Context, raw []byte) (*federation.ExternalNode, error) {
	card, err := federation.ParseCard(raw)
	if err != nil {
		return nil, err
	}
	return p.inst.node.ImportCard(ctx, card)
}

func (p pairing) ImportKey(ctx context.Context, publicKey, name string) (*federation.ExternalNode, error) {
	return p.inst.node.ImportKey(ctx, publicKey, name)
}

func (p pairing) AcceptPeer(ctx context.Context, id int64) error {
	return p.inst.node.AcceptPeer(ctx, id)
}

func (p pairing) RemovePeer(ctx context.Context, id int64) error {
	return p.inst.node.RemovePeer(ctx, id)
}
