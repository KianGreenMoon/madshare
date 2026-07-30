//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The swarm (federation F4): multi-source chunk fetch, the chunk manifest, the
// holdings tracker, and the seeding rate cap. Design:
// docs/architecture/federation.md §Distribution.
//
// A blob's swarm id is its whole-file SHA-256 (the same content address used
// everywhere), so it is NOT a Merkle root and per-chunk hashes cannot be
// derived from it. The manifest carries the per-chunk hashes explicitly; they
// enable early per-chunk verification and bad-chunk re-fetch across sources,
// while the assembled whole-file hash stays the authoritative anchor (verified
// before a blob enters the cache). Manifests come from trusted friends and a
// lie only wastes bandwidth (caught by the whole-file check).

const (
	// Adaptive chunk sizing: the bulk chunk size scales with the file so a small
	// track gets small chunks and a large one is capped. The cap doubles as the
	// **seek granularity** — a seek into an un-fetched region waits for the one
	// chunk covering it — so it is kept modest (1 MiB) rather than large. The
	// layout is written into the manifest, so a fetcher never assumes it and the
	// sizing policy can change without a protocol break.
	minChunkSize = 256 << 10 // 256 KiB — the ramp floor and small-file chunk
	maxChunkSize = 1 << 20   // 1 MiB — bulk cap = seek granularity
	targetChunks = 12        // rough bulk-chunk count a mid-size file aims for

	maxChunkWorkers      = 8        // parallel chunk fetches per transfer
	maxManifestChunk     = 64 << 20 // reject a manifest whose chunk size exceeds this (DoS guard)
	providerFailureLimit = 4        // consecutive fetch failures before dropping a holder (reset on success)
	seedWriteChunk       = 32 << 10 // rate-cap granularity on the serve path
)

// The fetch deadlines this file works to — Timeouts.PerChunk (one chunk's
// overall backstop), Timeouts.ChunkStall (the idle-read watchdog that catches a
// hung mesh connection long before the backstop) and Timeouts.Manifest (one
// manifest probe) — are node fields; see defaultIntervals/defaultTimeouts in
// node.go.

// errChunkCorrupt marks a chunk whose bytes failed per-chunk verification — the
// holder served wrong data, so it is dropped for the rest of the transfer
// (unlike a transient network error, which is retried).
var errChunkCorrupt = errors.New("chunk failed per-chunk verification")

