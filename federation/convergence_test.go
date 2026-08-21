//go:build !nofederation

package federation

// The many-nodes hypothesis (2026-08-21), pinned as tests.
//
// The design bets that every member sees its OWN madnetwork: membership is
// global (the gossiped component converges everywhere), but the content view is
// per-node — a bounded frontier of cached catalogs — so two far-apart nodes
// can hold two different versions of one album at the same moment. The bet is
// that this divergence is what makes the network stable at size (no consensus,
// no sync storms, bounded storage) AND that it costs no reach: every song is
// still fetchable from every vantage, because fetching needs a hash and a
// holder, never agreement.
//
// Both halves of that bet are claims about emergent behaviour, not about any
// one function, so both are tested on real embedded nodes over a real loopback
// mesh — the shape of test that caught the frontier-rotation bug the unit
// tests missed (docs/architecture/federation.md §Discovery beyond the friend
// ring, "a live 5-node chain caught").
//
//   - TestChainDiscoveryConvergesToFullReach: a friendship chain with a tight
//     budget still converges until every vantage names a holder for every
//     song, and the far end's song actually arrives at the near end.
//   - TestBoundedVantagesDivergeOnOneAlbumYetReachOnAsk: two vantages with
//     discovery off hold two versions of one album — agreeing on membership,
//     disagreeing on content, both correct — and an explicit ask (PullFrom)
//     closes the gap without waiting for any rotation.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// convergeIntervals is gossipIntervals plus an uncached membership read: these
// tests assert what a vantage KNOWS right now, and a memoized community would
// make the assertion race the memo instead of the network.
var convergeIntervals = func() Intervals {
	iv := gossipIntervals
	iv.MembershipTTL = noMemo
	return iv
}()

// chainNode is one embedded node plus everything a scenario asserts against.
type chainNode struct {
	node  *Node
	store *memStore
	hash  string // the one blob this node can serve, "" when it publishes nothing
	bytes []byte
}

// startChain brings up len(publish) nodes on one loopback underlay, friended
// ADJACENTLY — a chain of friendships, deliberately not a clique, because the
// per-node view only differs from the global one when friendship distance
// exists. publish[i] is the entry node i offers (nil publishes nothing);
// discovery[i] is its frontier bounds. Every node gets a cache dir, so any
// vantage can fetch.
//
// The transport is a hub on node 0's listener while the FRIENDSHIP graph is a
// chain; that mismatch is the design's own (`meshlab reach`): the friendship
// graph is not the transport, an address derives from a key, and distance is
// a fact about trust, not about dialling.
func startChain(t *testing.T, publish []*CatalogEntry, discovery []Discovery) []*chainNode {
	t.Helper()
	dir := t.TempDir()
	underlay := freeUnderlay(t)
	logger := log.New(io.Discard, "", 0)

	chain := make([]*chainNode, len(publish))
	for i := range publish {
		cn := &chainNode{store: newMemStore()}
		opts := []Option{
			WithIntervals(convergeIntervals),
			WithDiscovery(discovery[i]),
			WithCacheDir(t.TempDir()),
		}
		if e := publish[i]; e != nil {
			cn.bytes = []byte(fmt.Sprintf("payload %d: %s — %s", i, e.Artist, e.Title))
			sum := sha256.Sum256(cn.bytes)
			cn.hash = hex.EncodeToString(sum[:])
			e.Renditions = []CatalogRendition{{Hash: cn.hash, Size: int64(len(cn.bytes)), Codec: "mp3"}}
			blobPath := filepath.Join(dir, fmt.Sprintf("blob-%d.mp3", i))
			if err := os.WriteFile(blobPath, cn.bytes, 0o644); err != nil {
				t.Fatal(err)
			}
			cn.store.setPublished([]CatalogEntry{*e})
			hash := cn.hash
			opts = append(opts, WithBlobResolver(func(h string) (string, bool) {
				return blobPath, h == hash
			}))
		}
		name := fmt.Sprintf("chain-%c", 'a'+i)
		cfg := configFor(dir, name, underlay, i == 0)
		n, err := Start(cfg, cn.store, logger, opts...)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(n.Stop)
		cn.node = n
		chain[i] = cn
	}
	for i := 1; i < len(chain); i++ {
		mustFriend(t, chain[i].node, chain[i-1].node, chain[i].store, chain[i-1].store)
	}
	return chain
}

