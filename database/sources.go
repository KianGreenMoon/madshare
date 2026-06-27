package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Data-source kinds and statuses (migration 021_data_sources.sql). Only the
// 'symlink' kind exists in v0; 's3' joins the kind CHECK when that storage lands.
// See docs/architecture/data-sources.md.
const (
	SourceKindSymlink = "symlink"

	SourceStatusActive   = "active"   // last scan finished cleanly
	SourceStatusScanning = "scanning" // a scan is in flight
	SourceStatusError    = "error"    // last scan failed (or was interrupted by a crash)
)

// ErrSourceNotFound is returned by data-source lookups when no row matches the id.
var ErrSourceNotFound = errors.New("data source not found")

// DataSource is a row in the data_sources table: a logical origin that populates
// the shared 'links' storage. SummaryJSON carries the last scan's counts (opaque
// to the DB layer); ScannedAt is invalid until the first scan finishes.
type DataSource struct {
	ID          string
	Kind        string
	Name        string
	RootPath    string
	Status      string
	SummaryJSON sql.NullString
	CreatedAt   int64
	ScannedAt   sql.NullInt64
}

// InsertDataSource persists a new data_sources row. The caller supplies the id
// (an opaque unique token) and the initial status (typically 'scanning').
func (db *DB) InsertDataSource(ctx context.Context, ds *DataSource) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO data_sources (id, kind, name, root_path, status, summary_json, created_at, scanned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ds.ID, ds.Kind, ds.Name, ds.RootPath, ds.Status, ds.SummaryJSON, ds.CreatedAt, ds.ScannedAt,
	)
	if err != nil {
		return fmt.Errorf("insert data_sources: %w", err)
	}
	return nil
}

// ListDataSources returns every data source, newest first.
func (db *DB) ListDataSources(ctx context.Context) ([]*DataSource, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, kind, name, root_path, status, summary_json, created_at, scanned_at
		FROM data_sources
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list data_sources: %w", err)
	}
	defer rows.Close()

	var out []*DataSource
	for rows.Next() {
		var ds DataSource
		if err := rows.Scan(&ds.ID, &ds.Kind, &ds.Name, &ds.RootPath, &ds.Status,
			&ds.SummaryJSON, &ds.CreatedAt, &ds.ScannedAt); err != nil {
			return nil, fmt.Errorf("scan data_sources: %w", err)
		}
		out = append(out, &ds)
	}
	return out, rows.Err()
}

// GetDataSource returns one data source by id, or ErrSourceNotFound.
func (db *DB) GetDataSource(ctx context.Context, id string) (*DataSource, error) {
	var ds DataSource
	err := db.QueryRowContext(ctx, `
		SELECT id, kind, name, root_path, status, summary_json, created_at, scanned_at
		FROM data_sources WHERE id = ?`, id).
		Scan(&ds.ID, &ds.Kind, &ds.Name, &ds.RootPath, &ds.Status,
			&ds.SummaryJSON, &ds.CreatedAt, &ds.ScannedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get data_sources: %w", err)
	}
	return &ds, nil
}

// UpdateDataSourceStatus sets a source's status only (used to flag 'error'
// without a summary).
func (db *DB) UpdateDataSourceStatus(ctx context.Context, id, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE data_sources SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update data_sources status: %w", err)
	}
	return nil
}

