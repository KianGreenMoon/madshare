//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

// Helpers for the chaos scenario suite (docs/plans/mesh-testing.md §Phase T2).
// They are the faulted counterparts of startNodePair/startNodeTrio: the same
// real yggdrasil cores over the same loopback TCP underlay, with a netfault
// proxy spliced into each peering so the link can be degraded, partitioned or
// cut while the mesh is running.
//
// This file carries no build tag beyond !nofederation on purpose (§Gating): the
// scenarios must keep compiling against federation internals on every default
// `go test ./...`, and are kept from *running* by requireChaos.

// requireChaos skips unless MADSHARE_CHAOS is set. Every scenario in
// chaos_test.go must call it first — a scenario that forgets makes the default
// `go test ./...` minutes-slow, which is the mirror image of the rot that build
// tags cause.
func requireChaos(t *testing.T) {
	t.Helper()
	if os.Getenv("MADSHARE_CHAOS") == "" {
		t.Skip("chaos scenario; set MADSHARE_CHAOS=1 to run")
	}
}

// The shrunk clock every scenario runs on. Expressed in testTimeoutScale units,
// like the mesh deadlines, so -race stretches them along with everything else.
// These are what T1's seams exist for: a scenario that waited out the
// production 15-minute catalog sync or 2-minute per-chunk backstop would take
// longer to fail than to write.
const (
	chaosRefresh    = 200 * time.Millisecond * testTimeoutScale
	chaosSnapshot   = 50 * time.Millisecond * testTimeoutScale
	chaosControl    = 2 * time.Second * testTimeoutScale
	chaosChunkStall = 2 * time.Second * testTimeoutScale
	chaosPerChunk   = 6 * time.Second * testTimeoutScale
	// The whole-file (F3 fallback) backstop. When every holder is unreachable a
	// fetch runs the per-chunk budget down several times over and *then* falls
	// back to this path, whose dial goes into a mesh with no route — ChunkStall
	// does not bound that, since a dial which never connects never reaches a
	// response header. Hence chaosDeadline below rather than meshDeadline.
	chaosTransfer = 6 * time.Second * testTimeoutScale

	// drainQuiet is how long liveness must stay still before a scenario trusts a
	// post-partition baseline. Wall-clock, not scaled: it covers a late store
	// write, and last_seen has one-second granularity either way.
	drainQuiet = 4 * time.Second

	// chaosDeadline bounds a scenario's transfer. It is deliberately much larger
	// than meshDeadline (which bounds convergence polling): a chaos transfer is
	// *supposed* to hit timeouts and retries, and budgeting it like a healthy one
	// would only assert that the shrunk clock is smaller than itself. The real
	// budgets are the per-scenario ones.
	chaosDeadline = 90 * time.Second * testTimeoutScale
)

// chaosOpts prepends the shrunk clock to a node's options. Scenario-specific
// options (resolvers, cache dirs) follow and are free to override nothing —
// intervals and timeouts are separate Options, so they never collide.
func chaosOpts(extra ...Option) []Option {
	return append([]Option{
		WithIntervals(Intervals{
			Refresh:     chaosRefresh,
			CatalogSync: chaosRefresh,
			SnapshotTTL: chaosSnapshot,
		}),
		WithTimeouts(Timeouts{
			Control:    chaosControl,
			Manifest:   chaosChunkStall,
			ChunkStall: chaosChunkStall,
			PerChunk:   chaosPerChunk,
			Transfer:   chaosTransfer,
		}),
	}, extra...)
}

// ── Faulted topologies ───────────────────────────────────────────────────────
//
// Orientation matters and is fixed here so every scenario can reason about it:
// **the fetcher always dials, the seeder always listens**. A netfault proxy sits
// in front of each seeder's underlay listener and the fetcher peers to the
// proxy, so for every link
//
//	Up   = fetcher → seeder (requests)
//	Down = seeder → fetcher (blob bytes)
//
// which is why the fault builders below degrade Down when they mean "a slow
// seeder". The trio deliberately does NOT reuse startNodeTrio's hub shape (B and
// C both dialing A): there the two seeders share one underlay link, so they
// cannot be faulted independently — the whole point of the suite.