// readStall reads up to n bytes from r, cancelling the fetch (via cancel) if no
// bytes arrive for `stall` — so a hung mesh connection is detected in ~stall
// rather than waiting out the whole Timeouts.PerChunk backstop. exact requires
// all n bytes; onStall (may be nil) is called when the watchdog fires, which is
// what makes a stall countable in TransferStats rather than merely fatal.
func readStall(cancel context.CancelFunc, r io.Reader, n int64, exact bool, stall time.Duration, onStall func()) ([]byte, error) {
	buf := make([]byte, n)
	watchdog := time.AfterFunc(stall, func() {
		if onStall != nil {
			onStall()
		}
		cancel()
	})
	defer watchdog.Stop()
	var got int64
	for got < n {
		watchdog.Reset(stall)
		m, err := r.Read(buf[got:])
		got += int64(m)
		if err == io.EOF {
			if exact && got < n {
				return nil, io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return buf[:got], nil
}

// chunkSizeFor picks the chunk size for a blob of the given total size: the
// smallest power-of-two ≥ size/targetChunks, clamped to [minChunkSize,
// maxChunkSize]. Deterministic, so any node builds the same manifest.
func chunkSizeFor(size int64) int64 {
	if size <= minChunkSize {
		return minChunkSize
	}
	want := size / targetChunks
	cs := int64(minChunkSize)
	for cs < want && cs < maxChunkSize {
		cs <<= 1
	}
	if cs > maxChunkSize {
		cs = maxChunkSize
	}
	return cs
}

// chunkCount is the number of chunks a size splits into at chunkSize.
func chunkCount(size, chunkSize int64) int {
	if chunkSize <= 0 {
		return 0
	}
	return int((size + chunkSize - 1) / chunkSize)
}

// leadSizes returns the small "ramp" chunk sizes that precede the uniform bulk
// chunks: minChunkSize doubling up to (not including) bulk. This makes the first
// byte of a stream — and the first byte after a seek to the front — available
// after a small chunk, while the bulk stays efficient. Empty when bulk is
// already the floor (small files are uniformly min-chunked) or the file ends
// within the ramp (the trailing chunk simply covers the remainder).
func leadSizes(size, bulk int64) []int64 {
	if bulk <= minChunkSize {
		return nil
	}
	var lead []int64
	var acc int64
	for cs := int64(minChunkSize); cs < bulk; cs <<= 1 {
		if acc+cs >= size {
			break
		}
		lead = append(lead, cs)
		acc += cs
	}
	return lead
}

// chunkLayout is a blob's chunk boundary table: offsets[i] is the byte start of
// chunk i and offsets[len-1] == size, so chunk i is [offsets[i], offsets[i+1]).
// It unifies the small lead ramp and the uniform bulk into one lookup, replacing
// the old "idx*ChunkSize" assumption on the fetch/relay paths.
type chunkLayout struct{ offsets []int64 }

// buildLayout constructs the boundary table from the size, the bulk chunk size,
// and the lead ramp. Deterministic, so a fetcher rebuilds a holder's layout (and
// can guess it from the advertised size before the manifest arrives).
func buildLayout(size, bulk int64, lead []int64) *chunkLayout {
	offs := []int64{0}
	pos := int64(0)
	for _, ls := range lead {
		if pos >= size || ls <= 0 {
			break
		}
		pos += ls
		if pos > size {
			pos = size
		}
		offs = append(offs, pos)
	}
	for pos < size && bulk > 0 {
		pos += bulk
		if pos > size {
			pos = size
		}
		offs = append(offs, pos)
	}
	return &chunkLayout{offsets: offs}
}

func (l *chunkLayout) count() int { return len(l.offsets) - 1 }

// rangeOf returns [start, end) of chunk idx.
func (l *chunkLayout) rangeOf(idx int) (int64, int64) { return l.offsets[idx], l.offsets[idx+1] }

// offsetOf returns the byte start of chunk idx (or the total size for idx==count).
func (l *chunkLayout) offsetOf(idx int) int64 {
	if idx >= len(l.offsets) {
		return l.offsets[len(l.offsets)-1]
	}
	return l.offsets[idx]
}

// chunkAt returns the index of the chunk containing offset (clamped to a valid
// chunk index).
func (l *chunkLayout) chunkAt(offset int64) int {
	n := l.count()
	if n <= 0 {
		return 0
	}
	i := sort.Search(n, func(i int) bool { return l.offsets[i+1] > offset })
	if i >= n {
		i = n - 1
	}
	return i
}

// ── Chunk manifest ───────────────────────────────────────────────────────────

// blobManifest describes a blob's chunk layout for multi-source fetch. It is
// self-describing (ChunkSize is authoritative) and content-addressed (Hash is
// the whole-file SHA-256), so it is immutable and freely memoized.
type blobManifest struct {
	Protocol  int      `json:"protocol"`
	Hash      string   `json:"hash"`
	Size      int64    `json:"size"`
	ChunkSize int64    `json:"chunk_size"`           // bulk chunk size
	LeadSizes []int64  `json:"lead_sizes,omitempty"` // small ramp chunks before the bulk (empty = uniform)
	Filename  string   `json:"filename,omitempty"`
	Chunks    []string `json:"chunks"` // per-chunk SHA-256 hex, lead ramp then bulk, in order
}

// layout builds the boundary table implied by the manifest (bulk ChunkSize +
// LeadSizes ramp). Cheap; callers build it once per transfer.
func (m *blobManifest) layout() *chunkLayout {
	return buildLayout(m.Size, m.ChunkSize, m.LeadSizes)
}

// valid reports whether a manifest received from a friend is structurally sound
// for the requested hash: the ramp sizes must be positive and below the bulk
// size, the bulk size within a sane bound (a DoS guard against a manifest that
// would force a huge per-chunk allocation), and the chunk count must match the
// declared layout.
func (m *blobManifest) valid(hash string) bool {
	if m == nil || m.Hash != hash || m.Size < 0 || m.ChunkSize <= 0 || m.ChunkSize > maxManifestChunk {
		return false
	}
	for _, ls := range m.LeadSizes {
		if ls <= 0 || ls >= m.ChunkSize {
			return false
		}
	}
	return len(m.Chunks) == m.layout().count()
}

// manifest returns the (memoized) chunk manifest for a blob this node holds at
// path. Content-addressed, so it is computed once per hash and cached.
func (n *Node) manifest(path, hash string) (*blobManifest, error) {
	n.manifestMu.Lock()
	if m, ok := n.manifests[hash]; ok {
		n.manifestMu.Unlock()
		return m, nil
	}
	n.manifestMu.Unlock()

	m, err := buildManifest(path, hash)
	if err != nil {
		return nil, err
	}
	n.manifestMu.Lock()
	n.manifests[hash] = m
	n.manifestMu.Unlock()
	return m, nil
}

// buildManifest reads the file and hashes each chunk. The whole-file hash is
// the caller-supplied content address (already verified as the file's identity
// upstream); this only records the per-chunk hashes and the layout.
func buildManifest(path, hash string) (*blobManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	bulk := chunkSizeFor(size)
	lead := leadSizes(size, bulk)
	layout := buildLayout(size, bulk, lead)
	m := &blobManifest{
		Protocol:  ProtocolVersion,
		Hash:      hash,
		Size:      size,
		ChunkSize: bulk,
		LeadSizes: lead,
		Filename:  filepath.Base(path),
		Chunks:    make([]string, 0, layout.count()),
	}
	buf := make([]byte, bulk) // bulk is the largest chunk; lead chunks are smaller
	for i := 0; i < layout.count(); i++ {
		start, end := layout.rangeOf(i)
		if _, err := io.ReadFull(f, buf[:end-start]); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(buf[:end-start])
		m.Chunks = append(m.Chunks, hex.EncodeToString(sum[:]))
	}
	return m, nil
}

// handleManifest serves GET /madnetwork/v0/manifest/{hash}: the chunk layout of
// a blob this node holds and will seed to the requester (the same audience +
// seedable gate as the blob endpoint — a manifest is a description of bytes we
// are willing to hand over, so the two must never disagree).
func (n *Node) handleManifest(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	aud, ok := n.serveAudience(r)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hash := r.PathValue("hash")
	if !isBlobHash(hash) {
		http.NotFound(w, r)
		return
	}
	path, ok := n.seedableBlob(r.Context(), hash, aud)
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := n.manifest(path, hash)
	if err != nil {
		n.logger.Printf("federation: build manifest %s: %v", hash, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// fetchManifest pulls one friend's manifest for hash; nil (no error surfaced)
// when the holder lacks it or is too old to speak the endpoint.
func (n *Node) fetchManifest(ctx context.Context, p *Peer, hash string) *blobManifest {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return nil
	}
	// Share the pooled blob transport (not the short control client) so the
	// chunk fetches that follow reuse this warm connection; bound this one probe
	// so a slow holder doesn't stall the whole transfer.
	cctx, cancel := context.WithTimeout(ctx, n.timeouts.Manifest)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/manifest/%s", addr, MeshPort, hash)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := n.blobClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var m blobManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil
	}
	if !m.valid(hash) {
		return nil
	}
	return &m
}

// fetchAnyManifest returns the first valid manifest offered by any holder (and
// which holder), or nil when none serve one — the signal to fall back to the F3
// whole-file fetch.
func (n *Node) fetchAnyManifest(ctx context.Context, holders []*Peer, hash string) *blobManifest {
	for _, p := range holders {
		if ctx.Err() != nil {
			return nil
		}
		if m := n.fetchManifest(ctx, p, hash); m != nil {
			return m
		}
	}
	return nil
}

// ── Holdings tracker ─────────────────────────────────────────────────────────

// holdingsMessage is the reply to GET /madnetwork/v0/holdings: the flat list of
// cache-held content hashes this node will seed (the library is already in the
// catalog, so holdings carries only the download cache).
type holdingsMessage struct {
	Protocol int      `json:"protocol"`
	Hashes   []string `json:"hashes"`
}

// cacheHoldings lists the finished blobs in the download cache (skipping
// in-progress ".part" files, which fail the hash-shape check).
func (n *Node) cacheHoldings() []string {
	if n.cacheDir == "" {
		return nil
	}
	ents, err := os.ReadDir(n.cacheDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && isBlobHash(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// handleHoldings serves GET /madnetwork/v0/holdings — friends only; empty when
// seeding or cache-seeding is off (the node then advertises nothing extra
// beyond its catalog).
func (n *Node) handleHoldings(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	p := n.peerFromRemote(r)
	if p == nil || p.State != PeerFriend {
		http.Error(w, "holdings are served to friends only", http.StatusForbidden)
		return
	}
	aud, err := n.store.PeerAudience(r.Context(), p.ID)
	if err != nil {
		n.logger.Printf("federation: resolve audience of %q: %v", p.Display(), err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	enabled, cache, err := n.store.SeedingPolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hashes := []string{}
	// A guest-only friend is never served cache blobs (seedableBlob), so it is
	// not told about them either — advertising and serving read one rule.
	if enabled && cache && !aud.GuestOnly {
		hashes = n.cacheHoldings()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(holdingsMessage{Protocol: ProtocolVersion, Hashes: hashes})
}

// syncHoldings pulls one friend's cache-holdings list and replaces the cached
// copy (federation_holdings), so their downloaded blobs become fetchable from
// them. Called on the same cadence as the catalog sync.
func (n *Node) syncHoldings(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/holdings", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return // unreachable — the refresh loop retries
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return // older peer without the endpoint
	}
	var msg holdingsMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&msg); err != nil {
		return
	}
	valid := make([]string, 0, len(msg.Hashes))
	for _, h := range msg.Hashes {
		if isBlobHash(h) {
			valid = append(valid, h)
		}
	}
	if err := n.store.ReplacePeerHoldings(ctx, p.ID, valid); err != nil {
		n.logger.Printf("federation: store holdings of %q: %v", p.Display(), err)
	}
}

// ── Seeding gate ─────────────────────────────────────────────────────────────

// seedableBlob resolves a hash to a local path this node will serve to the given
// audience, honouring the seeding policy: nothing when seeding is off; a library
// blob when it is published *and inside the audience's scope* (F5); a cache blob
// only when cache-seeding is on — and never to a guest-only audience, because a
// blob this node merely fetched carries no license we can vouch for. Returns
// ("", false) when the hash is not seedable.
func (n *Node) seedableBlob(ctx context.Context, hash string, aud Audience) (string, bool) {
	enabled, cache, err := n.store.SeedingPolicy(ctx)
	if err != nil {
		n.logger.Printf("federation: seeding policy: %v", err)
		return "", false
	}
	if !enabled {
		return "", false
	}
	if n.resolveBlob != nil {
		if vis, found, verr := n.store.BlobVisibleTo(ctx, hash, aud); verr == nil && found && vis {
			if path, ok := n.resolveBlob(hash); ok {
				return path, true
			}
		}
	}
	if cache && !aud.GuestOnly && n.cacheDir != "" {
		path := filepath.Join(n.cacheDir, hash)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// serveAudience is the byte endpoints' gate: who this request is answered for,
// and whether it may be answered at all. A friend gets its mapped audience; any
// other mesh node gets the guest audience — the open swarm, which reaches
// guest-playable content and nothing else (F5, §Sharing scope). Blocked peers
// never arrive here; meshAuth refuses them the whole surface first.
func (n *Node) serveAudience(r *http.Request) (Audience, bool) {
	p := n.peerFromRemote(r)
	if p == nil {
		return GuestAudience, true
	}
	if p.State != PeerFriend {
		// A pending peer is not yet trusted — treat it exactly like a stranger
		// rather than granting it friendship's reach early.
		return GuestAudience, true
	}
	aud, err := n.store.PeerAudience(r.Context(), p.ID)
	if err != nil {
		n.logger.Printf("federation: resolve audience of %q: %v", p.Display(), err)
		return Audience{}, false
	}
	return aud, true
}

// ── Multi-source chunk fetch ─────────────────────────────────────────────────

// fetchRange fetches [start, start+length) from one holder over the mesh (a
// plain HTTP Range request against the F3 blob endpoint) and returns the bytes.
// The holder clamps at EOF, so a short file yields fewer than length bytes.
func (n *Node) fetchRange(ctx context.Context, p *Peer, hash string, start, length int64, onStall func()) ([]byte, error) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, n.timeouts.PerChunk)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", addr, MeshPort, hash)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	resp, err := n.blobClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("range %d-%d: holder answered %s", start, start+length-1, resp.Status)
	}
	return readStall(cancel, io.LimitReader(resp.Body, length), length, false, n.timeouts.ChunkStall, onStall)
}

// chunk0Prefetch is a speculative chunk-0 fetch overlapped with the manifest
// round trip. take() consumes it once the manifest is known, keeping the bytes
// only if the guessed layout matched and chunk 0 verifies.
type chunk0Prefetch struct {
	active   bool
	from     *Peer       // the holder it was fetched from (for stats crediting)
	guessLen int64       // the chunk-0 length we speculatively fetched
	ch       chan []byte // buffered(1): the fetched bytes, or nil on failure
}

// speculateChunk0 starts fetching chunk 0 from the first holder using the chunk
// layout implied by the advertised file size (deterministic via chunkSizeFor +
// the lead ramp), so it lands in parallel with the manifest fetch. With the ramp
// chunk 0 is a small lead chunk, so this speculative fetch is small.
func (n *Node) speculateChunk0(t *transfer, holders []*Peer) *chunk0Prefetch {
	pf := &chunk0Prefetch{ch: make(chan []byte, 1)}
	if len(holders) == 0 {
		pf.ch <- nil
		return pf
	}
	size := t.Size()
	bulk := chunkSizeFor(size)
	gl := buildLayout(size, bulk, leadSizes(size, bulk))
	if gl.count() == 0 { // unknown/zero size — skip the guess
		pf.ch <- nil
		return pf
	}
	_, end := gl.rangeOf(0)
	pf.guessLen = end // chunk 0 = [0, end)
	pf.active = true
	p := holders[0]
	pf.from = p
	go func() {
		data, err := n.fetchRange(n.transferCtx, p, t.hash, 0, pf.guessLen, t.stats.noteStall)
		if err != nil {
			pf.ch <- nil
			return
		}
		pf.ch <- data
	}()
	return pf
}

// take waits for the speculative fetch and returns chunk 0's bytes iff the
// manifest confirms the guessed chunk-0 boundary and the bytes verify against
// it; nil otherwise (fetchSwarm then fetches chunk 0 through the normal plan).
// It blocks only until the speculative fetch resolves (bounded by perChunkTimeout).
func (pf *chunk0Prefetch) take(man *blobManifest) []byte {
	if !pf.active {
		return nil
	}
	data := <-pf.ch
	if data == nil || len(man.Chunks) == 0 {
		return nil
	}
	_, end := man.layout().rangeOf(0)
	if end != pf.guessLen || int64(len(data)) != end {
		return nil // the real chunk-0 boundary differed from the guess
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != man.Chunks[0] {
		return nil
	}
	return data
}

// discard abandons the speculative fetch when no manifest was obtained. The
// buffered(1) channel lets the fetch goroutine finish its single send unread, so
// nothing leaks (it never touches the part file — only in-memory bytes).
func (pf *chunk0Prefetch) discard() {}

// fetchSwarm downloads a blob chunk-by-chunk from all advertising holders in
// parallel, verifying each chunk against the manifest. prefetched0 (when
// non-nil) is chunk 0 already fetched and verified during the manifest round
// trip — written up front so the first byte is ready immediately (from0 is the
// holder it came from, credited in the stats). The caller verifies the assembled
// whole-file hash afterwards. The part file is pre-sized so chunks can be
// written at their offsets (WriteAt is safe for concurrent non-overlapping
// writes).
func (n *Node) fetchSwarm(t *transfer, man *blobManifest, holders []*Peer, prefetched0 []byte, from0 *Peer) error {
	layout := man.layout()
	f, err := os.OpenFile(t.partPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if man.Size > 0 {
		if err := f.Truncate(man.Size); err != nil {
			f.Close()
			return err
		}
	}
	done0 := false
	if len(prefetched0) > 0 {
		if _, err := f.WriteAt(prefetched0, 0); err != nil {
			f.Close()
			return err
		}
		done0 = true
	}
	plan := newChunkPlan(man, layout, holders, done0, t.stats)
	t.stats.setChunks(layout.count())
	// Expose per-chunk readiness (the layout) + the seek-priority hook to the
	// streaming relay, pre-marking chunk 0 so a waiting reader is released without
	// a round trip.
	t.beginChunks(layout, plan.prioritize)
	if done0 {
		t.stats.noteSucceed(0, from0, int64(len(prefetched0)))
		t.chunkDone(0, plan.watermarkBytes())
	}
	workers := len(holders) * 2
	if workers > maxChunkWorkers {
		workers = maxChunkWorkers
	}
	if workers > len(man.Chunks) {
		workers = len(man.Chunks)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx, ok := plan.next()
				if !ok {
					return
				}
				p, pidx, ok := plan.pickProvider()
				if !ok {
					plan.fail(idx, -1, ErrNoHolder, false) // all providers dead → aborts
					continue
				}
				if err := n.fetchChunk(t, f, layout, man, idx, p); err != nil {
					plan.fail(idx, pidx, err, errors.Is(err, errChunkCorrupt))
				} else {
					n.observePeerAlive(p) // a verified chunk is liveness proof
					plan.succeed(idx, pidx, t)
				}
			}
		}()
	}
	wg.Wait()
	cerr := f.Close()
	if plan.err != nil {
		return plan.err
	}
	return cerr
}

// fetchChunk fetches one chunk over the mesh (a plain HTTP Range request against
// the F3 blob endpoint), verifies it against the manifest, and writes it at its
// offset.
func (n *Node) fetchChunk(t *transfer, f *os.File, layout *chunkLayout, man *blobManifest, idx int, p *Peer) error {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return err
	}
	start, end := layout.rangeOf(idx)
	length := end - start
	ctx, cancel := context.WithTimeout(n.transferCtx, n.timeouts.PerChunk)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", addr, MeshPort, t.hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	resp, err := n.blobClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 206 for a range; 200 is tolerated only for a single-chunk blob (the whole
	// file IS the one chunk).
	if resp.StatusCode != http.StatusPartialContent &&
		!(resp.StatusCode == http.StatusOK && len(man.Chunks) == 1) {
		return fmt.Errorf("chunk %d: holder answered %s", idx, resp.Status)
	}
	body, err := readStall(cancel, io.LimitReader(resp.Body, length), length, true, n.timeouts.ChunkStall, t.stats.noteStall)
	if err != nil {
		return fmt.Errorf("chunk %d: %w", idx, err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != man.Chunks[idx] {
		return fmt.Errorf("chunk %d: hash mismatch from %q: %w", idx, p.Display(), errChunkCorrupt)
	}
	if _, err := f.WriteAt(body, start); err != nil {
		return err
	}
	return nil
}

// chunkPlan schedules chunk fetches across holders: sequential-priority
// dispatch (lowest index first, so the streaming prefix grows in order), a
// contiguous-from-zero progress watermark, and per-chunk failover — a failed
// chunk is re-queued to a different holder and the offending holder is dropped.
type chunkPlan struct {
	mu   sync.Mutex
	cond *sync.Cond

	pending   []int  // chunk indices awaiting dispatch (initially in order)
	inFlight  int    // dispatched, not yet resolved
	done      []bool // per-chunk completion
	watermark int    // count of contiguous completed chunks from 0
	remaining int    // chunks not yet done
	aborted   bool
	err       error

	layout *chunkLayout

	providers []*Peer
	dead      []bool // per-provider
	provFails []int  // per-provider consecutive failures (reset on success)
	rr        int    // round-robin cursor

	// attempts counts every failed try at each chunk, across all holders, and
	// attemptLimit bounds it. This is what ENDS a hopeless transfer. It used to
	// be a side effect of retiring holders — the fetch stopped once the last one
	// was dropped — which forced the drop rule to double as the termination
	// rule and got healthy holders retired to make transfers finish. The two are
	// separate concerns now: retirement decides who to ask, this decides when to
	// give up. See the note above fail().
	attempts     []int
	attemptLimit int

	stats *transferStats // diagnostics sink; nil outside a real transfer
}

func newChunkPlan(man *blobManifest, layout *chunkLayout, holders []*Peer, done0 bool, st *transferStats) *chunkPlan {
	nc := layout.count()
	cp := &chunkPlan{
		done:      make([]bool, nc),
		remaining: nc,
		layout:    layout,
		providers: holders,
		dead:      make([]bool, len(holders)),
		provFails: make([]int, len(holders)),
		attempts:  make([]int, nc),
		stats:     st,
	}
	// One chunk may be retried as many times as the old rule allowed in total
	// before every holder was retired — so the worst case is bounded exactly as
	// it was, while no longer requiring anyone to be retired to get there.
	cp.attemptLimit = providerFailureLimit * len(holders)
	if cp.attemptLimit < providerFailureLimit {
		cp.attemptLimit = providerFailureLimit
	}
	start := 0
	if done0 && nc > 0 {
		cp.done[0] = true
		cp.watermark = 1
		cp.remaining = nc - 1
		start = 1
	}
	cp.pending = make([]int, 0, nc-start)
	for i := start; i < nc; i++ {
		cp.pending = append(cp.pending, i)
	}
	cp.cond = sync.NewCond(&cp.mu)
	return cp
}

// watermarkBytes is the contiguous-from-zero readable length in bytes.
func (cp *chunkPlan) watermarkBytes() int64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.layout.offsetOf(cp.watermark)
}

// prioritize moves the chunk covering a requested offset to the front of the
// dispatch queue (if still pending), so the next free worker fetches it — this
// is what makes a streaming tail/seek read fast instead of waiting out the
// sequential prefix.
func (cp *chunkPlan) prioritize(idx int) {
	cp.mu.Lock()
	if idx >= 0 && idx < len(cp.done) && !cp.done[idx] {
		for i, p := range cp.pending {
			if p == idx {
				copy(cp.pending[1:i+1], cp.pending[0:i])
				cp.pending[0] = idx
				break
			}
		}
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
}

// next hands out the next chunk to fetch, blocking while chunks are only
// in-flight (a failure may re-queue one). Returns false when the transfer is
// done or aborted.
func (cp *chunkPlan) next() (int, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for {
		if cp.aborted || cp.remaining == 0 {
			return 0, false
		}
		if len(cp.pending) > 0 {
			idx := cp.pending[0]
			cp.pending = cp.pending[1:]
			cp.inFlight++
			return idx, true
		}
		if cp.inFlight == 0 {
			return 0, false // nothing pending or in flight but work remains — give up
		}
		cp.cond.Wait()
	}
}

// pickProvider returns the next non-dead holder, round-robin.
func (cp *chunkPlan) pickProvider() (*Peer, int, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for tries := 0; tries < len(cp.providers); tries++ {
		i := cp.rr % len(cp.providers)
		cp.rr++
		if !cp.dead[i] {
			return cp.providers[i], i, true
		}
	}
	return nil, -1, false
}

// succeed records a completed chunk, advancing the contiguous progress
// watermark and publishing it to the transfer. A success clears the provider's
// failure streak — an intermittently-stalling holder is forgiven so it is not
// dropped over transient hiccups.
func (cp *chunkPlan) succeed(idx, pidx int, t *transfer) {
	cp.mu.Lock()
	cp.inFlight--
	var from *Peer
	if pidx >= 0 {
		cp.provFails[pidx] = 0
		from = cp.providers[pidx]
	}
	fresh := !cp.done[idx]
	if fresh {
		cp.done[idx] = true
		cp.remaining--
		for cp.watermark < len(cp.done) && cp.done[cp.watermark] {
			cp.watermark++
		}
	}
	start, end := cp.layout.rangeOf(idx)
	progress := cp.layout.offsetOf(cp.watermark)
	cp.cond.Broadcast()
	cp.mu.Unlock()
	if fresh {
		cp.stats.noteSucceed(idx, from, end-start)
	}
	t.chunkDone(idx, progress)
}

// fail re-queues the chunk for another attempt and decides whether to retire the
// holder that missed it.
//
// A corrupt chunk (wrong bytes) retires its holder immediately: bad bytes are
// evidence about the holder itself, and no amount of environmental bad luck
// produces them. A transient error (stall, timeout, unreachable) is weaker
// evidence, because it describes the holder AND the moment. The rule is
// therefore relative: a holder is retired once it is providerFailureLimit
// consecutive failures worse than the best live holder. When some peer is still
// delivering (streak 0) that is exactly the old absolute rule; when every holder
// is equally deep in failures the fetch is in a slow moment, not a bad holder,
// and nobody is retired. A sole holder has nothing to be compared against, so
// the absolute limit stands and the transfer can still end.
//
// Retiring holders is no longer how a hopeless transfer stops — see attempts /
// attemptLimit, which bound each chunk individually. That separation is the
// point: the old code could only end a fetch by killing every holder, so a
// healthy one had to be declared faulty for the transfer to finish at all
// (.issues/open-issues.md, -race run findings, item 3).
func (cp *chunkPlan) fail(idx, pidx int, err error, corrupt bool) {
	cp.mu.Lock()
	cp.inFlight--
	var from *Peer
	dropped := false
	if pidx >= 0 {
		from = cp.providers[pidx]
		if corrupt {
			cp.dead[pidx] = true
		} else {
			cp.provFails[pidx]++
			if cp.provFails[pidx] >= providerFailureLimit && cp.worseThanPeers(pidx) {
				cp.dead[pidx] = true
			}
		}
		dropped = cp.dead[pidx]
	}
	cp.attempts[idx]++
	switch {
	case cp.liveProviders() == 0:
		if !cp.aborted {
			cp.aborted, cp.err = true, err
		}
	case cp.attempts[idx] >= cp.attemptLimit:
		if !cp.aborted {
			cp.aborted, cp.err = true, fmt.Errorf(
				"chunk %d unfetchable after %d attempts: %w", idx, cp.attempts[idx], err)
		}
	default:
		cp.pending = append(cp.pending, idx)
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
	cp.stats.noteFail(idx, from, err, corrupt)
	if dropped {
		cp.stats.noteDropped(from)
	}
}

// worseThanPeers reports whether provider i is failing out of line with the
// other live holders — the relative half of the retirement rule above. Caller
// holds cp.mu.
//
// It compares consecutive-failure streaks, which are already maintained and are
// reset by any success: a holder that keeps delivering sits at 0, so anything
// providerFailureLimit above it is demonstrably the odd one out. With no live
// peer left to compare against it returns true, leaving the absolute limit in
// force so a fetch against a single dead holder still terminates.
//
// This leans on dispatch being round-robin. A streak of 0 is read as "this peer
// is doing fine", which is only sound because every live holder is handed work
// in rotation, so an idle holder cannot sit at 0 while others struggle. If
// pickProvider ever becomes speed-aware (.issues/open-issues.md, "swarm provider
// selection is speed-blind"), a deprioritised holder could hold a 0 streak
// without having earned it, and the benchmark would need to ignore holders that
// have not been tried recently.
func (cp *chunkPlan) worseThanPeers(i int) bool {
	best := -1
	for j := range cp.providers {
		if j == i || cp.dead[j] {
			continue
		}
		if best < 0 || cp.provFails[j] < best {
			best = cp.provFails[j]
		}
	}
	if best < 0 {
		return true
	}
	return cp.provFails[i] >= best+providerFailureLimit
}

// liveProviders counts non-dead holders; caller holds cp.mu.
func (cp *chunkPlan) liveProviders() int {
	live := 0
	for _, d := range cp.dead {
		if !d {
			live++
		}
	}
	return live
}

// ── Seeding rate cap ─────────────────────────────────────────────────────────

// rateLimiter is a simple byte-rate token bucket over the blob-serve write path
// (federation F4 seed_rate_kib). Nil means unlimited.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes/sec
	burst  float64 // max accumulated tokens
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter capped at bytesPerSec, or nil (unlimited)
// when bytesPerSec <= 0.
func newRateLimiter(bytesPerSec int64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	r := float64(bytesPerSec)
	return &rateLimiter{rate: r, burst: r, tokens: r, last: time.Now()}
}

// wait blocks until n bytes' worth of tokens are available (or ctx ends).
func (rl *rateLimiter) wait(ctx context.Context, n int) error {
	rl.mu.Lock()
	now := time.Now()
	rl.tokens += rl.rate * now.Sub(rl.last).Seconds()
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.last = now
	rl.tokens -= float64(n)
	var sleep time.Duration
	if rl.tokens < 0 {
		sleep = time.Duration(-rl.tokens / rl.rate * float64(time.Second))
	}
	rl.mu.Unlock()
	if sleep <= 0 {
		return nil
	}
	timer := time.NewTimer(sleep)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttledResponseWriter rate-limits the body written by http.ServeContent
// while passing Header/WriteHeader straight through.
type throttledResponseWriter struct {
	http.ResponseWriter
	rl  *rateLimiter
	ctx context.Context
}

func (t *throttledResponseWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n := len(p) - written
		if n > seedWriteChunk {
			n = seedWriteChunk
		}
		if err := t.rl.wait(t.ctx, n); err != nil {
			return written, err
		}
		m, err := t.ResponseWriter.Write(p[written : written+n])
		written += m
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// verifyFileHash reports whether the file at path hashes to the content hash —
// the authoritative whole-file check after chunk assembly.
func verifyFileHash(path, hash string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != hash {
		return fmt.Errorf("file hashes to %s, not the requested content hash", got)
	}
	return nil
}
