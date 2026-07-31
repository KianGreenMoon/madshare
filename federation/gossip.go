package federation

// Friend-list gossip (federation F6): the two record types nodes relay, their
// canonical encoding, and signing/verification. Design:
// docs/architecture/federation.md §"Friend-list gossip & the network graph".
//
// Untagged, like federation.go and for the same reason: the database layer
// stores these records and is built in both tag variants, so the types (and the
// verification the store gates admission on) must exist without the yggdrasil
// dependencies. Only the wire and the sync loop need -tags !nofederation.
//
// The shape of the whole feature follows from one property: a node relays
// records it did not write. A friend hands us a record authored by a node we
// have never met, so the sender's mesh address — which authenticates everything
// else in this package (friendship.go, "the source address *is* proof of key
// possession") — proves nothing about the record's contents. The author's own
// signature is what carries the claim across the hop, and it is why a relay can
// withhold a record but never forge one.
//
// That also fixes how the signature is computed. A record is verified against
// the *bytes as received*, never against a re-marshaled struct: a relay running
// an older build must be able to pass along a record carrying fields it cannot
// parse, and re-serializing would silently drop them and break the signature for
// everyone downstream. [signingInput] therefore canonicalizes the raw JSON —
// unknown fields and all — instead of the parsed value.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Bounds on a single record. These are anti-flood limits, not policy: they cap
// what one document can cost a receiver, and are checked before anything is
// stored (docs/architecture/federation.md §Friend-list gossip, "Anti-flood
// bounds").
const (
	// MaxGraphEdges caps the friendships one record may claim. A longer list is
	// *refused*, not truncated — a truncated friend list is a false statement
	// about a node's edges, and the map would draw it as fact.
	MaxGraphEdges = 512
	// MaxMarksPerRecord caps the distrust marks one record may carry, for the
	// same reason and with the same effect.
	MaxMarksPerRecord = 512
	// MaxMarkReasonRunes caps a mark's free text. Unlike the list lengths this
	// truncates rather than refuses: an over-long reason is still a usable
	// reason, and the peer-name rules (runes, never bytes — see CleanPeerName)
	// apply because it is remote input rendered in a UI.
	MaxMarkReasonRunes = 280
	// MaxOriginsPerBranch caps how many nodes any single friend may introduce
	// into our store. A sybil farm is cheap to mint and arrives through exactly
	// one edge, so the bound is per-branch rather than global: an honest friend
	// with a large network is unaffected, and a farm hits a ceiling that a
	// single block then clears entirely.
	MaxOriginsPerBranch = 5000
)

// sigField is the record field the signature itself lives in — excluded from
// the bytes it covers, since it cannot sign itself.
const sigField = "sig"

// ErrBadSignature marks a record whose signature does not verify against its
// claimed origin: a forgery, a corrupted relay, or a bug on the far side. The
// receiver drops it silently — there is no honest way to produce one.
var ErrBadSignature = errors.New("federation: gossip record signature does not verify")

// GraphEdge is one friendship as gossiped: the friend's key, the label the
// publisher uses for that friend, and when the friendship was made.
//
// The name is *hearsay about a stranger* — the publisher's private label for a
// node the receiver may never have met — so every surface rendering it shows
// the key beside it (docs/architecture/federation.md §Friendship, naming).
// Since is a durability signal: an old edge is worth more than a fresh one when
// trust weighting arrives (F7).
type GraphEdge struct {
	Key   string `json:"key"`
	Name  string `json:"name,omitempty"`
	Since int64  `json:"since,omitempty"`
}

// GraphRecord is one node's signed statement about its own friendships — the
// unit that propagates. Receivers keep the highest Seq per Origin, so a
// duplicate is dropped without re-propagation and gossip loops terminate
// without hop counts or TTLs.
type GraphRecord struct {
	Protocol int    `json:"protocol"`
	Origin   string `json:"origin"`
	Seq      int64  `json:"seq"`
	// IssuedAt is when the origin signed this record (unix seconds). It drives
	// expiry: a receiver drops the record after the TTL and stops serving it, so
	// an abandoned key fades from every store without anyone acting.
	IssuedAt  int64       `json:"issued_at"`
	Friends   []GraphEdge `json:"friends"`
	Signature string      `json:"sig,omitempty"`
}