// FinishDataSourceScan records the outcome of a scan: the final status, the
// summary JSON, and the scan completion time.
func (db *DB) FinishDataSourceScan(ctx context.Context, id, status, summaryJSON string, scannedAt int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE data_sources SET status = ?, summary_json = ?, scanned_at = ? WHERE id = ?`,
		status, summaryJSON, scannedAt, id)
	if err != nil {
		return fmt.Errorf("finish data_sources scan: %w", err)
	}
	return nil
}

// ResetStaleScans flips any source left in 'scanning' (a scan interrupted by a
// crash) to 'error' so it is not shown as perpetually in-flight. Returns the
// number of rows reset. Startup recovery, mirroring ResetStaleJobs.
func (db *DB) ResetStaleScans(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE data_sources SET status = ? WHERE status = ?`, SourceStatusError, SourceStatusScanning)
	if err != nil {
		return 0, fmt.Errorf("reset stale scans: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteDataSource removes a data_sources row. Its source_files attribution rows
// cascade away (ON DELETE CASCADE, migration 023), so the catalog records the
// source shared with another source — or owned by a local upload — remain,
// attributed only to whatever else still references them.
func (db *DB) DeleteDataSource(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM data_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete data_sources: %w", err)
	}
	return nil
}

// AttributeSourceFile records that source sourceID references file fileID
// (source_files, migration 023). INSERT OR IGNORE so re-scanning the same tree —
// or two sources sharing a hash — is idempotent; a removed source/file takes its
// rows with it via the FK cascade.
func (db *DB) AttributeSourceFile(ctx context.Context, sourceID string, fileID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO source_files (source_id, file_id) VALUES (?, ?)`, sourceID, fileID)
	if err != nil {
		return fmt.Errorf("attribute source file: %w", err)
	}
	return nil
}

// SourceHasAttribution reports whether source sourceID has any source_files rows.
// Used to skip the legacy backfill once a source is attributed (idempotence).
func (db *DB) SourceHasAttribution(ctx context.Context, sourceID string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM source_files WHERE source_id = ? LIMIT 1`, sourceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("source has attribution: %w", err)
	}
	return true, nil
}

// CountSourceFiles returns how many files source sourceID references.
func (db *DB) CountSourceFiles(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_files WHERE source_id = ?`, sourceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count source files: %w", err)
	}
	return n, nil
}

// SourceFileRef identifies a file a source references, with the fields the
// removal decision needs: the content hash (to unlink its symlink) and the
// storage backend (a 'links' row is hard-deleted; a 'local' upload is kept).
type SourceFileRef struct {
	ID             int64
	Hash           string
	StorageBackend string
}

// SourceExclusiveFiles returns the files source sourceID references that NO other
// source references — the candidate set for removal. A hash imported from a second
// source root is attributed to both, so it is excluded here (kept). Callers still
// keep 'local' rows (only the stray link is reclaimed); this query returns the
// backend so they can decide. See docs/architecture/data-sources.md (Removing a
// source).
func (db *DB) SourceExclusiveFiles(ctx context.Context, sourceID string) ([]SourceFileRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.id, f.hash, f.storage_backend
		FROM source_files sf
		JOIN files f ON f.id = sf.file_id
		WHERE sf.source_id = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM source_files o
		    WHERE o.file_id = sf.file_id AND o.source_id <> sf.source_id
		  )`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("source exclusive files: %w", err)
	}
	defer rows.Close()

	var out []SourceFileRef
	for rows.Next() {
		var r SourceFileRef
		if err := rows.Scan(&r.ID, &r.Hash, &r.StorageBackend); err != nil {
			return nil, fmt.Errorf("scan source exclusive file: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LinkFileRef is a links-backed file's id and its external original path, used by
// the legacy attribution backfill to match files against a source's root.
type LinkFileRef struct {
	ID         int64
	LinkTarget string
}

// ListLinkFiles returns every links-backed file with a non-null link_target. The
// backfill walks this once and attributes each to whichever source root contains
// its link_target (path logic lives in the sources package, off the DB layer).
func (db *DB) ListLinkFiles(ctx context.Context) ([]LinkFileRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, link_target FROM files
		WHERE storage_backend = ? AND link_target IS NOT NULL AND link_target <> ''`,
		StorageBackendLinks)
	if err != nil {
		return nil, fmt.Errorf("list link files: %w", err)
	}
	defer rows.Close()

	var out []LinkFileRef
	for rows.Next() {
		var r LinkFileRef
		if err := rows.Scan(&r.ID, &r.LinkTarget); err != nil {
			return nil, fmt.Errorf("scan link file: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
