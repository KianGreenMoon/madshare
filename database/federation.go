package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"daemonlord.ygg/madshare/federation"
)

// Trusted-peer table (federation F1, migration 026). *DB satisfies
// federation.PeerStore; the state machine itself lives in the federation
// package — this layer only persists rows.

const peerColumns = `
	p.id, p.public_key, p.name, p.heard_name, p.state, p.prev_state, p.user_id,
	p.created_at, p.last_seen,
	p.block_reason, p.blocked_at,
	COALESCE(u.username, '')`

// peerLabelExpr resolves a peer's display name in SQL for the surfaces that only
// ever show one — the admin's local label if set, else what the peer calls itself
// (migration 033). federation.Peer.Label is the Go twin; keep the two in step.
func peerLabelExpr(alias string) string {
	return `COALESCE(NULLIF(` + alias + `.name, ''), ` + alias + `.heard_name)`
}

func scanPeer(row interface{ Scan(...any) error }) (*federation.Peer, error) {
	var p federation.Peer
	var userID sql.NullInt64
	if err := row.Scan(&p.ID, &p.PublicKey, &p.Name, &p.HeardName, &p.State, &p.PrevState,
		&userID, &p.CreatedAt, &p.LastSeen,
		&p.BlockReason, &p.BlockedAt, &p.Username); err != nil {
		return nil, err
	}
	if userID.Valid {
		p.UserID = &userID.Int64
	}
	return &p, nil
}

// ListFederationPeers returns every peer, friends first, then pending, then
// blocked, newest first within a state — the order the admin page shows.
func (db *DB) ListFederationPeers(ctx context.Context) ([]*federation.Peer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_peers p
		LEFT JOIN users u ON u.id = p.user_id
		ORDER BY CASE p.state
			WHEN 'friend' THEN 0
			WHEN 'pending_incoming' THEN 1
			WHEN 'pending_outgoing' THEN 2
			ELSE 3 END, p.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list federation_peers: %w", err)
	}
	defer rows.Close()

	var out []*federation.Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan federation_peers: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFederationPeer returns one peer by id, or federation.ErrPeerNotFound.
func (db *DB) GetFederationPeer(ctx context.Context, id int64) (*federation.Peer, error) {
	p, err := scanPeer(db.QueryRowContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_peers p
		LEFT JOIN users u ON u.id = p.user_id
		WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, federation.ErrPeerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get federation_peers: %w", err)
	}
	return p, nil
}

// GetFederationPeerByKey returns one peer by its (lowercase hex) public key, or
// federation.ErrPeerNotFound.
func (db *DB) GetFederationPeerByKey(ctx context.Context, publicKey string) (*federation.Peer, error) {
	p, err := scanPeer(db.QueryRowContext(ctx, `
		SELECT `+peerColumns+`
		FROM federation_peers p
		LEFT JOIN users u ON u.id = p.user_id
		WHERE p.public_key = ?`, publicKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, federation.ErrPeerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get federation_peers by key: %w", err)
	}
	return p, nil
}

// InsertFederationPeer persists a new peer and returns its id. The key must be
// pre-normalized (federation.NormalizeKey); a duplicate key errors via the
// UNIQUE constraint.
func (db *DB) InsertFederationPeer(ctx context.Context, p *federation.Peer) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO federation_peers (public_key, name, heard_name, state, prev_state, user_id, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PublicKey, p.Name, p.HeardName, p.State, p.PrevState, nullableID(p.UserID), p.CreatedAt, p.LastSeen)
	if err != nil {
		return 0, fmt.Errorf("insert federation_peers: %w", err)
	}
	return res.LastInsertId()
}

// SetFederationPeerState moves a peer to state, recording prevState (what an
// unblock returns to; empty for normal transitions).
func (db *DB) SetFederationPeerState(ctx context.Context, id int64, state, prevState string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE federation_peers SET state = ?, prev_state = ? WHERE id = ?`, state, prevState, id)
	if err != nil {
		return fmt.Errorf("update federation_peers state: %w", err)
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
	res, err := db.ExecContext(ctx,
		`UPDATE federation_peers
		    SET state = ?, prev_state = ?, block_reason = ?, blocked_at = ?
		  WHERE id = ?`,
		federation.PeerBlocked, prevState, reason, at, id)
	if err != nil {
		return fmt.Errorf("block federation peer: %w", err)
	}
	return requirePeerRow(res)
}

// UpdateFederationPeerName renames a peer (local label only). Empty clears the
// label, so display falls back to what the peer calls itself.
func (db *DB) UpdateFederationPeerName(ctx context.Context, id int64, name string) error {
	res, err := db.ExecContext(ctx, `UPDATE federation_peers SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("update federation_peers name: %w", err)
	}
	return requirePeerRow(res)
}

// UpdateFederationPeerHeardName records what the peer calls itself, learned from
// a ping or pairing reply (migration 033). It deliberately cannot touch the local
// label: a peer renaming itself must never overwrite an admin's choice, which is
// what the single pre-033 column did.
func (db *DB) UpdateFederationPeerHeardName(ctx context.Context, id int64, name string) error {
	res, err := db.ExecContext(ctx, `UPDATE federation_peers SET heard_name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("update federation_peers heard_name: %w", err)
	}
	return requirePeerRow(res)
}

// SetFederationPeerUser maps the peer node to a local user account (nil clears
// the mapping).
func (db *DB) SetFederationPeerUser(ctx context.Context, id int64, userID *int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE federation_peers SET user_id = ? WHERE id = ?`, nullableID(userID), id)
	if err != nil {
		return fmt.Errorf("update federation_peers user: %w", err)
	}
	return requirePeerRow(res)
}

// TouchFederationPeerSeen records mesh contact with a peer. Monotonic: an
// out-of-order touch never moves last_seen backwards.
func (db *DB) TouchFederationPeerSeen(ctx context.Context, id int64, when int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE federation_peers SET last_seen = ? WHERE id = ? AND last_seen < ?`, when, id, when)
	if err != nil {
		return fmt.Errorf("touch federation_peers: %w", err)
	}
	return nil
}

// DeleteFederationPeer removes the peer row entirely.
func (db *DB) DeleteFederationPeer(ctx context.Context, id int64) error {
	res, err := db.ExecContext(ctx, `DELETE FROM federation_peers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete federation_peers: %w", err)
	}
	return requirePeerRow(res)
}

func nullableID(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

func requirePeerRow(res sql.Result) error {
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return federation.ErrPeerNotFound
	}
	return nil
}
