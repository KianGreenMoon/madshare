package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// The sync-group VIEW over federation_nodes (migration 046,
// docs/architecture/federation-nodes.md): the nodes this node pulls a cached
// catalog from. `sync_added_at > 0` marks a row as IN THE PULL ROTATION — the
// fact the pre-046 schema encoded as row existence in
// federation_catalog_sources — so a pending peer or a household home never
// joins the rotation by accident.
//
// Everything here is cache management: mark a node for pulling on first
// contact, record attempts and successes so the rotation is fair and the
// freshness window has something to read, and clear what we may no longer
// keep. The cached satellites (catalog, holdings, claim reports) key on the
// public key and are removed with the sync group; the node ROW survives while
// any other group (trust, household) still claims it.

const sourceColumns = `id, public_key, heard_name, catalog_serial, catalog_synced_at,
	attempted_at, first_seen, last_seen, hinted_at`

func scanSource(row interface{ Scan(...any) error }) (*federation.ExternalNode, error) {
	var s federation.ExternalNode
	if err := row.Scan(&s.ID, &s.PublicKey, &s.HeardName, &s.CatalogSerial,
		&s.CatalogSyncedAt, &s.AttemptedAt, &s.FirstSeen, &s.LastSeen, &s.HintedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// EnsureCatalogSource returns the sync view of a node key, adding the node to
// the pull rotation on first contact. The key is the identity, so the insert
// is an upsert on it: two sweeps racing to reach the same node must not
// produce two caches of it — and since 046 a node the admin already trusts
// keeps its row, id and observations when the sweep starts pulling from it.
func (db *DB) EnsureCatalogSource(ctx context.Context, publicKey string, now int64) (*federation.ExternalNode, error) {
	// The strict 64-hex check lives where a key ENTERS from outside — the pull-now
	// request, a card import — rather than here, which is called with keys the
	// store already holds. This guards only against creating a row nothing could
	// ever dial.
	key := strings.ToLower(strings.TrimSpace(publicKey))
	if key == "" {
		return nil, fmt.Errorf("ensure catalog source: empty key")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO federation_nodes (public_key, first_seen, sync_added_at)
		VALUES (?, ?, ?)
		ON CONFLICT (public_key) DO UPDATE SET
			sync_added_at = CASE WHEN federation_nodes.sync_added_at = 0 THEN excluded.sync_added_at
			                     ELSE federation_nodes.sync_added_at END,
			first_seen    = CASE WHEN federation_nodes.first_seen = 0 THEN excluded.first_seen
			                     ELSE federation_nodes.first_seen END`,
		key, now, now); err != nil {
		return nil, fmt.Errorf("ensure catalog source: %w", err)
	}
	src, err := scanSource(db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_nodes WHERE public_key = ?`, key))
	if err != nil {
		return nil, fmt.Errorf("read catalog source: %w", err)
	}
	return src, nil
}

// GetCatalogSource returns one pull-rotation node by key, or nil when we cache
// nothing from that node (a trust-only or household-only row answers nil too —
// existence of the ROW no longer means membership in the rotation).
func (db *DB) GetCatalogSource(ctx context.Context, publicKey string) (*federation.ExternalNode, error) {
	src, err := scanSource(db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_nodes
		  WHERE public_key = ? AND sync_added_at > 0`,
		strings.ToLower(strings.TrimSpace(publicKey))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get catalog source: %w", err)
	}
	return src, nil
}

// ListCatalogSources returns every pull-rotation node, least-recently-attempted
// first — which is the order the frontier rotation wants, so it can take the
// head of the list and be fair by construction.
func (db *DB) ListCatalogSources(ctx context.Context) ([]*federation.ExternalNode, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM federation_nodes
		  WHERE sync_added_at > 0 ORDER BY attempted_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list catalog sources: %w", err)
	}
	defer rows.Close()
	var out []*federation.ExternalNode
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
		`UPDATE federation_nodes SET attempted_at = ? WHERE id = ?`, at, id); err != nil {
		return fmt.Errorf("mark catalog source attempted: %w", err)
	}
	return nil
}

// TouchCatalogSourceSeen records a successful contact, and what the node called
// itself when it said so. last_seen only moves forward (an out-of-order write
// from a concurrent transfer must not age a node), and an empty name leaves the
// stored one alone. One clock and one heard_name since 046 — this writes the
// same columns the friendship ping does.
func (db *DB) TouchCatalogSourceSeen(ctx context.Context, id int64, at int64, heardName string) error {
	name := federation.CleanPeerName(heardName)
	if _, err := db.ExecContext(ctx, `
		UPDATE federation_nodes
		   SET last_seen  = MAX(last_seen, ?),
		       heard_name = CASE WHEN ? = '' THEN heard_name ELSE ? END
		 WHERE id = ?`, at, name, name, id); err != nil {
		return fmt.Errorf("touch catalog source: %w", err)
	}
	return nil
}

// MarkNodeUnreachable records a first-hand connect-class failure against a node
// (migration 048; docs/architecture/federation.md §Availability, "Reactive
// down-mark + the ping floor"). Forward-only, like every observation on this
// row: two transfers failing against the same holder in either order must land
// on the later moment, not the one that happened to commit last.
//
// Keyed by public key rather than by row id because most of the callers are on
// the transfer path, where a holder frequently has no source row of ours at all
// — and the key is the identity that never recycles. A node we hold no row for
// updates nothing, which is the correct no-op: the mark exists to hide cached
// catalog entries, and there are none.
//
// Nothing clears it. The browse reads it against last_seen, so the next
// successful contact retires the mark by moving the other clock past it.
func (db *DB) MarkNodeUnreachable(ctx context.Context, publicKey string, at int64) error {
	if publicKey == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE federation_nodes
		   SET unreachable_at = MAX(unreachable_at, ?)
		 WHERE public_key = ?`, at, publicKey); err != nil {
		return fmt.Errorf("mark node unreachable: %w", err)
	}
	return nil
}

// ApplyFreshnessHints records liveness a friend observed first-hand for nodes we
// only ever pull from (F7 item 10, docs/architecture/federation.md §Availability,
// "Two clocks, two windows"). seen maps a node key to the unix time that friend
// last touched it; at is when we heard the claim.
//
// The two columns say different things and both are needed. last_seen answers
// "when was this node last known alive", whoever observed it, so a hint writes it
// like a ping does — monotonically, since hints from two friends arrive in no
// particular order and the older one must not age the node. hinted_at answers
// "is a minute-cadence observer watching this node", which is what the browse
// consults to pick a window: fold the two together and a hinted node that dies
// would drop back to the 45-minute pull window exactly when its friends going
// quiet is the news.
//
// The UPDATE is scoped to pull-rotation rows, so a hint about a node we hold no
// catalog from silently matches nothing. That is the intended shape rather than
// a tolerated one: the sync group means the sweep pulled from that node, and
// hearsay must not be able to mint it.
func (db *DB) ApplyFreshnessHints(ctx context.Context, seen map[string]int64, at int64) (int, error) {
	if len(seen) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("apply freshness hints: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE federation_nodes
		   SET last_seen = MAX(last_seen, ?),
		       hinted_at = MAX(hinted_at, ?)
		 WHERE public_key = ? AND sync_added_at > 0`)
	if err != nil {
		return 0, fmt.Errorf("prepare freshness hint: %w", err)
	}
	defer stmt.Close()
	moved := 0
	for key, when := range seen {
		res, err := stmt.ExecContext(ctx, when, at, strings.ToLower(strings.TrimSpace(key)))
		if err != nil {
			return 0, fmt.Errorf("apply freshness hint: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			moved += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit freshness hints: %w", err)
	}
	return moved, nil
}

// DropCatalogSources takes nodes out of the pull rotation and deletes every
// catalog row, holding and claim report cached from them. The node ROW is
// deleted only when no other group claims it — an admin's trust decision and a
// household enrolment both outlive the cache (federation-nodes.md property 5),
// as do the observations they carry.
//
// An empty list is a no-op — the same refusal DropUnreachableGraph makes, for
// the same reason: "drop nothing" and "drop everything" must never be one call.
func (db *DB) DropCatalogSources(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("drop catalog sources: %w", err)
	}
	defer tx.Rollback()
	for _, id := range ids {
		// The satellites key on the public key and must go with the sync group
		// even when the row itself stays (CASCADE only covers row deletion).
		for _, q := range []string{
			`DELETE FROM federation_catalog WHERE node_key = (SELECT public_key FROM federation_nodes WHERE id = ?)`,
			`DELETE FROM federation_holdings WHERE node_key = (SELECT public_key FROM federation_nodes WHERE id = ?)`,
			`DELETE FROM federation_claim_reports WHERE node_key = (SELECT public_key FROM federation_nodes WHERE id = ?)`,
			`UPDATE federation_nodes
			    SET sync_added_at = 0, catalog_serial = '', catalog_synced_at = 0, attempted_at = 0
			  WHERE id = ?`,
			`DELETE FROM federation_nodes
			  WHERE id = ? AND trust_state IS NULL AND home_added_at = 0`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("drop catalog source %d: %w", id, err)
			}
		}
	}
	return tx.Commit()
}