// DistrustMark is one published block: whom, when, and why.
//
// The reason is what makes a mark actionable — a bare key is an anonymous
// downvote that forces the reader to ask out-of-band what happened. It is also
// why marks are the most dangerous thing in this file: they relay network-wide
// and are readable by their target (see the accepted risk recorded in
// docs/architecture/federation.md §Friend-list gossip).
type DistrustMark struct {
	Key    string `json:"key"`
	At     int64  `json:"at"`
	Reason string `json:"reason,omitempty"`
}

// MarkRecord is one node's signed distrust list. Separate from [GraphRecord]
// and independently sequenced, so a mark can expire while the friendship record
// it travelled with stays live.
type MarkRecord struct {
	Protocol  int            `json:"protocol"`
	Origin    string         `json:"origin"`
	Seq       int64          `json:"seq"`
	IssuedAt  int64          `json:"issued_at"`
	Marks     []DistrustMark `json:"marks"`
	Signature string         `json:"sig,omitempty"`
}

// signingInput is the canonical byte sequence a record's signature covers: the
// document with its "sig" field removed, object keys sorted, whitespace
// compacted.
//
// It works from raw JSON rather than a parsed struct on purpose. Values are
// carried as [json.RawMessage] and re-emitted verbatim (encoding/json compacts
// them), so a field this build has never heard of still contributes its exact
// bytes to the signature. That is what lets an old relay carry a new node's
// record without invalidating it — the property the whole relay design rests on.
func signingInput(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("federation: gossip record is not a JSON object: %w", err)
	}
	delete(doc, sigField)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalize gossip record: %w", err)
	}
	return out, nil
}

// signDocument signs a record with the node's own ed25519 key and returns the
// wire bytes, signature included. The returned bytes are what gets stored and
// relayed — callers must not re-marshal the parsed form (see the file comment).
func signDocument(priv ed25519.PrivateKey, doc any) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	delete(m, sigField)
	input, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalize gossip record: %w", err)
	}
	sig, err := json.Marshal(hex.EncodeToString(ed25519.Sign(priv, input)))
	if err != nil {
		return nil, fmt.Errorf("federation: encode signature: %w", err)
	}
	m[sigField] = sig
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	return out, nil
}

// verifySignature checks a record's signature against the key it claims to come
// from. The origin is self-certifying in the sense that matters here: a key that
// verifies the bytes is the key that wrote them, whoever handed them over.
func verifySignature(raw []byte, origin, signature string) error {
	key, err := NormalizeKey(origin)
	if err != nil {
		return fmt.Errorf("federation: gossip record origin: %w", err)
	}
	pub, err := hex.DecodeString(key)
	if err != nil {
		return fmt.Errorf("federation: gossip record origin: %w", err)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	input, err := signingInput(raw)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), input, sig) {
		return ErrBadSignature
	}
	return nil
}

// SignGraphRecord fills in the protocol version and signs rec, returning the
// wire bytes to publish. Friends are left in the caller's order — the builder
// sorts them (see the own-record build) so a record only changes when the
// friendships do.
func SignGraphRecord(priv ed25519.PrivateKey, rec GraphRecord) ([]byte, error) {
	rec.Protocol = ProtocolVersion
	rec.Signature = ""
	return signDocument(priv, rec)
}

// SignMarkRecord fills in the protocol version and signs rec.
func SignMarkRecord(priv ed25519.PrivateKey, rec MarkRecord) ([]byte, error) {
	rec.Protocol = ProtocolVersion
	rec.Signature = ""
	return signDocument(priv, rec)
}

