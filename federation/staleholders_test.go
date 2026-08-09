//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// What a stale holder in a fetch plan actually costs, measured rather than
// reasoned about (docs/ui/madplayer.md §"Level 2b, concretely").
//
// The report that prompted this: a madplayer fetching from a live server was
// handed holders last seen 21 and 54 hours earlier, and the same track that took
// 1m43s from one live holder took 4m12s–4m25s with those on the list. The
// arithmetic said Timeouts.ChunkStall × providerFailureLimit per dead holder;
// that was a hypothesis about production constants, and this is the experiment
// that CORRECTED it — see staleScale. The binding timeout is PerChunk, and the
// tax is far worse than the guess.
//
// FOUR nodes: three holders carrying the same blob and one fetcher. A "stale"
// holder is a KEY IN THE PLAN THAT NOTHING ON THE MESH ANSWERS TO, which is what
// a stale advertisement actually is — not a node that refuses (that fails fast
// and is the easy case), but one whose dial goes into a mesh with no route. The
// scenarios walk 0, 1, 2 and 3 of the three being stale, and each is compared
// against the same bytes over plain HTTP, which is what the relay is.
//
// Every scenario fetches a DIFFERENT blob: the cache is keyed by hash and a
// second fetch of the first blob would measure a cache hit.
//
// The clock is the chaos suite's shrunk one (chaosOpts), so the numbers are
// small; what carries over is the SHAPE and the ratio. staleScale converts them
// back to the shipped 20s ChunkStall.

// staleScale converts a measured cost to what it would be at the shipped
// timeouts.
//
// It is PerChunk, NOT ChunkStall, and finding that out was the point of running
// this rather than reasoning about it. The obvious hypothesis was the idle-read
// watchdog — no bytes for ChunkStall ⇒ the holder is hung — but a stale holder's
// dial NEVER CONNECTS, so no response header ever arrives and that watchdog is
// never armed. Every run below reports stalls=0 and fails with "context deadline
// exceeded" on the GET, which is the per-chunk backstop. At the shipped values
// that is 2 minutes rather than 20 seconds, so a dead holder is six times more
// expensive than the arithmetic said.
const staleScale = 2 * time.Minute / chaosPerChunk

// startNodeQuad starts three holders, each listening on its own underlay, and a
// fetcher dialling all three.
//
// Each holder gets its OWN link rather than the hub shape startNodeTrio uses:
// with one shared underlay two holders would be two hops away and the third one,
// and this measurement is about the holders differing in exactly one way.
func startNodeQuad(t *testing.T, holders []*memStore, fetcher *memStore, hOpts [][]Option, fOpts []Option) (hs []*Node, f *Node) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	var peers []string
	for i, store := range holders {
		addr := reserveUnderlay(t)
		peers = append(peers, "tcp://"+addr)
		name := fmt.Sprintf("holder%d", i)
		fc := config.FederationConfig{
			Name: name, KeyFile: filepath.Join(dir, name+".key"),
			Listen: []string{"tcp://" + addr},
		}
		n, err := Start(fc, store, logger, hOpts[i]...)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(n.Stop)
		hs = append(hs, n)
	}

	fc := config.FederationConfig{
		Name: "fetcher", KeyFile: filepath.Join(dir, "fetcher.key"), Peers: peers,
	}
	f, err := Start(fc, fetcher, logger, fOpts...)
	if err != nil {
		t.Fatalf("start fetcher: %v", err)
	}
	t.Cleanup(f.Stop)
	return hs, f
}

// publishBlobs publishes several blobs from one store and resolves all of them,
// so one holder can serve a different blob per scenario without republishing
// between measurements.
func publishBlobs(t *testing.T, store *memStore, blobs [][]byte) (hashes []string, resolver Option) {
	t.Helper()
	dir := t.TempDir()
	paths := map[string]string{}
	var entries []CatalogEntry
	for i, content := range blobs {
		hash := hashOf(content)
		path := filepath.Join(dir, fmt.Sprintf("track%d.mp3", i))
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		paths[hash] = path
		hashes = append(hashes, hash)
		entries = append(entries, CatalogEntry{
			Key: fmt.Sprintf("%d", i), RecordingKey: fmt.Sprintf("r%d", i),
			Title:      fmt.Sprintf("Track %d", i),
			Renditions: []CatalogRendition{{Hash: hash, Size: int64(len(content)), Codec: "mp3"}},
		})
	}
	store.setPublished(entries)
	return hashes, WithBlobResolver(func(h string) (string, bool) {
		p, ok := paths[h]
		return p, ok
	})
}

