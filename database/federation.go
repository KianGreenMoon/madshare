package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"daemonlord.ygg/madshare/federation"
)

// The trust-group VIEW over federation_nodes (migration 046,
// docs/architecture/federation-nodes.md). *DB satisfies federation.PeerStore;
// the state machine itself lives in the federation package — this layer only
// persists rows.
//
// A "peer" is a node row whose trust_state is non-NULL: an admin acted
// ([federation.ExternalNode.IsTrusted]). Since the struct fold there is one Go
// type for the whole row and the queries here are the VIEW — they select the
// trust group and leave the rest zero, which is why a row scanned by this file
// must never be written back wholesale.

const peerColumns = `
	p.id, p.public_key, p.label, p.heard_name, p.trust_state, p.prev_state, p.guest_only,
	p.trusted_at, p.last_seen,
	p.block_reason, p.blocked_at`

// peerLabelExpr resolves a node's display name in SQL for the surfaces that
// only ever show one — the admin's local label if set, else what the node
// calls itself. One heard_name column since 046, so this chain is the whole
// rule. federation.ExternalNode.Name is the Go twin; keep the two in step.
func peerLabelExpr(alias string) string {
	return `COALESCE(NULLIF(` + alias + `.label, ''), ` + alias + `.heard_name)`
}

func scanPeer(row interface{ Scan(...any) error }) (*federation.ExternalNode, error) {
	var p federation.ExternalNode
	if err := row.Scan(&p.ID, &p.PublicKey, &p.Label, &p.HeardName, &p.TrustState, &p.PrevState,
		&p.GuestOnly, &p.TrustedAt, &p.LastSeen,
		&p.BlockReason, &p.BlockedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListFederationPeers returns every trusted node, friends first, then pending,
// then blocked, newest first within a state — the order the admin page shows.
func (db *DB) ListFederationPeers(ctx context.Context) ([]*federation.ExternalNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_nodes p
		WHERE p.trust_state IS NOT NULL
		ORDER BY CASE p.trust_state
			WHEN 'friend' THEN 0
			WHEN 'pending_incoming' THEN 1
			WHEN 'pending_outgoing' THEN 2
			ELSE 3 END, p.trusted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list federation peers: %w", err)
	}
	defer rows.Close()

	var out []*federation.ExternalNode
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan federation peer: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFederationPeer returns one trusted node by id, or
// federation.ErrPeerNotFound. A node the sweep merely caches from (no trust
// group) is NOT a peer, so it answers not-found here even though its row
// exists — the id space now spans all nodes, the peer API deliberately does
// not.
func (db *DB) GetFederationPeer(ctx context.Context, id int64) (*federation.ExternalNode, error) {
	p, err := scanPeer(db.QueryRowContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_nodes p
		WHERE p.id = ? AND p.trust_state IS NOT NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, federation.ErrPeerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get federation peer: %w", err)
	}
	return p, nil
}

// GetFederationPeerByKey returns one trusted node by its (lowercase hex)
// public key, or federation.ErrPeerNotFound.
func (db *DB) GetFederationPeerByKey(ctx context.Context, publicKey string) (*federation.ExternalNode, error) {
	p, err := scanPeer(db.QueryRowContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_nodes p
		WHERE p.public_key = ? AND p.trust_state IS NOT NULL`, publicKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, federation.ErrPeerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get federation peer by key: %w", err)
	}
	return p, nil
}

// InsertFederationPeer establishes the trust group for a key and returns the
// node's id. The key must be pre-normalized (federation.NormalizeKey).
//
// An UPSERT since 046: the node may already have a row — a member the sweep
// pulled from becomes a friend without losing its observations or its cached
// catalog. The guard refuses a key that already carries a trust state (the
// callers all check first; this is the race backstop the old UNIQUE
// constraint provided).
func (db *DB) InsertFederationPeer(ctx context.Context, p *federation.ExternalNode) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO federation_nodes
			(public_key, label, heard_name, trust_state, prev_state, guest_only,
			 trusted_at, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(public_key) DO UPDATE SET
			label       = excluded.label,
			heard_name  = CASE WHEN excluded.heard_name <> '' THEN excluded.heard_name
			                   ELSE federation_nodes.heard_name END,
			trust_state = excluded.trust_state,
			prev_state  = excluded.prev_state,
			guest_only  = excluded.guest_only,
			trusted_at  = excluded.trusted_at,
			first_seen  = CASE WHEN federation_nodes.first_seen = 0 THEN excluded.first_seen
			                   ELSE federation_nodes.first_seen END,
			last_seen   = MAX(federation_nodes.last_seen, excluded.last_seen)
		WHERE federation_nodes.trust_state IS NULL
		RETURNING id`,
		p.PublicKey, p.Label, p.HeardName, p.TrustState, p.PrevState, p.GuestOnly,
		p.TrustedAt, p.TrustedAt, p.LastSeen).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("insert federation peer: key already has a trust state")
	}
	if err != nil {
		return 0, fmt.Errorf("insert federation peer: %w", err)
	}
	return id, nil
}

// SetFederationPeerState moves a peer to state, recording prevState (what an
// unblock returns to; empty for normal transitions).
func (db *DB) SetFederationPeerState(ctx context.Context, id int64, state, prevState string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE federation_nodes SET trust_state = ?, prev_state = ?
		 WHERE id = ? AND trust_state IS NOT NULL`, state, prevState, id)
	if err != nil {
		return fmt.Errorf("update federation peer state: %w", err)
	}
	return requirePeerRow(res)
}

// BlockFederationPeer blocks a peer and records what the published distrust
// mark will say: when, and why (F6). prevState is what an unblock returns to.
//
// Separate from SetFederationPeerState because a block is the one transition
// that carries evidence — every block becomes a mark the whole network reads,
// so the reason is part of the operation rather than an afterthought.
func (db *DB) BlockFederationPeer(ctx context.Context, id int64, prevState, reason string, at int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE federation_nodes
		   SET trust_state = ?, prev_state = ?, block_reason = ?, blocked_at = ?
		 WHERE id = ? AND trust_state IS NOT NULL`,
		federation.PeerBlocked, prevState, reason, at, id)
	if err != nil {
		return fmt.Errorf("block federation peer: %w", err)
	}
	return requirePeerRow(res)
}

