package database

import (
	"sort"
	"testing"
)

// openMem returns a fresh in-memory DB. The pool is pinned to 1 connection
// inside Open, so all queries hit the same database.
func openMem(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableNames(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return names
}

func TestOpen_CreatesExpectedTables(t *testing.T) {
	db := openMem(t)

	got := tableNames(t, db)
	want := []string{"album_images", "artist_images", "file_uploads", "files", "media_metadata", "schema_migrations"}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tables[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestOpen_RecordsMigrationVersion(t *testing.T) {
	db := openMem(t)

	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if v != 2 {
		t.Errorf("migration version = %d, want 2", v)
	}
}

// TestOpen_IdempotentMigrations runs migrate() a second time on an
// already-migrated DB and checks nothing changes / nothing errors.
func TestOpen_IdempotentMigrations(t *testing.T) {
	db := openMem(t)

	if err := db.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rows != 2 {
		t.Errorf("schema_migrations row count = %d, want 2 after re-run", rows)
	}
}

func TestOpen_ForeignKeysEnabled(t *testing.T) {
	db := openMem(t)
	var on int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if on != 1 {
		t.Errorf("foreign_keys = %d, want 1", on)
	}
}
