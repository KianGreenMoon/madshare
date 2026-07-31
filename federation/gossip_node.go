//go:build !nofederation

package federation

// Friend-list gossip, node side (federation F6): publishing this node's own
// record, serving the graph to friends, and pulling theirs. The record format
// and its verification are in gossip.go; the store is
// database/madnetwork_graph.go. Design: docs/architecture/federation.md
// §"Friend-list gossip & the network graph".
//
// The shape of a sync round, and why it is not a crawl:
//
//	we ask only our own friends — never a node we have not friended
//	they answer with a DIGEST of everything they hold: {origin, seq} pairs
//	we ask for the records whose sequence we lack, and store what verifies
//
// A friend's store holds records it collected from ITS friends, so what we
// learn each round reaches one ring further out, until every store holds the
// whole connected component. Nobody dials a stranger and nobody floods: an
// unchanged graph answers with a serial and no payload at all.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// maxGraphFetch caps the origins one fetch request may name, so a peer cannot
// ask us to assemble an unbounded response. A larger backlog simply takes more
// rounds — which is fine, since convergence was never single-round.
const maxGraphFetch = 256

// graphDigestMessage is the reply to GET /madnetwork/v0/graph: either
// Unchanged (the caller's serial still matches) or the full digest. Same
// not-modified shape as the catalog, and the reason gossip is nearly free at
// rest — a quiet graph costs one small round-trip per friend per round.
type graphDigestMessage struct {
	Protocol  int                `json:"protocol"`
	Serial    string             `json:"serial"`
	Unchanged bool               `json:"unchanged,omitempty"`
	Records   []GraphDigestEntry `json:"records,omitempty"`
	Marks     []GraphDigestEntry `json:"marks,omitempty"`
}

// graphFetchRequest names the records a caller wants the bytes of.
type graphFetchRequest struct {
	Records []string `json:"records,omitempty"`
	Marks   []string `json:"marks,omitempty"`
}

// graphFetchReply carries the requested records verbatim. They are
// [json.RawMessage] rather than parsed structs on purpose: re-encoding a
// record would break its author's signature (gossip.go).
type graphFetchReply struct {
	Protocol int               `json:"protocol"`
	Records  []json.RawMessage `json:"records,omitempty"`
	Marks    []json.RawMessage `json:"marks,omitempty"`
}

// graphDigest is one memoized digest and the serial taken over it, held for
// Intervals.GraphDigestTTL (see ownDigest).
type graphDigest struct {
	records []GraphDigestEntry
	marks   []GraphDigestEntry
	serial  string
	built   time.Time
}

// ResyncGraph asks the refresh loop to pull the graph from every friend on its
// next round, skipping the catalog cadence that normally gates it — the Rescan
// button on /admin/network.
//
// Graph only: the catalog's cost scales with a library rather than a friend
// list, and it already answers "unchanged" in the common case, so forcing it
// would buy freshness nobody asked for at the only price worth avoiding.
//
// Pressing twice does not run twice. The flag coalesces, and Nudge's buffered
// channel coalesces the wake-up, so a held-down button costs one round — the
// same shape as EnsureBlob deduping concurrent fetches of one hash.
//
// What it cannot do is worth saying where the method is: gossip reaches one ring
// per round, so this makes our view as fresh as our friends' STORES, not as
// fresh as the network (docs/architecture/federation.md §Refreshing the graph on
// demand).
func (n *Node) ResyncGraph() {
	// Clear the per-origin accept throttle first, or the button silently does
	// nothing. rateAdmits refuses a second record from one origin inside
	// Intervals.GraphAccept, and the records an admin is pressing this for are
	// exactly the ones we just declined — press within a minute of the last
	// round and every fetch comes back refused while the UI reports success.
	//
	// Dropping the throttle here is not a hole in it. It bounds what a peer can
	// PUSH at us unsolicited; this is a local, deliberate, permission-gated act,
	// and it still admits at most one record per origin — one round's worth,
	// bounded by the digest and by MaxOriginsPerBranch like any other round.
	n.acceptMu.Lock()
	clear(n.graphAccept)
	n.acceptMu.Unlock()

	n.forceGraph.Store(true)
	n.Nudge()
}

