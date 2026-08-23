//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

// The balance run (docs/plans/swarm-lab.md §Family 1): the swarm's
// decentralization claim, asserted. A well-behaved swarm depends on exactly one
// thing — the quality of the connections it is actually using. These scenarios
// pin the rest of the plausible influences at zero: graph position is free,
// dead holders are a bounded tax, and holder knowledge spreads far enough that
// content outlives the node that introduced it.
//
// Same rules as the rest of the chaos suite (tests/mesh/README.md): gated by
// requireChaos, shrunk clock via chaosOpts, budgets in testTimeoutScale units,
// faults themselves never scaled, and every conclusion backed by TransferStats
// rather than by the transfer merely finishing.

// startFaultedSwarm starts len(holderStores) holders, each listening behind its
// OWN netfault proxy, and one fetcher dialling all of them — startNodeQuad's
// shape (staleholders_test.go) with startFaultedPair's faultable links. Each
// holder gets its own link because the balance scenarios are about holders
// differing in exactly one, independently controlled way.
func startFaultedSwarm(t *testing.T, holderStores []*memStore, fetcherStore *memStore,
	hOpts [][]Option, fOpts []Option) (holders []*Node, fetcher *Node, links []*netfault.Proxy) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	var peers []string
	for i, store := range holderStores {
		real := reserveUnderlay(t)
		link := newLink(t, real)
		links = append(links, link)
		peers = append(peers, "tcp://"+link.Addr())
		name := fmt.Sprintf("holder%d", i)
		fc := config.FederationConfig{
			Name: name, KeyFile: filepath.Join(dir, name+".key"),
			Listen: []string{"tcp://" + real},
		}
		n, err := Start(fc, store, logger, hOpts[i]...)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(n.Stop)
		holders = append(holders, n)
	}

	fc := config.FederationConfig{
		Name: "fetcher", KeyFile: filepath.Join(dir, "fetcher.key"), Peers: peers,
	}
	fetcher, err := Start(fc, fetcherStore, logger, fOpts...)
	if err != nil {
		t.Fatalf("start fetcher: %v", err)
	}
	t.Cleanup(fetcher.Stop)
	return holders, fetcher, links
}

// awaitManifests blocks until the fetcher can pull a manifest from every
// provider in the plan — the "mesh has converged, sessions are warm" baseline
// every balance scenario needs before it may degrade anything.
func awaitManifests(t *testing.T, fetcher *Node, plan []*BlobProvider, hash string) {
	t.Helper()
	ctx := context.Background()
	waitFor(t, "every holder to be reachable", func() bool {
		for _, p := range plan {
			if fetcher.fetchManifest(ctx, p, hash) == nil {
				return false
			}
		}
		return true
	})
}

