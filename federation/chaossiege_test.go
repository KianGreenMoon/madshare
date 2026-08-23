//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The siege run (docs/plans/swarm-lab.md §Family 2): the balance run's lab plus
// LIARS — coordinated nodes serving wrong bytes and lying manifests. The
// design's promise is precise and deliberately limited, and these scenarios
// assert exactly it: liars can cost time and wasted transfer, never content
// (every fetch is anchored on the whole-file sha256); a farm behind one
// friendship is one voice and one block removes it whole; infiltrating k
// friendships buys exactly k voices, no more.
//
// A liar here is a REAL federation.Node whose blob resolver maps the true hash
// to a file with different bytes — same wire, same handlers, same manifest
// builder, so it produces a SELF-CONSISTENT lie: its manifest honestly
// describes its wrong file, which is the strongest lie the protocol admits.
// (A liar that also forged its manifest's internal consistency would only fail
// earlier.)

// publishLie is publishBlob's evil twin: the store advertises the TRUE hash and
// size, but the resolver serves same-length wrong bytes for it. The lie is
// derived deterministically (every byte XOR 0xA5), so several liars given the
// same content tell the SAME lie — the coordinated-farm shape, and the one that
// can capture a manifest quorum.
func publishLie(t *testing.T, store *memStore, content []byte) Option {
	t.Helper()
	hash := hashOf(content)
	lie := make([]byte, len(content))
	for i, b := range content {
		lie[i] = b ^ 0xA5
	}
	if hashOf(lie) == hash {
		t.Fatal("the lie hashes like the truth — the fixture is broken")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lie.mp3")
	if err := os.WriteFile(path, lie, 0o644); err != nil {
		t.Fatal(err)
	}
	store.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Swarm Song",
		Renditions: []CatalogRendition{{Hash: hash, Size: int64(len(content)), Codec: "mp3"}},
	}})
	return WithBlobResolver(func(h string) (string, bool) { return path, h == hash })
}

// TestSiegeLiarMinorityIsRetired: two honest holders and one liar. The honest
// pair wins the manifest agreement (two identical descriptions; the liar's
// differs), so the liar's chunks fail per-chunk verify — wrong bytes are
// unambiguous evidence about their sender. A minority liar is contained IN
// PLACE: retired on its first corrupt chunk, at the cost of that chunk and
// nothing else, with no honest holder blamed and no fallback needed.
func TestSiegeLiarMinorityIsRetired(t *testing.T) {
	requireChaos(t)

	// 3 MiB → 12 chunks. The first cut of this scenario used 4, and the liar
	// was never caught because it was never ASKED: two fast honest holders
	// absorbed every dispatch and the one chunk the liar was handed was won by
	// a hedge before it delivered (a lost race is not evidence). A scenario
	// that indicts a liar must first give it work.
	const blobSize = 3 << 20
	content := fillBytes(blobSize)
	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	fetcherStore := newMemStore()
	hash := hashOf(content)

	hOpts := make([][]Option, len(stores))
	for i, s := range stores[:2] {
		_, resolver := publishBlob(t, s, content)
		hOpts[i] = chaosOpts(resolver)
	}
	hOpts[2] = chaosOpts(publishLie(t, stores[2], content))

	cacheDir := t.TempDir()
	holders, fetcher, _ := startFaultedSwarm(t, stores, fetcherStore, hOpts,
		chaosOpts(WithCacheDir(cacheDir)))
	liar := holders[2]
	for i, h := range holders {
		makeFriends(t, h, fetcher, stores[i], fetcherStore)
	}

	// The liar leads the plan: holders[0] is where the chunk-0 speculation goes,
	// so the lie is guaranteed to meet a verification instead of depending on
	// the scheduler happening to pick the liar. The manifest agreement is still
	// the honest pair's — one differing description never reaches two votes.
	plan := []*BlobProvider{
		{Name: "liar", PublicKey: liar.PublicKeyHex()},
		{Name: "honest0", PublicKey: holders[0].PublicKeyHex()},
		{Name: "honest1", PublicKey: holders[1].PublicKeyHex()},
	}
	awaitManifests(t, fetcher, plan, hash)

	ctx := context.Background()
	tr, err := fetcher.EnsureBlobFrom(ctx, hash, blobSize, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("two honest holders were present and the fetch still failed: %v\n%s",
			err, describe(tr.Stats()))
	}
	st := tr.Stats()
	t.Logf("minority liar\n%s", describe(st))

	assertCached(t, cacheDir, hash, content)
	if len(st.Prior) > 0 || st.Mode != "swarm" {
		t.Errorf("mode=%s with %d abandoned attempt(s) — a MINORITY liar should be contained on the "+
			"swarm path, not by falling back\n%s", st.Mode, len(st.Prior), describe(st))
	}
	if st.Corrupt == 0 {
		t.Errorf("corrupt=0 — the liar was never caught lying, so this scenario proved nothing\n%s", describe(st))
	}
	for _, p := range st.Providers {
		switch p.PublicKey {
		case liar.PublicKeyHex():
			if !p.Dropped {
				t.Errorf("the liar served corrupt bytes and was not retired\n%s", describe(st))
			}
		default:
			if p.Dropped {
				t.Errorf("honest holder %s was retired in a fetch the liar lost\n%s", p.Name, describe(st))
			}
		}
	}
}