// graphSerial is the deterministic serial of a digest — the SHA-256 of its
// canonical JSON, exactly as CatalogSerial does for a catalog snapshot.
func graphSerial(records, marks []GraphDigestEntry) string {
	raw, _ := json.Marshal(struct {
		Records []GraphDigestEntry `json:"records"`
		Marks   []GraphDigestEntry `json:"marks"`
	}{records, marks})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ── Publishing our own record ────────────────────────────────────────────────

// publishOwnRecord signs and stores this node's own friend-list record when
// something warrants it: the friendships changed, or the heartbeat came due.
// It is a no-op while the publish setting is off.
//
// Our own record lives in the same store as everyone else's, so serving it to a
// friend needs no special case — the digest simply includes it.
func (n *Node) publishOwnRecord(ctx context.Context, peers []*Peer) {
	if n.store == nil || n.signKey == nil {
		return
	}
	publish, err := n.store.PublishFriendList(ctx)
	if err != nil {
		n.logger.Printf("federation: read gossip policy: %v", err)
		return
	}
	if !publish {
		// Nothing is withdrawn here: a record already in flight cannot be
		// recalled, from this store or anyone else's. It simply stops being
		// refreshed and ages out everywhere within Intervals.GraphTTL.
		return
	}

	origin := n.PublicKeyHex()
	edges := ownEdges(peers)
	prev := n.storedOwnRecord(ctx, origin)
	now := time.Now()
	if prev != nil && sameEdges(prev.Friends, edges) &&
		now.Sub(time.Unix(prev.IssuedAt, 0)) < n.intervals.GraphRepublish {
		return // unchanged, and the heartbeat is not due
	}

	// The sequence is anchored to wall-clock seconds, not just prev+1, so it
	// stays monotonic even if our own record expired and was swept while this
	// node was offline for longer than the TTL. A clock that jumps backwards
	// still advances, because prev+1 is the floor.
	seq := now.Unix()
	if prev != nil && prev.Seq >= seq {
		seq = prev.Seq + 1
	}
	rec := GraphRecord{Origin: origin, Seq: seq, IssuedAt: now.Unix(), Friends: edges}
	raw, err := SignGraphRecord(n.signKey, rec)
	if err != nil {
		n.logger.Printf("federation: sign own graph record: %v", err)
		return
	}
	parsed, err := ParseGraphRecord(raw)
	if err != nil {
		n.logger.Printf("federation: own graph record does not verify: %v", err)
		return
	}
	expires := now.Add(n.intervals.GraphTTL).Unix()
	if _, err := n.store.PutGraphRecord(ctx, parsed, raw, nil, expires, now.Unix()); err != nil {
		n.logger.Printf("federation: store own graph record: %v", err)
		return
	}
	n.invalidateGraphDigest()
	n.logger.Printf("federation: published friend-list record seq %d (%d friendships)", seq, len(edges))
}

// publishOwnMarkRecord signs and stores this node's distrust list — every peer
// it has blocked, with the reason and the time.
//
// Deliberately NOT gated on the publish-friend-list setting: that switch is
// about naming third parties this node is friends with, while a mark is this
// node's own judgement, and the decision was that every block is published
// (docs/architecture/federation.md §Friend-list gossip, D6b — there are no
// private blocks).
func (n *Node) publishOwnMarkRecord(ctx context.Context, peers []*Peer) {
	if n.store == nil || n.signKey == nil {
		return
	}
	origin := n.PublicKeyHex()
	marks := ownMarks(peers)
	prev := n.storedOwnMarks(ctx, origin)
	now := time.Now()

	switch {
	case prev == nil && len(marks) == 0:
		// Never blocked anyone: publish nothing rather than an empty record that
		// costs every store a row to say so.
		return
	case prev != nil && sameMarks(prev.Marks, marks) &&
		now.Sub(time.Unix(prev.IssuedAt, 0)) < n.intervals.GraphRepublish:
		return // unchanged, and the heartbeat is not due
	}
	// Note the asymmetry with the case above: once a record exists, an EMPTY one
	// still gets published. Lifting the last block has to supersede the record
	// that carried it — letting it expire instead would leave the mark standing
	// on every node for up to the TTL after the admin cleared it.

	seq := now.Unix()
	if prev != nil && prev.Seq >= seq {
		seq = prev.Seq + 1
	}
	raw, err := SignMarkRecord(n.signKey, MarkRecord{
		Origin: origin, Seq: seq, IssuedAt: now.Unix(), Marks: marks,
	})
	if err != nil {
		n.logger.Printf("federation: sign own distrust record: %v", err)
		return
	}
	parsed, err := ParseMarkRecord(raw)
	if err != nil {
		n.logger.Printf("federation: own distrust record does not verify: %v", err)
		return
	}
	expires := now.Add(n.intervals.GraphTTL).Unix()
	if _, err := n.store.PutMarkRecord(ctx, parsed, raw, nil, expires, now.Unix()); err != nil {
		n.logger.Printf("federation: store own distrust record: %v", err)
		return
	}
	n.invalidateGraphDigest()
	n.logger.Printf("federation: published distrust record seq %d (%d mark(s))", seq, len(marks))
}

// ownMarks is the distrust list this node publishes: its blocked peers,
// key-ordered so an unchanged set produces an unchanged record.
func ownMarks(peers []*Peer) []DistrustMark {
	marks := make([]DistrustMark, 0, len(peers))
	for _, p := range peers {
		if p.State != PeerBlocked {
			continue
		}
		marks = append(marks, DistrustMark{Key: p.PublicKey, At: p.BlockedAt, Reason: p.BlockReason})
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].Key < marks[j].Key })
	if len(marks) > MaxMarksPerRecord {
		marks = marks[:MaxMarksPerRecord]
	}
	return marks
}

