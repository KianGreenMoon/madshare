package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Federation F7 item 5 — catalog sources (docs/architecture/federation.md
// §Discovery beyond the friend ring): the nodes this node holds a cached
// catalog from. Migration 036 split them out of federation_peers, because the
// two tables answer different questions — a peer row says an admin decided
// something, a source row says the sweep pulled from it — and the community is
// full of nodes only the second is true of.
//
// Everything here is cache management: create a row when we first pull, record
// attempts and successes so the rotation is fair and the freshness window has
// something to read, and drop rows we may no longer keep. Nothing local
// references a source, so dropping one is always safe.

const sourceColumns = `id, public_key, heard_name, catalog_serial, catalog_synced_at,
	attempted_at, first_seen, last_seen`

func scanSource(row interface{ Scan(...any) error }) (*federation.CatalogSource, error) {
	var s federation.CatalogSource
	if err := row.Scan(&s.ID, &s.PublicKey, &s.HeardName, &s.CatalogSerial,
		&s.CatalogSyncedAt, &s.AttemptedAt, &s.FirstSeen, &s.LastSeen); err != nil {
		return nil, err
	}
	return &s, nil
}

// EnsureCatalogSource returns the source row for a node key, creating it on
// first contact. The key is the identity, so the insert is an upsert on it: two
// sweeps racing to reach the same node must not produce two caches of it.
func (db *DB) EnsureCatalogSource(ctx context.Context, publicKey string, now int64) (*federation.CatalogSource, error) {
	// The strict 64-hex check lives where a key ENTERS from outside — the pull-now
	// request, a card import — rather than here, which is called with keys the
	// store already holds. This guards only against creating a row nothing could
	// ever dial.
	key := strings.ToLower(strings.TrimSpace(publicKey))
	if key == "" {
		return nil, fmt.Errorf("ensure catalog source: empty key")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO federation_catalog_sources (public_key, first_seen)
		VALUES (?, ?)
		ON CONFLICT (public_key) DO NOTHING`, key, now); err != nil {
		return nil, fmt.Errorf("ensure catalog source: %w", err)
	}
	src, err := scanSource(db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_catalog_sources WHERE public_key = ?`, key))
	if err != nil {
		return nil, fmt.Errorf("read catalog source: %w", err)
	}
	return src, nil
}

// GetCatalogSource returns one source by key, or nil when we cache nothing from
// that node.
func (db *DB) GetCatalogSource(ctx context.Context, publicKey string) (*federation.CatalogSource, error) {
	src, err := scanSource(db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_catalog_sources WHERE public_key = ?`,
		strings.ToLower(strings.TrimSpace(publicKey))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get catalog source: %w", err)
	}
	return src, nil
}

// ListCatalogSources returns every source, least-recently-attempted first —
// which is the order the frontier rotation wants, so it can take the head of
// the list and be fair by construction.
func (db *DB) ListCatalogSources(ctx context.Context) ([]*federation.CatalogSource, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_catalog_sources ORDER BY attempted_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list catalog sources: %w", err)
	}
	defer rows.Close()
	var out []*federation.CatalogSource
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan catalog source: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkCatalogSourceAttempted records that we tried to pull from this source,
// successfully or not. The rotation reads this and not last_seen: a node that
// never answers must still lose its turn, or it would be retried ahead of every
// other member forever.
func (db *DB) MarkCatalogSourceAttempted(ctx context.Context, id int64, at int64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE federation_catalog_sources SET attempted_at = ? WHERE id = ?`, at, id); err != nil {
		return fmt.Errorf("mark catalog source attempted: %w", err)
	}
	return nil
}

// TouchCatalogSourceSeen records a successful contact, and what the node called
// itself when it said so. last_seen only moves forward (an out-of-order write
// from a concurrent transfer must not age a node), and an empty name leaves the
// stored one alone.
func (db *DB) TouchCatalogSourceSeen(ctx context.Context, id int64, at int64, heardName string) error {
	name := federation.CleanPeerName(heardName)
	if _, err := db.ExecContext(ctx, `
		UPDATE federation_catalog_sources
		   SET last_seen  = MAX(last_seen, ?),
		       heard_name = CASE WHEN ? = '' THEN heard_name ELSE ? END
		 WHERE id = ?`, at, name, name, id); err != nil {
		return fmt.Errorf("touch catalog source: %w", err)
	}
	return nil
}

// DropCatalogSources deletes sources and, by CASCADE, every catalog row,
// holding and claim report cached from them. An empty list is a no-op — the
// same refusal DropUnreachableGraph makes, for the same reason: "drop nothing"
// and "drop everything" must never be one call.
func (db *DB) DropCatalogSources(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("drop catalog sources: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM federation_catalog_sources WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare drop catalog source: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("drop catalog source %d: %w", id, err)
		}
	}
	return tx.Commit()
}
