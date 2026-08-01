//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

	// Pairing diagnostics (friendship.go): the last outbound attempt per node
	// key, so a pairing that never converges can say why. Keyed by key rather
	// than by peer id — a removed-and-reimported node is the same node.
	attemptMu sync.Mutex
	attempts  map[string]PairAttempt

	// Background cadences and deadlines, resolved once in Start from the
	// defaults plus any WithIntervals/WithTimeouts override. Only tests and the
	// mesh lab override them (docs/plans/mesh-testing.md T1).
	intervals Intervals
	timeouts  Timeouts
	// discovery bounds the frontier pull (F7 item 5, discovery.go). Unlike the
	// two above this one IS configuration — [federation] discovery_budget /
	// discovery_cap — because it is resource policy an operator has a stake in,
	// not a test seam.
	discovery Discovery

	// pullNow is the explicit "fetch this node's catalog on the next round"
	// queue, so an admin's interest beats the frontier rotation instead of
	// waiting its turn (discovery.go). Keyed by node key; drained each sweep.
	pullMu  sync.Mutex
	pullNow map[string]struct{}

	// Memoized own-catalog snapshots served to friends, one per audience class
	// (catalog.go, F5): a catalog is built for a specific audience, so the memo
	// is keyed by it. The key space is tiny — at depth 0 it is exactly {full,
	// guest-only} — so this is bounded by design, not by eviction.
	snapMu sync.Mutex
	snaps  map[Audience]*snapshot

	// Memoized gossip digest served to friends (gossip_node.go, F6). Not keyed
	// by audience like the catalog: the graph is friends-only, so every caller
	// gets the same answer. forceGraph is the Rescan button's flag — set by
	// ResyncGraph, consumed once per sweep, so holding the button down coalesces
	// into the round already running.
	digestMu   sync.Mutex
	digest     *graphDigest
	forceGraph atomic.Bool

	// F3 transfer wiring (transfer.go): the blob cache dir, the local blob
	// resolver (serving side + local short-circuit), the in-flight transfer
	// table, and the lifetime context detaching fetches from request contexts
	// (cache-through: the download outlives the requester).
	cacheDir       string
	resolveBlob    func(hash string) (path string, ok bool)
	transferMu     sync.Mutex
	transfers      map[string]*transfer
	transferCtx    context.Context
	transferCancel context.CancelFunc

	// F4 swarm wiring (swarm.go): a timeout-free client for blob/chunk fetches
	// (each fetch is bounded by its own context), the outbound seed rate cap
	// (nil = unlimited), and the memoized per-hash chunk manifests served to
	// friends (content-addressed, so immutable once built).
	blobClient  *http.Client
	seedLimiter *rateLimiter
	manifestMu  sync.Mutex
	manifests   map[string]*blobManifest

	// Availability / self-health (docs/plans/availability.md Phase 1).
	// readerAlive reports whether the netstack inbound reader is running — the
	// unambiguous self-health signal (a self-ping cannot test it: HandleLocal
	// loops local traffic inside gVisor, bypassing the reader). nil ⇒ treated as
	// healthy; wired to the netstack accessor once Phase 0 exposes it.
	readerAlive func() bool
	// lastTouch throttles last_seen writes from the transfer path (chunk
	// deliveries are frequent; last_seen is monotonic, so ≤1 write per source per
	// peerTouchThrottle is plenty). Keyed by SOURCE id, which is what the transfer
	// path has. meshAuth/pingPeer touch the peer row directly, unthrottled.
	touchMu   sync.Mutex
	lastTouch map[int64]time.Time

	// Friend-list gossip (F6, gossip_node.go). signKey is this node's ed25519
	// private key — the same identity the mesh address derives from — used to
	// sign the records it publishes, since a relayed record cannot lean on the
	// connection it arrived over. graphAccept rate-limits how often one origin
	// may push a new sequence at us.
	signKey     ed25519.PrivateKey
	acceptMu    sync.Mutex
	graphAccept map[string]time.Time

	// Our community (F7, membership.go): the keys we serve the Madnetwork scope
	// to, derived from the gossiped graph by the mutual-edge walk and indexed by
	// mesh address. Memoized because it is on every mesh request's path, and
	// recomputed on the sweep from the same peers+edges the retention walk reads.
	memberMu sync.Mutex
	members  *memberSet
}

// peerTouchThrottle bounds transfer-path last_seen writes per peer.
const peerTouchThrottle = 30 * time.Second