func sameMarks(a, b []DistrustMark) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// storedOwnMarks reads back the distrust record we last published, if any.
func (n *Node) storedOwnMarks(ctx context.Context, origin string) *MarkRecord {
	payloads, err := n.store.MarkPayloads(ctx, []string{origin}, 0)
	if err != nil {
		n.logger.Printf("federation: read own distrust record: %v", err)
		return nil
	}
	raw, ok := payloads[origin]
	if !ok {
		return nil
	}
	rec, err := ParseMarkRecord(raw)
	if err != nil {
		return nil
	}
	return rec
}

// ownEdges is the friendship set this node publishes: its friends, key-ordered
// so an unchanged friend list produces an unchanged record and the sequence
// does not bump on a mere reordering by the store.
//
// Only established friendships are published. A pending pairing is not a
// friendship yet, and a blocked peer is a judgement we publish as a mark
// instead — putting either on the map would misstate the graph.
func ownEdges(peers []*Peer) []GraphEdge {
	edges := make([]GraphEdge, 0, len(peers))
	for _, p := range peers {
		if p.State != PeerFriend {
			continue
		}
		edges = append(edges, GraphEdge{Key: p.PublicKey, Name: p.Label(), Since: p.CreatedAt})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Key < edges[j].Key })
	if len(edges) > MaxGraphEdges {
		edges = edges[:MaxGraphEdges] // our own list, so this is a cap we can only hit by having that many friends
	}
	return edges
}