// ParseGraphRecord validates raw as a signed friend-list record: well-formed
// JSON, a protocol this node speaks, a verifying signature, and edges within
// the anti-flood bounds. Names are sanitized in the returned value for display;
// raw is unchanged and stays the thing to store and relay.
func ParseGraphRecord(raw []byte) (*GraphRecord, error) {
	var rec GraphRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("federation: not a gossip record: %w", err)
	}
	if rec.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("federation: unsupported gossip record version %d (this node speaks %d)", rec.Protocol, ProtocolVersion)
	}
	if err := verifySignature(raw, rec.Origin, rec.Signature); err != nil {
		return nil, err
	}
	origin, err := NormalizeKey(rec.Origin)
	if err != nil {
		return nil, fmt.Errorf("federation: gossip record origin: %w", err)
	}
	rec.Origin = origin
	if rec.Seq < 0 || rec.IssuedAt < 0 {
		return nil, errors.New("federation: gossip record has a negative sequence or timestamp")
	}
	if len(rec.Friends) > MaxGraphEdges {
		return nil, fmt.Errorf("federation: gossip record claims %d friendships (limit %d)", len(rec.Friends), MaxGraphEdges)
	}
	seen := make(map[string]struct{}, len(rec.Friends))
	for i := range rec.Friends {
		key, err := NormalizeKey(rec.Friends[i].Key)
		if err != nil {
			return nil, fmt.Errorf("federation: gossip record edge %d: %w", i, err)
		}
		// A self-loop and a repeated key are both malformed rather than merely
		// odd: they inflate an edge count that later weighs a branch, and no
		// honest builder emits either.
		if key == origin {
			return nil, errors.New("federation: gossip record claims a friendship with itself")
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("federation: gossip record names %s twice", key)
		}
		seen[key] = struct{}{}
		rec.Friends[i].Key = key
		rec.Friends[i].Name = CleanPeerName(rec.Friends[i].Name)
		if rec.Friends[i].Since < 0 {
			rec.Friends[i].Since = 0
		}
	}
	return &rec, nil
}

// ParseMarkRecord validates raw as a signed distrust list, on the same terms as
// [ParseGraphRecord]. Reasons are capped rather than refused — an over-long one
// is still usable evidence.
func ParseMarkRecord(raw []byte) (*MarkRecord, error) {
	var rec MarkRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("federation: not a distrust record: %w", err)
	}
	if rec.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("federation: unsupported distrust record version %d (this node speaks %d)", rec.Protocol, ProtocolVersion)
	}
	if err := verifySignature(raw, rec.Origin, rec.Signature); err != nil {
		return nil, err
	}
	origin, err := NormalizeKey(rec.Origin)
	if err != nil {
		return nil, fmt.Errorf("federation: distrust record origin: %w", err)
	}
	rec.Origin = origin
	if rec.Seq < 0 || rec.IssuedAt < 0 {
		return nil, errors.New("federation: distrust record has a negative sequence or timestamp")
	}
	if len(rec.Marks) > MaxMarksPerRecord {
		return nil, fmt.Errorf("federation: distrust record carries %d marks (limit %d)", len(rec.Marks), MaxMarksPerRecord)
	}
	seen := make(map[string]struct{}, len(rec.Marks))
	for i := range rec.Marks {
		key, err := NormalizeKey(rec.Marks[i].Key)
		if err != nil {
			return nil, fmt.Errorf("federation: distrust record mark %d: %w", i, err)
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("federation: distrust record marks %s twice", key)
		}
		seen[key] = struct{}{}
		rec.Marks[i].Key = key
		rec.Marks[i].Reason = CleanMarkReason(rec.Marks[i].Reason)
		if rec.Marks[i].At < 0 {
			rec.Marks[i].At = 0
		}
	}
	return &rec, nil
}

// GraphDigestEntry is one line of the digest two nodes exchange before moving
// any payload: an origin and the sequence we hold for it. A sync round compares
// digests and fetches only what is missing, which is what keeps an unchanged
// graph costing one small round-trip instead of a transfer.
type GraphDigestEntry struct {
	Origin string `json:"origin"`
	Seq    int64  `json:"seq"`
}

// GraphEdgeClaim is one stored friendship claim as the network map reads it:
// who claims it, whom with, and under what label. A claim, not a fact — only
// the origin's own signature stands behind it.
type GraphEdgeClaim struct {
	Origin string
	Peer   string
	Name   string
	Since  int64
}

// StoredMark is one published block as the map reads it.
type StoredMark struct {
	Origin string
	Target string
	At     int64
	Reason string
}

// CleanMarkReason sanitizes and rune-caps a distrust mark's free text. Exactly
// [CleanPeerName]'s rules at a larger cap: remote input, rendered in a UI,
// counted in runes so a multi-byte character is never cut in half. A reason is
// read by a human deciding whether an accusation applies to them, so it has the
// same display-integrity stake a name does — more, since it is longer.
func CleanMarkReason(reason string) string { return sanitizeLabel(reason, MaxMarkReasonRunes) }