func nudgeAll(chain []*chainNode) {
	for _, cn := range chain {
		cn.node.Nudge()
	}
}

// knowsHolder reports whether this vantage can already name `holder` for a
// hash — the whole precondition of a fetch, and therefore the unit the
// convergence assertion is made of.
func (cn *chainNode) knowsHolder(hash, holderKey string) bool {
	_, holders, err := cn.store.MadnetworkBlobProviders(context.Background(), hash)
	if err != nil {
		return false
	}
	for _, h := range holders {
		if h.PublicKey == holderKey {
			return true
		}
	}
	return false
}

// fetchBytes runs a real transfer at this vantage and returns the bytes.
func (cn *chainNode) fetchBytes(t *testing.T, hash string) []byte {
	t.Helper()
	tr, err := cn.node.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob(%s): %v", hash[:8], err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatalf("transfer of %s did not finish", hash[:8])
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("transfer of %s: %v", hash[:8], err)
	}
	f, err := tr.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestChainDiscoveryConvergesToFullReach is "we can reach every song" measured
// on the worst shape we ship: five nodes in a friendship chain, each publishing
// one song nobody else has, with the frontier budget squeezed to ONE member
// catalog per cycle. The far ends are four friendships apart and never friend
// each other; everything they learn beyond their one neighbour arrives through
// the rotation.
//
// The assertion is deliberately simultaneous: one poll in which EVERY vantage
// names a holder for EVERY song. Divergence along the way is expected and not
// asserted against — the claim is that it converges, not that it is avoided.
func TestChainDiscoveryConvergesToFullReach(t *testing.T) {
	const size = 5
	publish := make([]*CatalogEntry, size)
	bounds := make([]Discovery, size)
	for i := 0; i < size; i++ {
		publish[i] = &CatalogEntry{
			Key: "1", RecordingKey: "r1",
			Title:  fmt.Sprintf("Only Here %d", i),
			Artist: fmt.Sprintf("Node %c", 'A'+i),
		}
		bounds[i] = Discovery{Budget: 1}
	}
	chain := startChain(t, publish, bounds)

	waitFor(t, "every vantage to name a holder for every song at once", func() bool {
		nudgeAll(chain)
		for i, vantage := range chain {
			for j, holder := range chain {
				if i == j {
					continue
				}
				if !vantage.knowsHolder(holder.hash, holder.node.PublicKeyHex()) {
					return false
				}
			}
		}
		return true
	})

	// Knowledge is not reach until bytes move: the near end fetches the far
	// end's song — a node it never friended, found purely by rotation.
	far := chain[size-1]
	if got := chain[0].fetchBytes(t, far.hash); string(got) != string(far.bytes) {
		t.Errorf("far fetch delivered %d bytes, want the far node's song verbatim", len(got))
	}
}

