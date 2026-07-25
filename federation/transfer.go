//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Direct transfer (federation F3): fetch-by-hash between friends over the
// mesh. The wire is a plain streaming HTTP GET with Range support (decision
// 2026-07-18 — chunking IS HTTP ranges between two trusted endpoints);
// integrity is the content hash itself, verified over the full byte stream,
// with the Merkle chunk protocol deferred to F4 where multi-source fetch
// actually needs per-chunk verification. Design: docs/architecture/federation.md.

// ── Serving side: GET /madnetwork/v0/blob/{hash} ─────────────────────────────

// handleBlob serves a blob this node holds and will seed to the requester. What
// it serves is decided by the requester's *audience* (F5): a friend may fetch
// any blob its own catalog advertises — matching what the F2 catalog + F4
// holdings showed it, filtered by share depth and the user mapping — while any
// other mesh node reaches guest-playable content only (the open swarm). Beyond
// that the seeding gate applies: a published library blob, or a cache blob when
// cache-seeding is on and the requester is a friend (seedableBlob, swarm.go). A
// staged, out-of-scope, or unknown hash is 404 even for a friend.
// http.ServeContent provides HEAD/Range (the swarm's chunk fetches are Range
// requests); Content-Disposition carries the origin filename so the fetching
// node can land it under its real name; the write path honours the seed rate cap.
func (n *Node) handleBlob(w http.ResponseWriter, r *http.Request) {
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
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	out := http.ResponseWriter(w)
	if n.seedLimiter != nil {
		out = &throttledResponseWriter{ResponseWriter: w, rl: n.seedLimiter, ctx: r.Context()}
	}
	http.ServeContent(out, r, info.Name(), info.ModTime(), f)
}

// ── Fetching side ────────────────────────────────────────────────────────────

// transfer is the concrete Transfer: one fetch of one hash, shared by every
// concurrent requester (streams and downloads of the same hash join it).
type transfer struct {
	hash     string
	path     string // final location (cache file, or a local blob short-circuit)
	partPath string // the growing file while the fetch runs ("" when born complete)
	done     chan struct{}

	mu       sync.Mutex
	size     int64
	filename string
	progress int64         // contiguous readable prefix from offset 0 (watermark)
	changed  chan struct{} // recreated on every progress/terminal update
	err      error
	finished bool

	// Chunk-mode readiness (F4 swarm path): per-chunk completion so the streaming
	// relay can read out-of-order regions (a prioritized tail/seek), the chunk
	// layout mapping an offset to its chunk (small lead ramp + uniform bulk), and
	// the plan's seek-priority hook that WaitFor pokes for a not-yet-fetched
	// offset. All nil in the sequential path (F3 whole-file fallback or a
	// born-complete transfer), which relies on the contiguous `progress` watermark.
	layout     *chunkLayout
	chunkOK    []bool
	prioritize func(chunkIdx int)

	// Diagnostics (T1). Independently locked — the swarm records into it from
	// the fetch workers while readers snapshot it, and it must never be in the
	// path that publishes progress.
	stats *transferStats
}

func newTransfer(hash, path, partPath string) *transfer {
	return &transfer{
		hash:     hash,
		path:     path,
		partPath: partPath,
		done:     make(chan struct{}),
		changed:  make(chan struct{}),
		stats:    newTransferStats(),
	}
}

// completedTransfer wraps an already-present file (cache hit or local blob).
func completedTransfer(hash, path string, size int64) *transfer {
	t := newTransfer(hash, path, "")
	t.size, t.progress, t.finished = size, size, true
	t.filename = filepath.Base(path)
	t.stats.setMode("local")
	t.stats.noteFirstByte()
	t.stats.finish()
	close(t.done)
	return t
}

func (t *transfer) Hash() string          { return t.hash }
func (t *transfer) Done() <-chan struct{} { return t.done }

func (t *transfer) Size() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size
}

func (t *transfer) Filename() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.filename
}

func (t *transfer) Progress() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

func (t *transfer) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Open opens the file for reading: the final path once the transfer completed,
// otherwise the growing partial. Opening the partial keeps working across the
// completion rename (the reader holds the inode).
func (t *transfer) Open() (*os.File, error) {
	t.mu.Lock()
	finished := t.finished
	t.mu.Unlock()
	if !finished && t.partPath != "" {
		if f, err := os.Open(t.partPath); err == nil {
			return f, nil
		}
	}
	return os.Open(t.path)
}

