//go:build !nofederation

package federation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

// The chaos scenario suite (docs/plans/mesh-testing.md §Phase T2): real
// yggdrasil meshes over a loopback TCP underlay with a netfault proxy spliced
// into each peering, so the swarm's claims can be asserted under a link that is
// slow, latent, cut, or flapping.
//
// Every scenario calls requireChaos first, so `go test ./...` skips them all
// while still compiling every line against current federation internals — the
// suite's job is catching regressions in this package, so it must not be the
// thing that rots. Run them with:
//
//	MADSHARE_CHAOS=1 go test -p 1 -run Chaos ./federation/...
//
// Budgets are testTimeoutScale-relative and shaped as "completes within N×" /
// "no stall longer than X", never exact timings: the mesh is stochastic and
// -race runs the userspace netstack several times slower.
//
// The faults themselves are NOT scaled that way. The rule is: scale what costs
// *us* time, never what the *link* does — a cut cable is no slower under -race,
// and pretending otherwise breaks scenarios in both directions (see the
// commentary on TestChaosFlappingLinkStaysFresh, which learned it twice).

// ── Transfers under a degraded link ──────────────────────────────────────────

// TestChaosSlowAndFastSeeder: one holder is crawling, the other is not. The
// transfer must not be dragged down to the slow holder's rate — the whole point
// of a multi-source swarm — and the fast holder must end up carrying the bulk of
// the bytes.
//
// Note what makes this pass: chunkPlan's provider selection is plain
// round-robin, so the slow holder is *dispatched* to about as often as the fast
// one. What keeps it from dominating is the per-chunk timeout plus
// providerFailureLimit — a holder that cannot deliver a chunk inside the budget
// accumulates failures and is dropped. The observable claim is therefore
// end-to-end (completes fast, fast holder carries the majority), not "the plan
// prefers the fast source".
func TestChaosSlowAndFastSeeder(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20) // 2 MiB
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c, _, linkC := startFaultedTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC), 0, 0)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	// C crawls: the whole blob at this rate needs well over an hour, and even one
	// 256 KiB chunk cannot finish inside the per-chunk budget at either clock
	// scale. That last part is deliberate — a holder that is slow but still fits
	// the budget keeps taking half the round-robin dispatches forever, which is a
	// real gap (see .issues/open-issues.md, "swarm provider selection is
	// speed-blind") rather than something to make a timing-dependent test of.
	linkC.Set(slowDown(4 << 10)) // 4 KiB/s

	started := time.Now()
	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	elapsed := time.Since(started)

	st := tr.Stats()
	t.Logf("slow+fast seeder: %v\n%s", elapsed, describe(st))
	if budget := 45 * time.Second * testTimeoutScale; elapsed > budget {
		t.Errorf("transfer took %v, over the %v budget — the slow holder gated it\n%s",
			elapsed, budget, describe(st))
	}
	fast, slow := providerBytes(st, a), providerBytes(st, c)
	if fast <= slow {
		t.Errorf("fast holder carried %d bytes, slow holder %d — the swarm did not route around the slow source\n%s",
			fast, slow, describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosSeederVanishesMidTransfer: a holder disappears once the transfer is
// under way. The surviving holder must finish it, and the stats must show a
// failover actually happened — "it completed" alone would also be true if the
// vanished holder had never been used.
func TestChaosSeederVanishesMidTransfer(t *testing.T) {
	requireChaos(t)
	content := fillBytes(4 << 20) // 4 MiB → ~9 chunks, room to vanish mid-way
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c, linkA, linkC := startFaultedTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC), 0, 0)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	// Both links throttled so the transfer lasts long enough to interrupt.
	linkA.Set(slowDown(512 << 10))
	linkC.Set(slowDown(512 << 10))

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if !awaitProgress(t, tr, 512<<10) {
		t.Fatalf("transfer finished before A could vanish\n%s", describe(tr.Stats()))
	}
	linkA.Set(partitioned) // A is gone: live connections cut, new dials refused

	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed after one holder vanished: %v\n%s", err, describe(tr.Stats()))
	}
	st := tr.Stats()
	t.Logf("seeder vanished: %s", describe(st))
	if st.Failovers == 0 {
		t.Errorf("no failover recorded — the transfer completed without ever re-routing a chunk\n%s", describe(st))
	}
	if got := providerBytes(st, c); got == 0 {
		t.Errorf("surviving holder carried no bytes\n%s", describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosAllSeedersVanish: the only holder disappears mid-transfer. The
// transfer must fail cleanly and promptly — no hang, no half-written blob
// promoted into the cache, no orphaned .part left behind for the next fetch to
// trip over.
func TestChaosAllSeedersVanish(t *testing.T) {
	requireChaos(t)
	content := fillBytes(4 << 20)
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	friendsHolding(t, a, b, storeA, storeB, content)
	link.Set(slowDown(512 << 10))

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if !awaitProgress(t, tr, 256<<10) {
		t.Fatal("transfer finished before the holder could vanish")
	}
	link.Set(partitioned)

	select {
	case <-tr.Done():
	case <-time.After(chaosDeadline):
		t.Fatalf("transfer hung after its only holder vanished\n%s", describe(tr.Stats()))
	}
	if tr.Err() == nil {
		t.Fatalf("transfer reported success with no reachable holder\n%s", describe(tr.Stats()))
	}
	t.Logf("all seeders vanished: %v\n%s", tr.Err(), describe(tr.Stats()))
	if _, err := os.Stat(filepath.Join(cacheB, hash)); !os.IsNotExist(err) {
		t.Error("an incomplete blob was promoted into the cache")
	}
	if _, err := os.Stat(filepath.Join(cacheB, hash+".part")); !os.IsNotExist(err) {
		t.Error(".part file left behind by a failed transfer")
	}
}

// TestChaosLatencyTimeToFirstByte: 300 ms RTT on a throttled link. The lead
// ramp (a small first chunk) plus the chunk-0 prefetch overlapped with the
// manifest probe exist so a player can start on a latent link without waiting
// out the file — assert the first byte lands early in the transfer, not near
// the end of it.
func TestChaosLatencyTimeToFirstByte(t *testing.T) {
	requireChaos(t)
	content := fillBytes(4 << 20)
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	friendsHolding(t, a, b, storeA, storeB, content)

	// Warm the mesh first: yggdrasil session setup is several round trips, and
	// charging those to the transfer would measure convergence, not the ramp.
	warmMesh(t, a, b)

	slowLatent := rtt(300 * time.Millisecond)
	slowLatent.Down.Bandwidth = 512 << 10 // ~8 s for the whole 4 MiB
	link.Set(slowLatent)

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	st := tr.Stats()
	t.Logf("300ms RTT TTFB: %s", describe(st))
	if st.FirstByte <= 0 {
		t.Fatalf("no time-to-first-byte recorded\n%s", describe(st))
	}
	if budget := 5 * time.Second * testTimeoutScale; st.FirstByte > budget {
		t.Errorf("time to first byte %v exceeds the %v budget\n%s", st.FirstByte, budget, describe(st))
	}
	// The load-bearing claim: the first byte is available long before the file
	// is. Without the ramp, chunk 0 would be a full bulk chunk and this margin
	// would collapse.
	if st.FirstByte*2 > st.Elapsed {
		t.Errorf("first byte at %v of a %v transfer — the lead ramp bought nothing\n%s",
			st.FirstByte, st.Elapsed, describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosTailSeekBeatsPrefix: a reader that seeks to the end of a slow
// transfer must get the tail chunk fetched out of order, not after the whole
// prefix. The assertion is the ordering itself — the tail is readable while the
// contiguous watermark is still far behind it.
func TestChaosTailSeekBeatsPrefix(t *testing.T) {
	requireChaos(t)
	content := fillBytes(4 << 20)
	size := int64(len(content))
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	friendsHolding(t, a, b, storeA, storeB, content)
	link.Set(slowDown(512 << 10)) // ~8 s for the file; the tail must arrive well inside that

	// The last chunk's start offset, from the same deterministic layout the
	// origin builds its manifest with.
	bulk := chunkSizeFor(size)
	layout := buildLayout(size, bulk, leadSizes(size, bulk))
	tailStart := layout.offsetOf(layout.count() - 1)

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), chaosDeadline)
	defer cancel()
	if err := tr.WaitFor(ctx, size-1); err != nil {
		t.Fatalf("waiting for the last byte: %v\n%s", err, describe(tr.Stats()))
	}
	watermark := tr.Progress()
	t.Logf("tail seek: tail readable at watermark %d of %d (tail starts at %d)", watermark, size, tailStart)
	if watermark >= tailStart {
		t.Errorf("the tail became readable only after the prefix reached %d (tail starts at %d) — seek priority did nothing",
			watermark, tailStart)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed after the seek: %v\n%s", err, describe(tr.Stats()))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosRateLimitedSeeder: the throttle is the seeder's own seed_rate_kib
// (the serving-side token bucket), not a degraded link. A holder that has
// deliberately capped itself must not starve the swarm — the other holder takes
// up the slack.
func TestChaosRateLimitedSeeder(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	// C caps itself at 4 KiB/s; A is unlimited. Links are perfect throughout —
	// this scenario is about the serving-side bucket alone. Same rate as the
	// slow-seeder scenario, for the same reason: below the per-chunk budget at
	// either clock scale.
	a, b, c, _, _ := startFaultedTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC), 0, 4)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	started := time.Now()
	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	elapsed := time.Since(started)
	st := tr.Stats()
	t.Logf("rate-limited seeder: %v\n%s", elapsed, describe(st))
	if budget := 45 * time.Second * testTimeoutScale; elapsed > budget {
		t.Errorf("transfer took %v, over the %v budget — the capped seeder gated it\n%s",
			elapsed, budget, describe(st))
	}
	if fast, capped := providerBytes(st, a), providerBytes(st, c); fast <= capped {
		t.Errorf("unlimited holder carried %d bytes, capped holder %d\n%s", fast, capped, describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// ── Availability under a cut link ────────────────────────────────────────────

// TestChaosPartitionThenHeal: friendship is durable across a network cut. While
// the link is down the friend's last_seen must stop advancing (that staleness is
// exactly what the browse's freshness window reads); once it heals, liveness
// must recover on its own and a catalog changed during the outage must sync —
// no admin action, no restart.
func TestChaosPartitionThenHeal(t *testing.T) {
	requireChaos(t)
	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Before The Cut",
		Renditions: []CatalogRendition{{Hash: "h1", Size: 10}},
	}})
	a, b, link := startFaultedPair(t, storeA, storeB, chaosOpts(), chaosOpts())
	makeFriends(t, a, b, storeA, storeB)

	peerA, err := storeB.GetFederationPeerByKey(context.Background(), a.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B's first catalog pull", func() bool { return len(storeB.cachedCatalog(peerA.ID)) == 1 })

	// Cut the link and let several refresh sweeps run into the wall. The baseline
	// is taken only once liveness has gone quiet — see settleLastSeen for why
	// reading it straight after the cut is racy.
	link.Set(partitioned)
	settled := settleLastSeen(t, storeB, a, drainQuiet)
	const staleWindow = 2 * time.Second // the scaled-down stand-in for reachable_window_sec
	time.Sleep(staleWindow + 6*chaosRefresh)
	if now := lastSeenOf(t, storeB, a); now != settled {
		// last_seen is monotonic and only written on a successful exchange, so any
		// movement means traffic crossed a link that is supposed to be cut.
		t.Errorf("last_seen advanced from %d to %d across a partitioned link", settled, now)
	}
	if p, _ := storeB.GetFederationPeerByKey(context.Background(), a.PublicKeyHex()); p.State != PeerFriend {
		t.Errorf("friendship state = %q after a partition, want it to survive", p.State)
	}

	// The browse half of "a friend went down": the row-hiding itself is a DB
	// predicate (last_seen >= now-window, database.MadnetworkView.Cutoff, unit
	// tested in database/madnetwork_test.go), so what only a real mesh can show
	// is the input to it — that an unreachable friend genuinely falls behind the
	// cutoff instead of being kept alive by some other code path.
	if cutoff := time.Now().Add(-staleWindow).Unix(); lastSeenOf(t, storeB, a) >= cutoff {
		t.Errorf("friend still counts as reachable after %v behind a cut link (last_seen %d, cutoff %d)",
			staleWindow, lastSeenOf(t, storeB, a), cutoff)
	}

	// A *network* partition must never be mistaken for a local fault: the
	// fail-open path keys on the netstack inbound reader, which is above the
	// underlay and stays alive when a peering is cut. If a cut link flipped this,
	// every outage would stop hiding anything (docs/architecture/federation.md
	// §Availability & node health).
	if !b.InboundHealthy() {
		t.Error("a partitioned peering reported the local inbound path as dead — fail-open would trigger on a remote fault")
	}

	// A's library changes while nobody can reach it.
	storeA.setPublished([]CatalogEntry{
		{Key: "1", RecordingKey: "r1", Title: "Before The Cut",
			Renditions: []CatalogRendition{{Hash: "h1", Size: 10}}},
		{Key: "2", RecordingKey: "r2", Title: "After The Heal",
			Renditions: []CatalogRendition{{Hash: "h2", Size: 20}}},
	})

	// Heal. Reconvergence is not instant — yggdrasil applies its own link retry
	// backoff — so poll rather than assert.
	healed := time.Now()
	link.Set(netfault.Fault{})
	waitFor(t, "liveness to recover after the heal", func() bool {
		return lastSeenOf(t, storeB, a) > settled
	})
	t.Logf("liveness recovered %v after the heal", time.Since(healed))
	waitFor(t, "the catalog changed during the outage to sync", func() bool {
		return len(storeB.cachedCatalog(peerA.ID)) == 2
	})
}

// TestChaosFlappingLinkStaysFresh is the anti-flap guarantee (the reason
// reachable_window_sec is several times the refresh cadence): a link that keeps
// dropping and recovering must not make a friend cross the freshness window, or
// its tracks would appear and vanish from the browse on every reload.
//
// The window is derived from the flap rather than written as a number, so the
// assertion tracks the design relationship (window ≫ disruption) instead of a
// constant that would go stale the moment the defaults move.
func TestChaosFlappingLinkStaysFresh(t *testing.T) {
	requireChaos(t)
	storeA, storeB := newMemStore(), newMemStore()
	a, b, link := startFaultedPair(t, storeA, storeB, chaosOpts(), chaosOpts())
	makeFriends(t, a, b, storeA, storeB)
	waitFor(t, "the first liveness exchange", func() bool { return lastSeenOf(t, storeB, a) > 0 })

	// One flap cycle: down for a short outage, then up for long enough that the
	// reconnect *and* a liveness exchange both land.
	//
	// The two halves scale differently, and getting that wrong is what makes this
	// scenario lie in either direction:
	//
	//   down — a physical link event. NOT scaled. testTimeoutScale exists because
	//     our userspace netstack is slower under -race; a cut cable is not. And
	//     scaling it backfires: yggdrasil's redial backoff grows with how long the
	//     peer stayed unreachable, on its own wall clock, so an 8×-longer outage
	//     costs far more than 8× to recover from and the friend goes stale for
	//     reasons that have nothing to do with anti-flap.
	//   recovery — reconnect plus a sweep completing. Ours, so scaled: measured
	//     ~4 s normally and ~23 s under -race, tracking the scale factor closely.
	//     An unscaled up-window is simply never long enough under -race, and the
	//     friend can never be refreshed at all.
	const (
		down     = 2 * time.Second
		recovery = 6 * time.Second * testTimeoutScale
		up       = 3 * recovery / 2 // headroom so a refresh lands, not just a reconnect
		cycles   = 3
	)
	// What the freshness window must absorb: one outage, the recovery after it,
	// and the gap since the last refresh before the cut. The same shape as
	// config.DefaultReachableWindowSec being a multiple of the refresh cadence —
	// the multiple is there to cover exactly this.
	window := down + recovery + 2*chaosRefresh

	deadline := time.Now().Add(cycles * (down + up))
	link.Script(flapSteps(cycles, down, up)...)

	var worst time.Duration
	for time.Now().Before(deadline) {
		age := time.Since(time.Unix(lastSeenOf(t, storeB, a), 0))
		if age > worst {
			worst = age
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("flapping link: worst last_seen age %v against a %v window", worst, window)
	if worst > window {
		t.Errorf("last_seen went %v stale on a link flapping every %v — it would cross a %v window and flip availability",
			worst, down+up, window)
	}
}

// ── Shared assertions ────────────────────────────────────────────────────────

// assertCached checks the transfer landed byte-exact in the cache.
func assertCached(t *testing.T, cacheDir, hash string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(cacheDir, hash))
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("cached bytes differ from the origin (%d vs %d)", len(got), len(want))
	}
}

// warmMesh drives one protocol round trip so yggdrasil's session setup is paid
// before a scenario starts measuring.
func warmMesh(t *testing.T, a, b *Node) {
	t.Helper()
	waitFor(t, "the mesh to converge", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), meshClientTimeout)
		defer cancel()
		return pingOK(ctx, b, a)
	})
}

// flapSteps builds a down/up timeline for Proxy.Script.
func flapSteps(cycles int, down, up time.Duration) []netfault.Step {
	steps := make([]netfault.Step, 0, 2*cycles)
	var at time.Duration
	for i := 0; i < cycles; i++ {
		steps = append(steps, netfault.Step{At: at, Fault: partitioned})
		at += down
		steps = append(steps, netfault.Step{At: at, Fault: netfault.Fault{}})
		at += up
	}
	return steps
}
