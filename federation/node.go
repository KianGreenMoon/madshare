//go:build !nofederation

package federation

import (
	"context"
	"crypto/ed25519"
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
	// mesh is the transport this node runs on — the yggdrasil core, the netstack
	// and the identity key (mesh.go). Adopted from the caller via WithMesh or
	// built by Start; either way Node.Stop stops it.
	mesh   *Mesh
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

	// Blobs this node has just finished fetching, waiting to be pushed to our
	// friends on the next refresh round (F9 item 2, announce.go). Completions
	// only: what we hold partially is read live from the transfer table, and what
	// we hold whole is already the entire cache directory.
	announceMu  sync.Mutex
	announceNew map[string]bool

	// F4 swarm wiring (swarm.go): a timeout-free client for blob/chunk fetches
	// (each fetch is bounded by its own context), the outbound seed rate cap
	// (nil = unlimited), and the memoized per-hash chunk manifests served to
	// friends (content-addressed, so immutable once built; an LRU capped at
	// maxManifestMemo, since blob deletions do not reach in here).
	blobClient *http.Client
	// The node's two live rate caps (rates.go), each adjustable at runtime:
	// outbound (what seeding costs the uplink) and inbound (what fetching costs
	// the downlink — a cap that did not exist before the swarm page). cfg*KiB and
	// cfgQuota are the config-file values every limiter falls back to when no
	// override is set, and limitResolver reads the overrides without this package
	// knowing what a database is.
	upRate, downRate     *adjustableRate
	cfgUpKiB, cfgDownKiB int
	cfgQuota             QuotaLimits
	rateMu               sync.Mutex
	ratesAt              time.Time
	limitResolver        func(context.Context) (LimitOverrides, error)
	// quotas bounds what a requester we have no direct relationship with may
	// cost us — bytes and concurrent serves, per node and across the class
	// (F7 item 6, quota.go). Friends bypass it; all-zero admits everything, which
	// is the shipped default. Live: it resolves on the same memo as the rates.
	quotas       *quotas
	manifestMu   sync.Mutex
	manifests    map[string]*manifestEntry
	manifestTick uint64

	// Byte accounting for the swarm page (traffic.go): what this process has
	// moved, per hash and per counterparty, kept in memory and drained by api's
	// flusher. Separate from transferStats on purpose — that measures the swarm's
	// behaviour, this measures the wire.
	traffic *trafficTable

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

	// The reactive down-mark's two pieces of in-memory state (reachability.go,
	// §Availability "Reactive down-mark + the ping floor"). contactMu guards the
	// relative self-guard: WHO last answered us and WHEN, which is what tells
	// "one node is silent while others answer" (evidence about it) from
	// "everything is silent" (evidence about us). floorPinged is the ping
	// floor's own attempt clock, so a node that neither answers nor earns a mark
	// is still retried once per cycle rather than once a minute.
	contactMu    sync.Mutex
	lastReplyAt  time.Time
	lastReplyKey string
	floorPinged  map[string]time.Time

	// The underlay kick (reachability.go kickUnderlay): kickPeers asks the
	// transport to redial its down peerings now (Mesh.KickPeers), kickedAt
	// (unix nanos) throttles it to one kick per Intervals.Kick. A func rather
	// than a call through n.mesh so the narrow tests — which assemble a Node
	// with no transport — can count kicks; nil = no transport = no kick.
	kickPeers func()
	kickedAt  atomic.Int64

	// Friend-list gossip (F6, gossip_node.go). signKey is this node's ed25519
	// private key — the same identity the mesh address derives from — used to
	// sign the records it publishes, since a relayed record cannot lean on the
	// connection it arrived over. graphAccept rate-limits how often one origin
	// may push a new sequence at us.
	signKey     ed25519.PrivateKey
	acceptMu    sync.Mutex
	graphAccept map[string]time.Time

	// token is the capability token this node presents outbound (token.go,
	// §"The household") — nil on a server, which needs none. It is the missing
	// middle of F7 item 9: issuing and verifying were both built, and nothing
	// ever put the header on a request.
	token TokenSource

	// Our community (F7, membership.go): the keys we serve the Madnetwork scope
	// to, derived from the gossiped graph by the mutual-edge walk and indexed by
	// mesh address. Memoized because it is on every mesh request's path, and
	// recomputed on the sweep from the same peers+edges the retention walk reads.
	memberMu sync.Mutex
	members  *memberSet

	// Branch attribution for the browse (F7 item 10, branches.go): which direct
	// friends each reachable node speaks through, so popularity can be counted
	// per branch instead of per key. Same walk as the network map, memoized on
	// the same TTL as the community and for the same reason — it is read once
	// per browse request and changes only on the sweep.
	branchMu sync.Mutex
	branches *branchMemo
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
		// Above yggdrasil's minimumBackoffLimit (5 s), so a kicked peering is
		// never redialled harder than the floor its own knob allows.
		Kick: 10 * time.Second,
	}
	// The frontier: four member catalogs per 15-minute cycle reaches ~16 new
	// nodes an hour, which fills the cap in about half a day without ever
	// dialling in a storm; the cap holds a few hundred foreign libraries, which
	// is megabytes, not gigabytes. Both are first guesses meant to be tuned
	// against a real network — see §Open questions 2.
	defaultDiscovery = Discovery{Budget: 4, Cap: 200}
	defaultTimeouts  = Timeouts{
		Control:  15 * time.Second,
		Manifest: 20 * time.Second,
		// A mesh dial that has not completed in 5 s is into a hole in the network,
		// not merely a distant node: the underlay is already connected, so this
		// covers routing and one TCP handshake over it. Kept well under ChunkStall
		// so the two failures stay distinguishable in a readout.
		Connect:    5 * time.Second,
		ChunkStall: 20 * time.Second,
		PerChunk:   2 * time.Minute,
		Transfer:   30 * time.Minute,
		Retry:      500 * time.Millisecond,
	}
)

