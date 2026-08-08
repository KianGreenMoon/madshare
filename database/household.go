package database

import (
	"context"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Home nodes (migration 044, docs/architecture/federation.md §"The household") —
// the servers a listener node signs in to, recorded one-sidedly.
//
// The whole table is four columns and three queries, and that is the point: it
// is a trust record rather than a relationship. federation_peers has a state
// machine, a block reason, a user mapping and a gossip edge because a peering is
// negotiated by two admins; this is one device deciding, by itself, whose word
// it will take about who a stranger is.

// AddHomeNode records a server this node signs in to, or refreshes what is known
// about one already recorded.
//
// Idempotent on the key, because signing in again is the ordinary case — a token
// is renewed hourly and a client that re-derives its home list on every launch
// must not accumulate rows. The key is the identity; base_url and name are
// display facts that may legitimately change under it (a server moves, or is
// renamed), so they are overwritten while added_at is not.
func (db *DB) AddHomeNode(ctx context.Context, publicKey, baseURL, name string, at int64) error {
	key, err := federation.NormalizeKey(publicKey)
	if err != nil {
		return fmt.Errorf("add home node: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO federation_home_nodes (public_key, base_url, name, added_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(public_key) DO UPDATE SET
			base_url = excluded.base_url,
			name     = excluded.name`,
		key, strings.TrimSpace(baseURL), strings.TrimSpace(name), at)
	if err != nil {
		return fmt.Errorf("insert federation_home_nodes: %w", err)
	}
	return nil
}

// ListHomeNodes returns every home server, oldest first. Empty on a server,
// which signs in to nothing.
func (db *DB) ListHomeNodes(ctx context.Context) ([]federation.HomeNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT public_key, base_url, name, added_at
		FROM federation_home_nodes
		ORDER BY added_at, public_key`)
	if err != nil {
		return nil, fmt.Errorf("list federation_home_nodes: %w", err)
	}
	defer rows.Close()

	var out []federation.HomeNode
	for rows.Next() {
		var h federation.HomeNode
		if err := rows.Scan(&h.PublicKey, &h.BaseURL, &h.Name, &h.AddedAt); err != nil {
			return nil, fmt.Errorf("scan federation_home_nodes: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RemoveHomeNode forgets a home server: signing out stops us serving its
// devices, on the next request rather than on a timer.
//
// Deleting a row that is not there is not an error. A client signing out of a
// server it never enrolled with is doing exactly the right thing, and the state
// it is asking for is the state it gets.
func (db *DB) RemoveHomeNode(ctx context.Context, publicKey string) error {
	key, err := federation.NormalizeKey(publicKey)
	if err != nil {
		return fmt.Errorf("remove home node: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM federation_home_nodes WHERE public_key = ?`, key); err != nil {
		return fmt.Errorf("delete federation_home_nodes: %w", err)
	}
	return nil
}
