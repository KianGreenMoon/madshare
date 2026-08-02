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

// Mesh is a placeholder for the yggdrasil transport (mesh.go). Stripping
// federation strips the mesh with it: -tags nofederation removes the yggdrasil
// and gVisor dependencies from the binary entirely, so there is no transport
// left to serve [[listen_mesh]] on either. main refuses such a config at
// startup rather than starting without it.
type Mesh struct{}

// Start always fails in nofederation builds.
func Start(config.FederationConfig, PeerStore, *log.Logger, ...Option) (*Node, error) {
	return nil, errCompiledOut
}

// StartTransport always fails in nofederation builds.
func StartTransport(config.YggdrasilConfig, *log.Logger) (*Mesh, error) {
	return nil, errCompiledOut
}

func (m *Mesh) Stop()                    {}
func (m *Mesh) Address() net.IP          { return nil }
func (m *Mesh) PublicKeyHex() string     { return "" }
func (m *Mesh) InboundReaderAlive() bool { return false }
func (m *Mesh) ListenMesh(int) (net.Listener, error) {
	return nil, errCompiledOut
}

func (m *Mesh) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errCompiledOut
}

func (n *Node) Stop()                {}
func (n *Node) Mesh() *Mesh          { return nil }
func (n *Node) Address() net.IP      { return nil }
func (n *Node) PublicKeyHex() string { return "" }
func (n *Node) Name() string         { return "" }
func (n *Node) Info() NodeInfo       { return NodeInfo{} }
func (n *Node) Nudge()               {}
func (n *Node) InboundHealthy() bool { return true }

func (n *Node) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errCompiledOut
}

func (n *Node) Peers(context.Context) ([]*Peer, error)          { return nil, errCompiledOut }
func (n *Node) ImportCard(context.Context, Card) (*Peer, error) { return nil, errCompiledOut }
func (n *Node) ImportKey(context.Context, string, string) (*Peer, error) {
	return nil, errCompiledOut
}
func (n *Node) AcceptPeer(context.Context, int64) error        { return errCompiledOut }
func (n *Node) BlockPeer(context.Context, int64, string) error { return errCompiledOut }
func (n *Node) BlockKey(context.Context, string, string, string) error {
	return errCompiledOut
}
func (n *Node) UnblockPeer(context.Context, int64) error        { return errCompiledOut }
func (n *Node) RemovePeer(context.Context, int64) error         { return errCompiledOut }
func (n *Node) RenamePeer(context.Context, int64, string) error { return errCompiledOut }
func (n *Node) MapPeerUser(context.Context, int64, *int64) error {
	return errCompiledOut
}

func (n *Node) EnsureBlob(context.Context, string) (Transfer, error) {
	return nil, errCompiledOut
}

func (n *Node) IssueCapabilityToken(string, bool) (CapabilityGrant, error) {
	return CapabilityGrant{}, errCompiledOut
}

func (n *Node) ResyncGraph() {}

func (n *Node) PullFrom(string) error { return errCompiledOut }

func (n *Node) NetworkMap(context.Context) (NetworkMap, error) {
	return NetworkMap{}, errCompiledOut
}

// BranchMap answers nil rather than the compiled-out error: its caller reads a
// missing attribution as "one source, one voice", which is the correct weighting
// for a build with no graph at all.
func (n *Node) BranchMap(context.Context) (map[string][]string, error) {
	return nil, nil
}

// HopMap answers nil for the same reason: a build with no graph can place
// nobody, and "distance unknown" is the honest answer its callers already
// handle (they sort such nodes last, alphabetically).
func (n *Node) HopMap(context.Context) (map[string]int, error) {
	return nil, nil
}

func (n *Node) ClaimReports(context.Context) ([]*ClaimReport, error) {
	return nil, errCompiledOut
}

func (n *Node) SetClaimDisposition(context.Context, int64, string) error {
	return errCompiledOut
}

func (n *Node) EvictCachedBlob(string) error { return nil }