// Start serves the federation protocol on [MeshPort] of the node's mesh address
// and brings the friendship layer up. store persists the trusted-peer table (nil
// disables friendship — F0 behaviour, used by narrow tests). The returned Node
// must be Stop()ed on shutdown. logger receives yggdrasil's info/warn/error
// output and the node's own friendship events.
//
// The transport underneath comes from [WithMesh] when the caller already has one
// (the server starts the mesh first, because [[listen_mesh]] can be served
// without federation at all); otherwise Start brings one up from fc's own
// key_file/peers/listen. Either way Node.Stop stops it.
func Start(fc config.FederationConfig, store PeerStore, logger *log.Logger, opts ...Option) (*Node, error) {
	var o nodeOptions
	for _, opt := range opts {
		opt(&o)
	}
	mesh := o.mesh
	if mesh == nil {
		var err error
		mesh, err = StartTransport(config.YggdrasilConfig{
			KeyFile: fc.KeyFile,
			Peers:   fc.Peers,
			Listen:  fc.Listen,
		}, logger)
		if err != nil {
			return nil, err
		}
	}
	lis, err := mesh.ListenMesh(MeshPort)
	if err != nil {
		mesh.Stop()
		return nil, err
	}

	name := CleanPeerName(fc.Name)
	if name == "" {
		if host, err := os.Hostname(); err == nil {
			name = CleanPeerName(host)
		}
	}
	// The config layer of the member budget; runtime overrides are laid over it
	// on every limit refresh (quota.go, rates.go).
	cfgQuota := QuotaLimits{
		MemberRateKiB:         fc.MemberRateKiB,
		PerMemberRateKiB:      fc.PerMemberRateKiB,
		MemberMaxTransfers:    fc.MemberMaxTransfers,
		PerMemberMaxTransfers: fc.PerMemberMaxTransfers,
	}
	transferCtx, transferCancel := context.WithCancel(context.Background())
	n := &Node{
		signKey:        mesh.signKey,
		graphAccept:    map[string]time.Time{},
		intervals:      o.intervals.withDefaults(defaultIntervals),
		timeouts:       o.timeouts.withDefaults(defaultTimeouts),
		discovery:      o.discovery.withDefaults(defaultDiscovery),
		mesh:           mesh,
		store:          store,
		name:           name,
		logger:         logger,
		nudge:          make(chan struct{}, 1),
		attempts:       map[string]PairAttempt{},
		pullNow:        map[string]struct{}{},
		cacheDir:       o.cacheDir,
		resolveBlob:    o.resolveBlob,
		token:          o.token,
		transfers:      map[string]*transfer{},
		transferCtx:    transferCtx,
		transferCancel: transferCancel,
		upRate:         &adjustableRate{},
		downRate:       &adjustableRate{},
		cfgUpKiB:       fc.SeedRateKiB,
		cfgDownKiB:     fc.FetchRateKiB,
		cfgQuota:       cfgQuota,
		limitResolver:  o.limitResolver,
		quotas:         newQuotas(cfgQuota),
		manifests:      map[string]*manifestEntry{},
		lastTouch:      map[int64]time.Time{},
		floorPinged:    map[string]time.Time{},
		traffic:        newTrafficTable(),
	}
	// Self-health signal: the netstack inbound reader's liveness (the unambiguous
	// signal — a self-ping can't test it, HandleLocal loops local traffic inside
	// gVisor). When it reports dead, the merged browse fails open rather than
	// blanking the view (docs/architecture/federation.md §Availability).
	n.readerAlive = mesh.InboundReaderAlive
	n.kickPeers = mesh.KickPeers
	// Apply the config caps up front so the very first request is limited by what
	// the file says, without waiting for the first override refresh to resolve.
	n.upRate.set(int64(fc.SeedRateKiB) * 1024)
	n.downRate.set(int64(fc.FetchRateKiB) * 1024)
	n.client = &http.Client{
		Transport: n.present(&http.Transport{DialContext: n.dialMesh}),
		Timeout:   n.timeouts.Control,
	}
	// Blob/chunk fetches can be large and slow over the mesh; each is bounded by
	// its own context plus an idle-read watchdog (readStall), so this client
	// carries no global timeout — but a ResponseHeaderTimeout catches a holder
	// that accepts the connection then never answers (a hung mesh path) fast. It
	// shares the idle-read budget: both detect the same "connected but silent"
	// failure, only on different sides of the response header.
	//
	// The DIAL is bounded separately and much sooner (F9 item 3): neither of those
	// two covers a holder that never connects at all, which is what a stale
	// advertisement is.
	n.blobClient = &http.Client{Transport: n.present(&http.Transport{
		DialContext:           n.dialHolder,
		ResponseHeaderTimeout: n.timeouts.ChunkStall,
	})}
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
	// The protocol listener is down; the transport goes with it, since Start
	// adopted whichever Mesh it was given (mesh.go, "Ownership").
	n.mesh.Stop()
}