// ── The network map (F6) ─────────────────────────────────────────────────────

// NetworkMap is the gossiped graph as an admin sees it: every node reachable
// through some chain of friendships, how far away it is, which of our friends
// vouched for it, and what the network says about it.
type NetworkMap struct {
	Nodes []MapNode `json:"nodes"`
	Edges []MapEdge `json:"edges"`
	// Radius is the greatest distance any node sits at — how far this node can
	// currently see.
	Radius int `json:"radius"`
}

// Map node states, in the order the UI ranks them. Anything else is a stranger:
// a node we know of only because the graph names it.
const (
	MapSelf    = "self"
	MapFriend  = "friend"
	MapPending = "pending"
	MapBlocked = "blocked"
)

// MapNode is one node on the map.
type MapNode struct {
	Key     string `json:"key"`
	Address string `json:"address,omitempty"`
	// Name is the best label available, in falling order of trust: our own local
	// label, then what the graph calls it. Beyond our friend list a name is
	// hearsay about a stranger, so Named reports whether it is ours or theirs and
	// every surface renders the key beside it.
	Name  string `json:"name,omitempty"`
	Named string `json:"named,omitempty"` // "local" | "heard"
	State string `json:"state,omitempty"`
	// PeerID is the trusted-peer row when we have one, so the map can drive the
	// peer operations (unblock, remove) that are addressed by id. Absent for a
	// node we know only from the graph.
	PeerID int64 `json:"peer_id,omitempty"`
	// Distance is friendship hops from us: 0 is this node, 1 a direct friend.
	Distance int `json:"distance"`
	// Via are the direct friends this node is reachable through — the branch
	// attribution. Blocking every key here removes the node from our view, which
	// is what "snipping a branch" means concretely.
	Via []string `json:"via,omitempty"`
	// Marks are the published distrust marks against this node.
	Marks []MapMark `json:"marks,omitempty"`
	// MarkBranches counts those marks the way they must be READ: one branch is
	// one voice (§Trust graph). A sybil farm behind a single friendship shouts
	// once, however many keys it mints.
	MarkBranches int `json:"mark_branches,omitempty"`
}

// MapEdge is one friendship drawn on the map. Undirected: a friendship has two
// ends, and whether one or both ends published a record about it changes
// nothing about the edge being there.
type MapEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Mutual reports whether both ends published the claim. A one-sided edge is
	// not a lie — the other end may simply publish no record — but it is weaker
	// evidence, so the map draws it faintly rather than identically.
	Mutual bool `json:"mutual"`
}

// MapMark is one distrust mark shown against a node.
type MapMark struct {
	Origin     string `json:"origin"`
	OriginName string `json:"origin_name,omitempty"`
	Reason     string `json:"reason,omitempty"`
	At         int64  `json:"at,omitempty"`
	// Branch is the direct friend this accusation reached us through, or "self"
	// for our own. It is what MarkBranches counts.
	Branch string `json:"branch,omitempty"`
}