// startFaultedPair starts a seeder (a, listening) and a fetcher (b, dialing)
// whose single peering runs through the returned proxy.
func startFaultedPair(t *testing.T, storeA, storeB *memStore, optsA, optsB []Option) (*Node, *Node, *netfault.Proxy) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	real := reserveUnderlay(t)
	link := newLink(t, real)

	a := startChaosNode(t, "a", dir, storeA, logger, config.FederationConfig{Listen: []string{"tcp://" + real}}, optsA)
	b := startChaosNode(t, "b", dir, storeB, logger, config.FederationConfig{Peers: []string{"tcp://" + link.Addr()}}, optsB)
	return a, b, link
}

// startFaultedTrio starts two seeders (a, c — both listening) and one fetcher
// (b, dialing both), each peering through its own proxy so the two sources can
// be degraded independently. a and c reach each other only via b, which no
// scenario here depends on. seedRateA/seedRateC set the seeders' built-in
// outbound cap (config seed_rate_kib, KiB/s; 0 = unlimited) — the throttle on
// the serving side, as distinct from a netfault cap on the link.
func startFaultedTrio(t *testing.T, sA, sB, sC *memStore, oA, oB, oC []Option, seedRateA, seedRateC int) (a, b, c *Node, linkA, linkC *netfault.Proxy) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	realA, realC := reserveUnderlay(t), reserveUnderlay(t)
	linkA, linkC = newLink(t, realA), newLink(t, realC)

	a = startChaosNode(t, "a", dir, sA, logger, config.FederationConfig{
		Listen: []string{"tcp://" + realA}, SeedRateKiB: seedRateA}, oA)
	c = startChaosNode(t, "c", dir, sC, logger, config.FederationConfig{
		Listen: []string{"tcp://" + realC}, SeedRateKiB: seedRateC}, oC)
	b = startChaosNode(t, "b", dir, sB, logger, config.FederationConfig{
		Peers: []string{"tcp://" + linkA.Addr(), "tcp://" + linkC.Addr()},
	}, oB)
	return a, b, c, linkA, linkC
}

func startChaosNode(t *testing.T, name, dir string, store *memStore, logger *log.Logger, fc config.FederationConfig, opts []Option) *Node {
	t.Helper()
	fc.Name, fc.KeyFile = "node-"+name, filepath.Join(dir, name+".key")
	n, err := Start(fc, store, logger, opts...)
	if err != nil {
		t.Fatalf("start node %s: %v", name, err)
	}
	t.Cleanup(n.Stop)
	return n
}

// reserveUnderlay picks a free loopback port the same way the existing helpers
// do (reserve-then-close; the window is why chaos runs are serial, -p 1).
func reserveUnderlay(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	return addr
}

// reserveUnderlayUDP is the datagram counterpart. TCP and UDP port spaces are
// separate, so this must probe the one it will actually bind.
func reserveUnderlayUDP(t *testing.T) string {
	t.Helper()
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()
	return addr
}

