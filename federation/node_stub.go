//go:build nofederation

// Stub used when madshare is compiled with -tags nofederation, producing a
// standalone server with no yggdrasil/gVisor dependencies. main checks
// Available before calling Start, so the stub only has to satisfy the compiler
// (including api.FederationNode, which madshare.go assigns a *Node to).
package federation

import (
	"context"
	"errors"
	"log"
	"net"

	"daemonlord.ygg/madshare/config"
)

// Available is false in -tags nofederation builds; see node.go for the real build.
const Available = false

var errCompiledOut = errors.New("federation compiled out (-tags nofederation)")

// Node is a placeholder; no instance is ever created in nofederation builds.
type Node struct{}

// Start always fails in nofederation builds.
func Start(config.FederationConfig, PeerStore, *log.Logger, ...Option) (*Node, error) {
	return nil, errCompiledOut
}

func (n *Node) Stop()                {}
func (n *Node) Address() net.IP      { return nil }
func (n *Node) PublicKeyHex() string { return "" }
func (n *Node) Name() string           { return "" }
func (n *Node) OnlinePeerIDs() []int64 { return nil }
func (n *Node) CachedHashes() []string { return nil }
func (n *Node) Info() NodeInfo       { return NodeInfo{} }
func (n *Node) Nudge()               {}

func (n *Node) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errCompiledOut
}

func (n *Node) Peers(context.Context) ([]*Peer, error)          { return nil, errCompiledOut }
func (n *Node) ImportCard(context.Context, Card) (*Peer, error) { return nil, errCompiledOut }
func (n *Node) AcceptPeer(context.Context, int64) error         { return errCompiledOut }
func (n *Node) BlockPeer(context.Context, int64) error          { return errCompiledOut }
func (n *Node) UnblockPeer(context.Context, int64) error        { return errCompiledOut }
func (n *Node) RemovePeer(context.Context, int64) error         { return errCompiledOut }
func (n *Node) RenamePeer(context.Context, int64, string) error { return errCompiledOut }
func (n *Node) MapPeerUser(context.Context, int64, *int64) error {
	return errCompiledOut
}

func (n *Node) EnsureBlob(context.Context, string) (Transfer, error) {
	return nil, errCompiledOut
}
