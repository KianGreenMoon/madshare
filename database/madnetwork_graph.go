package database

// The gossiped network graph (federation F6): the signed records this node
// holds and the denormalized edges/marks queried off them. *DB satisfies
// federation.GraphStore here. Design: docs/architecture/federation.md
// §"Friend-list gossip & the network graph"; the record format and its
// verification live in federation/gossip.go.
//
// Two invariants run through this file:
//
//   - The payload column is the record VERBATIM. Nothing here re-encodes it,
//     because the author's signature covers the bytes as they were written and
//     a record may carry fields this build cannot parse.
//   - The edges/marks tables are derived, rewritten inside the same transaction
//     that replaces their record, so a reader never sees one origin's edges
//     half-updated.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// PutGraphRecord stores a verified friend-list record if it is newer than what
// we hold for that origin, rewriting the origin's edges to match.
//
// The seq comparison is the whole loop-termination story: a record already seen
// returns false and the caller relays nothing, so a cycle in the friendship
// graph cannot circulate a document forever. It is also what makes a sync round
// idempotent — receiving the same record from three friends costs three
// comparisons and one write.
func (db *DB) PutGraphRecord(ctx context.Context, rec *federation.GraphRecord, payload []byte, receivedFrom *int64, expiresAt, now int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("put graph record: %w", err)
	}
	defer tx.Rollback()

	newer, err := seqIsNewer(ctx, tx, `federation_graph_records`, rec.Origin, rec.Seq)
	if err != nil || !newer {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO federation_graph_records
		        (origin, seq, issued_at, expires_at, payload, received_from, stored_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(origin) DO UPDATE SET
		        seq = excluded.seq, issued_at = excluded.issued_at,
		        expires_at = excluded.expires_at, payload = excluded.payload,
		        received_from = excluded.received_from, stored_at = excluded.stored_at`,
		rec.Origin, rec.Seq, rec.IssuedAt, expiresAt, string(payload), receivedFrom, now,
	); err != nil {
		return false, fmt.Errorf("store graph record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_graph_edges WHERE origin = ?`, rec.Origin); err != nil {
		return false, fmt.Errorf("clear graph edges: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO federation_graph_edges (origin, peer, name, since) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return false, fmt.Errorf("prepare graph edge insert: %w", err)
	}
	defer ins.Close()
	for _, e := range rec.Friends {
		if _, err := ins.ExecContext(ctx, rec.Origin, e.Key, e.Name, e.Since); err != nil {
			return false, fmt.Errorf("insert graph edge: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("put graph record: %w", err)
	}
	return true, nil
}

// PutMarkRecord is [DB.PutGraphRecord] for a distrust list.
func (db *DB) PutMarkRecord(ctx context.Context, rec *federation.MarkRecord, payload []byte, receivedFrom *int64, expiresAt, now int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("put mark record: %w", err)
	}
	defer tx.Rollback()

	newer, err := seqIsNewer(ctx, tx, `federation_mark_records`, rec.Origin, rec.Seq)
	if err != nil || !newer {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO federation_mark_records
		        (origin, seq, issued_at, expires_at, payload, received_from, stored_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(origin) DO UPDATE SET
		        seq = excluded.seq, issued_at = excluded.issued_at,
		        expires_at = excluded.expires_at, payload = excluded.payload,
		        received_from = excluded.received_from, stored_at = excluded.stored_at`,
		rec.Origin, rec.Seq, rec.IssuedAt, expiresAt, string(payload), receivedFrom, now,
	); err != nil {
		return false, fmt.Errorf("store mark record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_marks WHERE origin = ?`, rec.Origin); err != nil {
		return false, fmt.Errorf("clear marks: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO federation_marks (origin, target, at, reason) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return false, fmt.Errorf("prepare mark insert: %w", err)
	}
	defer ins.Close()
	for _, m := range rec.Marks {
		if _, err := ins.ExecContext(ctx, rec.Origin, m.Key, m.At, m.Reason); err != nil {
			return false, fmt.Errorf("insert mark: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("put mark record: %w", err)
	}
	return true, nil
}

// seqIsNewer reports whether seq beats what table already holds for origin.
// A missing row is newer by definition; an equal seq is not, so a record that
// has been round the network and come back is a no-op.
func seqIsNewer(ctx context.Context, tx *sql.Tx, table, origin string, seq int64) (bool, error) {
	var have int64
	err := tx.QueryRowContext(ctx, `SELECT seq FROM `+table+` WHERE origin = ?`, origin).Scan(&have)
	switch {
	case err == sql.ErrNoRows:
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read stored sequence: %w", err)
	default:
		return seq > have, nil
	}
}

// GraphDigest lists what this node holds — records and marks, unexpired, in
// origin order so the digest is byte-stable for a serial to be taken over it.
func (db *DB) GraphDigest(ctx context.Context, now int64) ([]federation.GraphDigestEntry, []federation.GraphDigestEntry, error) {
	records, err := digestFrom(ctx, db, `federation_graph_records`, now)
	if err != nil {
		return nil, nil, err
	}
	marks, err := digestFrom(ctx, db, `federation_mark_records`, now)
	if err != nil {
		return nil, nil, err
	}
	return records, marks, nil
}

func digestFrom(ctx context.Context, db *DB, table string, now int64) ([]federation.GraphDigestEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT origin, seq FROM `+table+` WHERE expires_at > ? ORDER BY origin`, now)
	if err != nil {
		return nil, fmt.Errorf("read graph digest: %w", err)
	}
	defer rows.Close()
	var out []federation.GraphDigestEntry
	for rows.Next() {
		var e federation.GraphDigestEntry
		if err := rows.Scan(&e.Origin, &e.Seq); err != nil {
			return nil, fmt.Errorf("scan graph digest: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GraphPayloads returns the stored bytes of the named friend-list records.
func (db *DB) GraphPayloads(ctx context.Context, origins []string, now int64) (map[string][]byte, error) {
	return payloadsFrom(ctx, db, `federation_graph_records`, origins, now)
}

// MarkPayloads returns the stored bytes of the named distrust records.
func (db *DB) MarkPayloads(ctx context.Context, origins []string, now int64) (map[string][]byte, error) {
	return payloadsFrom(ctx, db, `federation_mark_records`, origins, now)
}

func payloadsFrom(ctx context.Context, db *DB, table string, origins []string, now int64) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(origins) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(origins)+1)
	args = append(args, now)
	for _, o := range origins {
		args = append(args, o)
	}
	q := `SELECT origin, payload FROM ` + table +
		` WHERE expires_at > ? AND origin IN (?` + strings.Repeat(`, ?`, len(origins)-1) + `)`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read graph payloads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var origin, payload string
		if err := rows.Scan(&origin, &payload); err != nil {
			return nil, fmt.Errorf("scan graph payload: %w", err)
		}
		out[origin] = []byte(payload)
	}
	return out, rows.Err()
}

// GraphKnowsKey reports whether key is one we would accept a record from: a
// direct friend of ours, or a node already named by a record we hold.
//
// Blocked peers count — we still know who they are, and a record from a blocked
// node is refused at the mesh door (meshAuth), not here.
func (db *DB) GraphKnowsKey(ctx context.Context, key string) (bool, error) {
	var found int
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM federation_peers      WHERE public_key = ?)
		     OR EXISTS(SELECT 1 FROM federation_graph_edges WHERE peer       = ?)`,
		key, key).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check graph key: %w", err)
	}
	return found == 1, nil
}

// GraphIntroducedCount is how many stored records arrived through one friend —
// the per-branch quota's input.
func (db *DB) GraphIntroducedCount(ctx context.Context, peerID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_graph_records WHERE received_from = ?`, peerID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count introduced origins: %w", err)
	}
	return n, nil
}

// ExpireGraph drops every record past its expiry, with its derived rows, and
// returns how many records went.
//
// Deleting the derived rows first (while their record still exists to select
// by) keeps the two tables consistent without a foreign key: the edges table is
// keyed by origin rather than by a record id, precisely so replacing a record
// in place does not churn ids.
func (db *DB) ExpireGraph(ctx context.Context, now int64) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("expire graph: %w", err)
	}
	defer tx.Rollback()

	total := 0
	for _, t := range []struct{ records, derived, key string }{
		{`federation_graph_records`, `federation_graph_edges`, `origin`},
		{`federation_mark_records`, `federation_marks`, `origin`},
	} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+t.derived+` WHERE `+t.key+` IN
			     (SELECT origin FROM `+t.records+` WHERE expires_at <= ?)`, now); err != nil {
			return 0, fmt.Errorf("expire derived graph rows: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM `+t.records+` WHERE expires_at <= ?`, now)
		if err != nil {
			return 0, fmt.Errorf("expire graph records: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("expire graph records: %w", err)
		}
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("expire graph: %w", err)
	}
	return total, nil
}

// GraphEdges returns every unexpired friendship claim, origin-ordered — the
// input the network map walks for reachability and branch attribution.
func (db *DB) GraphEdges(ctx context.Context, now int64) ([]federation.GraphEdgeClaim, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT e.origin, e.peer, e.name, e.since
		   FROM federation_graph_edges e
		   JOIN federation_graph_records r ON r.origin = e.origin
		  WHERE r.expires_at > ?
		  ORDER BY e.origin, e.peer`, now)
	if err != nil {
		return nil, fmt.Errorf("read graph edges: %w", err)
	}
	defer rows.Close()
	var out []federation.GraphEdgeClaim
	for rows.Next() {
		var e federation.GraphEdgeClaim
		if err := rows.Scan(&e.Origin, &e.Peer, &e.Name, &e.Since); err != nil {
			return nil, fmt.Errorf("scan graph edge: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GraphMarks returns every unexpired distrust mark. Callers weight them by
// branch before display — one branch is one voice, so a farm marking the same
// key cannot shout louder than a single honest friend.
func (db *DB) GraphMarks(ctx context.Context, now int64) ([]federation.StoredMark, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT m.origin, m.target, m.at, m.reason
		   FROM federation_marks m
		   JOIN federation_mark_records r ON r.origin = m.origin
		  WHERE r.expires_at > ?
		  ORDER BY m.target, m.origin`, now)
	if err != nil {
		return nil, fmt.Errorf("read graph marks: %w", err)
	}
	defer rows.Close()
	var out []federation.StoredMark
	for rows.Next() {
		var m federation.StoredMark
		if err := rows.Scan(&m.Origin, &m.Target, &m.At, &m.Reason); err != nil {
			return nil, fmt.Errorf("scan graph mark: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