func TestStaleHoldersCostAFetch(t *testing.T) {
	requireChaos(t)

	const blobSize = 2 << 20 // 2 MiB — several chunks, so dispatch spreads
	scenarios := []struct {
		name  string
		stale int
	}{
		{"0 stale (3 live)", 0},
		{"1 stale (2 live)", 1},
		{"2 stale (1 live)", 2},
		{"3 stale (0 live)", 3},
	}

	// One blob per scenario, all carried by all three holders.
	blobs := make([][]byte, len(scenarios))
	for i := range blobs {
		b := fillBytes(blobSize)
		b[0] = byte(i + 1) // distinct content ⇒ distinct hash
		blobs[i] = b
	}

	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	fetcherStore := newMemStore()
	cacheDir := t.TempDir()

	hOpts := make([][]Option, len(stores))
	var hashes []string
	for i, s := range stores {
		hs, resolver := publishBlobs(t, s, blobs)
		hashes = hs
		hOpts[i] = chaosOpts(resolver)
	}
	holders, fetcher := startNodeQuad(t, stores, fetcherStore, hOpts,
		chaosOpts(WithCacheDir(cacheDir)))

	// The fetcher is served because each holder calls it a friend. A listener
	// node reaches the same place by a capability token instead; that changes
	// which arm of serveAudience answers, not what a fetch costs, and this
	// measurement is about the cost.
	for i, h := range holders {
		makeFriends(t, h, fetcher, stores[i], fetcherStore)
	}

	// The stale holders, minted once so every scenario names the same absent
	// nodes — a fresh key per scenario would be a different experiment each time.
	ghosts := make([]string, 3)
	for i := range ghosts {
		_, ghosts[i] = newSigner(t)
	}

	ctx := context.Background()
	live := make([]*BlobProvider, len(holders))
	for i, h := range holders {
		live[i] = &BlobProvider{PublicKey: h.PublicKeyHex()}
	}
	waitFor(t, "every holder to be reachable", func() bool {
		for _, p := range live {
			if fetcher.fetchManifest(ctx, p, hashes[0]) == nil {
				return false
			}
		}
		return true
	})

	// The relay comparison: the same bytes over ordinary HTTP on loopback, which
	// is the shape of GET /api/madnetwork/stream/{hash} — no mesh, no swarm, one
	// well-connected server. It is the floor every mesh number is read against.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blobs[0])
	}))
	defer relay.Close()
	relayTook := timeRelay(t, relay.URL, blobSize)

	type result struct {
		name  string
		took  time.Duration
		err   error
		stats TransferStats
	}
	results := make([]result, 0, len(scenarios))

	for i, sc := range scenarios {
		// Freshest first, stale appended — the order a real plan arrives in, since
		// MadnetworkBlobProviders sorts by last_seen.
		plan := make([]*BlobProvider, 0, len(live))
		plan = append(plan, live[:len(live)-sc.stale]...)
		for g := 0; g < sc.stale; g++ {
			// A REAL ed25519 key with no node behind it. It has to be well-formed:
			// the first version of this test used k("ghost0"), which is not hex, so
			// every ghost was refused locally by NormalizeKey in microseconds and the
			// measurement said a stale holder costs nothing. A stale advertisement is
			// not a malformed one — it names a key that derives to a valid mesh
			// address that nothing answers to, so the dial goes into a mesh with no
			// route, and that is the whole expense.
			plan = append(plan, &BlobProvider{PublicKey: ghosts[g]})
		}

		fctx, cancel := context.WithTimeout(ctx, chaosDeadline)
		start := time.Now()
		tr, err := fetcher.EnsureBlobFrom(fctx, hashes[i], int64(blobSize), plan)
		if err == nil {
			select {
			case <-tr.Done():
				err = tr.Err()
			case <-fctx.Done():
				err = fctx.Err()
			}
		}
		took := time.Since(start)
		cancel()

		r := result{name: sc.name, took: took, err: err}
		if tr != nil {
			r.stats = tr.Stats()
		}
		results = append(results, r)
	}

	// ── the table ────────────────────────────────────────────────────────────
	t.Logf("2 MiB blob, 3 holders. PerChunk=%s here, %s shipped (×%d) — that is the "+
		"binding timeout, since a dead holder's dial never connects and ChunkStall never arms",
		chaosPerChunk, 2*time.Minute, staleScale)
	t.Logf("%-18s %12s %12s  %s", "scenario", "measured", "shipped", "outcome")
	t.Logf("%-18s %12s %12s  %s", "relay (plain HTTP)", relayTook.Round(time.Millisecond), "—", "ok")
	for _, r := range results {
		outcome := "ok"
		if r.err != nil {
			outcome = "FAILED: " + r.err.Error()
		}
		t.Logf("%-18s %12s %12s  %s (mode=%s %d/%d chunks, retries=%d failovers=%d stalls=%d)",
			r.name, r.took.Round(time.Millisecond), (r.took * staleScale).Round(time.Second),
			outcome, r.stats.Mode, r.stats.ChunksDone, r.stats.Chunks,
			r.stats.Retries, r.stats.Failovers, r.stats.Stalls)
		for _, p := range r.stats.Providers {
			note := ""
			if p.Dropped {
				note = " retired"
			}
			if p.LastError != "" {
				note += ": " + p.LastError
			}
			t.Logf("%-18s   holder %s… %8d B in %2d chunk(s), %d failure(s)%s",
				"", short(p.PublicKey), p.Bytes, p.Chunks, p.Failures, note)
		}
	}

	// ── the claims ───────────────────────────────────────────────────────────
	if results[0].err != nil {
		t.Fatalf("the all-live fetch failed: %v", results[0].err)
	}
	if results[len(results)-1].err == nil {
		t.Error("a plan of nothing but stale holders reported success")
	}
	for _, r := range results[:len(results)-1] {
		if r.err != nil {
			t.Errorf("%s: a live holder was present and the fetch still failed: %v", r.name, r.err)
		}
	}
	// The point of the whole exercise: dead entries in the plan cost time even
	// though a live holder is right there carrying every byte.
	if results[1].took <= results[0].took {
		t.Errorf("one stale holder cost nothing (%s vs %s all-live) — the tax this "+
			"test exists to measure did not appear", results[1].took, results[0].took)
	}
}

// timeRelay downloads the blob over plain HTTP and returns how long it took.
func timeRelay(t *testing.T, url string, want int) time.Duration {
	t.Helper()
	start := time.Now()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("relay GET: %v", err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("relay read: %v", err)
	}
	took := time.Since(start)
	if n != int64(want) {
		t.Fatalf("relay delivered %d bytes, want %d", n, want)
	}
	return took
}

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}
