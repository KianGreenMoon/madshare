// Package database wraps the SQLite store used by Madshare for file,
// upload, and media-metadata records.
package database

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"
)

// unicode_lower folds case the Unicode-aware way (Go's strings.ToLower handles
// German umlauts and other non-ASCII letters), unlike SQLite's built-in LOWER,
// which only lowercases ASCII. It is used on both sides of the search LIKE
// predicates so e.g. searching "über" matches "Über". Registered for every
// connection opened after this point, hence in init() before any Open.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("unicode_lower", 1,
		func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, ok := args[0].(string)
			if !ok {
				return args[0], nil // NULL / non-text: pass through unchanged
			}
			return strings.ToLower(s), nil
		})
}

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

// withConnectionPragmas appends the modernc.org/sqlite connection settings that
// must apply to every connection in the pool:
//
//   - foreign_keys — ON DELETE CASCADE enforcement (per-connection pragma).
//   - busy_timeout — wait on a held write lock instead of failing immediately
//     with SQLITE_BUSY. Matters because the worker pools (image variants, media
//     analysis) and the background prune job put sustained concurrent write
//     pressure on the on-disk DB (the pool is multi-connection; only :memory: is
//     pinned to one).
//   - _txlock=immediate — begin every transaction with BEGIN IMMEDIATE so it
//     takes the write lock up front. Without it a transaction that reads (SELECT)
//     and only later writes (e.g. hardDelete: SELECT id … then DELETE) holds a
//     deferred read snapshot and, if another connection commits a write in the
//     gap, the write upgrade fails with SQLITE_BUSY *immediately* — busy_timeout
//     cannot wait that out, because waiting can't resolve the deadlock. Acquiring
//     the write lock at BEGIN turns that deadlock into a normal wait the
//     busy_timeout absorbs. Every transaction here is read-write, so there is no
//     read-concurrency cost. (This is what produced "delete file: database is
//     locked" when pruning a file concurrently with the analysis pool.)
//
// The driver reads _pragma and _txlock query parameters from the DSN. Settings
// already named in the dsn are left as-is.
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
	if !strings.Contains(dsn, "_txlock=") {
		dsn += sep(dsn) + "_txlock=immediate"
	}
	return dsn
}

// Close closes the underlying database handle.
func (db *DB) Close() error {
	return db.DB.Close()
}
