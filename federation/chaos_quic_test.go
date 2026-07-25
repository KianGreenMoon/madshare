//go:build !nofederation

package federation

import (
	"context"
	"testing"
	"time"
)

// The datagram half of the chaos suite (docs/plans/mesh-testing.md §Phase T3):
// the same real yggdrasil meshes, peered over a quic:// underlay with a netfault
// datagram relay in the seam, so packets can be lost, reordered and duplicated
// for real.
//
// Why a second transport at all: on a TCP underlay none of those three faults
// can be injected honestly. The kernel repairs the wire before yggdrasil sees
// it, so "5 % loss" there would arrive as an occasional stall and a reset — the
// symptoms of loss, not loss. One layer down, a dropped datagram is a dropped
// datagram, and QUIC's recovery, yggdrasil's link and the swarm's per-chunk
// verification all have to earn their keep.
//
// Every scenario converges the mesh on a clean link and degrades it afterwards.
// QUIC's handshake and the pairing exchange are the least interesting things a
// lossy path can break, and letting setup fail probabilistically would make each
// scenario flaky for reasons unrelated to what it asserts.
//
// Each also checks the injector's own counters before believing its result: a
// transfer that "survived 15 % loss" on a link that dropped nothing has proved
// nothing, and that failure mode is silent.

// ── Transfers over a hostile path ────────────────────────────────────────────

// TestChaosLossyPathCompletes: a heavily lossy underlay must not break a
// transfer, only slow it. Loss is applied in both directions — acknowledgements
// live on the same wire as the data — and the bytes have to land verbatim.
//
// The claim worth pinning is that loss stays a *transport* problem: it must
// never surface as a corrupt chunk, because the swarm's response to corruption
// is to drop the holder that sent it. A lossy path that got misread as a lying
// holder would retire every source it had and fail a transfer that should merely
// have taken longer.
func TestChaosLossyPathCompletes(t *testing.T) {
	requireChaos(t)
	const lossRate = 0.15
	content := fillBytes(2 << 20)
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startQUICPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	friendsHolding(t, a, b, storeA, storeB, content)
	warmMesh(t, a, b)

	link.Set(lossy(lossRate))

	start := time.Now()
	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed across a %.0f%% lossy path: %v\n%s\n%s",
			lossRate*100, err, describe(tr.Stats()), describeLink(link))
	}
	st := tr.Stats()
	t.Logf("%.0f%% loss: %v elapsed\n%s\n%s", lossRate*100, time.Since(start),
		describe(st), describeLink(link))

	assertLossy(t, link, lossRate)
	if st.Corrupt > 0 {
		t.Errorf("%d chunks failed verification on a lossy path — loss reached the "+
			"swarm as corruption, which retires holders that did nothing wrong\n%s",
			st.Corrupt, describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosScrambledPathKeepsChunksIntact: datagrams arriving out of order and
// twice over. These are the faults a reliable stream hides completely, and no
// loss is injected alongside them, so nothing here can be explained by missing
// bytes — if a chunk fails its sha256, reassembly is genuinely wrong.
//
// Two holders, each behind its own scrambled path, so the chunks being stitched
// together arrive from two independently disordered sources.
func TestChaosScrambledPathKeepsChunksIntact(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c, linkA, linkC := startQUICTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC))
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))
	warmMesh(t, a, b)
	warmMesh(t, c, b)

	// 20 % held back by 30 ms — long enough to be overtaken by several
	// successors — and 10 % delivered twice.
	scramble := scrambled(0.2, 0.1, 30*time.Millisecond)
	linkA.Set(scramble)
	linkC.Set(scramble)

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed across a reordered/duplicated path: %v\n%s\nA: %s\nC: %s",
			err, describe(tr.Stats()), describeLink(linkA), describeLink(linkC))
	}
	st := tr.Stats()
	t.Logf("scrambled paths: %s\nA: %s\nC: %s", describe(st), describeLink(linkA), describeLink(linkC))

	assertScrambled(t, "A", linkA)
	assertScrambled(t, "C", linkC)

	if st.Corrupt > 0 {
		t.Errorf("%d chunks failed sha256 with no loss injected — reordering or "+
			"duplication is reaching the assembled file\n%s", st.Corrupt, describe(st))
	}
	// The end-to-end anchor: the whole-file hash. A chunk assembled from
	// duplicated datagrams could in principle verify per chunk and still land in
	// the wrong place, and this is what would catch it.
	assertCached(t, cacheB, hash, content)
}

// ── Availability under sustained loss ────────────────────────────────────────

// TestChaosSustainedLossStaysReachable is the availability half of T3: a friend
// on a permanently lossy path must still count as reachable. Liveness is a
// passive last_seen, refreshed by the sweep's ping, and the browse hides tracks
// held only by a friend that has fallen behind the freshness window — so a link
// that merely drops packets must never be enough to make a friend's whole
// library blink out.
//
// This is the same guarantee TestChaosFlappingLinkStaysFresh makes for a link
// that keeps dropping, with the failure mode inverted: there the path vanishes
// and returns, here it never vanishes but never quite works either. Loss is the
// harder case for a passive liveness scheme, because nothing is ever reported
// down — the timestamps just quietly stop keeping up.
func TestChaosSustainedLossStaysReachable(t *testing.T) {
	requireChaos(t)
	const lossRate = 0.05
	storeA, storeB := newMemStore(), newMemStore()
	a, b, link := startQUICPair(t, storeA, storeB, chaosOpts(), chaosOpts())
	makeFriends(t, a, b, storeA, storeB)
	waitFor(t, "the first liveness exchange", func() bool { return lastSeenOf(t, storeB, a) > 0 })

	link.Set(lossy(lossRate))

	// What the freshness window must absorb on a lossy path: a handful of sweeps
	// that lost their packets, plus the one-second granularity last_seen is
	// stored at. Derived from the refresh cadence rather than written as a
	// number, because that is the actual design relationship —
	// config.DefaultReachableWindowSec is a multiple of the refresh interval, and
	// the multiple exists to cover exactly this.
	//
	// Deliberately not derived from chaosControl: a ping that runs the protocol
	// timeout down is the pathology this scenario is meant to catch, so budgeting
	// for one would assert almost nothing. Observed worst age is ~1 s at 5 %
	// loss — essentially the storage granularity — so the headroom below is
	// several times what a healthy run needs.
	window := 6*chaosRefresh + 2*time.Second
	observe := 20 * time.Second * testTimeoutScale

	var worst time.Duration
	deadline := time.Now().Add(observe)
	for time.Now().Before(deadline) {
		if age := time.Since(time.Unix(lastSeenOf(t, storeB, a), 0)); age > worst {
			worst = age
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("%.0f%% sustained loss: worst last_seen age %v against a %v window\n%s",
		lossRate*100, worst, window, describeLink(link))

	assertLossy(t, link, lossRate)
	if worst > window {
		t.Errorf("last_seen went %v stale under %.0f%% packet loss — it would cross a "+
			"%v window and blink the friend's library out of the browse",
			worst, lossRate*100, window)
	}
	if p, _ := storeB.GetFederationPeerByKey(context.Background(), a.PublicKeyHex()); p.State != PeerFriend {
		t.Errorf("friendship state = %q under packet loss, want it to survive", p.State)
	}
	// Loss on a peering is a remote condition. If it flipped the local inbound
	// health signal, fail-open would trigger and the browse would stop hiding
	// anything at all (docs/architecture/federation.md §Availability & node health).
	if !b.InboundHealthy() {
		t.Error("a lossy peering reported the local inbound path as dead — fail-open would trigger on a remote fault")
	}
}
