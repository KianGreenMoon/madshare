//go:build !nofederation

package federation

// The graph half of the memStore fake (F6) — the in-memory twin of
// database/madnetwork_graph.go. It reproduces the rules the real store enforces
// rather than stubbing them out, because those rules are what the mesh tests
// are actually asserting: highest-seq-wins (so a relayed duplicate stops), the
// admission check (so a record nobody names is refused) and expiry.

import (
	"context"
	"sort"
)

// memRecord is one stored signed document: the payload verbatim plus the
// denormalized content the queries read. Only one of edges/marks is populated,
// matching which table the real store would have written.
type memRecord struct {
	seq       int64
	issuedAt  int64
	expiresAt int64
	payload   []byte
	from      *int64
	edges     []GraphEdge
	marks     []DistrustMark
}

func (m *memStore) PutGraphRecord(_ context.Context, rec *GraphRecord, payload []byte, receivedFrom *int64, expiresAt, _ int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if have, ok := m.graph[rec.Origin]; ok && rec.Seq <= have.seq {
		return false, nil
	}
	m.graph[rec.Origin] = &memRecord{
		seq: rec.Seq, issuedAt: rec.IssuedAt, expiresAt: expiresAt,
		payload: append([]byte(nil), payload...), from: receivedFrom,
		edges: append([]GraphEdge(nil), rec.Friends...),
	}
	return true, nil
}

func (m *memStore) PutMarkRecord(_ context.Context, rec *MarkRecord, payload []byte, receivedFrom *int64, expiresAt, _ int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if have, ok := m.marks[rec.Origin]; ok && rec.Seq <= have.seq {
		return false, nil
	}
	m.marks[rec.Origin] = &memRecord{
		seq: rec.Seq, issuedAt: rec.IssuedAt, expiresAt: expiresAt,
		payload: append([]byte(nil), payload...), from: receivedFrom,
		marks: append([]DistrustMark(nil), rec.Marks...),
	}
	return true, nil
}

func (m *memStore) GraphDigest(_ context.Context, now int64) ([]GraphDigestEntry, []GraphDigestEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return digestOf(m.graph, now), digestOf(m.marks, now), nil
}

func digestOf(recs map[string]*memRecord, now int64) []GraphDigestEntry {
	var out []GraphDigestEntry
	for origin, r := range recs {
		if r.expiresAt > now {
			out = append(out, GraphDigestEntry{Origin: origin, Seq: r.seq})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Origin < out[j].Origin })
	return out
}

func (m *memStore) GraphPayloads(_ context.Context, origins []string, now int64) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return payloadsOf(m.graph, origins, now), nil
}

func (m *memStore) MarkPayloads(_ context.Context, origins []string, now int64) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return payloadsOf(m.marks, origins, now), nil
}

func payloadsOf(recs map[string]*memRecord, origins []string, now int64) map[string][]byte {
	out := map[string][]byte{}
	for _, o := range origins {
		if r, ok := recs[o]; ok && r.expiresAt > now {
			out[o] = r.payload
		}
	}
	return out
}

func (m *memStore) GraphKnowsKey(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.peers {
		if p.PublicKey == key {
			return true, nil
		}
	}
	for _, r := range m.graph {
		for _, e := range r.edges {
			if e.Key == key {
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *memStore) GraphIntroducedCount(_ context.Context, peerID int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.graph {
		if r.from != nil && *r.from == peerID {
			n++
		}
	}
	return n, nil
}

func (m *memStore) ExpireGraph(_ context.Context, now int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, recs := range []map[string]*memRecord{m.graph, m.marks} {
		for origin, r := range recs {
			if r.expiresAt <= now {
				delete(recs, origin)
				n++
			}
		}
	}
	return n, nil
}

func (m *memStore) GraphEdges(_ context.Context, now int64) ([]GraphEdgeClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []GraphEdgeClaim
	for origin, r := range m.graph {
		if r.expiresAt <= now {
			continue
		}
		for _, e := range r.edges {
			out = append(out, GraphEdgeClaim{Origin: origin, Peer: e.Key, Name: e.Name, Since: e.Since})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].Peer < out[j].Peer
	})
	return out, nil
}

func (m *memStore) BlockFederationPeer(_ context.Context, id int64, prevState, reason string, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[id]
	if !ok {
		return ErrPeerNotFound
	}
	p.State, p.PrevState, p.BlockReason, p.BlockedAt = PeerBlocked, prevState, reason, at
	return nil
}

// PublishFriendList mirrors the DB default (on). A test that wants a silent
// node clears publishFriends.
func (m *memStore) PublishFriendList(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.silent, nil
}

func (m *memStore) GraphMarks(_ context.Context, now int64) ([]StoredMark, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredMark
	for origin, r := range m.marks {
		if r.expiresAt <= now {
			continue
		}
		for _, mk := range r.marks {
			out = append(out, StoredMark{Origin: origin, Target: mk.Key, At: mk.At, Reason: mk.Reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Origin < out[j].Origin
	})
	return out, nil
}