// TestSiegeLiarQuorumCannotForgeTheBytes: one honest holder against TWO
// coordinated liars with an identical lie — the farm shape, and the strongest
// position the protocol allows an attacker. Two identical votes capture the
// manifest agreement; the honest holder's true chunks now look corrupt against
// the adopted lie; the swarm can assemble a complete, chunk-verified, WRONG
// file. The anchor holds anyway: the assembled-hash verify fails, the fetch
// falls back to the whole-file path, the liars' attempts fail its sha256, and
// the honest holder's attempt verifies. A quorum of liars buys wasted transfer,
// never forged content.
func TestSiegeLiarQuorumCannotForgeTheBytes(t *testing.T) {
	requireChaos(t)

	const blobSize = 1 << 20
	content := fillBytes(blobSize)
	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	fetcherStore := newMemStore()
	hash := hashOf(content)

	hOpts := make([][]Option, len(stores))
	_, resolveHonest := publishBlob(t, stores[0], content)
	hOpts[0] = chaosOpts(resolveHonest)
	hOpts[1] = chaosOpts(publishLie(t, stores[1], content))
	hOpts[2] = chaosOpts(publishLie(t, stores[2], content))

	cacheDir := t.TempDir()
	holders, fetcher, _ := startFaultedSwarm(t, stores, fetcherStore, hOpts,
		chaosOpts(WithCacheDir(cacheDir)))
	honest := holders[0]
	for i, h := range holders {
		makeFriends(t, h, fetcher, stores[i], fetcherStore)
	}

	// Liars first: the plan order a farm would engineer, and the one that makes
	// the fallback walk pay the full price (both liars' whole files downloaded
	// and discarded) before the honest holder settles it.
	plan := []*BlobProvider{
		{Name: "liar0", PublicKey: holders[1].PublicKeyHex()},
		{Name: "liar1", PublicKey: holders[2].PublicKeyHex()},
		{Name: "honest", PublicKey: honest.PublicKeyHex()},
	}
	awaitManifests(t, fetcher, plan, hash)

	ctx := context.Background()
	start := time.Now()
	tr, err := fetcher.EnsureBlobFrom(ctx, hash, blobSize, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("an honest holder was present and the fetch still failed: %v\n%s",
			err, describe(tr.Stats()))
	}
	elapsed := time.Since(start)
	st := tr.Stats()
	t.Logf("liar quorum, settled in %v\n%s", elapsed.Round(time.Millisecond), describe(st))

	// The invariant, on EVERY interior path: the bytes are the truth.
	assertCached(t, cacheDir, hash, content)

	// Which interior path ran depends on probe timing — normally the liar pair
	// captures the agreement and the assembled-hash verify forces the whole-file
	// fallback; if one liar's probe is slow the 1-vs-1 disagreement gives way to
	// the whole-file path directly. Both are the defense working. What must
	// never happen is the fetch ending as an ordinary healthy swarm transfer:
	// then the lie never engaged and this scenario proved nothing.
	incident := st.Mode == "whole" || len(st.Prior) > 0 || st.Corrupt > 0
	for _, p := range st.Providers {
		incident = incident || p.Dropped
	}
	if !incident {
		t.Errorf("no fallback, no corrupt chunk, nobody retired — the siege never engaged\n%s", describe(st))
	}
}

