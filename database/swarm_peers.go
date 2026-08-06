package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Per-counterparty swarm traffic accounting (docs/architecture/swarm-admin.md
// §Migration 042). The companion to the F7 member quotas: those bound what a
// member may cost us, these say what one has.
//
// One row per node, never per (blob, node) — that is what keeps the table
// bounded by the size of the community rather than by the size of the library.
// It is written from the same drain, in the same transaction, as the per-blob
// table, so the two ledgers of the same bytes agree until someone forgets on one
// side; neither is ever computed from the other.

// SwarmPeerTraffic is one counterparty's all-time byte accounting, with the name
// and class resolved at read time — the row itself stores only the key.
type SwarmPeerTraffic struct {
	// Key is the node's ed25519 public key, lowercase hex. The EMPTY STRING is
	// the unplaced bucket: every requester we could not name, folded together.
	Key  string `json:"key"`
	Up   int64  `json:"up_bytes"`
	Down int64  `json:"down_bytes"`
	// FirstAt/LastAt are unix seconds, stamped by the flusher.
	FirstAt int64 `json:"first_at"`
	LastAt  int64 `json:"last_at"`

	// Name is what we currently call it: the admin's own label if there is one,
	// otherwise the node's own claim, from either the peer row or the source
	// row. Empty when we hold neither, which is a fact worth showing.
	Name string `json:"name,omitempty"`
	// Kind places the node NOW, not when the bytes moved: "unplaced" (the
	// bucket), the peer row's state when we have one (friend / blocked /
	// pending_*), "member" when only the discovery sweep knows it, and "gone"
	// when nothing does — an unfriended node, or one the frontier rotation has
	// evicted. A gone row keeps its bytes: what a node cost us does not stop
	// being true when we forget who it was.
	Kind string `json:"kind"`
}

// Placed reports whether this row is a named node rather than the bucket.
func (p SwarmPeerTraffic) Placed() bool { return p.Key != "" }

// SwarmPeerTrafficDelta is one counterparty's increment in a single flush. Like
// its per-blob sibling it is additive by construction, so a retried write can
// only ever over-count what the next drain would have carried.
type SwarmPeerTrafficDelta struct {
	// Key empty means "could not place this requester": the flusher folds every
	// such delta into the one bucket row rather than minting a row per address.
	Key  string
	Up   int64
	Down int64
}

// addSwarmPeerTraffic folds the peer half of one flush into the table. Called
// inside AddSwarmTraffic's transaction — the two ledgers are written together or
// not at all, so their totals cannot drift by a lost interval.
func addSwarmPeerTraffic(ctx context.Context, tx *sql.Tx, peers []SwarmPeerTrafficDelta, at int64) error {
	const q = `
		INSERT INTO swarm_peer_traffic (public_key, up_bytes, down_bytes, first_at, last_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(public_key) DO UPDATE SET
			up_bytes   = up_bytes + excluded.up_bytes,
			down_bytes = down_bytes + excluded.down_bytes,
			last_at    = excluded.last_at`
	for _, d := range peers {
		if d.Up == 0 && d.Down == 0 {
			continue // never move a quiet node's clock
		}
		key := strings.ToLower(strings.TrimSpace(d.Key))
		if _, err := tx.ExecContext(ctx, q, key, d.Up, d.Down, at, at); err != nil {
			return fmt.Errorf("add swarm peer traffic %q: %w", key, err)
		}
	}
	return nil
}

// The identity half of both queries below, written once. Who a key belongs to
// and what it is to us are decided in ONE place: two copies of a classification
// rule are two rules that eventually disagree, and this one is what an admin
// reads a row's meaning off.
//
// The label chain prefers the admin's own name for a peer over the node's claim,
// and takes the claim from either table. The class places the node NOW: the peer
// row's state when there is one (so a blocked node says so), "member" when only
// the discovery sweep knows it, "gone" when nothing does.
const swarmPeerIdentityCols = `
	       COALESCE(NULLIF(p.name, ''), NULLIF(p.heard_name, ''), NULLIF(s.heard_name, ''), '') AS name,
	       CASE
	         WHEN k.key = ''               THEN 'unplaced'
	         WHEN p.state IS NOT NULL      THEN p.state
	         WHEN s.public_key IS NOT NULL THEN 'member'
	         ELSE 'gone'
	       END AS kind`

const swarmPeerIdentityJoins = `
	LEFT JOIN federation_peers p ON p.public_key = k.key
	LEFT JOIN federation_catalog_sources s ON s.public_key = k.key`

// ListSwarmPeerTraffic returns every counterparty this node has ever traded
// with, busiest first, with the bucket row last whatever its size — it is a
// summary of strangers, not a peer, and reading it as the top node would be
// wrong.
//
// The name and class come from LEFT JOINs rather than stored columns: a heard
// name is a claim that changes, and a node can leave our community without its
// history ceasing to be true.
func (db *DB) ListSwarmPeerTraffic(ctx context.Context) ([]SwarmPeerTraffic, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT k.key, k.up_bytes, k.down_bytes, k.first_at, k.last_at,`+
		swarmPeerIdentityCols+`
		FROM (SELECT public_key AS key, up_bytes, down_bytes, first_at, last_at
		      FROM swarm_peer_traffic) k`+swarmPeerIdentityJoins+`
		ORDER BY (k.key = '') ASC,
		         (k.up_bytes + k.down_bytes) DESC,
		         k.last_at DESC, k.key ASC`)
	if err != nil {
		return nil, fmt.Errorf("list swarm peer traffic: %w", err)
	}
	defer rows.Close()

	var out []SwarmPeerTraffic
	for rows.Next() {
		var p SwarmPeerTraffic
		if err := rows.Scan(&p.Key, &p.Up, &p.Down, &p.FirstAt, &p.LastAt, &p.Name, &p.Kind); err != nil {
			return nil, fmt.Errorf("scan swarm peer traffic: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list swarm peer traffic: %w", err)
	}
	return out, nil
}

// ResolveSwarmPeers names keys that have no stored row yet — a counterparty this
// process has traded with since the last flush. Without it the panel would
// contradict the strip for up to one flush interval, listing nobody while the
// summary says two nodes are pulling; with it, such a row appears at once,
// carrying only its session bytes.
//
// Returns one entry per key given, in that order, with zeroed counters.
func (db *DB) ResolveSwarmPeers(ctx context.Context, keys []string) ([]SwarmPeerTraffic, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, strings.ToLower(strings.TrimSpace(k)))
	}
	// One row per key, as a values list, so the identity rule above can be
	// reused verbatim rather than restated against a different FROM.
	q := `
		SELECT k.key,` + swarmPeerIdentityCols + `
		FROM (SELECT ? AS key` + strings.Repeat(" UNION ALL SELECT ?", len(keys)-1) +
		`) k` + swarmPeerIdentityJoins
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve swarm peers: %w", err)
	}
	defer rows.Close()

	byKey := map[string]SwarmPeerTraffic{}
	for rows.Next() {
		var p SwarmPeerTraffic
		if err := rows.Scan(&p.Key, &p.Name, &p.Kind); err != nil {
			return nil, fmt.Errorf("scan resolved swarm peer: %w", err)
		}
		byKey[p.Key] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve swarm peers: %w", err)
	}
	out := make([]SwarmPeerTraffic, 0, len(keys))
	for _, a := range args {
		key := a.(string)
		if p, ok := byKey[key]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, SwarmPeerTraffic{Key: key, Kind: "gone"})
	}
	return out, nil
}

// ForgetSwarmPeerTraffic deletes the accounting rows for the given keys (the
// empty string being the bucket), returning how many went.
//
// The peer-side twin of ForgetSwarmTraffic, and deliberately independent of it:
// forgetting what a blob moved does not debit the nodes that moved it, and
// forgetting a node does not rewrite any blob's history. Charging one to the
// other would need the per-pair table this design refuses to keep.
func (db *DB) ForgetSwarmPeerTraffic(ctx context.Context, keys []string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, strings.ToLower(strings.TrimSpace(k)))
	}
	q := `DELETE FROM swarm_peer_traffic WHERE public_key IN (?` +
		strings.Repeat(",?", len(keys)-1) + `)`
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("forget swarm peer traffic: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("forget swarm peer traffic: %w", err)
	}
	return int(n), nil
}

// ForgetAllSwarmPeerTraffic drops every counterparty row. Its own method rather
// than an empty key list, so "forget everything" can never be what an empty
// selection accidentally means.
func (db *DB) ForgetAllSwarmPeerTraffic(ctx context.Context) (int, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM swarm_peer_traffic`)
	if err != nil {
		return 0, fmt.Errorf("forget all swarm peer traffic: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("forget all swarm peer traffic: %w", err)
	}
	return int(n), nil
}