func (t *transfer) WaitFor(ctx context.Context, offset int64) error {
	for {
		t.mu.Lock()
		avail := t.availLocked(offset)
		finished, err, ch := t.finished, t.err, t.changed
		layout, prioritize := t.layout, t.prioritize
		t.mu.Unlock()
		if avail > 0 {
			return nil
		}
		if finished {
			if err != nil {
				return err
			}
			return io.EOF // offset at or beyond the verified end
		}
		// Seek-priority (chunk mode): nudge the swarm to fetch the chunk covering
		// this offset next, so a tail/seek read is not gated on the sequential
		// prefix reaching it.
		if layout != nil && prioritize != nil {
			prioritize(layout.chunkAt(offset))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// availLocked returns how many bytes are readable contiguously starting at
// offset. In chunk mode it walks the per-chunk bitmap from offset's chunk (so an
// out-of-order tail chunk is readable before the middle arrives); otherwise it
// uses the contiguous watermark. Caller holds t.mu.
func (t *transfer) availLocked(offset int64) int64 {
	if offset < 0 {
		return 0
	}
	if t.finished && t.err == nil {
		if offset >= t.size {
			return 0
		}
		return t.size - offset
	}
	if t.layout != nil && len(t.chunkOK) > 0 {
		ci := t.layout.chunkAt(offset)
		if ci < 0 || ci >= len(t.chunkOK) || !t.chunkOK[ci] {
			return 0
		}
		end := t.layout.offsetOf(ci + 1)
		for j := ci + 1; j < len(t.chunkOK) && t.chunkOK[j]; j++ {
			end = t.layout.offsetOf(j + 1)
		}
		if end <= offset {
			return 0
		}
		return end - offset
	}
	if t.progress > offset {
		return t.progress - offset
	}
	return 0
}

// Available reports the readable byte count starting at offset (see availLocked).
func (t *transfer) Available(offset int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.availLocked(offset)
}

// setMeta records the origin's Content-Length and filename (first responder
// wins; retries against another holder keep them consistent because the bytes
// must hash identically anyway).
func (t *transfer) setMeta(size int64, filename string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if size > 0 {
		t.size = size
	}
	if filename != "" {
		t.filename = filename
	}
}

func (t *transfer) addProgress(n int64) {
	t.mu.Lock()
	t.progress += n
	readable := t.progress > 0
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
	if readable {
		t.stats.noteFirstByte()
	}
}

func (t *transfer) resetProgress() {
	t.mu.Lock()
	t.progress = 0
	t.layout = nil
	t.chunkOK = nil
	t.prioritize = nil
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
	t.stats.resetAttempt()
}

// beginChunks switches the transfer into chunk mode: readability becomes
// per-chunk (for the streaming relay's random-access reads) and WaitFor gains a
// seek-priority hook into the fetch plan. Called by fetchSwarm before dispatch.
func (t *transfer) beginChunks(layout *chunkLayout, prioritize func(int)) {
	t.mu.Lock()
	t.layout = layout
	t.chunkOK = make([]bool, layout.count())
	t.prioritize = prioritize
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}

// chunkDone marks one chunk readable and advances the contiguous watermark to
// watermarkBytes (the swarm computes it). It publishes both, waking readers
// blocked on this chunk's offset or on the front-of-file progress.
func (t *transfer) chunkDone(idx int, watermarkBytes int64) {
	t.mu.Lock()
	if idx >= 0 && idx < len(t.chunkOK) {
		t.chunkOK[idx] = true
	}
	if watermarkBytes > t.progress {
		t.progress = watermarkBytes
	}
	readable := t.progress > 0
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
	if readable {
		t.stats.noteFirstByte()
	}
}

func (t *transfer) finish(err error) {
	t.mu.Lock()
	t.err = err
	t.finished = true
	if err == nil {
		t.size = t.progress
	}
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
	t.stats.finish()
	close(t.done)
}

// Stats snapshots the transfer's diagnostics (see [TransferStats]).
func (t *transfer) Stats() TransferStats {
	t.mu.Lock()
	hash, size, progress := t.hash, t.size, t.progress
	t.mu.Unlock()
	return t.stats.snapshot(hash, size, progress)
}

// EnsureBlob returns the Transfer for hash, starting a fetch when needed:
// a local library blob or an already-cached file is returned complete; an
// in-flight fetch is joined; otherwise a new fetch starts against the friends
// whose catalogs advertise the hash (most recently seen first). The fetch runs
// on the node's own lifetime context — it outlives the requester on purpose
// (cache-through: the file keeps landing in the cache after a browser
// disconnects).
func (n *Node) EnsureBlob(ctx context.Context, hash string) (Transfer, error) {
	if !isBlobHash(hash) {
		return nil, fmt.Errorf("federation: invalid content hash")
	}
	if n.resolveBlob != nil {
		if path, ok := n.resolveBlob(hash); ok {
			if info, err := os.Stat(path); err == nil {
				return completedTransfer(hash, path, info.Size()), nil
			}
		}
	}
	if n.store == nil || n.cacheDir == "" {
		return nil, fmt.Errorf("federation: transfers not configured")
	}
	final := filepath.Join(n.cacheDir, hash)
	if info, err := os.Stat(final); err == nil {
		return completedTransfer(hash, final, info.Size()), nil
	}

	n.transferMu.Lock()
	defer n.transferMu.Unlock()
	if t, ok := n.transfers[hash]; ok {
		return t, nil
	}
	size, holders, err := n.store.MadnetworkBlobProviders(ctx, hash)
	if err != nil {
		return nil, err
	}
	if len(holders) == 0 {
		return nil, ErrNoHolder
	}
	if err := os.MkdirAll(n.cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("federation: create cache dir: %w", err)
	}
	t := newTransfer(hash, final, final+".part")
	t.size = size
	n.transfers[hash] = t
	go n.runTransfer(t, holders)
	return t, nil
}

// runTransfer fetches a blob from its holders. It prefers the F4 swarm path
// (multi-source parallel chunk fetch via the manifest); if no holder serves a
// manifest — an older F3-only peer — it falls back to the single-source
// whole-file streaming fetch. Either way the assembled bytes are verified
// against the content hash before entering the cache.
func (n *Node) runTransfer(t *transfer, holders []*Peer) {
	defer func() {
		n.transferMu.Lock()
		delete(n.transfers, t.hash)
		n.transferMu.Unlock()
	}()

	// Overlap the manifest fetch with a speculative chunk-0 prefetch so the first
	// playable byte does not wait for two serial mesh round-trips: the chunk
	// layout is deterministic from the file size (chunkSizeFor), so chunk 0 is
	// fetched from the first holder while the manifest loads, and kept only if the
	// manifest confirms the layout and the chunk verifies.
	pf := n.speculateChunk0(t, holders)

	if man := n.fetchAnyManifest(n.transferCtx, holders, t.hash); man != nil {
		t.setMeta(man.Size, man.Filename)
		t.stats.setMode("swarm")
		err := n.fetchSwarm(t, man, holders, pf.take(man), pf.from)
		if err == nil {
			if verr := verifyFileHash(t.partPath, t.hash); verr != nil {
				err = fmt.Errorf("assembled blob failed verification: %w", verr)
			} else if rerr := os.Rename(t.partPath, t.path); rerr != nil {
				err = rerr
			} else {
				n.logger.Printf("federation: fetched %s via swarm (%d chunks, %d bytes)",
					t.hash, len(man.Chunks), man.Size)
				t.finish(nil)
				return
			}
		}
		os.Remove(t.partPath)
		n.logger.Printf("federation: swarm fetch %s failed (%v); falling back to whole-file", t.hash, err)
		t.resetProgress()
	} else {
		pf.discard()
	}

	n.runWhole(t, holders)
}

// runWhole is the F3 fallback: try each advertising friend in order until one
// delivers the whole blob with bytes that verify against the content hash.
func (n *Node) runWhole(t *transfer, holders []*Peer) {
	t.stats.setMode("whole")
	var lastErr error
	for _, p := range holders {
		if n.transferCtx.Err() != nil {
			t.finish(n.transferCtx.Err())
			return
		}
		err := n.fetchFrom(t, p)
		if err == nil {
			n.observePeerAlive(p)                 // a completed fetch is liveness proof
			t.stats.noteSucceed(wholePiece, p, 0) // bytes were credited as they arrived
			n.logger.Printf("federation: fetched %s from %q (%d bytes)", t.hash, p.Name, t.Progress())
			t.finish(nil)
			return
		}
		lastErr = err
		t.stats.noteFail(wholePiece, p, err, false)
		n.logger.Printf("federation: fetch %s from %q: %v", t.hash, p.Name, err)
		t.resetProgress()
	}
	os.Remove(t.partPath)
	t.finish(fmt.Errorf("federation: fetch %s: %w", t.hash, lastErr))
}

// fetchFrom streams the blob from one friend into the partial file, hashing as
// it goes; matching bytes are renamed into the cache atomically.
func (n *Node) fetchFrom(t *transfer, p *Peer) error {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(n.transferCtx, n.timeouts.Transfer)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", addr, MeshPort, t.hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// The friendship client has a short global timeout for control calls; blob
	// fetches are bounded by ctx instead.
	client := &http.Client{Transport: &http.Transport{DialContext: n.DialContext}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("holder answered %s", resp.Status)
	}
	filename := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = filepath.Base(params["filename"])
	}
	t.setMeta(resp.ContentLength, filename)

	f, err := os.OpenFile(t.partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		nr, rerr := resp.Body.Read(buf)
		if nr > 0 {
			if _, werr := f.Write(buf[:nr]); werr != nil {
				f.Close()
				return werr
			}
			hasher.Write(buf[:nr])
			t.stats.noteBytes(p, int64(nr))
			t.addProgress(int64(nr))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != t.hash {
		return fmt.Errorf("bytes hash to %s, not the requested content hash", got)
	}
	return os.Rename(t.partPath, t.path)
}

// isBlobHash reports whether s is a well-formed content hash (64 lowercase hex
// chars) — anything else can never resolve and must not reach the filesystem.
func isBlobHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
