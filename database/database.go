// Package database wraps the SQLite store used by Madshare for file,
// upload, and media-metadata records.
package database

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// DB is the application's database handle. It wraps *sql.DB so callers
// can depend on a concrete type with repository methods attached.
type DB struct {
	*sql.DB
}

// Open opens (or creates) a SQLite database at dsn, applies WAL +
// foreign-keys pragmas, and runs any pending migrations.
//
// Use ":memory:" for an in-process database (mostly for tests).
func Open(dsn string) (*DB, error) {
	// SQLite enforces ON DELETE CASCADE only when foreign_keys is ON on the
	// *executing* connection, and the pragma does not persist across the
	// pooled connections database/sql opens lazily. Issuing the pragma once
	// (below) only configures whichever connection happens to serve that Exec.
	// Encode it in the DSN instead so modernc.org/sqlite applies it to every
	// connection in the pool. :memory: is special-cased: each connection gets
	// its own private database, so we pin the pool to a single connection.
	openDSN := dsn
	if dsn != ":memory:" {
		openDSN = withConnectionPragmas(dsn)
	}

	sqlDB, err := sql.Open("sqlite", openDSN)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	if dsn == ":memory:" {
		sqlDB.SetMaxOpenConns(1)
	}

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := sqlDB.Exec(pragma); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("apply %q: %w", pragma, err)
		}
	}

	db := &DB{DB: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// withConnectionPragmas appends the modernc.org/sqlite connection pragmas that
// must apply to every connection in the pool: foreign-key enforcement and a
// busy_timeout so a connection waits on a held write lock instead of failing
// immediately with SQLITE_BUSY. The latter matters now that the image-variant
// worker pool creates sustained concurrent write pressure on the on-disk DB
// (the pool is multi-connection; only :memory: is pinned to one). The driver
// reads _pragma query parameters from the DSN and issues them on each new
// connection. Pragmas already named in the dsn are left as-is.
func withConnectionPragmas(dsn string) string {
	sep := func(d string) string {
		if strings.Contains(d, "?") {
			return "&"
		}
		return "?"
	}
	if !strings.Contains(dsn, "_pragma=foreign_keys") {
		dsn += sep(dsn) + "_pragma=foreign_keys(1)"
	}
	if !strings.Contains(dsn, "_pragma=busy_timeout") {
		dsn += sep(dsn) + "_pragma=busy_timeout(5000)"
	}
	return dsn
}

// Close closes the underlying database handle.
func (db *DB) Close() error {
	return db.DB.Close()
}