// TestBoundedVantagesDivergeOnOneAlbumYetReachOnAsk pins the other half of the
// bet: the divergence itself. Four nodes a—b—c—d; b and c each publish a
// version of ONE album — same name, different tagset, different bytes — and
// the far ends run with discovery OFF (budget 0, the "friends only" setting an
// admin can choose), which is the bounded-view regime made deterministic: at
// any community size past the cap, some catalogs are simply not in a vantage's
// cache, and budget 0 is that condition held still enough to assert against.
//
// So a sees the album only as b's version, d only as c's — one album, two
// truths, both correct about what their holder actually serves. The test then
// asserts the two properties that make this safe rather than broken:
//
//   1. membership still converged — the vantages disagree about CONTENT while
//      agreeing about the community, which is the split the design promises;
//   2. an explicit ask (PullFrom, "interest beats fairness") closes the gap at
//      once: a pulls c, sees the second version, and fetches its bytes from a
//      node it never friended — without d's view changing, because one
//      vantage's interest is not a network event.
func TestBoundedVantagesDivergeOnOneAlbumYetReachOnAsk(t *testing.T) {
	album := func(year int64, title string) *CatalogEntry {
		return &CatalogEntry{
			Key: "1", RecordingKey: "r1", Title: title,
			Artist: "Chain Artist", AlbumArtist: "Chain Artist",
			Album: "Faraway Line", Year: &year,
		}
	}
	publish := []*CatalogEntry{nil, album(2001, "First Cut"), album(2011, "Remaster"), nil}
	bounds := []Discovery{{Budget: -1}, {Budget: 1}, {Budget: 1}, {Budget: -1}}
	chain := startChain(t, publish, bounds)
	a, b, c, d := chain[0], chain[1], chain[2], chain[3]
	ctx := context.Background()

	// Membership converges across the whole chain even though a and d discover
	// nothing: the graph travels by gossip, which no budget touches.
	waitFor(t, "the far ends to count each other members", func() bool {
		nudgeAll(chain)
		for _, pair := range [][2]*chainNode{{a, d}, {d, a}} {
			ms, err := pair[0].node.community(ctx)
			if err != nil {
				return false
			}
			if _, ok := ms.keys[pair[1].node.PublicKeyHex()]; !ok {
				return false
			}
		}
		return true
	})

	// Each far end learns its neighbour's version — the friend pull no budget
	// binds — and nothing else.
	waitFor(t, "each far end to cache its neighbour's catalog", func() bool {
		nudgeAll(chain)
		return len(a.store.cachedFrom(b.node.PublicKeyHex())) == 1 &&
			len(d.store.cachedFrom(c.node.PublicKeyHex())) == 1
	})

	// The divergence, held still: a knows nothing of c's version, d nothing of
	// b's. Not eventual, structural — budget 0 pulls friends and nobody else.
	if got := a.store.cachedFrom(c.node.PublicKeyHex()); len(got) != 0 {
		t.Fatalf("a cached c's catalog (%d entries) although its discovery is off", len(got))
	}
	if got := d.store.cachedFrom(b.node.PublicKeyHex()); len(got) != 0 {
		t.Fatalf("d cached b's catalog (%d entries) although its discovery is off", len(got))
	}
	if ay := *a.store.cachedFrom(b.node.PublicKeyHex())[0].Year; ay != 2001 {
		t.Errorf("a's version of the album is %d, want its neighbour's 2001", ay)
	}
	if dy := *d.store.cachedFrom(c.node.PublicKeyHex())[0].Year; dy != 2011 {
		t.Errorf("d's version of the album is %d, want its neighbour's 2011", dy)
	}

	// And each vantage's partial truth is fully usable: a plays its version.
	if got := a.fetchBytes(t, b.hash); string(got) != string(b.bytes) {
		t.Error("a could not fetch the version of the album it sees")
	}

	// The ask. One PullFrom and a's next sweep carries c's catalog — no
	// rotation waited out, no friendship made.
	if err := a.node.PullFrom(c.node.PublicKeyHex()); err != nil {
		t.Fatalf("PullFrom: %v", err)
	}
	waitFor(t, "a to see the second version after asking", func() bool {
		a.node.Nudge()
		return len(a.store.cachedFrom(c.node.PublicKeyHex())) == 1
	})
	if got := a.fetchBytes(t, c.hash); string(got) != string(c.bytes) {
		t.Error("a could not fetch the version it just asked the network about")
	}

	// d's view did not move: one vantage's interest is not a network event.
	if got := d.store.cachedFrom(b.node.PublicKeyHex()); len(got) != 0 {
		t.Errorf("d's view changed (%d entries from b) although only a asked", len(got))
	}
}

// configFor builds one embedded node's config: node 0 listens on the shared
// underlay, everyone later peers to it.
func configFor(dir, name, underlay string, listen bool) config.FederationConfig {
	cfg := config.FederationConfig{Name: name, KeyFile: filepath.Join(dir, name+".key")}
	if listen {
		cfg.Listen = []string{underlay}
	} else {
		cfg.Peers = []string{underlay}
	}
	return cfg
}
