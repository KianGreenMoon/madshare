//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Direct transfer (federation F3): fetch-by-hash between friends over the
// mesh. The wire is a plain streaming HTTP GET with Range support (decision
// 2026-07-18 — chunking IS HTTP ranges between two trusted endpoints);
// integrity is the content hash itself, verified over the full byte stream,
// with the Merkle chunk protocol deferred to F4 where multi-source fetch
// actually needs per-chunk verification. Design: docs/architecture/federation-swarm.md.

// ── Serving side: GET /madnetwork/v0/blob/{hash} ─────────────────────────────

// handleBlob serves a blob this node holds and will seed to the requester. What
// it serves is decided by the requester's *audience* (F5, F7): a friend may
// fetch any blob its own catalog advertises — matching what the catalog and
// holdings showed it, filtered by scope and its guest-only flag — a member of our
// community reaches everything scoped Madnetwork, and a node outside the
// community reaches nothing unless this node opted to answer guests, and then
// guest-playable content only. Beyond that the seeding gate applies: a published
// library blob, or a cache blob when cache-seeding is on and the requester is in
// our community (seedableBlob, swarm.go). A staged, out-of-scope, or unknown
// hash is 404 even for a friend.
// http.ServeContent provides HEAD/Range (the swarm's chunk fetches are Range
// requests); Content-Disposition carries the origin filename so the fetching
// node can land it under its real name; the write path honours the seed rate cap.
func (n *Node) handleBlob(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	aud, key, ok := n.serveAudienceKey(r)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hash := r.PathValue("hash")
	if !isBlobHash(hash) {
		http.NotFound(w, r)
		return
	}
	if !aud.Serves() {
		http.NotFound(w, r) // nothing to weigh: this requester gets no bytes at all
		return
	}
	// The member budget (F7 item 6, quota.go), before the blob is looked up: it
	// is a fact about us, so refusing here confirms nothing about whether we hold
	// the hash — and it costs no storage read. A direct friend bypasses it.
	rls, release, ok := n.admitServe(r, aud)
	if !ok {
		// 429 rather than 503: the swarm reads a refusal as "ask another holder"
		// and its retirement rule is relative, so a busy node is de-ranked, not
		// condemned. Being unable to serve now is information, not a fault.
		http.Error(w, "over the member quota — try another holder", http.StatusTooManyRequests)
		return
	}
	defer release()
	path, ok := n.seedableBlob(r.Context(), hash, aud)
	if !ok {
		// Not held whole — but a download in progress may still be able to answer
		// a range out of its verified chunks (F9 item 1, swarm.go).
		n.servePartialBlob(w, r, hash, aud, rls, key)
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
	http.ServeContent(n.seedWriter(w, r, hash, key, rls), r, info.Name(), info.ModTime(), f)
}

// seedWriter wraps a blob response in the two layers every seeded byte goes
// through: the throttle (the node's outbound cap plus the requester's quota
// buckets) and, around it, the meter (docs/architecture/swarm-admin.md). The
// meter wraps the throttle rather than the other way round, so it counts bytes
// that reached the client rather than bytes we merely intended to send; and
// unlike the throttle it is ALWAYS present, since the shipped default is
// unlimited and an unmeasured default would leave every node's contribution
// unknown.
//
// The node's outbound cap is resolved here, once per response. A swarm fetch is
// one response per chunk, so a change an admin just made lands within a chunk;
// only the F3 whole-file fallback holds a single response long enough to miss
// one.
func (n *Node) seedWriter(w http.ResponseWriter, r *http.Request, hash, key string, rls []*rateLimiter) http.ResponseWriter {
	addr := ""
	if ip := remoteIP(r); ip != nil {
		addr = ip.String()
	}
	return metered(throttled(w, r.Context(), n.upLimiters(r.Context(), rls)), func(b int64) {
		n.noteUp(hash, key, addr, b)
	})
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
	// received is every byte that came off the wire for this transfer, including
	// what verification later rejected and what a fallback abandoned. Waste is
	// this minus what was delivered, computed once when the transfer ends
	// (traffic.go) — which is why no discard site has to remember to report.
	received int64

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

// addReceived credits wire bytes. Deliberately not folded into addProgress:
// progress is what a reader may READ (verified, in place), while this is what
// arrived — and a chunk that fails its hash advances one and not the other.
func (t *transfer) addReceived(n int64) {
	t.mu.Lock()
	t.received += n
	t.mu.Unlock()
}

// Received is every byte this transfer pulled off the wire.
func (t *transfer) Received() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.received
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

// ByteRange is a half-open [Start, End) extent of a blob that a node holds
// complete — the unit F9 item 1 advertises and serves.
//
// Deliberately bytes rather than chunk indices. The chunk layout is a policy
// output derived from the file size, and it is NOT bound to the content hash
// (the swarm id is the flat whole-file SHA-256, not a hash of a metadata block
// the way a BitTorrent infohash is), so §Distribution reserves the right to
// change the sizing policy "without a protocol break". That freedom holds only
// while chunk indices never leave the fetcher — today they do not, because
// fetchChunk speaks `Range: bytes=…`. Advertising indices would quietly convert
// the freedom into a compatibility break; a byte offset means the same thing on
// every node forever.
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// succeeded reports whether the fetch ended with the blob in hand.
func (t *transfer) succeeded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished && t.err == nil
}

// CompleteRanges reports the byte extents of this transfer that are VERIFIED and
// therefore safe to re-seed, coalesced into maximal runs.
//
// "Verified" is the whole point, and it is why the sequential path contributes
// nothing. In chunk mode each entry of chunkOK was set only after that chunk's
// SHA-256 matched the manifest (fetchChunk verifies before WriteAt), so those
// bytes are as trustworthy as a finished blob's. The F3 whole-file fallback has
// no such guarantee: it streams straight into the part file and checks the hash
// only at the end, so its `progress` watermark counts bytes RECEIVED, not bytes
// proven, and re-seeding them would spread whatever a bad holder sent us. A
// transfer running in that mode advertises nothing.
func (t *transfer) CompleteRanges() []ByteRange {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		if t.err != nil || t.size <= 0 {
			return nil
		}
		return []ByteRange{{Start: 0, End: t.size}}
	}
	if t.layout == nil || len(t.chunkOK) == 0 {
		return nil // sequential mode: received but unproven, see above
	}
	var out []ByteRange
	for i := 0; i < len(t.chunkOK); i++ {
		if !t.chunkOK[i] {
			continue
		}
		j := i
		for j+1 < len(t.chunkOK) && t.chunkOK[j+1] {
			j++
		}
		out = append(out, ByteRange{Start: t.layout.offsetOf(i), End: t.layout.offsetOf(j + 1)})
		i = j
	}
	return out
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

// publishLocked wakes every reader blocked in WaitFor by rotating the changed
// channel. Caller holds t.mu.
func (t *transfer) publishLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *transfer) addProgress(n int64) {
	t.mu.Lock()
	t.progress += n
	readable := t.progress > 0
	t.publishLocked()
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
	t.publishLocked()
	t.mu.Unlock()
	t.stats.resetAttempt()
}