// Mesh returns the transport this node runs on, so a caller that let Start build
// it can still serve its own [[listen_mesh]] listeners on the same address.
func (n *Node) Mesh() *Mesh { return n.mesh }

// Address returns the node's self-certifying mesh IPv6 address (200::/7).
func (n *Node) Address() net.IP { return n.mesh.Address() }

// PublicKeyHex returns the node's ed25519 public key — its madnetwork identity
// — as lowercase hex.
func (n *Node) PublicKeyHex() string { return n.mesh.PublicKeyHex() }

// Name returns the node-card display name ([federation].name, hostname
// fallback) — the label the merged browse uses for the self holder.
func (n *Node) Name() string { return n.name }

// DialContext dials through the mesh (for http.Transport.DialContext), so
// outbound protocol calls reach peers' mesh listeners without any TUN.
func (n *Node) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return n.mesh.DialContext(ctx, network, address)
}

// dialHolder is DialContext under [Timeouts].Connect — the transfer path's
// dialer (F9 item 3).
//
// It is a separate deadline rather than a smaller PerChunk because the two
// answer different questions. Once bytes are moving, a chunk may legitimately
// take minutes over a slow multi-hop path; before they are, a dial that has not
// completed in seconds is into a hole in the mesh and every further second is
// pure waste. Nothing else in the request had a bound on it: ResponseHeaderTimeout
// starts only after the request is written, and readStall's watchdog only after
// the response header arrives.
//
// The timeout governs the dial alone. Cancelling it once the connection exists
// does not disturb the connection — the netstack's DialContext, like net.Dialer's,
// reads ctx for the handshake and not for the conn's lifetime.
func (n *Node) dialHolder(ctx context.Context, network, address string) (net.Conn, error) {
	if n.timeouts.Connect <= 0 {
		return n.dialMesh(ctx, network, address)
	}
	dctx, cancel := context.WithTimeout(ctx, n.timeouts.Connect)
	defer cancel()
	return n.dialMesh(dctx, network, address)
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
	mux.HandleFunc("GET /madnetwork/v0/have/{hash}", n.handleHave)
	mux.HandleFunc("POST /madnetwork/v0/announce", n.handleAnnounce)
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
