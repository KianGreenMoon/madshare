package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Per-blob swarm traffic accounting (docs/architecture/swarm-admin.md).
//
// One row per content hash this node has ever moved, in either direction. The
// rows are written from exactly one place — api's flusher, draining the running
// node's in-memory counters — and every write is an increment, never an
// assignment. Nothing on the transfer path touches this package, which is what
// keeps fetching and seeding database-free.
//
// The node's all-time totals are SUM() over this table rather than counters kept
// beside it: two stores of one number eventually disagree.

// SwarmTraffic is one hash's all-time byte accounting.
type SwarmTraffic struct {
	Hash string `json:"hash"`
	// Up is what this node has served to the mesh, Down what it has pulled off
	// it, and Wasted the part of Down that was thrown away (a chunk that failed
	// verification, an abandoned attempt).
	Up      int64 `json:"up_bytes"`
	Down    int64 `json:"down_bytes"`
	Wasted  int64 `json:"wasted_bytes"`
	FirstAt int64 `json:"first_at"`
	LastAt  int64 `json:"last_at"`
}

// SwarmTrafficDelta is one hash's increment in a single flush. Deltas are
// additive by construction, so a flush that is retried after a partial failure
// can only over-count what the *next* drain would have carried anyway — which is
// why the flusher drains and commits rather than reading and writing back.
type SwarmTrafficDelta struct {
	Hash   string
	Up     int64
	Down   int64
	Wasted int64
}

// AddSwarmTraffic folds a batch of deltas into the table in one transaction,
// stamping last_at (and first_at for a hash never seen before) with at.
//
// Zero-valued deltas are skipped rather than written: a drain that found nothing
// for a hash must not move its clock, or a blob nobody has touched for a month
// would look freshly active.
func (db *DB) AddSwarmTraffic(ctx context.Context, deltas []SwarmTrafficDelta, at int64) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add swarm traffic: %w", err)
	}
	defer tx.Rollback()
	const q = `
		INSERT INTO swarm_traffic (hash, up_bytes, down_bytes, wasted_bytes, first_at, last_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			up_bytes     = up_bytes + excluded.up_bytes,
			down_bytes   = down_bytes + excluded.down_bytes,
			wasted_bytes = wasted_bytes + excluded.wasted_bytes,
			last_at      = excluded.last_at`
	for _, d := range deltas {
		if d.Hash == "" || (d.Up == 0 && d.Down == 0 && d.Wasted == 0) {
			continue
		}
		if _, err := tx.ExecContext(ctx, q, d.Hash, d.Up, d.Down, d.Wasted, at, at); err != nil {
			return fmt.Errorf("add swarm traffic %s: %w", d.Hash, err)
		}
	}
	return tx.Commit()
}

// SwarmTrafficTotals is this node's all-time contribution: the sum over every
// blob it has ever moved. Indexed aggregate, so the summary strip costs one
// query no matter how many hashes have rows.
func (db *DB) SwarmTrafficTotals(ctx context.Context) (SwarmTraffic, error) {
	var t SwarmTraffic
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(up_bytes), 0), COALESCE(SUM(down_bytes), 0),
		       COALESCE(SUM(wasted_bytes), 0)
		FROM swarm_traffic`).Scan(&t.Up, &t.Down, &t.Wasted)
	if err != nil {
		return SwarmTraffic{}, fmt.Errorf("swarm traffic totals: %w", err)
	}
	return t, nil
}

// GetSwarmTraffic returns one hash's row, or nil when it has never moved.
func (db *DB) GetSwarmTraffic(ctx context.Context, hash string) (*SwarmTraffic, error) {
	t := &SwarmTraffic{Hash: hash}
	err := db.QueryRowContext(ctx, `
		SELECT up_bytes, down_bytes, wasted_bytes, first_at, last_at
		FROM swarm_traffic WHERE hash = ?`, hash).
		Scan(&t.Up, &t.Down, &t.Wasted, &t.FirstAt, &t.LastAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get swarm traffic %s: %w", hash, err)
	}
	return t, nil
}

// ForgetSwarmTraffic deletes the accounting rows for the given hashes, returning
// how many went.
//
// The only thing that deletes here. Removing a cached blob or a library file
// deliberately does NOT: the bytes really moved, and a node's contribution
// history must not be erasable as a side effect of housekeeping. Forgetting is
// therefore an explicit act, and the UI says what it costs — these bytes leave
// the node's all-time totals with them, because those totals are this table.
func (db *DB) ForgetSwarmTraffic(ctx context.Context, hashes []string) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(hashes))
	for _, h := range hashes {
		args = append(args, h)
	}
	q := `DELETE FROM swarm_traffic WHERE hash IN (?` +
		strings.Repeat(",?", len(hashes)-1) + `)`
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("forget swarm traffic: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("forget swarm traffic: %w", err)
	}
	return int(n), nil
}