// The production cadences and deadlines — what a node runs with unless a caller
// passes WithIntervals/WithTimeouts. They live together rather than next to
// their users so the values a lab has to shrink are readable in one place.
var (
	defaultIntervals = Intervals{
		// Most refresh rounds are cheap: a ping per friend and a not-modified
		// catalog check. The catalog cadence is deliberately far slower than the
		// ping — a library changes rarely, liveness constantly.
		Refresh:     time.Minute,
		CatalogSync: CatalogCycle,
		SnapshotTTL: time.Minute,
		// Gossip (F6) ages by wall clock, not by hops: a record lives 7 days and
		// its author re-signs every 6 hours. The ratio is what makes an offline
		// weekend invisible while an abandoned node still fades within a week.
		GraphRepublish: 6 * time.Hour,
		GraphTTL:       7 * 24 * time.Hour,
		GraphAccept:    time.Minute,
		GraphDigestTTL: 30 * time.Second,
		MembershipTTL:  time.Minute,
	}
	// The frontier: four member catalogs per 15-minute cycle reaches ~16 new
	// nodes an hour, which fills the cap in about half a day without ever
	// dialling in a storm; the cap holds a few hundred foreign libraries, which
	// is megabytes, not gigabytes. Both are first guesses meant to be tuned
	// against a real network — see §Open questions 2.
	defaultDiscovery = Discovery{Budget: 4, Cap: 200}
	defaultTimeouts  = Timeouts{
		Control:    15 * time.Second,
		Manifest:   20 * time.Second,
		ChunkStall: 20 * time.Second,
		PerChunk:   2 * time.Minute,
		Transfer:   30 * time.Minute,
	}
)

