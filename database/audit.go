package database

import (
	"context"
	"database/sql"
	"time"
)

// RecordAudit appends a row to the audit log. actorUserID is invalid for
// system or anonymous actions.
func (db *DB) RecordAudit(ctx context.Context, actorUserID sql.NullInt64, action, target, detail string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit_log (actor_user_id, action, target, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		actorUserID, action, target, detail, time.Now().Unix())
	return err
}

// AuditEntry is a row in the audit_log table.
type AuditEntry struct {
	ID          int64
	ActorUserID sql.NullInt64
	Action      string
	Target      sql.NullString
	Detail      sql.NullString
	CreatedAt   int64
}

// RecentAudit returns up to limit audit rows, newest first.
func (db *DB) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, actor_user_id, action, target, detail, created_at
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.Action, &e.Target, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
