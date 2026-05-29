package database

import (
	"context"
	"database/sql"
	"path/filepath"
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
	want := []string{
		"album_images", "api_tokens", "artist_images", "audit_log", "file_uploads",
		"files", "media_metadata", "role_permissions", "roles", "schema_migrations",
		"sessions", "user_roles", "users",
	}
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
	if v != 4 {
		t.Errorf("migration version = %d, want 4", v)
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
	if rows != 4 {
		t.Errorf("schema_migrations row count = %d, want 4 after re-run", rows)
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

// TestOpen_FileDB_CascadeAcrossPooledConns is the load-bearing check that the
// DSN _pragma=foreign_keys(1) fix works for a real file-backed DB, where the
// pool may hand out multiple connections. With concurrency allowed, a delete
// served by a connection that never ran "PRAGMA foreign_keys = ON" would skip
// the cascade — this test would catch that regression.
func TestOpen_FileDB_CascadeAcrossPooledConns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madshare.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open file db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()

	// Force the pool to materialise several connections and hold them open, so
	// the insert/delete below are served by connections that were created
	// lazily by the pool rather than the single one Open ran its one-off
	// "PRAGMA foreign_keys = ON" against. Without the DSN-level pragma those
	// connections have foreign_keys OFF and the cascade silently no-ops.
	const conns = 8
	db.SetMaxOpenConns(conns)
	held := make([]*sql.Conn, 0, conns)
	for i := 0; i < conns; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("open conn %d: %v", i, err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close() // return to pool; they remain open and reusable
	}

	hash := "9999000000000000000000000000000000000000000000000000000000000000"
	f := newFile(hash)
	if err := db.InsertFile(ctx, f, newUpload("song.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	if _, found, err := db.DeleteFileByHash(ctx, hash); err != nil || !found {
		t.Fatalf("DeleteFileByHash: found=%v err=%v", found, err)
	}

	var uploads, meta int
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploads)
	db.QueryRow(`SELECT COUNT(*) FROM media_metadata`).Scan(&meta)
	if uploads != 0 {
		t.Errorf("file_uploads rows = %d, want 0 (FK cascade not enforced on file DB)", uploads)
	}
	if meta != 0 {
		t.Errorf("media_metadata rows = %d, want 0 (FK cascade not enforced on file DB)", meta)
	}
}
