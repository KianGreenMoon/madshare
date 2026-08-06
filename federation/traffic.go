//go:build !nofederation

package federation

import (
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Swarm traffic accounting (docs/architecture/swarm-admin.md).
//
// Bytes are counted HERE, in memory, and persisted by `api`'s flusher. That
// split is the one the cache index already makes, for the same reason: moving
// bytes must not require a database, so this package gains no edge to one, and
// no write ever lands in a chunk-fetch loop.
//
// It is also deliberately SEPARATE from transferStats (stats.go). That structure
// measures the swarm's *behaviour* — failovers, stalls, per-provider chunks —
// and the mesh tests assert on its exact semantics. This measures the *wire*:
// every byte that crossed it, including the ones later thrown away. Counting
// wire bytes into transferStats would silently change what those assertions mean.

// The exported shapes — TrafficCounters, TrafficDelta, PeerTraffic and
// TrafficSnapshot — live in federation.go, beside TransferStats: they cross the
// package boundary into `api`, which must still compile under -tags
// nofederation, where this file is gone.

// add folds o into c.
func (c *TrafficCounters) add(o TrafficCounters) {
	c.Up += o.Up
	c.Down += o.Down
	c.Wasted += o.Wasted
}

// zero reports whether nothing has been counted — a delta not worth writing.
func (c TrafficCounters) zero() bool { return c.Up == 0 && c.Down == 0 && c.Wasted == 0 }

// trafficTable is the node's live accounting. It keeps two per-hash maps: what
// this process has moved (session, read by the page) and what has not been
// written to the database yet (pending, taken by the flusher). They are separate
// because draining must not reset the view — "this session" means since the
// process started, not since the last flush.
type trafficTable struct {
	mu      sync.Mutex
	since   time.Time
	totals  TrafficCounters
	session map[string]*TrafficCounters
	pending map[string]*TrafficCounters
	peers   map[string]*PeerTraffic
}

func newTrafficTable() *trafficTable {
	return &trafficTable{
		since:   time.Now(),
		session: map[string]*TrafficCounters{},
		pending: map[string]*TrafficCounters{},
		peers:   map[string]*PeerTraffic{},
	}
}

// note credits one hash. Every method here is nil-safe so a Node built without a
// traffic table (older tests, the stub paths) counts nothing rather than panics.
func (t *trafficTable) note(hash string, c TrafficCounters) {
	if t == nil || hash == "" || c.zero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totals.add(c)
	entry := func(m map[string]*TrafficCounters) *TrafficCounters {
		e := m[hash]
		if e == nil {
			e = &TrafficCounters{}
			m[hash] = e
		}
		return e
	}
	entry(t.session).add(c)
	entry(t.pending).add(c)
}

// notePeer credits the counterparty. Keyed by public key when we have one, so an
// identified peer's two directions land on one row; an unplaceable requester is
// keyed by its mesh address, which is self-certifying but anonymous.
func (t *trafficTable) notePeer(key, addr string, up, down int64) {
	if t == nil || (up == 0 && down == 0) {
		return
	}
	id := "k:" + key
	if key == "" {
		if addr == "" {
			return
		}
		id = "a:" + addr
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.peers[id]
	if p == nil {
		p = &PeerTraffic{Key: key, Addr: addr}
		t.peers[id] = p
	}
	if p.Addr == "" {
		p.Addr = addr // learned on a later request from the same key
	}
	p.Up += up
	p.Down += down
	p.LastAt = time.Now()
}

// snapshot copies the session view. The maps are copied rather than shared: a
// reader must not hold a reference into a table the fetch workers keep writing.
func (t *trafficTable) snapshot() TrafficSnapshot {
	out := TrafficSnapshot{Hashes: map[string]TrafficCounters{}}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out.TrafficCounters = t.totals
	out.Since = t.since
	for h, c := range t.session {
		out.Hashes[h] = *c
	}
	out.Peers = make([]PeerTraffic, 0, len(t.peers))
	for _, p := range t.peers {
		out.Peers = append(out.Peers, *p)
	}
	sort.Slice(out.Peers, func(i, j int) bool {
		a, b := out.Peers[i], out.Peers[j]
		if a.Up+a.Down != b.Up+b.Down {
			return a.Up+a.Down > b.Up+b.Down
		}
		return a.Key+a.Addr < b.Key+b.Addr
	})
	return out
}

// drain takes the un-flushed deltas and clears them, leaving the session view
// untouched. A crash after this and before the commit loses at most one
// interval's accounting, which is the trade that keeps database writes off the
// transfer path entirely.
func (t *trafficTable) drain() []TrafficDelta {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return nil
	}
	out := make([]TrafficDelta, 0, len(t.pending))
	for h, c := range t.pending {
		if c.zero() {
			continue
		}
		out = append(out, TrafficDelta{Hash: h, TrafficCounters: *c})
	}
	t.pending = map[string]*TrafficCounters{}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// ── Node-level hooks ─────────────────────────────────────────────────────────

// noteUp credits bytes served to a peer over the mesh.
func (n *Node) noteUp(hash, key, addr string, bytes int64) {
	if bytes <= 0 {
		return
	}
	n.traffic.note(hash, TrafficCounters{Up: bytes})
	n.traffic.notePeer(key, addr, bytes, 0)
}

// noteDown credits bytes pulled off the mesh from a holder. Called with WIRE
// bytes — what arrived, whether or not it survived verification — so the figure
// describes what the mesh cost us rather than what we kept.
func (n *Node) noteDown(hash, key string, bytes int64) {
	if bytes <= 0 {
		return
	}
	n.traffic.note(hash, TrafficCounters{Down: bytes})
	n.traffic.notePeer(key, "", 0, bytes)
}

// noteTransferEnd books the waste of a finished transfer: received minus
// delivered. Computed once, at the end, rather than at every discard point —
// which is both simpler and more honest, since it catches every way bytes can be
// thrown away (a chunk that failed its hash and was re-fetched, an abandoned
// swarm attempt, a whole-file try against a holder that lied) without any of
// them needing to remember to report.
func (n *Node) noteTransferEnd(t *transfer) {
	got := t.Received()
	if got <= 0 {
		return
	}
	kept := int64(0)
	if t.Err() == nil {
		kept = t.Size()
	}
	if wasted := got - kept; wasted > 0 {
		n.traffic.note(t.hash, TrafficCounters{Wasted: wasted})
	}
}

// Traffic snapshots this session's byte accounting (see [TrafficSnapshot]).
func (n *Node) Traffic() TrafficSnapshot { return n.traffic.snapshot() }

// DrainTraffic takes the per-hash deltas that have not been persisted yet and
// clears them. The caller commits them; the session view is unaffected.
func (n *Node) DrainTraffic() []TrafficDelta { return n.traffic.drain() }

// ── Metering ─────────────────────────────────────────────────────────────────

// meteredResponseWriter counts body bytes on their way out, passing
// Header/WriteHeader straight through — the seeding counterpart of
// meteredReader, and the only thing that has ever counted an outbound byte.
type meteredResponseWriter struct {
	http.ResponseWriter
	count func(int64)
}

func (m *meteredResponseWriter) Write(p []byte) (int, error) {
	n, err := m.ResponseWriter.Write(p)
	if n > 0 {
		m.count(int64(n))
	}
	return n, err
}

// metered wraps a response writer so every body byte is counted. Always applied,
// unlike throttled, which returns w untouched when nothing is capped.
func metered(w http.ResponseWriter, count func(int64)) http.ResponseWriter {
	return &meteredResponseWriter{ResponseWriter: w, count: count}
}

// meteredReader counts bytes as they are read. It wraps the response body INSIDE
// the stall watchdog, so what it counts is what crossed the wire even when the
// read is later abandoned.
type meteredReader struct {
	r     io.Reader
	count func(int64)
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.count(int64(n))
	}
	return n, err
}

// metered wraps a reader so every byte read is credited to hash against the
// holder p, and to the transfer's own received total (which is what the waste
// calculation is a difference of).
func (n *Node) metered(t *transfer, p *BlobProvider, r io.Reader) io.Reader {
	key := ""
	if p != nil {
		key = p.PublicKey
	}
	return &meteredReader{r: r, count: func(b int64) {
		t.addReceived(b)
		n.noteDown(t.hash, key, b)
	}}
}
