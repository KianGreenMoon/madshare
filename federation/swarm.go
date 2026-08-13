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
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The swarm (federation F4): multi-source chunk fetch, the chunk manifest, the
// holdings tracker, and the seeding rate cap. Design:
// docs/architecture/federation-swarm.md §Distribution.
//
// A blob's swarm id is its whole-file SHA-256 (the same content address used
// everywhere), so it is NOT a Merkle root and per-chunk hashes cannot be
// derived from it. The manifest carries the per-chunk hashes explicitly; they
// enable early per-chunk verification and bad-chunk re-fetch across sources,
// while the assembled whole-file hash stays the authoritative anchor (verified
// before a blob enters the cache). A lying manifest therefore costs bandwidth
// and a failed transfer, never the wrong file — and since F9 item 3 a manifest
// is only believed when two holders describe the blob the same way, because F7
// widened the swarm from "holders an admin picked" to the whole community.

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

	maxChunkWorkers  = 8        // parallel chunk fetches per transfer
	maxManifestChunk = 64 << 20 // reject a manifest whose chunk size exceeds this (DoS guard)
	seedWriteChunk   = 32 << 10 // rate-cap granularity on the serve path

	// manifestProbes is how many holders are asked for the manifest at once. Two
	// have to agree before it is trusted (fetchAgreedManifest), so probing one at
	// a time would serialise a round trip nothing else waits for; probing every
	// holder would dial a crowd to settle a question two of them can answer.
	manifestProbes = 4
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

