//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggstack/src/netstack"

	"daemonlord.ygg/madshare/config"
)

// Mesh is the yggdrasil transport on its own: the identity key, the core (the
// encrypted overlay link), and the gVisor userspace netstack on top of it — no
// TUN device and no root. It is everything needed to *have* a mesh address and
// speak over it, and nothing about madnetwork.
//
// The separation is what lets a server be reachable from anywhere while
// federating with nobody ([[listen_mesh]] with [yggdrasil].enabled and
// [federation].enabled = false). Node is madnetwork built on top of a Mesh:
// the protocol listener, friendship, catalogs, transfers. See
// docs/plans/mesh-listener.md §4.
//
// Ownership: whoever creates a Mesh stops it, EXCEPT that Start adopts the one
// it is given (or builds its own) and Node.Stop then stops it. A process
// therefore stops exactly one of the two.
type Mesh struct {
	core   *core.Core
	stack  *netstack.YggdrasilNetstack
	logger *log.Logger
	// signKey is the node identity the mesh address derives from. It lives here
	// rather than on Node because it is the key that produced the address, and
	// gossip signing (F6) borrows it from the transport rather than owning it.
	signKey ed25519.PrivateKey
}

// StartTransport loads (or creates) the node key and brings the yggdrasil core
// and its netstack up with the configured underlay peers and listeners. It
// serves nothing by itself — the caller either hands it to Start (madnetwork on
// top) or serves its own listeners on it via ListenMesh, or both.
//
// The returned Mesh must be Stop()ed, unless it is handed to Start, which takes
// that responsibility over.
func StartTransport(yc config.YggdrasilConfig, logger *log.Logger) (*Mesh, error) {
	nodeCfg, err := loadOrCreateKey(yc.KeyFile)
	if err != nil {
		return nil, err
	}
	var coreOpts []core.SetupOption
	for _, l := range yc.Listen {
		coreOpts = append(coreOpts, core.ListenAddress(l))
	}
	for _, p := range yc.Peers {
		coreOpts = append(coreOpts, core.Peer{URI: p})
	}
	c, err := core.New(nodeCfg.Certificate, &coreLogger{logger}, coreOpts...)
	if err != nil {
		return nil, fmt.Errorf("federation: start yggdrasil core: %w", err)
	}
	stack, err := netstack.CreateYggdrasilNetstack(c)
	if err != nil {
		c.Stop()
		return nil, fmt.Errorf("federation: create netstack: %w", err)
	}
	m := &Mesh{
		core:    c,
		stack:   stack,
		logger:  logger,
		signKey: ed25519.PrivateKey(nodeCfg.PrivateKey),
	}
	m.writeIdentityFiles(yc.KeyFile)
	return m, nil
}

// writeIdentityFiles drops this node's public key and mesh address beside the
// key file, as <keyfile>.pub and <keyfile minus .key>.addr — with the default
// key that is data/federation.key.pub and data/federation.addr.
//
// Pure convenience: nothing reads them back, they are not config, and deleting
// them costs nothing. They exist because the two facts an operator constantly
// needs — the address to hand out and the key a friend identifies this node by —
// are otherwise only in a log line that scrolls away, or behind a running
// server's admin page. A script that prints the address should not have to parse
// logs or derive it from an ed25519 key.
//
// Rewritten whenever they disagree with the running identity, not merely created
// when absent: they are derived outputs, and a stale .addr left over from a
// replaced key is worse than no file at all — it is a wrong address that looks
// authoritative. Deleting them still regenerates them, which is the "create if
// missing" case.
//
// Never fatal. A read-only data dir is a legitimate deployment, and a node that
// cannot write a convenience file is still a perfectly good node.
func (m *Mesh) writeIdentityFiles(keyFile string) {
	if keyFile == "" {
		return
	}
	for path, content := range map[string]string{
		keyFile + ".pub": m.PublicKeyHex() + "\n",
		strings.TrimSuffix(keyFile, ".key") + ".addr": m.Address().String() + "\n",
	} {
		if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
			continue
		}
		// 0644, unlike the key's 0600: a public key and an address are things you
		// publish, and the whole point is that anything on the host can read them.
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			m.logger.Printf("federation: could not write %s: %v (informational file only; the node is unaffected)", path, err)
		}
	}
}

// Address returns this node's yggdrasil address (200::/7), derived from its
// public key and therefore stable across restarts.
func (m *Mesh) Address() net.IP { return m.core.Address() }

// PublicKeyHex returns the node's public key, the identity a peer names it by.
func (m *Mesh) PublicKeyHex() string { return hex.EncodeToString(m.core.PublicKey()) }

// DialContext dials through the mesh netstack, so any http.Client using it as a
// transport reaches mesh addresses and nothing else.
func (m *Mesh) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return m.stack.DialContext(ctx, network, address)
}

// ListenMesh returns a listener on the given port of this node's mesh address.
//
// It is an ordinary net.Listener over the userspace netstack, so an http.Server
// serves and shuts down on it exactly as on a kernel socket — with two
// differences worth knowing. Low ports are free: there is no kernel here and so
// no privileged-port rule, which is why the web UI can sit on 80 without root
// or setcap. And the address is NOT reachable from this host: with no TUN
// device, packets only move gVisor → ipv6rwc → core → mesh, so a local curl
// fails while a remote mesh peer succeeds.
func (m *Mesh) ListenMesh(port int) (net.Listener, error) {
	lis, err := m.stack.ListenTCP(&net.TCPAddr{IP: m.core.Address(), Port: port})
	if err != nil {
		return nil, fmt.Errorf("federation: listen on mesh address port %d: %w", port, err)
	}
	return lis, nil
}

// InboundReaderAlive reports whether the netstack's inbound reader goroutine is
// running — the one unambiguous signal that this node can still receive mesh
// traffic (a self-ping cannot test it: HandleLocal loops local traffic inside
// gVisor, never touching the reader). The availability filter fails open on
// false; see docs/architecture/federation.md §Availability.
func (m *Mesh) InboundReaderAlive() bool { return m.stack.InboundReaderAlive() }

// Stop tears the netstack down and then the core.
//
// The order matters: aborting the stack's endpoints while the mesh is still up
// lets their RSTs reach peers, and the inbound reader then exits on the core's
// own shutdown. Skipping the stack close leaves a whole gVisor stack and its
// goroutines running for the life of the process — harmless for a server that
// stops one node at exit, cumulative for anything that starts and stops many
// (TestStopReleasesNetstack).
func (m *Mesh) Stop() {
	m.stack.Close()
	m.core.Stop()
}
