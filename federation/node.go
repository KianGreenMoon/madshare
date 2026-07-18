//go:build !nofederation

package federation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	yggconfig "github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggstack/src/netstack"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/internal/version"
)

// Available reports whether federation is compiled into this binary. It is
// false in -tags nofederation builds (see node_stub.go).
const Available = true

// Node is the running embedded madnetwork node: the yggdrasil core (identity +
// encrypted transport), the userspace netstack on top of it, and the mesh-side
// HTTP listener serving the federation protocol. Friendship (the trusted-peer
// table, pairing, the refresh loop) lives in friendship.go and is active only
// when a PeerStore is wired.
type Node struct {
	core   *core.Core
	stack  *netstack.YggdrasilNetstack
	srv    *http.Server
	store  PeerStore
	name   string
	logger *log.Logger
	client *http.Client // outbound protocol calls, dialing through the mesh

	nudge      chan struct{} // wakes the refresh loop early (import/accept)
	loopCancel context.CancelFunc
	loopDone   chan struct{}
}

// Start loads (or creates) the node key, brings up the yggdrasil core with the
// configured underlay peers/listeners, and serves the federation protocol on
// [MeshPort] of the node's mesh address. store persists the trusted-peer table
// (nil disables friendship — F0 behaviour, used by narrow tests). The returned
// Node must be Stop()ed on shutdown. logger receives yggdrasil's info/warn/error
// output and the node's own friendship events.
func Start(fc config.FederationConfig, store PeerStore, logger *log.Logger) (*Node, error) {
	nodeCfg, err := loadOrCreateKey(fc.KeyFile)
	if err != nil {
		return nil, err
	}

	var opts []core.SetupOption
	for _, l := range fc.Listen {
		opts = append(opts, core.ListenAddress(l))
	}
	for _, p := range fc.Peers {
		opts = append(opts, core.Peer{URI: p})
	}
	c, err := core.New(nodeCfg.Certificate, &coreLogger{logger}, opts...)
	if err != nil {
		return nil, fmt.Errorf("federation: start yggdrasil core: %w", err)
	}
	stack, err := netstack.CreateYggdrasilNetstack(c)
	if err != nil {
		c.Stop()
		return nil, fmt.Errorf("federation: create netstack: %w", err)
	}
	lis, err := stack.ListenTCP(&net.TCPAddr{IP: c.Address(), Port: MeshPort})
	if err != nil {
		c.Stop()
		return nil, fmt.Errorf("federation: listen on mesh address: %w", err)
	}

	name := CleanPeerName(fc.Name)
	if name == "" {
		if host, err := os.Hostname(); err == nil {
			name = CleanPeerName(host)
		}
	}
	n := &Node{
		core:   c,
		stack:  stack,
		store:  store,
		name:   name,
		logger: logger,
		nudge:  make(chan struct{}, 1),
	}
	n.client = &http.Client{
		Transport: &http.Transport{DialContext: n.DialContext},
		Timeout:   15 * time.Second,
	}
	n.srv = &http.Server{Handler: n.protocolHandler()}
	go func() {
		if err := n.srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			logger.Printf("federation: mesh listener: %v", err)
		}
	}()
	if store != nil {
		loopCtx, cancel := context.WithCancel(context.Background())
		n.loopCancel = cancel
		n.loopDone = make(chan struct{})
		go n.refreshLoop(loopCtx)
	}
	return n, nil
}

// Stop shuts the refresh loop, the mesh listener, and the yggdrasil core down.
func (n *Node) Stop() {
	if n.loopCancel != nil {
		n.loopCancel()
		<-n.loopDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.srv.Shutdown(ctx)
	n.core.Stop()
}

// Address returns the node's self-certifying mesh IPv6 address (200::/7).
func (n *Node) Address() net.IP { return n.core.Address() }

// PublicKeyHex returns the node's ed25519 public key — its madnetwork identity
// — as lowercase hex.
func (n *Node) PublicKeyHex() string { return hex.EncodeToString(n.core.PublicKey()) }

// DialContext dials through the mesh (for http.Transport.DialContext), so
// outbound protocol calls reach peers' mesh listeners without any TUN.
func (n *Node) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return n.stack.DialContext(ctx, network, address)
}

// loadOrCreateKey returns a yggdrasil NodeConfig whose private key is persisted
// at path: an existing PEM file is the node's durable identity; a missing one
// is created (0600) from a freshly generated key. The self-signed certificate
// (what core.New consumes) is derived from the key on every start.
func loadOrCreateKey(path string) (*yggconfig.NodeConfig, error) {
	cfg := yggconfig.GenerateConfig()
	pem, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := cfg.UnmarshalPEMPrivateKey(pem); err != nil {
			return nil, fmt.Errorf("federation: parse key file %s: %w", path, err)
		}
	case os.IsNotExist(err):
		out, err := cfg.MarshalPEMPrivateKey()
		if err != nil {
			return nil, fmt.Errorf("federation: marshal new node key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, fmt.Errorf("federation: create key dir: %w", err)
		}
		if err := os.WriteFile(path, out, 0600); err != nil {
			return nil, fmt.Errorf("federation: write key file %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("federation: read key file %s: %w", path, err)
	}
	if err := cfg.GenerateSelfSignedCertificate(); err != nil {
		return nil, fmt.Errorf("federation: derive node certificate: %w", err)
	}
	return cfg, nil
}

// protocolHandler is the madnetwork protocol surface served on the mesh:
// the ping (F0) and the pairing endpoint (F1, friendship.go). meshAuth wraps
// everything — a blocked peer gets nothing, not even a ping, and any request
// from a known peer refreshes its last_seen.
func (n *Node) protocolHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /madnetwork/v0/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol":   ProtocolVersion,
			"software":   version.Name,
			"version":    version.Get().Version,
			"public_key": n.PublicKeyHex(),
			"address":    n.Address().String(),
		})
	})
	mux.HandleFunc("POST /madnetwork/v0/pair", n.handlePair)
	return n.meshAuth(mux)
}

// coreLogger adapts the standard library logger to yggdrasil's leveled Logger
// interface. Debug/trace output is dropped — it is very chatty and madshare has
// no verbosity switch yet.
type coreLogger struct{ l *log.Logger }

func (c *coreLogger) Printf(f string, a ...interface{}) { c.l.Printf("federation: "+f, a...) }
func (c *coreLogger) Println(a ...interface{}) {
	c.l.Println(append([]interface{}{"federation:"}, a...)...)
}
func (c *coreLogger) Infof(f string, a ...interface{})  { c.Printf(f, a...) }
func (c *coreLogger) Infoln(a ...interface{})           { c.Println(a...) }
func (c *coreLogger) Warnf(f string, a ...interface{})  { c.Printf(f, a...) }
func (c *coreLogger) Warnln(a ...interface{})           { c.Println(a...) }
func (c *coreLogger) Errorf(f string, a ...interface{}) { c.Printf(f, a...) }
func (c *coreLogger) Errorln(a ...interface{})          { c.Println(a...) }
func (c *coreLogger) Debugf(string, ...interface{})     {}
func (c *coreLogger) Debugln(...interface{})            {}
func (c *coreLogger) Traceln(...interface{})            {}