// walkGraph is the reachability walk every other rule is expressed in terms of:
// multi-source BFS from selfKey over the gossiped edges, never traversing
// THROUGH a blocked node, returning each reachable key's distance and the direct
// friends it was discovered through.
//
// Two properties matter and both are deliberate. **Edges with our own key at an
// end come from peers alone** — a friendship of ours is a fact we hold, not a
// claim to be weighed, so a node whose admin never removed us cannot put itself
// back on our inner ring by publishing that we are friends. Other nodes' edges
// stay single-claim, because an edge somebody claims is worth seeing. And
// **branch snipping falls out of the walk**: nodes reachable only behind a
// blocked node drop out, while a node also vouched for by another friend keeps
// whatever distance and labels remain (docs/architecture/federation.md
// §Forgetting).
func walkGraph(selfKey string, peers []*Peer, edges []GraphEdgeClaim) (dist map[string]int, via map[string]map[string]bool) {
	adj := map[string]map[string]bool{}
	link := func(a, b string) {
		if adj[a] == nil {
			adj[a] = map[string]bool{}
		}
		adj[a][b] = true
	}
	for _, e := range edges {
		if e.Origin == e.Peer || e.Origin == selfKey || e.Peer == selfKey {
			continue // hearsay about our own friendships is not evidence
		}
		link(e.Origin, e.Peer)
		link(e.Peer, e.Origin)
	}
	// Our own friendships, from the only source entitled to state them.
	for _, p := range peers {
		if p.State == PeerFriend {
			link(selfKey, p.PublicKey)
			link(p.PublicKey, selfKey)
		}
	}

	byKey := peerByKeyOf(peers)
	blocked := func(key string) bool {
		p, ok := byKey[key]
		return ok && p.State == PeerBlocked
	}

	dist = map[string]int{selfKey: 0}
	via = map[string]map[string]bool{}
	frontier := []string{selfKey}
	for depth := 0; len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			// A blocked node is shown, but nothing is discovered through it.
			if cur != selfKey && blocked(cur) {
				continue
			}
			for peer := range adj[cur] {
				labels := via[cur]
				if depth == 0 {
					labels = map[string]bool{peer: true} // a direct friend is its own branch
				}
				if _, seen := dist[peer]; !seen {
					dist[peer] = depth + 1
					next = append(next, peer)
				}
				// Labels merge even for an already-seen node: reachability through a
				// second friend is exactly what keeps it on the map when the first is
				// blocked.
				if dist[peer] >= depth+1 {
					if via[peer] == nil {
						via[peer] = map[string]bool{}
					}
					for l := range labels {
						via[peer][l] = true
					}
				}
			}
		}
		frontier = next
	}
	return dist, via
}

func peerByKeyOf(peers []*Peer) map[string]*Peer {
	m := make(map[string]*Peer, len(peers))
	for _, p := range peers {
		m[p.PublicKey] = p
	}
	return m
}

// ReachableKeys is [walkGraph]'s answer as a set: every origin whose record this
// node still has a reason to hold. It is what the sweep keeps and drops, and it
// is deliberately the same walk the map draws — a branch that vanished from the
// picture but stayed in the store is the gap §Forgetting exists to close.
//
// Our own key is always present, so an empty result is impossible and a caller
// deleting "everything not in here" can never empty the store by accident.
func ReachableKeys(selfKey string, peers []*Peer, edges []GraphEdgeClaim) map[string]struct{} {
	dist, _ := walkGraph(selfKey, peers, edges)
	out := make(map[string]struct{}, len(dist))
	for key := range dist {
		out[key] = struct{}{}
	}
	// A peer row of ours is a direct relationship: its record stays even while
	// the pairing is pending or the node is blocked, so an admin can still see
	// and undo what they did.
	for _, p := range peers {
		out[p.PublicKey] = struct{}{}
	}
	out[selfKey] = struct{}{}
	return out
}

