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
