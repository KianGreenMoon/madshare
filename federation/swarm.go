//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// Adaptive chunk sizing: the chunk size scales with the file so a small
	// track gets small chunks (finer streaming/parallelism) and a large one is
	// capped. The chosen size is written into the manifest, so a fetcher never
	// assumes it and this policy can change without a protocol break.
	minChunkSize = 256 << 10 // 256 KiB
	maxChunkSize = 4 << 20   // 4 MiB
	targetChunks = 12        // rough chunk count a mid-size file aims for

	maxChunkWorkers = 8               // parallel chunk fetches per transfer
	perChunkTimeout = 5 * time.Minute // bound one chunk fetch
	seedWriteChunk  = 32 << 10        // rate-cap granularity on the serve path
)

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

// ── Chunk manifest ───────────────────────────────────────────────────────────

// blobManifest describes a blob's chunk layout for multi-source fetch. It is
// self-describing (ChunkSize is authoritative) and content-addressed (Hash is
// the whole-file SHA-256), so it is immutable and freely memoized.
type blobManifest struct {
	Protocol  int      `json:"protocol"`
	Hash      string   `json:"hash"`
	Size      int64    `json:"size"`
	ChunkSize int64    `json:"chunk_size"`
	Filename  string   `json:"filename,omitempty"`
	Chunks    []string `json:"chunks"` // per-chunk SHA-256 hex, in order
}

// valid reports whether a manifest received from a friend is structurally sound
// for the requested hash — the chunk count must match the declared layout.
func (m *blobManifest) valid(hash string) bool {
	return m != nil && m.Hash == hash && m.Size >= 0 && m.ChunkSize > 0 &&
		len(m.Chunks) == chunkCount(m.Size, m.ChunkSize)
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
	cs := chunkSizeFor(size)
	m := &blobManifest{
		Protocol:  ProtocolVersion,
		Hash:      hash,
		Size:      size,
		ChunkSize: cs,
		Filename:  filepath.Base(path),
		Chunks:    make([]string, 0, chunkCount(size, cs)),
	}
	buf := make([]byte, cs)
	for {
		nr, rerr := io.ReadFull(f, buf)
		if nr > 0 {
			sum := sha256.Sum256(buf[:nr])
			m.Chunks = append(m.Chunks, hex.EncodeToString(sum[:]))
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return m, nil
}

// handleManifest serves GET /madnetwork/v0/manifest/{hash}: the chunk layout of
// a blob this node holds and will seed (same friends-only + seedable gate as
// the blob endpoint).
func (n *Node) handleManifest(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	p := n.peerFromRemote(r)
	if p == nil || p.State != PeerFriend {
		http.Error(w, "manifests are served to friends only", http.StatusForbidden)
		return
	}
	hash := r.PathValue("hash")
	if !isBlobHash(hash) {
		http.NotFound(w, r)
		return
	}
	path, ok := n.seedableBlob(r.Context(), hash)
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
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/manifest/%s", addr, MeshPort, hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := n.client.Do(req)
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
	enabled, cache, err := n.store.SeedingPolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hashes := []string{}
	if enabled && cache {
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
		n.logger.Printf("federation: store holdings of %q: %v", p.Name, err)
	}
}

// ── Seeding gate ─────────────────────────────────────────────────────────────

// seedableBlob resolves a hash to a local path this node will serve to friends,
// honouring the seeding policy: nothing when seeding is off; a published library
// blob always; a cache blob only when cache-seeding is on. Returns ("", false)
// when the hash is not seedable.
func (n *Node) seedableBlob(ctx context.Context, hash string) (string, bool) {
	enabled, cache, err := n.store.SeedingPolicy(ctx)
	if err != nil {
		n.logger.Printf("federation: seeding policy: %v", err)
		return "", false
	}
	if !enabled {
		return "", false
	}
	if n.resolveBlob != nil {
		if vis, found, verr := n.store.BlobPubliclyVisible(ctx, hash); verr == nil && found && vis {
			if path, ok := n.resolveBlob(hash); ok {
				return path, true
			}
		}
	}
	if cache && n.cacheDir != "" {
		path := filepath.Join(n.cacheDir, hash)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// ── Multi-source chunk fetch ─────────────────────────────────────────────────

// fetchSwarm downloads a blob chunk-by-chunk from all advertising holders in
// parallel, verifying each chunk against the manifest. The caller verifies the
// assembled whole-file hash afterwards. The part file is pre-sized so chunks
// can be written at their offsets (WriteAt is safe for concurrent
// non-overlapping writes).
func (n *Node) fetchSwarm(t *transfer, man *blobManifest, holders []*Peer) error {
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
	plan := newChunkPlan(man, holders)
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
					plan.fail(idx, -1, ErrNoHolder) // all providers dead → aborts
					continue
				}
				if err := n.fetchChunk(t, f, man, idx, p); err != nil {
					plan.fail(idx, pidx, err)
				} else {
					plan.succeed(idx, t)
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
func (n *Node) fetchChunk(t *transfer, f *os.File, man *blobManifest, idx int, p *Peer) error {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return err
	}
	start := int64(idx) * man.ChunkSize
	end := start + man.ChunkSize
	if end > man.Size {
		end = man.Size
	}
	length := end - start
	ctx, cancel := context.WithTimeout(n.transferCtx, perChunkTimeout)
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
	body := make([]byte, length)
	if _, err := io.ReadFull(io.LimitReader(resp.Body, length), body); err != nil {
		return fmt.Errorf("chunk %d: %w", idx, err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != man.Chunks[idx] {
		return fmt.Errorf("chunk %d: hash mismatch from %q", idx, p.Name)
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

	chunkSize int64
	size      int64

	providers []*Peer
	dead      []bool // per-provider
	rr        int    // round-robin cursor
}

func newChunkPlan(man *blobManifest, holders []*Peer) *chunkPlan {
	nc := len(man.Chunks)
	cp := &chunkPlan{
		pending:   make([]int, nc),
		done:      make([]bool, nc),
		remaining: nc,
		chunkSize: man.ChunkSize,
		size:      man.Size,
		providers: holders,
		dead:      make([]bool, len(holders)),
	}
	for i := range cp.pending {
		cp.pending[i] = i
	}
	cp.cond = sync.NewCond(&cp.mu)
	return cp
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
// watermark and publishing it to the transfer.
func (cp *chunkPlan) succeed(idx int, t *transfer) {
	cp.mu.Lock()
	cp.inFlight--
	if !cp.done[idx] {
		cp.done[idx] = true
		cp.remaining--
		for cp.watermark < len(cp.done) && cp.done[cp.watermark] {
			cp.watermark++
		}
	}
	progress := int64(cp.watermark) * cp.chunkSize
	if progress > cp.size {
		progress = cp.size
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
	t.setProgress(progress)
}

// fail marks a provider dead (pidx < 0 = no provider to blame) and re-queues the
// chunk, unless no live provider remains — then it aborts the whole transfer.
func (cp *chunkPlan) fail(idx, pidx int, err error) {
	cp.mu.Lock()
	cp.inFlight--
	if pidx >= 0 {
		cp.dead[pidx] = true
	}
	if cp.liveProviders() == 0 {
		if !cp.aborted {
			cp.aborted, cp.err = true, err
		}
	} else {
		cp.pending = append(cp.pending, idx)
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
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