// newLink starts a transparent proxy in front of target; scenarios degrade it
// with Set/Script once the mesh has converged.
func newLink(t *testing.T, target string) *netfault.Proxy {
	t.Helper()
	p, err := netfault.New(target, netfault.Fault{})
	if err != nil {
		t.Fatalf("netfault proxy for %s: %v", target, err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// newDatagramLink is newLink for a quic:// peering.
func newDatagramLink(t *testing.T, target string) *netfault.UDPProxy {
	t.Helper()
	p, err := netfault.NewUDP(target, netfault.DatagramFault{})
	if err != nil {
		t.Fatalf("netfault datagram proxy for %s: %v", target, err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// ── QUIC topologies (T3) ─────────────────────────────────────────────────────
//
// The same shapes over a quic:// underlay, which is what makes loss, reordering
// and duplication expressible at all: on a TCP underlay the kernel repairs them
// before yggdrasil ever sees them, so a "5 % loss" knob there would be fiction
// (docs/plans/mesh-testing.md §Known gotchas). Orientation is unchanged — the
// fetcher dials, the seeders listen, Down carries blob bytes.
//
// The links start transparent on purpose. QUIC's handshake and yggdrasil's
// pairing are the least interesting things a lossy path can break, and letting a
// scenario's setup fail probabilistically would make every one of them flaky for
// reasons that have nothing to do with what they assert. Converge first, then
// degrade.

// startQUICPair starts a seeder (a, listening) and a fetcher (b, dialing) whose
// single quic:// peering runs through the returned datagram proxy.
func startQUICPair(t *testing.T, storeA, storeB *memStore, optsA, optsB []Option) (*Node, *Node, *netfault.UDPProxy) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	real := reserveUnderlayUDP(t)
	link := newDatagramLink(t, real)

	a := startChaosNode(t, "a", dir, storeA, logger, config.FederationConfig{Listen: []string{"quic://" + real}}, optsA)
	b := startChaosNode(t, "b", dir, storeB, logger, config.FederationConfig{Peers: []string{"quic://" + link.Addr()}}, optsB)
	return a, b, link
}

// startQUICTrio is startFaultedTrio over quic://: two seeders (a, c), one
// fetcher (b) dialing both through its own datagram proxy per link.
func startQUICTrio(t *testing.T, sA, sB, sC *memStore, oA, oB, oC []Option) (a, b, c *Node, linkA, linkC *netfault.UDPProxy) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	realA, realC := reserveUnderlayUDP(t), reserveUnderlayUDP(t)
	linkA, linkC = newDatagramLink(t, realA), newDatagramLink(t, realC)

	a = startChaosNode(t, "a", dir, sA, logger, config.FederationConfig{Listen: []string{"quic://" + realA}}, oA)
	c = startChaosNode(t, "c", dir, sC, logger, config.FederationConfig{Listen: []string{"quic://" + realC}}, oC)
	b = startChaosNode(t, "b", dir, sB, logger, config.FederationConfig{
		Peers: []string{"quic://" + linkA.Addr(), "quic://" + linkC.Addr()},
	}, oB)
	return a, b, c, linkA, linkC
}

// ── Fault builders ───────────────────────────────────────────────────────────

// slowDown caps the seeder→fetcher direction (see the orientation note above).
func slowDown(bytesPerSec int64) netfault.Fault {
	return netfault.Fault{Down: netfault.Dir{Bandwidth: bytesPerSec}}
}

// rtt splits a round-trip time across the two directions.
func rtt(d time.Duration) netfault.Fault {
	half := d / 2
	return netfault.Fault{
		Up:   netfault.Dir{Latency: half},
		Down: netfault.Dir{Latency: half},
	}
}

// partitioned is the cut link: new dials refused, live connections killed.
var partitioned = netfault.Fault{Partition: true}

// ── Datagram fault builders (T3) ─────────────────────────────────────────────

// lossy drops the given share of datagrams in BOTH directions. Both, because a
// one-sided drop rate is not a thing a path does: loss is a property of the
// wire, and QUIC's recovery only means anything when its acknowledgements are
// exposed to the same weather as the data they acknowledge. A Down-only rate
// would let every retransmission through unharmed and quietly make the scenario
// easier than it claims to be.
func lossy(rate float64) netfault.DatagramFault {
	return netfault.DatagramFault{
		Up:   netfault.DatagramDir{Loss: rate},
		Down: netfault.DatagramDir{Loss: rate},
	}
}

// scrambled reorders and duplicates datagrams without dropping any — the two
// faults a reliable stream hides completely, isolated from loss so a failure
// cannot be blamed on missing bytes.
func scrambled(reorder, duplicate float64, delay time.Duration) netfault.DatagramFault {
	d := netfault.DatagramDir{Reorder: reorder, ReorderDelay: delay, Duplicate: duplicate}
	return netfault.DatagramFault{Up: d, Down: d}
}

// There is no datagramPartition builder to match `partitioned` because nothing
// needs one yet: netfault.DatagramFault{Partition: true} is the whole of it, and
// a cut path is already covered on the stream side, where it is the harder case
// (TCP must redial; a datagram heal resumes the QUIC connection as it stands).

// ── Scenario plumbing ────────────────────────────────────────────────────────

// friendsHolding wires the common setup: A publishes content, B befriends it and
// gets a cached catalog row advertising the hash. Returns the hash.
func friendsHolding(t *testing.T, a, b *Node, storeA, storeB *memStore, content []byte) string {
	t.Helper()
	makeFriends(t, a, b, storeA, storeB)
	hash := hashOf(content)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	return hash
}

// awaitProgress waits until a transfer has read at least want bytes contiguously,
// so a scenario can degrade the link *mid*-transfer rather than before it. It
// returns false if the transfer ended first.
func awaitProgress(t *testing.T, tr Transfer, want int64) bool {
	t.Helper()
	deadline := time.Now().Add(chaosDeadline)
	for {
		if tr.Progress() >= want {
			return true
		}
		select {
		case <-tr.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("transfer never reached %d bytes (stuck at %d)", want, tr.Progress())
		}
	}
}

// awaitTransfer waits for a transfer to end within chaosDeadline and returns its
// error (nil = success). The per-scenario budget is asserted separately — this
// one only distinguishes "slow, as expected" from "hung".
func awaitTransfer(t *testing.T, tr Transfer) error {
	t.Helper()
	select {
	case <-tr.Done():
		return tr.Err()
	case <-time.After(chaosDeadline):
		t.Fatalf("transfer did not finish within %v\n%s", chaosDeadline, describe(tr.Stats()))
		return nil
	}
}

// pingOK reports whether one node can reach another's protocol ping over the
// mesh right now — the cheapest "is this link carrying traffic" probe there is.
func pingOK(ctx context.Context, from, to *Node) bool {
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", to.Address(), MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Transport: &http.Transport{DialContext: from.DialContext}}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// lastSeenOf reads a peer's stored liveness timestamp from the fetcher's store.
func lastSeenOf(t *testing.T, store *memStore, of *Node) int64 {
	t.Helper()
	p, err := store.GetFederationPeerByKey(context.Background(), of.PublicKeyHex())
	if err != nil {
		t.Fatalf("peer lookup: %v", err)
	}
	return p.LastSeen
}

// settleLastSeen returns a peer's last_seen once it has stopped moving for
// quiet. Cutting a link does not stop liveness writes instantly: pingPeer
// timestamps the *store write*, not the reply, so an exchange that succeeded
// just before the cut can land seconds later — reliably so under -race, where
// the goroutine may not be scheduled again for a while. A baseline taken
// straight after a partition is therefore racy; this one is not.
func settleLastSeen(t *testing.T, store *memStore, of *Node, quiet time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(meshDeadline)
	last, since := lastSeenOf(t, store, of), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("last_seen never stopped moving (still advancing at %d) — is the link really cut?", last)
		}
		time.Sleep(100 * time.Millisecond)
		if v := lastSeenOf(t, store, of); v != last {
			last, since = v, time.Now()
		}
	}
	return last
}

// providerBytes pulls one holder's byte count out of a transfer snapshot.
func providerBytes(st TransferStats, of *Node) int64 {
	for _, p := range st.Providers {
		if p.PublicKey == of.PublicKeyHex() {
			return p.Bytes
		}
	}
	return 0
}

// ── Datagram assertions (T3) ─────────────────────────────────────────────────

// describeLink renders a datagram relay's counters. Every QUIC scenario logs
// them next to the transfer stats: the two together are what distinguish "the
// swarm coped with a hostile path" from "the path was never hostile".
func describeLink(p *netfault.UDPProxy) string {
	s := p.Stats()
	seen := s.Lost + s.PacketsUp + s.PacketsDown
	var rate float64
	if seen > 0 {
		rate = float64(s.Lost) / float64(seen)
	}
	return fmt.Sprintf("link: flows=%d packets=%d/%d bytes=%d/%d lost=%d (%.1f%%) "+
		"reordered=%d duplicated=%d overflowed=%d refused=%d",
		s.Flows, s.PacketsUp, s.PacketsDown, s.BytesUp, s.BytesDown,
		s.Lost, rate*100, s.Reordered, s.Duplicated, s.Overflowed, s.Refused)
}

// assertLossy checks the path was as hostile as the scenario claims. Without
// this a "survived 15 % loss" result is indistinguishable from a link that
// dropped nothing — the most dangerous way for a fault suite to be green.
//
// It also guards the other end: the relay's own transmit queue drops when it
// overflows, and drops the scenario did not ask for would make the measured rate
// (and any conclusion drawn from it) something other than what was configured.
func assertLossy(t *testing.T, p *netfault.UDPProxy, want float64) {
	t.Helper()
	s := p.Stats()
	seen := s.Lost + s.PacketsUp + s.PacketsDown
	if s.Lost == 0 {
		t.Fatalf("the link dropped nothing across %d datagrams — the loss knob is not "+
			"wired up, so this scenario proved nothing\n%s", seen, describeLink(p))
	}
	if got := float64(s.Lost) / float64(seen); got < want/2 {
		t.Errorf("measured loss %.3f against a configured %.2f — the path was far "+
			"gentler than the scenario claims\n%s", got, want, describeLink(p))
	}
	if s.Overflowed > s.Lost/10 {
		t.Errorf("%d datagrams were dropped by relay queue overflow, not by the loss "+
			"knob — the injector is adding weather of its own\n%s", s.Overflowed, describeLink(p))
	}
}

// assertScrambled checks reordering and duplication actually happened, and that
// no loss crept in — the scenario's whole argument is that a chunk failure could
// only mean misordered reassembly, which requires the bytes to all be there.
func assertScrambled(t *testing.T, name string, p *netfault.UDPProxy) {
	t.Helper()
	s := p.Stats()
	if s.Reordered == 0 {
		t.Errorf("link %s reordered nothing — the knob is not wired up\n%s", name, describeLink(p))
	}
	if s.Duplicated == 0 {
		t.Errorf("link %s duplicated nothing — the knob is not wired up\n%s", name, describeLink(p))
	}
	if s.Lost > 0 || s.Overflowed > 0 {
		t.Errorf("link %s lost %d datagrams (%d to queue overflow) in a scenario that "+
			"injects no loss — a chunk failure could no longer be blamed on ordering alone\n%s",
			name, s.Lost, s.Overflowed, describeLink(p))
	}
}

// describe renders a stats snapshot for a failure message — every chaos failure
// wants to know who carried what.
func describe(st TransferStats) string {
	mode := st.Mode
	// A fallback resets the live counters, so name the path that was actually
	// walked — "swarm→whole" reads very differently from a bare "whole".
	for i := len(st.Prior) - 1; i >= 0; i-- {
		if st.Prior[i].Mode != "" {
			mode = st.Prior[i].Mode + "→" + mode
		}
	}
	s := fmt.Sprintf("mode=%s ttfb=%v elapsed=%v chunks=%d/%d retries=%d failovers=%d stalls=%d corrupt=%d",
		mode, st.FirstByte, st.Elapsed, st.ChunksDone, st.Chunks,
		st.Retries, st.Failovers, st.Stalls, st.Corrupt)
	for _, a := range st.Prior {
		s += fmt.Sprintf("\n  [abandoned %s] ttfb=%v chunks=%d/%d",
			a.Mode, a.FirstByte, a.ChunksDone, a.Chunks)
	}
	for _, p := range st.Providers {
		s += fmt.Sprintf("\n  %-8s bytes=%-9d chunks=%-3d failures=%d dropped=%v %s",
			p.Name, p.Bytes, p.Chunks, p.Failures, p.Dropped, p.LastError)
	}
	return s
}