// TestChaosBalanceFollowsBandwidth: three holders carrying the same blob behind
// links capped 1:2:4. The byte split must follow capacity — and only capacity:
// the thin holder is deprioritized, never punished, and the aggregate must beat
// the best single pipe, which is the whole point of fetching from a swarm.
func TestChaosBalanceFollowsBandwidth(t *testing.T) {
	requireChaos(t)

	// 3 MiB → 256 KiB bulk chunks → 12 chunks: enough grain to read a split
	// from. The caps keep a 256 KiB chunk comfortably inside chaosPerChunk even
	// with two requests outstanding on the thin link (512 KiB at 128 KiB/s = 4 s
	// against 6 s) — a cap that starved a chunk past the deadline would turn
	// "slow" into "failing" and change the scenario's character (README,
	// Troubleshooting: pick rates that behave at both clock scales).
	const blobSize = 3 << 20
	caps := []int64{128 << 10, 256 << 10, 512 << 10}

	content := fillBytes(blobSize)
	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	fetcherStore := newMemStore()
	hOpts := make([][]Option, len(stores))
	var hash string
	for i, s := range stores {
		h, resolver := publishBlob(t, s, content)
		hash = h
		hOpts[i] = chaosOpts(resolver)
	}
	cacheDir := t.TempDir()
	holders, fetcher, links := startFaultedSwarm(t, stores, fetcherStore, hOpts,
		chaosOpts(WithCacheDir(cacheDir)))
	for i, h := range holders {
		makeFriends(t, h, fetcher, stores[i], fetcherStore)
	}

	plan := make([]*BlobProvider, len(holders))
	for i, h := range holders {
		plan[i] = &BlobProvider{
			Name:      fmt.Sprintf("cap%dK", caps[i]>>10),
			PublicKey: h.PublicKeyHex(),
		}
	}
	awaitManifests(t, fetcher, plan, hash)

	// Degrade AFTER convergence, always.
	for i, l := range links {
		l.Set(slowDown(caps[i]))
	}

	ctx := context.Background()
	start := time.Now()
	tr, err := fetcher.EnsureBlobFrom(ctx, hash, blobSize, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	elapsed := time.Since(start)
	st := tr.Stats()
	t.Logf("balance across 1:2:4 pipes, elapsed %v\n%s", elapsed.Round(time.Millisecond), describe(st))

	assertCached(t, cacheDir, hash, content)
	if st.Corrupt != 0 {
		t.Errorf("corrupt=%d on three honest holders\n%s", st.Corrupt, describe(st))
	}
	for _, p := range st.Providers {
		if p.Dropped {
			t.Errorf("holder %s was retired for being slow — a thin pipe is deprioritized, never punished\n%s",
				p.Name, describe(st))
		}
	}

	// The split follows capacity. Ideal shares are 1/7, 2/7 and 4/7; the
	// tolerances leave room for chunk granularity (1/12 each) and endgame
	// hedging, which is stochastic by design — but not for a scheduler that has
	// stopped reading throughput.
	//
	// Asserted at scale 1 ONLY: under -race the netstack's CPU-bound loopback
	// throughput (~35 KiB/s per holder, measured on the first -race run of this
	// file) sits below every cap here, so the link shaping stops being the
	// limiting factor and the split honestly collapses toward even — the
	// scheduler is still right, the FAULT has vanished. Caps low enough to bind
	// under -race would starve a chunk past PerChunk at scale 1 and turn "slow"
	// into "failing" (README, Troubleshooting — the cap/scale rule, met from a
	// third direction). Completion, integrity and no-retirement hold at every
	// scale above.
	if testTimeoutScale == 1 {
		slow, fast := providerBytes(st, holders[0]), providerBytes(st, holders[2])
		var total int64
		for _, p := range st.Providers {
			total += p.Bytes
		}
		if total == 0 {
			t.Fatalf("no provider bytes recorded\n%s", describe(st))
		}
		if fast <= slow {
			t.Errorf("the 4x pipe carried %d bytes against the 1x pipe's %d — the split is not following capacity\n%s",
				fast, slow, describe(st))
		}
		if fast*100 < total*35 {
			t.Errorf("the fastest holder carried %d of %d bytes (want ≥35%% against a 57%% ideal)\n%s",
				fast, total, describe(st))
		}
		if slow*100 > total*25 {
			t.Errorf("the slowest holder carried %d of %d bytes (want ≤25%% against a 14%% ideal)\n%s",
				slow, total, describe(st))
		}
	} else {
		t.Logf("split-ratio assertions skipped at scale %d — the caps do not bind under -race", testTimeoutScale)
	}

	// The aggregate claim: three pipes together must beat the best of them
	// alone — size/fastest-cap is what a single-source fetch from the fat
	// holder would need at best. Scaled because the budget is time WE wait on;
	// the caps themselves are the fault and stay fixed (README, Troubleshooting).
	budget := time.Duration(blobSize/(512<<10)) * time.Second * testTimeoutScale
	if elapsed > budget {
		t.Errorf("elapsed %v against %v (the best single pipe's floor) — the swarm is not aggregating its pipes\n%s",
			elapsed.Round(time.Millisecond), budget, describe(st))
	}
}

// TestChaosBalanceIgnoresUnderlayDistance: a holder two routed hops away with
// the fatter pipe must out-carry the adjacent holder with the thin one.
// `meshlab reach` measured that DISTANCE is flat for a single fetch; this is
// the same finding as a SCHEDULING claim — the swarm must not prefer near over
// fast, because on a mesh the two are unrelated.
func TestChaosBalanceIgnoresUnderlayDistance(t *testing.T) {
	requireChaos(t)

	const blobSize = 3 << 20
	content := fillBytes(blobSize)
	storeNear, storeMid, storeFar := newMemStore(), newMemStore(), newMemStore()
	storeF := newMemStore()
	hash, resolveNear := publishBlob(t, storeNear, content)
	_, resolveFar := publishBlob(t, storeFar, content)

	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	// fetcher —(linkNear)— near            near: one hop, capped 128 KiB/s
	// fetcher —(linkMidF)— mid —(linkFar)— far
	//                                      far: two hops + 60 ms RTT, 256 KiB/s
	// mid holds nothing and befriends nobody — it exists only to route, which
	// is exactly the role a backbone yggdrasil node plays.
	realNear, realMid := reserveUnderlay(t), reserveUnderlay(t)
	linkNear, linkMidF, linkFar := newLink(t, realNear), newLink(t, realMid), newLink(t, realMid)

	near := startChaosNode(t, "near", dir, storeNear, logger,
		config.FederationConfig{Listen: []string{"tcp://" + realNear}}, chaosOpts(resolveNear))
	startChaosNode(t, "mid", dir, storeMid, logger,
		config.FederationConfig{Listen: []string{"tcp://" + realMid}}, chaosOpts())
	far := startChaosNode(t, "far", dir, storeFar, logger,
		config.FederationConfig{Peers: []string{"tcp://" + linkFar.Addr()}}, chaosOpts(resolveFar))
	cacheDir := t.TempDir()
	fetcher := startChaosNode(t, "fetch", dir, storeF, logger,
		config.FederationConfig{Peers: []string{"tcp://" + linkNear.Addr(), "tcp://" + linkMidF.Addr()}},
		chaosOpts(WithCacheDir(cacheDir)))

	// Both holders serve the fetcher as a direct friend: friendship distance is
	// deliberately NOT the variable here (the scheduler never sees it — a fetch
	// plan is a flat list of keys); underlay distance and link quality are.
	makeFriends(t, near, fetcher, storeNear, storeF)
	makeFriends(t, far, fetcher, storeFar, storeF)

	plan := []*BlobProvider{
		{Name: "near-thin", PublicKey: near.PublicKeyHex()},
		{Name: "far-fat", PublicKey: far.PublicKeyHex()},
	}
	awaitManifests(t, fetcher, plan, hash)

	linkNear.Set(slowDown(128 << 10))
	// On linkFar the ROLES ARE REVERSED: far is the dialer on its own link (it
	// peers out to mid), so blob bytes travel client→target — the proxy's Up.
	// Down there carries the requests. The suite's orientation rule ("Down
	// carries blob bytes") is written for links the FETCHER dials; capping Down
	// here would cap nothing, and the first run of this scenario proved it
	// (far measured 1486 KiB/s through its "256 KiB/s" link).
	linkFar.Set(netfault.Fault{
		Up:   netfault.Dir{Latency: 30 * time.Millisecond, Bandwidth: 256 << 10},
		Down: netfault.Dir{Latency: 30 * time.Millisecond},
	})

	ctx := context.Background()
	tr, err := fetcher.EnsureBlobFrom(ctx, hash, blobSize, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	st := tr.Stats()
	t.Logf("near (1 hop, 128 KiB/s) vs far (2 hops + 60 ms RTT, 256 KiB/s)\n%s", describe(st))

	assertCached(t, cacheDir, hash, content)
	if st.Corrupt != 0 {
		t.Errorf("corrupt=%d on two honest holders\n%s", st.Corrupt, describe(st))
	}
	for _, p := range st.Providers {
		if p.Dropped {
			t.Errorf("holder %s was retired — neither holder here earned that\n%s", p.Name, describe(st))
		}
	}
	// Scale 1 only, same reason as TestChaosBalanceFollowsBandwidth: under
	// -race neither cap binds (loopback throughput is CPU-bound below both), so
	// the split honestly evens out and "far carries more" stops being implied
	// by anything.
	if testTimeoutScale == 1 {
		if farB, nearB := providerBytes(st, far), providerBytes(st, near); farB <= nearB {
			t.Errorf("far (fat pipe) carried %d bytes against near's %d — the scheduler is paying for "+
				"distance it was never billed for\n%s", farB, nearB, describe(st))
		}
	} else {
		t.Logf("split assertion skipped at scale %d — the caps do not bind under -race", testTimeoutScale)
	}
}

// TestChaosDeadHoldersDoNotGateTheSwarm: one live holder plus three dead ones,
// one per way of being dead — a ghost (a well-formed key nothing answers to), a
// cut node (link partitioned after convergence) and a mute one (routable once,
// 10-minute latency now). Each class fails differently on the wire; all three
// must cost the same bounded tax: at most two dispatches × Timeouts.Connect
// each (the F9 item 3 bound TestStaleHoldersCostAFetch pinned for ghosts),
// while the live holder carries the transfer.
func TestChaosDeadHoldersDoNotGateTheSwarm(t *testing.T) {
	requireChaos(t)

	const blobSize = 1 << 20
	// Two blobs: the baseline fetch and the mixed one — the cache is keyed by
	// hash, and a second fetch of the first blob would measure a cache hit.
	blobs := [][]byte{fillBytes(blobSize), fillBytes(blobSize)}
	blobs[1][0] ^= 0xFF

	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	fetcherStore := newMemStore()
	hOpts := make([][]Option, len(stores))
	var hashes []string
	for i, s := range stores {
		hs, resolver := publishBlobs(t, s, blobs)
		hashes = hs
		hOpts[i] = chaosOpts(resolver)
	}
	cacheDir := t.TempDir()
	holders, fetcher, links := startFaultedSwarm(t, stores, fetcherStore, hOpts,
		chaosOpts(WithCacheDir(cacheDir)))
	live, cut, mute := holders[0], holders[1], holders[2]
	for i, h := range holders {
		makeFriends(t, h, fetcher, stores[i], fetcherStore)
	}

	pLive := &BlobProvider{Name: "live", PublicKey: live.PublicKeyHex()}
	pCut := &BlobProvider{Name: "cut", PublicKey: cut.PublicKeyHex()}
	pMute := &BlobProvider{Name: "mute", PublicKey: mute.PublicKeyHex()}
	awaitManifests(t, fetcher, []*BlobProvider{pLive, pCut, pMute}, hashes[0])

	ctx := context.Background()

	// Baseline: the live holder alone.
	start := time.Now()
	tr, err := fetcher.EnsureBlobFrom(ctx, hashes[0], blobSize, []*BlobProvider{pLive})
	if err != nil {
		t.Fatalf("baseline EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("baseline transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	baseline := time.Since(start)

	// Now the three deaths. The faults are outages and are NOT scale-adjusted
	// (README, Troubleshooting: scale what costs us time, never what the link
	// does). The mute link's latency makes the next mesh dial's SYN undeliverable
	// in practice, which is what "a hung box" looks like from outside.
	links[1].Set(partitioned)
	links[2].Set(netfault.Fault{
		Up:   netfault.Dir{Latency: 10 * time.Minute},
		Down: netfault.Dir{Latency: 10 * time.Minute},
	})
	_, ghost := newSigner(t)
	plan := []*BlobProvider{pLive, pCut, pMute, {Name: "ghost", PublicKey: ghost}}

	start = time.Now()
	tr, err = fetcher.EnsureBlobFrom(ctx, hashes[1], blobSize, plan)
	if err != nil {
		t.Fatalf("mixed EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("one live holder was present and the fetch still failed: %v\n%s", err, describe(tr.Stats()))
	}
	mixed := time.Since(start)
	st := tr.Stats()
	t.Logf("baseline (1 live) %v; with 3 dead holders %v\n%s",
		baseline.Round(time.Millisecond), mixed.Round(time.Millisecond), describe(st))

	assertCached(t, cacheDir, hashes[1], blobs[1])
	if st.Corrupt != 0 {
		t.Errorf("corrupt=%d — nobody here serves wrong bytes\n%s", st.Corrupt, describe(st))
	}
	if b := providerBytes(st, live); b < blobSize {
		t.Errorf("the live holder delivered %d of %d bytes — someone dead was credited with bytes\n%s",
			b, blobSize, describe(st))
	}

	// The bound, per staleholders_test.go: each dead holder may absorb at most
	// two dispatches, each bounded by the dial deadline, plus one Connect of
	// scheduler slack overall. Losing the dial bound (a dispatch falls through
	// to PerChunk) or the load rule (a dead holder keeps being chosen) blows
	// this budget by construction.
	budget := baseline + time.Duration(2*3+1)*chaosConnect
	if mixed > budget {
		t.Errorf("with 3 dead holders the fetch took %v against a budget of %v (baseline %v + 6 dead "+
			"dispatches × Connect + slack) — a dead holder is gating the swarm again\n%s",
			mixed.Round(time.Millisecond), budget.Round(time.Millisecond),
			baseline.Round(time.Millisecond), describe(st))
	}
}

// TestChaosHolderSpreadSurvivesTheOrigin: holder INFORMATION spreads far enough
// that content outlives the node that introduced it. A publishes, its friend B
// materializes, and B's friend C — a stranger to A — learns of B's copy only
// passively, from the holdings its own sweep pulls. Then A is stopped, and C's
// fetch must still land byte-exact, served from B's cache. The decentralization
// headline in one sentence: a blob that has been fetched once no longer depends
// on its origin.
func TestChaosHolderSpreadSurvivesTheOrigin(t *testing.T) {
	requireChaos(t)

	const blobSize = 700 << 10
	content := fillBytes(blobSize)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB, cacheC := t.TempDir(), t.TempDir()

	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	realA, realB := reserveUnderlay(t), reserveUnderlay(t)

	// A is started by hand rather than via startChaosNode: this scenario stops
	// it mid-test, and Node.Stop is not a call to make twice.
	fcA := config.FederationConfig{
		Name: "origin", KeyFile: filepath.Join(dir, "a.key"),
		Listen: []string{"tcp://" + realA},
	}
	a, err := Start(fcA, storeA, logger, chaosOpts(resolveA)...)
	if err != nil {
		t.Fatalf("start origin: %v", err)
	}
	var stopped sync.Once
	stopA := func() { stopped.Do(a.Stop) }
	t.Cleanup(stopA)

	b := startChaosNode(t, "b", dir, storeB, logger, config.FederationConfig{
		Listen: []string{"tcp://" + realB}, Peers: []string{"tcp://" + realA},
	}, chaosOpts(WithCacheDir(cacheB)))
	c := startChaosNode(t, "c", dir, storeC, logger, config.FederationConfig{
		Peers: []string{"tcp://" + realB},
	}, chaosOpts(WithCacheDir(cacheC)))

	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, b, c, storeB, storeC)

	// B materializes the blob — a real fetch into its download cache.
	ctx := context.Background()
	seedBlobCatalog(t, storeB, a, hash, int64(blobSize))
	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("B EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("B's transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	assertCached(t, cacheB, hash, content)

	// C learns of B's copy PASSIVELY: its own sweep pulls B's holdings on the
	// (shrunk) catalog cadence. Nothing seeds C's store by hand — the spread is
	// the thing under test.
	waitFor(t, "C to learn that B holds the blob", func() bool {
		_, provs, err := storeC.MadnetworkBlobProviders(ctx, hash)
		if err != nil {
			return false
		}
		for _, p := range provs {
			if p.PublicKey == b.PublicKeyHex() {
				return true
			}
		}
		return false
	})

	// The origin goes away — off, not merely unreachable.
	stopA()

	tr, err = c.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("C EnsureBlob with the origin down: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("C's fetch failed with the origin down: %v\n%s", err, describe(tr.Stats()))
	}
	st := tr.Stats()
	t.Logf("fetched from the second-generation holder with the origin stopped\n%s", describe(st))
	assertCached(t, cacheC, hash, content)
	if b := providerBytes(st, b); b == 0 {
		t.Errorf("B is the only live holder and served no bytes — who did?\n%s", describe(st))
	}
}