// errChunkAbsent marks a holder answering 416 for a chunk: it has the blob but
// not this slice of it yet (F9 item 1 — a partial seeder). Unlike every other
// failure this is a fact about the CHUNK, not about the holder, so it must not
// count towards retiring anyone. It still costs the chunk an attempt, which is
// what keeps a swarm of partials that collectively lack a chunk terminating
// rather than looping.
var errChunkAbsent = errors.New("holder does not have that chunk yet")

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
	// A reader that blocks on OUR OWN rate limiter is not a silent holder, so the
	// watchdog is suspended for the length of that pause and re-armed after
	// (traffic.go, wireReader). It has to bracket the wait rather than follow it:
	// one Read can return a whole chunk buffer whose tokens take seconds to earn,
	// and the timer would otherwise fire mid-sleep. Every call here is on this
	// goroutine — Read is synchronous — so the timer is never touched
	// concurrently. Without this, capping the inbound rate would make every
	// holder look stalled and get it retired.
	if pr, ok := r.(pausingReader); ok {
		pr.onPause(func() func() {
			watchdog.Stop()
			return func() { watchdog.Reset(stall) }
		})
	}
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
func (n *Node) fetchManifest(ctx context.Context, p *BlobProvider, hash string) *blobManifest {
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

// agreement is a manifest's content-identifying fingerprint: everything a
// fetcher acts on, and nothing a holder may honestly differ about.
//
// Filename is excluded, and that exclusion is the point — a blob's name is a
// fact about the holder's copy, not about the bytes. The library seeder knows it
// as "track.mp3" while a node that fetched it has it under its hash, and reading
// that as disagreement would make every mixed swarm look like a lie. Protocol is
// excluded for the same reason: it describes the speaker, not the file.
func (m *blobManifest) agreement() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%d|%v|", m.Size, m.ChunkSize, m.LeadSizes)
	for _, c := range m.Chunks {
		h.Write([]byte(c))
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fetchAgreedManifest returns a chunk manifest two holders independently
// describe the same way, or nil — the signal to fall back to the F3 whole-file
// fetch, which carries its own reference and needs no manifest at all.
//
// **Why agreement and not the first valid answer** (F9 item 3; the defect is
// .issues/open-issues.md, "a lying manifest retires every honest holder"). The
// per-chunk hashes are not bound to the content hash and cannot be — the swarm
// id is a flat whole-file SHA-256, not a hash over a metadata block the way a
// BitTorrent infohash is. So a manifest is a claim, taken on the word of
// whichever holder answered first, and every chunk fetched from every HONEST
// holder then fails against it. The assembled whole-file hash still protects the
// downloader from the wrong file; what it does not protect is the transfer, or
// the honest holders' standing in it.
//
// Two independent claims are cheap here — manifests are small, memoized, and
// probed in parallel with everything else a transfer starts with — and they
// catch a single liar. They do not catch collusion, and are not meant to.
//
// Three outcomes, and the two edges matter:
//
//   - Two holders agree ⇒ that manifest, returned as soon as the second vote
//     lands rather than after the slowest probe.
//   - Exactly one holder in the first wave that answers at all ⇒ believed.
//     Refusing would break the case F9 item 1 exists for: a
//     partial seeder cannot BUILD a manifest (buildManifest reads the whole
//     file), so a swarm of one complete holder and several partials has exactly
//     one voice by construction.
//   - Two holders answer differently with no majority ⇒ nil. One of them is
//     lying and nothing here can say which, so the swarm gives way rather than
//     picking a side.
func (n *Node) fetchAgreedManifest(ctx context.Context, holders []*BlobProvider, hash string) *blobManifest {
	return agreedManifest(ctx, holders, func(p *BlobProvider) *blobManifest {
		return n.fetchManifest(ctx, p, hash)
	})
}

// agreedManifest is fetchAgreedManifest's rule with the mesh taken out of it, so
// the three outcomes above can be exercised without three nodes.
func agreedManifest(ctx context.Context, holders []*BlobProvider, probe func(*BlobProvider) *blobManifest) *blobManifest {
	seen := map[string]*blobManifest{}
	votes := map[string]int{}
	answers := 0

	for start := 0; start < len(holders); start += manifestProbes {
		if ctx.Err() != nil {
			return nil
		}
		end := min(start+manifestProbes, len(holders))
		wave := make(chan *blobManifest, end-start)
		for _, p := range holders[start:end] {
			go func(p *BlobProvider) { wave <- probe(p) }(p)
		}
		for range end - start {
			m := <-wave
			if m == nil {
				continue
			}
			answers++
			key := m.agreement()
			if _, ok := seen[key]; !ok {
				seen[key] = m
			}
			if votes[key]++; votes[key] > 1 {
				return seen[key]
			}
		}
		// A wave that produced a disagreement is already decided: a third opinion
		// would break the tie, but asking for one means dialling further into a
		// plan whose holders are already contradicting each other, and the
		// whole-file path is right there. Only a wave that produced NOTHING is
		// worth widening.
		if answers > 0 {
			break
		}
	}
	if answers == 1 {
		for _, m := range seen {
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
	// Partial carries downloads in flight that already have verified bytes to
	// serve (F9 item 1). Separate from Hashes because the two are different
	// promises — a hash here answers only some ranges — and because an older node
	// ignoring an unknown field degrades exactly right: it simply does not learn
	// about partial seeders.
	Partial []string `json:"partial,omitempty"`
}

// cacheHoldings lists the finished blobs in the download cache (skipping
// in-progress ".part" files, which fail the hash-shape check).
// CacheHoldings is cacheHoldings for a caller outside this package: what this
// node has fetched and would seed.
//
// A listener node pushes exactly this list to its home server, which is the only
// way anything learns it holds anything (§"The household", "Being found"). The
// directory is the authority in both cases — a device advertising from an index
// could advertise a blob it has already swept.
func (n *Node) CacheHoldings() []string { return n.cacheHoldings() }

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

// partialHoldings lists the in-flight downloads that have verified bytes to
// offer, excluding any hash already complete in the cache.
//
// It reads the LIVE TRANSFER TABLE, never the directory — the one place this
// deliberately differs from cacheHoldings. A finished blob is self-describing:
// its name is its hash and its bytes verify. A `.part` is not. Which of its
// bytes are proven lives only in the running transfer's chunk map, so a part
// file left behind by a previous process has nothing advertisable in it at all
// — which is the same reason the cache page reaps those unconditionally at
// startup.
func (n *Node) partialHoldings(complete []string) []string {
	n.transferMu.Lock()
	live := make([]*transfer, 0, len(n.transfers))
	for _, t := range n.transfers {
		live = append(live, t)
	}
	n.transferMu.Unlock()

	done := make(map[string]bool, len(complete))
	for _, h := range complete {
		done[h] = true
	}
	out := make([]string, 0, len(live))
	for _, t := range live {
		if t.partPath == "" || done[t.hash] || len(t.CompleteRanges()) == 0 {
			continue
		}
		out = append(out, t.hash)
	}
	sort.Strings(out) // a stable answer, so a repeated read does not reshuffle
	return out
}

// handleHoldings serves GET /madnetwork/v0/holdings — our community only (F7:
// the swarm's boundary is the madnetwork, so a member seeds from us exactly as a
// direct friend does); empty when seeding or cache-seeding is off, and for a
// guest-limited friend, which is never served cache blobs. Advertising and
// serving read one rule.
//
// A node outside the community is refused rather than answered with an empty
// list: like the catalog this is a listing, and the guest switch does not open
// it — what it opens is the byte endpoints, where 404 keeps a hash's existence
// unconfirmed.
func (n *Node) handleHoldings(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "transfer not configured", http.StatusServiceUnavailable)
		return
	}
	aud, ok := n.serveAudience(r)
	if !ok {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !aud.InCommunity() {
		http.Error(w, "holdings are served inside the madnetwork only", http.StatusForbidden)
		return
	}
	policy, err := n.store.SeedingPolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hashes := []string{}
	var partial []string
	if policy.Enabled && policy.Cache && aud.ServesCache() && !aud.GuestOnly {
		hashes = n.cacheHoldings()
		partial = n.partialHoldings(hashes)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(holdingsMessage{
		Protocol: ProtocolVersion, Hashes: hashes, Partial: partial,
	})
}

// syncHoldings pulls one source's cache-holdings list and replaces the cached
// copy (federation_holdings), so their downloaded blobs become fetchable from
// them. Called on the same cadence as the catalog sync, for the same set of
// nodes — a member seeds from its cache exactly as a friend does.
func (n *Node) syncHoldings(ctx context.Context, p *CatalogSource) {
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
	// Complete and partial holdings are UNIONED into the stored set. The wire
	// keeps them apart — an operator reading the endpoint should see the truth,
	// and a scheduler that ranks a complete holder above a partial one will want
	// it — but nothing downstream can act on the distinction yet, so persisting
	// it would mean a migration for a column no query reads. Storing a partial as
	// an ordinary holder is safe because the fetcher no longer punishes a 416
	// (errChunkAbsent): the worst case is one fast refusal from a live node,
	// which is nothing like the connect timeout a genuinely dead holder costs.
	// Ranking partials is item 3's call, and that is where the column belongs if
	// it is ever wanted.
	valid := make([]string, 0, len(msg.Hashes)+len(msg.Partial))
	seen := make(map[string]bool, cap(valid))
	for _, h := range append(append([]string(nil), msg.Hashes...), msg.Partial...) {
		if isBlobHash(h) && !seen[h] {
			seen[h] = true
			valid = append(valid, h)
		}
	}
	if err := n.store.ReplaceSourceHoldings(ctx, p.ID, valid); err != nil {
		n.logger.Printf("federation: store holdings of %q: %v", p.Display(), err)
	}
}

// ── Partial holdings (F9 item 1) ─────────────────────────────────────────────

// haveMessage is the reply to GET /madnetwork/v0/have/{hash}: which byte extents
// of one blob this node holds complete and will serve. A finished blob answers
// with the single range [0, size); a fetch in progress answers with its verified
// chunks, coalesced. See [ByteRange] for why this is bytes and not chunk indices.
type haveMessage struct {
	Protocol int         `json:"protocol"`
	Hash     string      `json:"hash"`
	Size     int64       `json:"size"`     // 0 when not yet known
	Complete bool        `json:"complete"` // the whole blob is here
	Ranges   []ByteRange `json:"ranges"`
}

// maxHaveRanges bounds the reply. Our partials are near-prefixes — dispatch is
// sequential-priority, so a fetch in progress is usually ONE range and at most a
// few; only seek-priority fragments one at all. (This is the opposite of a
// BitTorrent client, whose rarest-first picker scatters pieces on purpose and so
// needs a bitmap.) The cap exists so that a pathological partial cannot make this
// reply unbounded. Truncating to the LARGEST ranges keeps a short answer useful:
// a fetcher told about less than we hold loses a little parallelism, never
// correctness.
const maxHaveRanges = 64

// liveTransfer returns the in-flight fetch for hash, or nil.
func (n *Node) liveTransfer(hash string) *transfer {
	n.transferMu.Lock()
	defer n.transferMu.Unlock()
	return n.transfers[hash]
}

// seedsPartials reports whether an unfinished download may be served to aud.
//
// A `.part` answers under the CACHE branch's rule EXACTLY — the same condition
// handleHoldings applies to the cache list, which is what advertises it. It is
// deliberately not a third branch beside library and cache (§Distribution,
// "Making it a swarm", item 1): an unlanded blob has no published appearance and
// cannot acquire one until it verifies, so the library rule could never admit it
// anyway, and a third rule would be a third thing to keep in agreement with the
// invariant that catalog and bytes read one rule.
func (n *Node) seedsPartials(ctx context.Context, aud Audience) bool {
	if !aud.Serves() || aud.GuestOnly || !aud.ServesCache() {
		return false
	}
	policy, err := n.store.SeedingPolicy(ctx)
	if err != nil {
		n.logger.Printf("federation: seeding policy: %v", err)
		return false
	}
	return policy.Enabled && policy.Cache
}

// haveRanges answers what this node can serve of hash to aud. The bool is false
// when there is nothing to offer — the caller answers 404, never an empty 200,
// because this request names a hash and a reply must not confirm one exists here
// (the same rule the blob and manifest endpoints follow). "I am fetching this but
// can serve none of it yet" is precisely the fact `seed_cache` lets an operator
// withhold.
func (n *Node) haveRanges(ctx context.Context, hash string, aud Audience) (haveMessage, bool) {
	msg := haveMessage{Protocol: ProtocolVersion, Hash: hash}
	// A blob held whole answers whole, through the ordinary seeding gate — the
	// same resolution handleBlob does, so /have can never advertise a byte the
	// blob endpoint would refuse.
	if path, ok := n.seedableBlob(ctx, hash, aud); ok {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			msg.Size = info.Size()
			msg.Complete = true
			msg.Ranges = []ByteRange{{Start: 0, End: info.Size()}}
			return msg, true
		}
	}
	if !n.seedsPartials(ctx, aud) {
		return msg, false
	}
	t := n.liveTransfer(hash)
	if t == nil {
		return msg, false
	}
	rs := capRanges(t.CompleteRanges())
	if len(rs) == 0 {
		return msg, false
	}
	msg.Size = t.Size()
	msg.Ranges = rs
	return msg, true
}

// capRanges trims a range list to maxHaveRanges, keeping the largest extents and
// restoring offset order (a fetcher reads them as a plan, so order is kindness).
func capRanges(rs []ByteRange) []ByteRange {
	if len(rs) <= maxHaveRanges {
		return rs
	}
	out := append([]ByteRange(nil), rs...)
	sort.Slice(out, func(i, j int) bool { return out[i].End-out[i].Start > out[j].End-out[j].Start })
	out = out[:maxHaveRanges]
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// parseSingleRange parses a single-range `bytes=` header against a known size and
// returns the half-open [start, end). Multi-range requests are refused: the swarm
// never sends one, and a partial holder answering a multipart body would have to
// prove every part present.
//
// This exists because a partial CANNOT go through http.ServeContent, which parses
// the range against the file's extent — and a part file's extent is a lie (see
// servePartialBlob).
func parseSingleRange(hdr string, size int64) (int64, int64, bool) {
	const prefix = "bytes="
	if size <= 0 || !strings.HasPrefix(hdr, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(hdr, prefix))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	var start, end int64
	if first == "" { // suffix form: the final N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		start, end = size-n, size
	} else {
		s, err := strconv.ParseInt(first, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false
		}
		start, end = s, size
		if last != "" {
			e, err := strconv.ParseInt(last, 10, 64)
			if err != nil || e < s {
				return 0, 0, false
			}
			if end = e + 1; end > size { // HTTP ranges are inclusive
				end = size
			}
		}
	}
	if end <= start {
		return 0, 0, false
	}
	return start, end, true
}

// rangeCovered reports whether [start, end) lies entirely within ONE advertised
// extent. Deliberately one rather than a union of several: the extents are
// already coalesced into maximal runs, so a request spanning two of them is a
// request that spans a hole.
func rangeCovered(rs []ByteRange, start, end int64) bool {
	for _, x := range rs {
		if start >= x.Start && end <= x.End {
			return true
		}
	}
	return false
}

// servePartialBlob answers out of an in-flight download's verified chunks, for a
// hash this node does not hold whole (F9 item 1). The quota admission and the
// rate limiters have already been taken by handleBlob, so a partial costs a
// requester exactly what a complete blob does.
//
// **The part file's length is a lie, and that is the trap this function exists
// for.** fetchSwarm pre-truncates it to the full size so chunks can be written at
// their offsets, so it is full-length and mostly ZEROS from the first moment.
// http.ServeContent would happily serve any range of it — megabytes of zeros that
// verify at neither chunk nor file level. Every byte handed out here is therefore
// gated on CompleteRanges (per-chunk SHA-256 verified), never on the file's
// extent.
//
// A partial answers RANGED requests only. A request with no Range is answered as
// if we did not hold the hash at all: a short 200 body would tell the F3
// whole-file path it had received the blob, and it would learn otherwise only at
// the final hash — one wasted transfer to say something a 404 says for free.
func (n *Node) servePartialBlob(w http.ResponseWriter, r *http.Request, hash string, aud Audience, rls []*rateLimiter, key string) {
	if !n.seedsPartials(r.Context(), aud) {
		http.NotFound(w, r)
		return
	}
	t := n.liveTransfer(hash)
	if t == nil || t.partPath == "" {
		http.NotFound(w, r)
		return
	}
	ranges := t.CompleteRanges()
	size := t.Size()
	if len(ranges) == 0 || size <= 0 {
		http.NotFound(w, r)
		return
	}
	start, end, ok := parseSingleRange(r.Header.Get("Range"), size)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !rangeCovered(ranges, start, end) {
		// 416 rather than 404: the hash IS here, this slice of it is not yet. The
		// distinction matters to a fetcher deciding whether to come back.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "that range has not arrived here yet", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	f, err := os.Open(t.partPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if name := t.Filename(); name != "" {
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))

	addr := ""
	if ip := remoteIP(r); ip != nil {
		addr = ip.String()
	}
	out := metered(throttled(w, r.Context(), n.upLimiters(r.Context(), rls)), func(b int64) {
		n.noteUp(hash, key, addr, b)
	})
	out.WriteHeader(http.StatusPartialContent)
	if _, err := io.Copy(out, io.NewSectionReader(f, start, end-start)); err != nil {
		n.logger.Printf("federation: serve partial %s [%d,%d): %v", hash, start, end, err)
	}
}

// fetchHave asks one holder which extents of a blob it will serve. nil when it
// does not answer — which covers three different nodes and deliberately does not
// tell them apart: one too old to know the endpoint (404 from the mux), one that
// holds nothing of this hash (404 by the rule that a reply must never confirm a
// hash exists here), and one that is simply gone. Only the last is a fault, and
// nothing here can prove which it was, so none of them costs the holder anything
// beyond the coverage staying unknown — read as "has everything", the pre-F9
// assumption.
func (n *Node) fetchHave(ctx context.Context, p *BlobProvider, hash string) *haveMessage {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, n.timeouts.Manifest)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/have/%s", addr, MeshPort, hash)
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
	var msg haveMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&msg); err != nil {
		return nil
	}
	if msg.Hash != hash {
		return nil
	}
	return &msg
}

// probeCoverage asks every holder in a plan what it can serve, in the
// background, and feeds the answers to the scheduler as they land (F9 item 3).
//
// It does not block dispatch, and must not: the answer is an optimisation, while
// the transfer's first chunk is the thing a listener is waiting for. Chunks
// dispatched before the answers arrive are exactly what happened before this
// existed — a partial holder answers 416 and the scheduler learns the same fact
// one round trip later.
//
// This is also the answer to a note left in syncHoldings: the store unions
// complete and partial holdings and no column distinguishes them, because a
// scheduler that wants to know asks the holder directly and gets a live answer
// rather than a fifteen-minute-old one.
func (n *Node) probeCoverage(hash string, plan *chunkPlan) {
	for i, p := range plan.providers {
		go func(i int, p *BlobProvider) {
			if msg := n.fetchHave(n.transferCtx, p, hash); msg != nil {
				plan.setCoverage(i, msg)
			} else {
				plan.probeUnanswered(i)
			}
		}(i, p)
	}
}

// handleHave serves GET /madnetwork/v0/have/{hash}.
func (n *Node) handleHave(w http.ResponseWriter, r *http.Request) {
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
	msg, ok := n.haveRanges(r.Context(), hash, aud)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

// ── Seeding gate ─────────────────────────────────────────────────────────────

// seedableBlob resolves a hash to a local path this node will serve to the given
// audience, honouring the seeding policy: nothing when seeding is off or the
// audience is served nothing at all; a library blob when it is published *and
// inside the audience's scope* (F5); a cache blob only when cache-seeding is on
// *and the requester is in our community* — a blob this node merely fetched is
// somebody else's content that we hold, so re-seeding it is defensible inside
// the network it came from and nowhere else. Returns ("", false) when the hash
// is not seedable.
func (n *Node) seedableBlob(ctx context.Context, hash string, aud Audience) (string, bool) {
	if !aud.Serves() {
		return "", false
	}
	policy, err := n.store.SeedingPolicy(ctx)
	if err != nil {
		n.logger.Printf("federation: seeding policy: %v", err)
		return "", false
	}
	if !policy.Enabled {
		return "", false
	}
	if n.resolveBlob != nil {
		if vis, found, verr := n.store.BlobVisibleTo(ctx, hash, aud); verr == nil && found && vis {
			if path, ok := n.resolveBlob(hash); ok {
				return path, true
			}
		}
	}
	if policy.Cache && aud.ServesCache() && n.cacheDir != "" {
		path := filepath.Join(n.cacheDir, hash)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// serveAudience is the byte endpoints' gate: who this request is answered for,
// and whether it may be answered at all (F7, §Principals & access). Three
// classes, resolved in order of how much we know:
//
//	a direct friend      its mapped audience — the only class that also reaches
//	                     what an admin restricted to hand-picked nodes
//	a member             our community, from the mutual-edge walk: the
//	                     Madnetwork scope, cache blobs included
//	a listener node      a member's signed vouch for one bearer key (F7 item 9):
//	                     the member audience, narrowed by the token's guest bit
//	anyone else          nothing, unless this node opted to answer guests, and
//	                     then guest-playable content only
//
// Note what the token bearer is not: a friend. It draws on the member budget
// like every other non-friend (quota.go), which is the answer to a home server
// enrolling a thousand devices — they share one class ceiling.
//
// The bool reports whether the request could be *decided*, not whether it is
// allowed: false means a storage error the caller should answer with a 500. A
// refusal comes back as an audience that serves nothing, so every endpoint keeps
// its own 404-vs-403 choice — a stranger is never told a hash exists.
//
// Blocked peers never arrive here; meshAuth refuses them the whole surface
// first, and they are excluded from the community walk besides.
func (n *Node) serveAudience(r *http.Request) (Audience, bool) {
	aud, _, ok := n.serveAudienceKey(r)
	return aud, ok
}

// serveAudienceKey is serveAudience plus the requester's public key when the
// resolution happened to establish one — a friend's from the peer row, a
// member's from the community index. It exists so the seeding path can account
// bytes against an identity without paying for a second lookup; a requester it
// could not place (a guest, a token bearer) comes back with an empty key and is
// accounted by its mesh address, which is self-certifying but anonymous.
func (n *Node) serveAudienceKey(r *http.Request) (Audience, string, bool) {
	if p := n.peerFromRemote(r); p != nil && p.State == PeerFriend {
		aud, err := n.store.PeerAudience(r.Context(), p.ID)
		if err != nil {
			n.logger.Printf("federation: resolve audience of %q: %v", p.Display(), err)
			return Audience{}, "", false
		}
		return aud, p.PublicKey, true
	}
	// Not a friend — but possibly a member. A pending peer takes this path too:
	// it has not been accepted, so it gets whatever its key earns in the
	// community and not a step more.
	if key, ok, err := n.memberFromRemote(r); err != nil {
		n.logger.Printf("federation: resolve community membership: %v", err)
		return Audience{}, "", false
	} else if ok {
		return MemberAudience, key, true
	}
	// Still unplaced — but it may be carrying a vouch (F7 item 9, token.go). This
	// is a listener node: a madplayer publishes no friend list and appears in
	// nobody else's, so no walk can reach it, and a token from a node we *can*
	// place is the only way it is ever more than a stranger. Checked here rather
	// than first because a friend or a member already has everything a token
	// buys, so presenting one must never cost them their own standing.
	if aud, ok, err := n.tokenAudience(r); err != nil {
		n.logger.Printf("federation: resolve capability token: %v", err)
		return Audience{}, "", false
	} else if ok {
		return aud, "", true
	}
	policy, err := n.store.SeedingPolicy(r.Context())
	if err != nil {
		n.logger.Printf("federation: seeding policy: %v", err)
		return Audience{}, "", false
	}
	if policy.Guests {
		return GuestAudience, "", true
	}
	return Audience{}, "", true // an outsider: decided, and served nothing
}

// ── Multi-source chunk fetch ─────────────────────────────────────────────────

// fetchRange fetches [start, start+length) from one holder over the mesh (a
// plain HTTP Range request against the F3 blob endpoint) and returns the bytes.
// The holder clamps at EOF, so a short file yields fewer than length bytes.
func (n *Node) fetchRange(ctx context.Context, p *BlobProvider, t *transfer, start, length int64, onStall func()) ([]byte, error) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, n.timeouts.PerChunk)
	defer cancel()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", addr, MeshPort, t.hash)
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
	return readStall(cancel, n.wire(cctx, t, p, io.LimitReader(resp.Body, length)),
		length, false, n.timeouts.ChunkStall, onStall)
}

// chunk0Prefetch is a speculative chunk-0 fetch overlapped with the manifest
// round trip. take() consumes it once the manifest is known, keeping the bytes
// only if the guessed layout matched and chunk 0 verifies.
type chunk0Prefetch struct {
	active   bool
	from     *BlobProvider // the holder it was fetched from (for stats crediting)
	guessLen int64         // the chunk-0 length we speculatively fetched
	ch       chan []byte   // buffered(1): the fetched bytes, or nil on failure
}

// speculateChunk0 starts fetching chunk 0 from the first holder using the chunk
// layout implied by the advertised file size (deterministic via chunkSizeFor +
// the lead ramp), so it lands in parallel with the manifest fetch. With the ramp
// chunk 0 is a small lead chunk, so this speculative fetch is small.
func (n *Node) speculateChunk0(t *transfer, holders []*BlobProvider) *chunk0Prefetch {
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
		data, err := n.fetchRange(n.transferCtx, p, t, 0, pf.guessLen, t.stats.noteStall)
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
func (n *Node) fetchSwarm(t *transfer, man *blobManifest, holders []*BlobProvider, prefetched0 []byte, from0 *BlobProvider) error {
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
	plan := newChunkPlan(n.transferCtx, layout, holders, done0, t.stats, n.timeouts)
	n.probeCoverage(t.hash, plan)
	t.stats.setChunks(layout.count())
	// Expose per-chunk readiness (the layout) + the seek-priority hook to the
	// streaming relay, pre-marking chunk 0 so a waiting reader is released without
	// a round trip.
	t.beginChunks(layout, plan.prioritize)
	if done0 {
		t.stats.noteSucceed(0, from0, int64(len(prefetched0)))
		t.chunkDone(0, plan.watermarkBytes())
	}
	// Two workers per holder, bounded in total — but what each HOLDER may be
	// asked for at once is bounded separately (maxHolderRequests), and that is
	// the load-bearing one: this formula counts advertised holders, not answering
	// ones, so four holders of which one is alive puts every worker on the
	// survivor.
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
				d, ok := plan.take()
				if !ok {
					return
				}
				// The clock brackets the fetch alone, which is what makes it a
				// throughput sample rather than a queueing one: the wait for a
				// worker, and the wait for this holder to come off a backoff, both
				// happened inside take().
				started := time.Now()
				err := n.fetchChunk(d.ctx, d.cancel, t, f, layout, man, d.idx, d.p)
				d.cancel()
				if err != nil {
					plan.fail(d.idx, d.pidx, err, errors.Is(err, errChunkCorrupt))
				} else {
					n.observePeerAlive(d.p) // a verified chunk is liveness proof
					plan.succeed(d.idx, d.pidx, t, time.Since(started))
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
//
// The context and its cancel come from the SCHEDULER rather than being made
// here: the plan is what knows this attempt has stopped being useful, because
// another holder delivered the same chunk first (F9 item 4). cancel is passed
// along as the idle-read watchdog's trigger exactly as before.
//
// Two copies of one chunk can therefore write at the same offset concurrently.
// That is safe by construction rather than by luck: each copy is verified
// against the same manifest hash before WriteAt, so the bytes are identical, and
// pwrite of identical bytes over the same range cannot produce anything else.
func (n *Node) fetchChunk(ctx context.Context, cancel context.CancelFunc, t *transfer,
	f *os.File, layout *chunkLayout, man *blobManifest, idx int, p *BlobProvider) error {

	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return err
	}
	start, end := layout.rangeOf(idx)
	length := end - start
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
	// A partial seeder answers 416 for a slice it has not fetched yet. That is
	// information about the chunk, not a fault of the holder's, so it carries its
	// own error and never counts against anyone (see errChunkAbsent).
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return fmt.Errorf("chunk %d from %q: %w", idx, p.Display(), errChunkAbsent)
	}
	// A holder over its member quota (F7 item 6) refuses deliberately, and the
	// design says the swarm reads that as "ask another holder". Carrying its own
	// error is what keeps it out of the failure streak — and, since dispatch is
	// now throughput-aware, out of the throughput estimate: a quota refusal read
	// as slowness would starve a busy-but-fast peer by the very mechanism meant
	// to find fast ones.
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("chunk %d from %q: %w", idx, p.Display(), errChunkBusy)
	}
	// 206 for a range; 200 is tolerated only for a single-chunk blob (the whole
	// file IS the one chunk).
	if resp.StatusCode != http.StatusPartialContent &&
		!(resp.StatusCode == http.StatusOK && len(man.Chunks) == 1) {
		return fmt.Errorf("chunk %d: holder answered %s", idx, resp.Status)
	}
	body, err := readStall(cancel, n.wire(ctx, t, p, io.LimitReader(resp.Body, length)),
		length, true, n.timeouts.ChunkStall, t.stats.noteStall)
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

// setRate changes the cap in place, KEEPING the tokens accumulated so far
// (settled at the old rate first). Rebuilding the bucket instead would hand a
// full burst to whoever nudged the knob — a rate limit an operator could reset
// by fidgeting, and one a requester could provoke.
func (rl *rateLimiter) setRate(bytesPerSec int64) {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	rl.tokens += rl.rate * now.Sub(rl.last).Seconds()
	rl.last = now
	r := float64(bytesPerSec)
	rl.rate, rl.burst = r, r
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
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
// while passing Header/WriteHeader straight through. Several buckets can apply
// at once (F7 item 6): the global seed cap, the class cap over all non-friends,
// and the requester's own — each is waited on in turn, so the slowest binds.
type throttledResponseWriter struct {
	http.ResponseWriter
	rls []*rateLimiter
	ctx context.Context
}

func (t *throttledResponseWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n := len(p) - written
		if n > seedWriteChunk {
			n = seedWriteChunk
		}
		for _, rl := range t.rls {
			if err := rl.wait(t.ctx, n); err != nil {
				return written, err
			}
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