// Start loads (or creates) the node key, brings up the yggdrasil core with the
// configured underlay peers/listeners, and serves the federation protocol on
// [MeshPort] of the node's mesh address. store persists the trusted-peer table
// (nil disables friendship — F0 behaviour, used by narrow tests). The returned
// Node must be Stop()ed on shutdown. logger receives yggdrasil's info/warn/error
// output and the node's own friendship events.
func Start(fc config.FederationConfig, store PeerStore, logger *log.Logger, opts ...Option) (*Node, error) {
	var o nodeOptions
	for _, opt := range opts {
		opt(&o)
	}
	nodeCfg, err := loadOrCreateKey(fc.KeyFile)
	if err != nil {
		return nil, err
	}

	var coreOpts []core.SetupOption
	for _, l := range fc.Listen {
		coreOpts = append(coreOpts, core.ListenAddress(l))
	}
	for _, p := range fc.Peers {
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
	transferCtx, transferCancel := context.WithCancel(context.Background())
	n := &Node{
		signKey:        ed25519.PrivateKey(nodeCfg.PrivateKey),
		graphAccept:    map[string]time.Time{},
		intervals:      o.intervals.withDefaults(defaultIntervals),
		timeouts:       o.timeouts.withDefaults(defaultTimeouts),
		discovery:      o.discovery.withDefaults(defaultDiscovery),
		core:           c,
		stack:          stack,
		store:          store,
		name:           name,
		logger:         logger,
		nudge:          make(chan struct{}, 1),
		attempts:       map[string]PairAttempt{},
		pullNow:        map[string]struct{}{},
		cacheDir:       o.cacheDir,
		resolveBlob:    o.resolveBlob,
		transfers:      map[string]*transfer{},
		transferCtx:    transferCtx,
		transferCancel: transferCancel,
		seedLimiter:    newRateLimiter(int64(fc.SeedRateKiB) * 1024),
		manifests:      map[string]*blobManifest{},
		lastTouch:      map[int64]time.Time{},
	}
	// Self-health signal: the netstack inbound reader's liveness (the unambiguous
	// signal — a self-ping can't test it, HandleLocal loops local traffic inside
	// gVisor). When it reports dead, the merged browse fails open rather than
	// blanking the view (docs/architecture/federation.md §Availability).
	n.readerAlive = stack.InboundReaderAlive
	n.client = &http.Client{
		Transport: &http.Transport{DialContext: n.DialContext},
		Timeout:   n.timeouts.Control,
	}
	// Blob/chunk fetches can be large and slow over the mesh; each is bounded by
	// its own context plus an idle-read watchdog (readStall), so this client
	// carries no global timeout — but a ResponseHeaderTimeout catches a holder
	// that accepts the connection then never answers (a hung mesh path) fast. It
	// shares the idle-read budget: both detect the same "connected but silent"
	// failure, only on different sides of the response header.
	n.blobClient = &http.Client{Transport: &http.Transport{
		DialContext:           n.DialContext,
		ResponseHeaderTimeout: n.timeouts.ChunkStall,
	}}
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

// Stop shuts the refresh loop, in-flight transfers, the mesh listener, and the
// yggdrasil core down.
func (n *Node) Stop() {
	if n.loopCancel != nil {
		n.loopCancel()
		<-n.loopDone
	}
	n.transferCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.srv.Shutdown(ctx)
	// Tear the userspace netstack down before the core: aborting the stack's
	// endpoints while the mesh is still up lets their RSTs reach peers, and the
	// inbound reader then exits on the core's own shutdown. Skipping this leaves
	// a full gVisor stack (and its goroutines) running for the life of the
	// process — harmless for a server that stops one node at exit, cumulative
	// for anything that starts and stops many.
	n.stack.Close()
	n.core.Stop()
}

// Address returns the node's self-certifying mesh IPv6 address (200::/7).
func (n *Node) Address() net.IP { return n.core.Address() }

// PublicKeyHex returns the node's ed25519 public key — its madnetwork identity
// — as lowercase hex.
func (n *Node) PublicKeyHex() string { return hex.EncodeToString(n.core.PublicKey()) }

// Name returns the node-card display name ([federation].name, hostname
// fallback) — the label the merged browse uses for the self holder.
func (n *Node) Name() string { return n.name }

// DialContext dials through the mesh (for http.Transport.DialContext), so
// outbound protocol calls reach peers' mesh listeners without any TUN.
func (n *Node) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return n.stack.DialContext(ctx, network, address)
}

// InboundHealthy reports whether this node's inbound mesh path appears to be
// working. The merged madnetwork browse consults it to decide whether to hide
// currently-unreachable friends' tracks (healthy) or to stop hiding and show the
// last-known catalog (unhealthy) — a local netstack fault must never look like
// "the whole network is gone" (docs/architecture/federation.md §Availability &
// node health). nil signal ⇒ healthy.
func (n *Node) InboundHealthy() bool {
	return n.readerAlive == nil || n.readerAlive()
}

// observePeerAlive records that a holder just delivered data on the transfer
// path (an in-flight download is continuous liveness proof). Throttled per
// holder, and a no-op without a store; last_seen is monotonic so an out-of-order
// write is harmless. meshAuth and pingPeer touch directly and are not throttled.
//
// It writes to the SOURCE row, not the peer row, because since F7 item 5 most
// holders are members with no peer row at all — and the freshness window reads
// the later of the two clocks anyway, so a friend delivering bytes stays fresh
// through this path exactly as it did.
func (n *Node) observePeerAlive(p *BlobProvider) {
	if n.store == nil || p == nil || p.SourceID == 0 {
		return
	}
	now := time.Now()
	n.touchMu.Lock()
	if last, ok := n.lastTouch[p.SourceID]; ok && now.Sub(last) < peerTouchThrottle {
		n.touchMu.Unlock()
		return
	}
	n.lastTouch[p.SourceID] = now
	n.touchMu.Unlock()
	if err := n.store.TouchCatalogSourceSeen(n.transferCtx, p.SourceID, now.Unix(), ""); err != nil {
		n.logger.Printf("federation: touch source %d (transfer): %v", p.SourceID, err)
	}
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
// the ping (F0), pairing (F1, friendship.go), the catalog (F2, catalog.go),
// the blob server (F3, transfer.go), and the swarm's chunk manifest + holdings
// tracker (F4, swarm.go). meshAuth wraps everything — a blocked peer gets
// nothing, not even a ping, and any request from a known peer refreshes its
// last_seen.
func (n *Node) protocolHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /madnetwork/v0/ping", func(w http.ResponseWriter, r *http.Request) {
		reply := map[string]any{
			"protocol": ProtocolVersion,
			"software": version.Name,
			"version":  version.Get().Version,
			// The self-name, so a friend's ping keeps its heard name current
			// (migration 033) — this node renaming itself propagates within a
			// minute instead of never. Also the first field of the NodeInfo-style
			// health card §Availability sketches.
			"name":       n.name,
			"public_key": n.PublicKeyHex(),
			"address":    n.Address().String(),
		}
		// Freshness hints (F7 item 10, freshness.go): what we saw first-hand of
		// our own friends, for the asking friend's benefit. Opt-in, so the plain
		// ping every other caller makes stays four small fields.
		if hints := n.freshnessHints(r); len(hints) > 0 {
			reply["hints"] = hints
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	})
	mux.HandleFunc("POST /madnetwork/v0/pair", n.handlePair)
	mux.HandleFunc("GET /madnetwork/v0/catalog", n.handleCatalog)
	mux.HandleFunc("GET /madnetwork/v0/blob/{hash}", n.handleBlob)
	mux.HandleFunc("GET /madnetwork/v0/manifest/{hash}", n.handleManifest)
	mux.HandleFunc("GET /madnetwork/v0/holdings", n.handleHoldings)
	mux.HandleFunc("GET /madnetwork/v0/graph", n.handleGraph)
	mux.HandleFunc("POST /madnetwork/v0/graph/fetch", n.handleGraphFetch)
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
