package database

import (
	"context"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// The household VIEW over federation_nodes (migrations 044→046,
// docs/architecture/federation-access.md §"The household") — the servers a
// listener node signs in to, recorded one-sidedly. `home_added_at > 0` marks
// the group present.
//
// It is a trust record rather than a relationship: the trust group has a
// state machine, a block reason, a user mapping and a gossip edge because a
// peering is negotiated by two admins; this is one device deciding, by
// itself, whose word it will take about who a stranger is. Holding it on the
// same ROW as everything else known about that key publishes nothing — only
// gossip publishes edges (federation-nodes.md property 2).

// AddHomeNode records a server this node signs in to, or refreshes what is
// known about one already recorded.
//
// Idempotent on the key, because signing in again is the ordinary case — a token
// is renewed hourly and a client that re-derives its home list on every launch
// must not accumulate rows. The key is the identity; base_url and the name are
// display facts that may legitimately change under it (a server moves, or is
// renamed), so they are refreshed while home_added_at is not. The name lands in
// heard_name — it is the server's own claim, the same fact a ping learns — and
// an empty claim leaves a known name alone.
func (db *DB) AddHomeNode(ctx context.Context, publicKey, baseURL, name string, at int64) error {
	key, err := federation.NormalizeKey(publicKey)
	if err != nil {
		return fmt.Errorf("add home node: %w", err)
	}
	heard := strings.TrimSpace(name)
	_, err = db.ExecContext(ctx, `
		INSERT INTO federation_nodes (public_key, heard_name, first_seen, home_added_at, home_base_url)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(public_key) DO UPDATE SET
			home_added_at = CASE WHEN federation_nodes.home_added_at = 0 THEN excluded.home_added_at
			                     ELSE federation_nodes.home_added_at END,
			home_base_url = excluded.home_base_url,
			heard_name    = CASE WHEN excluded.heard_name <> '' THEN excluded.heard_name
			                     ELSE federation_nodes.heard_name END,
			first_seen    = CASE WHEN federation_nodes.first_seen = 0 THEN excluded.first_seen
			                     ELSE federation_nodes.first_seen END`,
		key, heard, at, at, strings.TrimSpace(baseURL))
	if err != nil {
		return fmt.Errorf("add home node: %w", err)
	}
	return nil
}

// ListHomeNodes returns every home server, oldest first. Empty on a server,
// which signs in to nothing.
func (db *DB) ListHomeNodes(ctx context.Context) ([]federation.HomeNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT public_key, home_base_url, heard_name, home_added_at
		FROM federation_nodes
		WHERE home_added_at > 0
		ORDER BY home_added_at, public_key`)
	if err != nil {
		return nil, fmt.Errorf("list home nodes: %w", err)
	}
	defer rows.Close()

	var out []federation.HomeNode
	for rows.Next() {
		var h federation.HomeNode
		if err := rows.Scan(&h.PublicKey, &h.BaseURL, &h.Name, &h.AddedAt); err != nil {
			return nil, fmt.Errorf("scan home node: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RemoveHomeNode forgets a home server: signing out stops us serving its
// devices, on the next request rather than on a timer. The group is cleared;
// the row goes only when nothing else — a trust decision, the pull rotation —
// still claims it.
//
// Clearing a group that is not set is not an error. A client signing out of a
// server it never enrolled with is doing exactly the right thing, and the state
// it is asking for is the state it gets.
func (db *DB) RemoveHomeNode(ctx context.Context, publicKey string) error {
	key, err := federation.NormalizeKey(publicKey)
	if err != nil {
		return fmt.Errorf("remove home node: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove home node: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_nodes SET home_added_at = 0, home_base_url = ''
		 WHERE public_key = ?`, key); err != nil {
		return fmt.Errorf("remove home node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM federation_nodes
		 WHERE public_key = ? AND trust_state IS NULL AND home_added_at = 0 AND sync_added_at = 0`, key); err != nil {
		return fmt.Errorf("remove home node row: %w", err)
	}
	return tx.Commit()
}
