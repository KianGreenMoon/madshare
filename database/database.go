// Package database wraps the SQLite store used by Madshare for file,
// upload, and media-metadata records.
package database

import (
	"database/sql"
	"fmt"

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
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// Pragmas must be set on the live connection. For :memory: DBs each
	// connection has its own database, so pin the pool to one connection.
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

// Close closes the underlying database handle.
func (db *DB) Close() error {
	return db.DB.Close()
}