// TestSiegeOneBranchIsOneVoice: the graph half of the siege, pure and ungated —
// a 12-key sybil farm, every edge mutually published (the farm CAN publish
// edges among its own keys; what it cannot forge is a signature from anyone
// else, pinned in gossip_test.go). Three rules make the answer structural
// rather than reactive, asserted here at farm scale and together:
//
//   - ADMISSION: the farm IS admitted to membership — mutual edges are the
//     rule and the farm followed it. The defense is not exclusion.
//   - ATTRIBUTION: every farm key hangs off the one friendship it arrived
//     through, so any count weighted by branch (mergeVersions' ordering, the
//     held/missing lanes) hears ONE voice from it, whatever the farm's size.
//   - EVICTION: blocking that one friend removes the entire farm in one act.
//
// And the stated limit: a farm spread across k friendships owns exactly k
// voices — no more, and the tests elsewhere make sure no clever edge set makes
// it more; no less, because a compromised friend IS a voice, which is why
// blocking exists.
func TestSiegeOneBranchIsOneVoice(t *testing.T) {
	const farmSize = 12
	farm := make([]string, farmSize)
	for i := range farm {
		farm[i] = k(fmt.Sprintf("s%02d", i))
	}
	mutual := func(edges []GraphEdgeClaim, a, b string) []GraphEdgeClaim {
		return append(edges,
			GraphEdgeClaim{Origin: a, Peer: b, Name: "farm"},
			GraphEdgeClaim{Origin: b, Peer: a, Name: "farm"},
		)
	}

	t.Run("one branch, one voice, one block", func(t *testing.T) {
		peers := []*ExternalNode{{PublicKey: k("f1"), TrustState: PeerFriend}}
		var edges []GraphEdgeClaim
		for _, s := range farm {
			edges = mutual(edges, k("f1"), s)
		}
		// The farm also meshes among itself — a real farm would, and it must
		// not buy a second route into the community.
		for i := 1; i < len(farm); i++ {
			edges = mutual(edges, farm[i-1], farm[i])
		}

		members := MemberKeys(k("me"), peers, edges)
		for _, s := range farm {
			if _, ok := members[s]; !ok {
				t.Fatalf("farm key %s was refused membership — mutual edges are the rule, and the "+
					"farm followed it; the defense is attribution, not exclusion", s)
			}
		}

		branches := branchesOf(k("me"), peers, edges)
		roots := map[string]bool{}
		for _, s := range farm {
			via := branches[s]
			if len(via) != 1 || via[0] != k("f1") {
				t.Errorf("farm key %s via %v, want exactly [f1] — a 12-key farm behind one friendship "+
					"must be one voice", s, via)
			}
			for _, r := range via {
				roots[r] = true
			}
		}
		if len(roots) != 1 {
			t.Errorf("the farm spans %d branch roots, want 1", len(roots))
		}

		// Blocking the one friend the farm arrived through evicts all of it.
		peers[0].TrustState = PeerBlocked
		members = MemberKeys(k("me"), peers, edges)
		branches = branchesOf(k("me"), peers, edges)
		for _, s := range farm {
			if _, ok := members[s]; ok {
				t.Errorf("farm key %s is still a member after its branch root was blocked", s)
			}
			if len(branches[s]) != 0 {
				t.Errorf("farm key %s still has voices after the block: %v", s, branches[s])
			}
		}
	})

	t.Run("k branches are exactly k voices", func(t *testing.T) {
		peers := []*ExternalNode{
			{PublicKey: k("f1"), TrustState: PeerFriend},
			{PublicKey: k("f2"), TrustState: PeerFriend},
			{PublicKey: k("f3"), TrustState: PeerFriend},
		}
		friends := []string{k("f1"), k("f2"), k("f3")}
		var edges []GraphEdgeClaim
		for i, s := range farm {
			edges = mutual(edges, friends[i%3], s)
		}

		branches := branchesOf(k("me"), peers, edges)
		roots := map[string]bool{}
		for i, s := range farm {
			via := branches[s]
			if len(via) != 1 || via[0] != friends[i%3] {
				t.Errorf("farm key %s via %v, want [%s] alone", s, via, friends[i%3])
			}
			for _, r := range via {
				roots[r] = true
			}
		}
		// The design's stated limit, pinned so nobody mistakes it for a hole:
		// infiltrating three friendships buys three voices. Not twelve.
		if len(roots) != 3 {
			t.Errorf("a farm across 3 friendships spans %d branch roots, want exactly 3", len(roots))
		}
	})
}