// discardPartial drops a failed attempt's bytes so the next one starts from a
// clean file, and does it by truncating IN PLACE — never by unlinking.
//
// That distinction is the whole point. api.copyTransfer opens the part file
// exactly once and holds that descriptor for the entire response, so replacing
// the path with a fresh inode does not give the reader a fresh file: it strands
// the reader on the old, now-unlinked one. fetchSwarm pre-sizes that file to the
// blob's full length (`f.Truncate(man.Size)`, swarm.go), so past the last chunk
// that landed it reads as ZEROS rather than EOF. The client then receives a
// response that satisfies Content-Length exactly, reports no error, and logs
// nothing, while the decoder falls silent wherever the swarm stopped
// (.issues/open-issues.md, "Madnetwork playback stops mid-track").
//
// Progress is reset FIRST so no reader is cleared to read the region being
// dropped while it is being dropped; after the reset, WaitFor holds every
// reader until the next attempt has actually written past its offset.
//
// The success path needs none of this — os.Rename preserves the inode — and
// neither do runWhole's per-holder retries, which O_TRUNC the same file. Only
// this transition ever replaced it.
func (t *transfer) discardPartial() error {
	t.resetProgress()
	if t.partPath == "" {
		return nil
	}
	if err := os.Truncate(t.partPath, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// beginChunks switches the transfer into chunk mode: readability becomes
// per-chunk (for the streaming relay's random-access reads) and WaitFor gains a
// seek-priority hook into the fetch plan. Called by fetchSwarm before dispatch.
func (t *transfer) beginChunks(layout *chunkLayout, prioritize func(int)) {
	t.mu.Lock()
	t.layout = layout
	t.chunkOK = make([]bool, layout.count())
	t.prioritize = prioritize
	t.publishLocked()
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
	t.publishLocked()
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
	t.publishLocked()
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

// ActiveTransfers snapshots every fetch running right now
// (docs/architecture/madnetwork-cache.md). Two things need it: the cache page's
// "downloading" line, and — load-bearing — deciding which `.part` files are
// abandoned. A partial belonging to a live transfer must never be reaped, and
// this is the only thing that knows which those are.
//
// Ordered by hash so a repeated read of an unchanged set does not reshuffle.
func (n *Node) ActiveTransfers() []TransferStats {
	live := n.liveTransfers()
	out := make([]TransferStats, 0, len(live))
	for _, t := range live {
		out = append(out, t.Stats())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// EvictCachedBlob drops this node's download-cache copy of hash. Safe for a hash
// never cached, and safe while something is reading the file — POSIX keeps an
// open descriptor alive across the unlink.
//
// Called once a fetched blob has landed in the library (F8 follow-up,
// .issues/open-issues.md "Cache seeding overrides a recording's sharing scope").
// From then on the two copies are duplicates served under DIFFERENT rules, and
// only the library branch of seedableBlob applies the recording's sharing scope.
// EnsureBlob resolves the library BEFORE the cache, so nothing is lost by
// deleting the duplicate: a later fetch short-circuits locally either way.
//
// The `.part` of an in-flight transfer is deliberately not touched — it is not
// this file, and a verified transfer renames over the final name anyway.
func (n *Node) EvictCachedBlob(hash string) error {
	if n.cacheDir == "" || !isBlobHash(hash) {
		return nil
	}
	// The memoized manifest goes with the bytes. It is content-addressed, so a
	// stale entry could never serve wrong data — dropping it only keeps the memo
	// from accumulating entries for blobs this node no longer holds (it is
	// rebuilt on demand if the blob ever comes back).
	n.manifestMu.Lock()
	delete(n.manifests, hash)
	n.manifestMu.Unlock()
	if err := os.Remove(filepath.Join(n.cacheDir, hash)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("federation: evict cached blob: %w", err)
	}
	return nil
}

// EnsureBlob returns the Transfer for hash, starting a fetch when needed:
// a local library blob or an already-cached file is returned complete; an
// in-flight fetch is joined; otherwise a new fetch starts against the friends
// whose catalogs advertise the hash (most recently seen first). The fetch runs
// on the node's own lifetime context — it outlives the requester on purpose
// (cache-through: the file keeps landing in the cache after a browser
// disconnects).
func (n *Node) EnsureBlob(ctx context.Context, hash string) (Transfer, error) {
	return n.ensureBlob(ctx, hash, func(ctx context.Context) (int64, []*BlobProvider, error) {
		return n.store.MadnetworkBlobProviders(ctx, hash)
	})
}

// EnsureBlobFrom is EnsureBlob against a holder list the caller already has,
// for a node whose own catalogs cannot answer the question.
//
// That node is a listener one (§"The household"). Its cached-catalog tables are
// filled by syncSources, which pulls from friends and members; a node with
// neither has empty tables forever, so EnsureBlob's discovery step returns
// ErrNoHolder on every hash in the network. What it does have is a home server
// it browses over HTTP, whose rows name the holders — so the list arrives from
// outside instead of being looked up.
//
// A server keeps discovering its own holders and must: this is an addition for
// the one participant that cannot, not a replacement for the one that can.
//
// size may be 0 when the caller does not know it; the manifest supplies the real
// one. Everything after discovery is shared with EnsureBlob — the same dedupe
// map, cache directory, swarm path and whole-file verification — so a fetch
// started either way joins the other.
func (n *Node) EnsureBlobFrom(ctx context.Context, hash string, size int64, holders []*BlobProvider) (Transfer, error) {
	return n.ensureBlob(ctx, hash, func(context.Context) (int64, []*BlobProvider, error) {
		return size, holders, nil
	})
}

// ensureBlob is both entry points: everything except where the holders come
// from, which is the only thing they disagree about.
func (n *Node) ensureBlob(ctx context.Context, hash string,
	discover func(context.Context) (int64, []*BlobProvider, error)) (Transfer, error) {

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
	// The store is required even when the caller supplied the holders: the
	// transfer path itself reads and writes it (peer liveness, traffic), so a
	// store-less node is not merely undiscoverable, it cannot run a fetch.
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
	size, holders, err := discover(ctx)
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
// (multi-source parallel chunk fetch via the manifest) and falls back to the
// single-source whole-file streaming fetch when the swarm cannot be trusted or
// cannot finish.
//
// The fallback is NOT version compatibility — no released node ever spoke
// /blob without /manifest (F3 and F4 first shipped in the same release,
// v0.7.0) — it is the one fetch mode that carries its own reference. The
// manifest is only a claim (the swarm id is a flat SHA-256, so per-chunk
// hashes cannot be bound to it, §Distribution), and when that claim is
// unobtainable (only partial holders remain — they cannot build one — or the
// probes time out), contradicted (a 1-vs-1 disagreement, errManifestSuspect)
// or proven wrong (the assembled bytes fail the content hash), fetching whole
// against the content hash is the only move that needs no manifest trust at
// all. Either way the assembled bytes are verified against the content hash
// before entering the cache.
func (n *Node) runTransfer(t *transfer, holders []*BlobProvider) {
	defer func() {
		n.transferMu.Lock()
		delete(n.transfers, t.hash)
		n.transferMu.Unlock()
		// Book the waste now that the outcome is known: every path below ends in
		// t.finish, so received-minus-delivered is final here.
		n.noteTransferEnd(t)
		// A blob that just landed is seedable from this moment, and the swarm
		// should not wait out a fifteen-minute holdings pull to hear about it
		// (F9 item 2, announce.go).
		if t.succeeded() {
			n.noteAcquired(t.hash)
		}
	}()

	// Overlap the manifest fetch with a speculative chunk-0 prefetch so the first
	// playable byte does not wait for two serial mesh round-trips: the chunk
	// layout is deterministic from the file size (chunkSizeFor), so chunk 0 is
	// fetched from the first holder while the manifest loads. fetchSwarm adopts
	// it into the chunk plan (never waiting on it — a dribbling holders[0] must
	// not gate the swarm start) and keeps the bytes only if they verify.
	pf := n.speculateChunk0(t, holders)

	if man := n.fetchAgreedManifest(n.transferCtx, holders, t.hash); man != nil {
		t.setMeta(man.Size, man.Filename)
		t.stats.setMode("swarm")
		err := n.fetchSwarm(t, man, holders, pf)
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
		n.logger.Printf("federation: swarm fetch %s failed (%v); falling back to whole-file", t.hash, err)
		if derr := t.discardPartial(); derr != nil {
			// Not fatal — fetchFrom opens with O_TRUNC anyway, so the next attempt
			// starts clean regardless. Logged because a part file we cannot shrink
			// is a disk leak until the startup reaper gets it.
			n.logger.Printf("federation: discard partial %s: %v", t.hash, derr)
		}
	} else {
		pf.discard()
	}

	n.runWhole(t, holders)
}

// runWhole is the F3 fallback: try each advertising holder in order until one
// delivers the whole blob with bytes that verify against the content hash.
func (n *Node) runWhole(t *transfer, holders []*BlobProvider) {
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
			n.logger.Printf("federation: fetched %s from %q (%d bytes)", t.hash, p.Display(), t.Progress())
			t.finish(nil)
			return
		}
		lastErr = err
		t.stats.noteFail(wholePiece, p, err, false)
		n.logger.Printf("federation: fetch %s from %q: %v", t.hash, p.Display(), err)
		t.resetProgress()
	}
	os.Remove(t.partPath)
	t.finish(fmt.Errorf("federation: fetch %s: %w", t.hash, lastErr))
}

// fetchFrom streams the blob from one holder into the partial file, hashing as
// it goes; matching bytes are renamed into the cache atomically.
func (n *Node) fetchFrom(t *transfer, p *BlobProvider) error {
	url, err := holderURL(p.PublicKey, "blob/"+t.hash)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(n.transferCtx, n.timeouts.Transfer)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// The pooled blob client, not a fresh one: it is bounded by ctx rather than by
	// the control client's short global timeout, it reuses whatever connection the
	// swarm phase warmed, and — load-bearing — its transport is the one wrapped by
	// WithCapabilityToken. A private client here would present no token, so a
	// listener node whose only standing is its home server's vouch would be served
	// 404 the moment a fetch fell back to this path (§"The household": the reason
	// the token is presented by a RoundTripper and not by each request builder is
	// exactly that the next builder forgets).
	resp, err := n.blobClient.Do(req)
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
	// Counting and the inbound cap both live in the wire reader. This path has no
	// stall watchdog to appease — it is bounded by ctx (Timeouts.Transfer), which
	// a cap eats into, so a very low one can time a large whole-file fetch out.
	// That is inherent to slowing a transfer down, and the swarm path (the norm)
	// is bounded per chunk instead.
	src := n.wire(ctx, t, p, resp.Body)
	for {
		nr, rerr := src.Read(buf)
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