// UpdateFederationPeerName renames a peer (local label only). Empty clears the
// label, so display falls back to what the peer calls itself.
func (db *DB) UpdateFederationPeerName(ctx context.Context, id int64, name string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE federation_nodes SET label = ?
		 WHERE id = ? AND trust_state IS NOT NULL`, name, id)
	if err != nil {
		return fmt.Errorf("update federation peer name: %w", err)
	}
	return requirePeerRow(res)
}

// UpdateFederationPeerHeardName records what the peer calls itself, learned
// from a ping or pairing reply. It deliberately cannot touch the local label:
// a peer renaming itself must never overwrite an admin's choice.
func (db *DB) UpdateFederationPeerHeardName(ctx context.Context, id int64, name string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE federation_nodes SET heard_name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("update federation node heard_name: %w", err)
	}
	return requirePeerRow(res)
}

// SetFederationPeerGuestOnly sets or clears the admin's per-peer demotion
// (federation.ExternalNode.GuestOnly).
func (db *DB) SetFederationPeerGuestOnly(ctx context.Context, id int64, guestOnly bool) error {
	res, err := db.ExecContext(ctx, `
		UPDATE federation_nodes SET guest_only = ?
		 WHERE id = ? AND trust_state IS NOT NULL`, guestOnly, id)
	if err != nil {
		return fmt.Errorf("update federation peer guest_only: %w", err)
	}
	return requirePeerRow(res)
}

// TouchFederationPeerSeen records mesh contact with a node. Monotonic: an
// out-of-order touch never moves last_seen backwards. One clock since 046 —
// this is the same column every other observation path updates.
func (db *DB) TouchFederationPeerSeen(ctx context.Context, id int64, when int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE federation_nodes SET last_seen = ? WHERE id = ? AND last_seen < ?`, when, id, when)
	if err != nil {
		return fmt.Errorf("touch federation node: %w", err)
	}
	return nil
}

// DeleteFederationPeer withdraws the admin's trust decision: the trust group
// is CLEARED, not the row — the node's observations, cached catalog and
// traffic history all outlive the relationship (federation-nodes.md property
// 3). A row still in the pull rotation stays for the sweep's retention to
// judge; a row nothing else claims goes with the group, since no sweep would
// ever visit it again.
func (db *DB) DeleteFederationPeer(ctx context.Context, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete federation peer: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE federation_nodes
		   SET trust_state = NULL, prev_state = '', label = '', guest_only = 0,
		       trusted_at = 0, block_reason = '', blocked_at = 0
		 WHERE id = ? AND trust_state IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("delete federation peer: %w", err)
	}
	if err := requirePeerRow(res); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM federation_nodes
		 WHERE id = ? AND trust_state IS NULL AND home_added_at = 0 AND sync_added_at = 0`, id); err != nil {
		return fmt.Errorf("delete federation node row: %w", err)
	}
	return tx.Commit()
}

func requirePeerRow(res sql.Result) error {
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return federation.ErrPeerNotFound
	}
	return nil
}