// BuildNetworkMap computes the map from raw store contents. Pure so the
// reachability rules — which are the whole feature — are testable without a
// mesh.
func BuildNetworkMap(selfKey string, peers []*Peer, edges []GraphEdgeClaim, marks []StoredMark) NetworkMap {
	peerByKey := peerByKeyOf(peers)

	heard := map[string]map[string]int{} // key → name → times claimed
	claimed := map[[2]string]bool{}
	for _, e := range edges {
		if e.Origin == e.Peer || e.Origin == selfKey || e.Peer == selfKey {
			continue // see walkGraph: our own edges are not claims
		}
		claimed[[2]string{e.Origin, e.Peer}] = true
		if e.Name != "" {
			if heard[e.Peer] == nil {
				heard[e.Peer] = map[string]int{}
			}
			heard[e.Peer][e.Name]++
		}
	}

	dist, via := walkGraph(selfKey, peers, edges)

	// Every node we hold a peer row for belongs on the map, reachable through the
	// graph or not: a pending pairing and a blocked key are direct relationships
	// of ours, and an admin has to be able to see and undo them here rather than
	// only in the peer list. They sit on the inner ring with no edge drawn, which
	// is exactly what they are — known to us, vouched for by nobody.
	for _, p := range peers {
		if p.PublicKey == selfKey {
			continue
		}
		if _, seen := dist[p.PublicKey]; !seen {
			dist[p.PublicKey] = 1
		}
	}

	// Marks, grouped by target and weighted by branch.
	byTarget := map[string][]MapMark{}
	for _, m := range marks {
		if _, reachable := dist[m.Origin]; !reachable && m.Origin != selfKey {
			continue // an accusation from outside our view is not evidence we can place
		}
		branch := "self"
		if m.Origin != selfKey {
			for l := range via[m.Origin] {
				branch = l
				break // one branch is one voice; which representative hardly matters
			}
		}
		byTarget[m.Target] = append(byTarget[m.Target], MapMark{
			Origin: m.Origin, OriginName: displayName(m.Origin, peerByKey, heard),
			Reason: m.Reason, At: m.At, Branch: branch,
		})
	}

	out := NetworkMap{}
	for key, d := range dist {
		node := MapNode{Key: key, Distance: d}
		if p, ok := peerByKey[key]; ok && p.Name != "" {
			node.Name, node.Named = p.Name, "local"
		} else if n := commonName(heard[key]); n != "" {
			node.Name, node.Named = n, "heard"
		}
		if p, ok := peerByKey[key]; ok {
			node.PeerID = p.ID
		}
		switch {
		case key == selfKey:
			node.State = MapSelf
		case peerByKey[key] == nil:
			// a stranger: known only because the graph names it
		case peerByKey[key].State == PeerFriend:
			node.State = MapFriend
		case peerByKey[key].State == PeerBlocked:
			node.State = MapBlocked
		default:
			node.State = MapPending
		}
		for l := range via[key] {
			node.Via = append(node.Via, l)
		}
		sort.Strings(node.Via)
		node.Marks = byTarget[key]
		sort.Slice(node.Marks, func(i, j int) bool { return node.Marks[i].Origin < node.Marks[j].Origin })
		branches := map[string]bool{}
		for _, m := range node.Marks {
			branches[m.Branch] = true
		}
		node.MarkBranches = len(branches)
		if d > out.Radius {
			out.Radius = d
		}
		out.Nodes = append(out.Nodes, node)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Distance != out.Nodes[j].Distance {
			return out.Nodes[i].Distance < out.Nodes[j].Distance
		}
		return out.Nodes[i].Key < out.Nodes[j].Key
	})

	// Edges, deduped to one line per friendship, and only between nodes that
	// survived the walk — an edge to a snipped node would draw a line to nothing.
	seen := map[[2]string]bool{}
	for _, e := range edges {
		a, b := e.Origin, e.Peer
		if a == b || a == selfKey || b == selfKey {
			continue // our own edges are drawn below, from our peer rows
		}
		if _, ok := dist[a]; !ok {
			continue
		}
		if _, ok := dist[b]; !ok {
			continue
		}
		lo, hi := a, b
		if lo > hi {
			lo, hi = hi, lo
		}
		if seen[[2]string{lo, hi}] {
			continue
		}
		seen[[2]string{lo, hi}] = true
		out.Edges = append(out.Edges, MapEdge{
			From: lo, To: hi,
			Mutual: claimed[[2]string{lo, hi}] && claimed[[2]string{hi, lo}],
		})
	}
	// Our own friendships, the only edges on this map that are not claims at all:
	// we accepted the pairing, so they are drawn solid rather than weighed for
	// mutuality like a pair of third parties' records.
	for _, p := range peers {
		if p.State != PeerFriend {
			continue
		}
		lo, hi := selfKey, p.PublicKey
		if lo > hi {
			lo, hi = hi, lo
		}
		if seen[[2]string{lo, hi}] {
			continue
		}
		seen[[2]string{lo, hi}] = true
		out.Edges = append(out.Edges, MapEdge{From: lo, To: hi, Mutual: true})
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].From != out.Edges[j].From {
			return out.Edges[i].From < out.Edges[j].From
		}
		return out.Edges[i].To < out.Edges[j].To
	})
	return out
}

// displayName resolves a key to the best label available for it, best evidence
// first: our own label, then what the node told *us* directly in a handshake or
// ping, and only then the name the graph gossips about it — which is hearsay from
// third parties about a node we may never have met.
func displayName(key string, peers map[string]*Peer, heard map[string]map[string]int) string {
	if p, ok := peers[key]; ok {
		if label := p.Label(); label != "" {
			return label
		}
	}
	return commonName(heard[key])
}

// commonName picks the label most of the graph uses for a node, ties broken
// alphabetically so the map does not reshuffle between loads.
func commonName(names map[string]int) string {
	best, bestN := "", 0
	for name, n := range names {
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}
	return best
}