func sameEdges(a, b []GraphEdge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// storedOwnRecord reads back the record we last published, if any. now is 0 so
// an expired-but-unswept record still answers: its sequence is what keeps the
// next one monotonic.
func (n *Node) storedOwnRecord(ctx context.Context, origin string) *GraphRecord {
	payloads, err := n.store.GraphPayloads(ctx, []string{origin}, 0)
	if err != nil {
		n.logger.Printf("federation: read own graph record: %v", err)
		return nil
	}
	raw, ok := payloads[origin]
	if !ok {
		return nil
	}
	rec, err := ParseGraphRecord(raw)
	if err != nil {
		return nil // our own key changed, or the row is corrupt: republish from scratch
	}
	return rec
}

// expireGraph ages the store on both of its clocks. Records past their TTL go
// first — an abandoned key fades from every store without anyone acting — and
// then the reachability walk collects what an admin's action orphaned: block or
// remove a friend and the branch behind them is no longer reachable from our
// key, so we stop holding it, stop offering it in our digest, and (because
// nothing we hold names those origins any more) stop admitting it.
//
// Order matters. Expiring first means the walk runs over live edges only, so a
// branch held up solely by a record that just aged out is collected in the same
// pass rather than the next one.
func (n *Node) expireGraph(ctx context.Context, peers []*Peer) {
	if n.store == nil {
		return
	}
	now := time.Now().Unix()
	if dropped, err := n.store.ExpireGraph(ctx, now); err != nil {
		n.logger.Printf("federation: expire graph: %v", err)
		return
	} else if dropped > 0 {
		n.invalidateGraphDigest()
		n.logger.Printf("federation: expired %d gossip record(s)", dropped)
	}

	edges, err := n.store.GraphEdges(ctx, now)
	if err != nil {
		n.logger.Printf("federation: read graph edges for retention: %v", err)
		return
	}
	keep := ReachableKeys(n.PublicKeyHex(), peers, edges)
	dropped, err := n.store.DropUnreachableGraph(ctx, keep)
	if err != nil {
		n.logger.Printf("federation: drop unreachable gossip records: %v", err)
		return
	}
	if dropped > 0 {
		// Worth a line rather than silence: this is the one place the store
		// shrinks because of something an admin did, and a map that loses a third
		// of its nodes should be explainable from the log.
		n.logger.Printf("federation: dropped %d gossip record(s) no longer reachable from us", dropped)
		n.invalidateGraphDigest()
	}
}

// ── Serving the graph ────────────────────────────────────────────────────────

// ownDigest is the digest we serve, memoized for Intervals.GraphDigestTTL —
// exactly what ownSnapshot does for the catalog, and for the same reason.
//
// This is the whole answer to a friend that pulls too often (a rescan button
// held down, a buggy build, a loop): the second pull inside the window costs a
// mutex and a slice header instead of two table scans. A per-peer cooldown
// answering 429 was rejected on purpose — syncGraph reads every non-200 as "an
// older peer without the endpoint" and returns, so a refusal is indistinguishable
// from absence and a mistuned one would silently stop two honest nodes
// converging (docs/architecture/federation.md §Refreshing the graph on demand).
//
// Unlike the catalog there is no per-audience split: the graph is served to
// friends and to nobody else, so every caller gets the same bytes.
func (n *Node) ownDigest(ctx context.Context) ([]GraphDigestEntry, []GraphDigestEntry, string, error) {
	n.digestMu.Lock()
	defer n.digestMu.Unlock()
	if n.digest != nil && time.Since(n.digest.built) < n.intervals.GraphDigestTTL {
		return n.digest.records, n.digest.marks, n.digest.serial, nil
	}
	records, marks, err := n.store.GraphDigest(ctx, time.Now().Unix())
	if err != nil {
		return nil, nil, "", err
	}
	n.digest = &graphDigest{
		records: records, marks: marks,
		serial: graphSerial(records, marks), built: time.Now(),
	}
	return n.digest.records, n.digest.marks, n.digest.serial, nil
}

// invalidateGraphDigest forces the next serve to rebuild. Called where the store
// changes in a way a friend should hear about promptly — a record we accepted,
// a branch we dropped — so the TTL bounds staleness for the quiet case without
// adding it to the case that matters.
func (n *Node) invalidateGraphDigest() {
	n.digestMu.Lock()
	n.digest = nil
	n.digestMu.Unlock()
}

// handleGraph serves GET /madnetwork/v0/graph?since=<serial> — the digest of
// everything we hold. Friends only, like the catalog: the graph names third
// parties, so it is not a stranger's to read.
func (n *Node) handleGraph(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "graph not configured", http.StatusServiceUnavailable)
		return
	}
	if p := n.peerFromRemote(r); p == nil || p.State != PeerFriend {
		http.Error(w, "the network graph is served to friends only", http.StatusForbidden)
		return
	}
	records, marks, serial, err := n.ownDigest(r.Context())
	if err != nil {
		n.logger.Printf("federation: build graph digest: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	msg := graphDigestMessage{Protocol: ProtocolVersion, Serial: serial}
	if r.URL.Query().Get("since") == msg.Serial {
		msg.Unchanged = true
	} else {
		msg.Records, msg.Marks = records, marks
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

// handleGraphFetch serves POST /madnetwork/v0/graph/fetch: the raw bytes of the
// named records. A POST because the request is a list of keys — too many for a
// query string once a network is more than a handful of nodes.
func (n *Node) handleGraphFetch(w http.ResponseWriter, r *http.Request) {
	if n.store == nil {
		http.Error(w, "graph not configured", http.StatusServiceUnavailable)
		return
	}
	if p := n.peerFromRemote(r); p == nil || p.State != PeerFriend {
		http.Error(w, "the network graph is served to friends only", http.StatusForbidden)
		return
	}
	var req graphFetchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.Records) > maxGraphFetch {
		req.Records = req.Records[:maxGraphFetch]
	}
	if len(req.Marks) > maxGraphFetch {
		req.Marks = req.Marks[:maxGraphFetch]
	}
	now := time.Now().Unix()
	reply := graphFetchReply{Protocol: ProtocolVersion}
	if len(req.Records) > 0 {
		payloads, err := n.store.GraphPayloads(r.Context(), req.Records, now)
		if err != nil {
			n.logger.Printf("federation: read graph payloads: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		for _, origin := range req.Records {
			if raw, ok := payloads[origin]; ok {
				reply.Records = append(reply.Records, json.RawMessage(raw))
			}
		}
	}
	if len(req.Marks) > 0 {
		payloads, err := n.store.MarkPayloads(r.Context(), req.Marks, now)
		if err != nil {
			n.logger.Printf("federation: read mark payloads: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		for _, origin := range req.Marks {
			if raw, ok := payloads[origin]; ok {
				reply.Marks = append(reply.Marks, json.RawMessage(raw))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// ── Pulling a friend's graph ─────────────────────────────────────────────────

// syncGraph runs one gossip round against one friend: pull their digest, work
// out what we lack, fetch it, and store whatever verifies and is admissible.
func (n *Node) syncGraph(ctx context.Context, p *Peer) {
	addr, err := AddrForKeyHex(p.PublicKey)
	if err != nil {
		return
	}
	base := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/graph", addr, MeshPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return // unreachable — the refresh loop retries
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return // an older peer without the endpoint, or not-yet-friend on their side
	}
	var digest graphDigestMessage
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		return
	}

	now := time.Now().Unix()
	haveRecords, haveMarks, err := n.store.GraphDigest(ctx, now)
	if err != nil {
		n.logger.Printf("federation: read own graph digest: %v", err)
		return
	}
	wantRecords := missing(haveRecords, digest.Records)
	wantMarks := missing(haveMarks, digest.Marks)
	if len(wantRecords) == 0 && len(wantMarks) == 0 {
		return
	}

	// The friend's OWN record goes first, and the reply preserves request order.
	// Until it lands, the nodes it names are strangers to our admission check,
	// so a record from one of them arriving earlier in the same batch would only
	// be refused. Ordering it this way is what lets one round reach a friend's
	// friends instead of stalling at the friend itself.
	for i, origin := range wantRecords {
		if origin == p.PublicKey {
			wantRecords[0], wantRecords[i] = wantRecords[i], wantRecords[0]
			break
		}
	}
	if len(wantRecords) > maxGraphFetch {
		wantRecords = wantRecords[:maxGraphFetch]
	}
	if len(wantMarks) > maxGraphFetch {
		wantMarks = wantMarks[:maxGraphFetch]
	}

	body, err := json.Marshal(graphFetchRequest{Records: wantRecords, Marks: wantMarks})
	if err != nil {
		return
	}
	fetch, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/fetch", bytes.NewReader(body))
	if err != nil {
		return
	}
	fetch.Header.Set("Content-Type", "application/json")
	fresp, err := n.client.Do(fetch)
	if err != nil {
		return
	}
	defer fresp.Body.Close()
	if fresp.StatusCode != http.StatusOK {
		return
	}
	var reply graphFetchReply
	if err := json.NewDecoder(fresp.Body).Decode(&reply); err != nil {
		return
	}

	stored := 0
	for _, raw := range reply.Records {
		if n.acceptGraphRecord(ctx, p, raw) {
			stored++
		}
	}
	for _, raw := range reply.Marks {
		if n.acceptMarkRecord(ctx, p, raw) {
			stored++
		}
	}
	if stored > 0 {
		// What we just learned is what our own friends have not heard yet, so the
		// memoized digest must not sit on it for the rest of its window.
		n.invalidateGraphDigest()
		n.logger.Printf("federation: learned %d gossip record(s) via %q", stored, p.Label())
	}
}

// missing lists the origins offered by a peer whose sequence we do not already
// hold. An equal sequence is not missing — that is the check that stops a
// record circulating forever.
func missing(have, offered []GraphDigestEntry) []string {
	held := make(map[string]int64, len(have))
	for _, e := range have {
		held[e.Origin] = e.Seq
	}
	var want []string
	for _, e := range offered {
		if seq, ok := held[e.Origin]; !ok || e.Seq > seq {
			want = append(want, e.Origin)
		}
	}
	return want
}

// acceptGraphRecord verifies, admits and stores one relayed record. Every
// refusal below is silent: a peer relaying junk is not necessarily hostile, and
// nothing here is grounds for an alarm an admin has to read.
func (n *Node) acceptGraphRecord(ctx context.Context, from *Peer, raw []byte) bool {
	rec, err := ParseGraphRecord(raw)
	if err != nil {
		return false // unsigned, malformed, or over the per-record bounds
	}
	if !n.admitRecord(ctx, from, rec.Origin) {
		return false
	}
	expires := time.Unix(rec.IssuedAt, 0).Add(n.intervals.GraphTTL).Unix()
	stored, err := n.store.PutGraphRecord(ctx, rec, raw, &from.ID, expires, time.Now().Unix())
	if err != nil {
		n.logger.Printf("federation: store graph record: %v", err)
		return false
	}
	return stored
}

// acceptMarkRecord is the same for a distrust list.
func (n *Node) acceptMarkRecord(ctx context.Context, from *Peer, raw []byte) bool {
	rec, err := ParseMarkRecord(raw)
	if err != nil {
		return false
	}
	if !n.admitRecord(ctx, from, rec.Origin) {
		return false
	}
	expires := time.Unix(rec.IssuedAt, 0).Add(n.intervals.GraphTTL).Unix()
	stored, err := n.store.PutMarkRecord(ctx, rec, raw, &from.ID, expires, time.Now().Unix())
	if err != nil {
		n.logger.Printf("federation: store mark record: %v", err)
		return false
	}
	return stored
}

// admitRecord applies the three bounds that keep a relayed record from costing
// us anything unbounded: it must come from a node someone in our store already
// names, its author may not push at us faster than an honest node would, and no
// single friend may introduce more than its share of the store.
//
// Our own record is always admissible — it is not relayed to us, we wrote it.
func (n *Node) admitRecord(ctx context.Context, from *Peer, origin string) bool {
	if origin == n.PublicKeyHex() {
		return true
	}
	known, err := n.store.GraphKnowsKey(ctx, origin)
	if err != nil {
		n.logger.Printf("federation: graph admission check: %v", err)
		return false
	}
	if !known {
		return false // named by nobody we hold: junk a friend invented
	}
	if !n.rateAdmits(origin) {
		return false
	}
	count, err := n.store.GraphIntroducedCount(ctx, from.ID)
	if err != nil {
		n.logger.Printf("federation: graph branch quota: %v", err)
		return false
	}
	return count < MaxOriginsPerBranch
}

// rateAdmits bounds how often one origin's record may be accepted
// (Intervals.GraphAccept). A sybil farm churning sequences costs a map lookup
// here rather than a write.
//
// Refusing is safe because it is never final: the record stays in the friend's
// digest, so the next round asks for it again. Convergence is therefore delayed
// by at most one interval, never lost.
func (n *Node) rateAdmits(origin string) bool {
	now := time.Now()
	n.acceptMu.Lock()
	defer n.acceptMu.Unlock()
	if last, ok := n.graphAccept[origin]; ok && now.Sub(last) < n.intervals.GraphAccept {
		return false
	}
	n.graphAccept[origin] = now
	return true
}

// NetworkMap builds the admin-facing view of the gossiped graph: who is out
// there, how far, through whom, and what the network says about them.
func (n *Node) NetworkMap(ctx context.Context) (NetworkMap, error) {
	if n.store == nil {
		return NetworkMap{}, nil
	}
	now := time.Now().Unix()
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		return NetworkMap{}, err
	}
	edges, err := n.store.GraphEdges(ctx, now)
	if err != nil {
		return NetworkMap{}, err
	}
	marks, err := n.store.GraphMarks(ctx, now)
	if err != nil {
		return NetworkMap{}, err
	}
	m := BuildNetworkMap(n.PublicKeyHex(), peers, edges, marks)
	// Mesh addresses are derived here rather than stored: the key is the
	// identity, the address is a function of it, and the map shows both beside
	// every name because a gossiped name is hearsay about a stranger.
	for i := range m.Nodes {
		if addr, err := AddrForKeyHex(m.Nodes[i].Key); err == nil {
			m.Nodes[i].Address = addr.String()
		}
	}
	return m, nil
}
