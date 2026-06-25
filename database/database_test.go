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

	// Migration 014 renames the old string-keyed cover tables aside; the cover
	// backfill (a startup pass) drains and drops the *_old leftovers. Run it so
	// the asserted set is the steady-state schema.
	if err := db.BackfillCoverEntities(context.Background()); err != nil {
		t.Fatalf("BackfillCoverEntities: %v", err)
	}

	got := tableNames(t, db)
	want := []string{
		"album_images", "albums", "api_tokens",
		"artist_images", "artists", "audio_fingerprints", "audit_log",
		"data_sources", "file_uploads", "files",
		"image_processing_jobs", "media_analysis_jobs", "media_metadata",
		"playlist_items", "playlists", "recordings",
		"role_permissions", "roles",
		"schema_migrations", "sessions", "settings", "user_roles", "users",
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
	if v != 21 {
		t.Errorf("migration version = %d, want 21", v)
	}
}

// TestOpen_IdempotentMigrations runs migrate() a second time on an
// already-migrated DB and checks nothing changes / nothing errors.
// TestMigration016_UpgradesLegacyData exercises migration 016 on a pre-existing
// (v15) database, the path the fresh-DB tests don't reach: it relabels the
// empty-key buckets, backfills required titles from filenames, and installs the
// empty-guard triggers. It builds a DB manually and applies migrations 1..15,
// seeds legacy rows, then applies 016.
func TestMigration016_UpgradesLegacyData(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL"} {
		if _, err := sqlDB.Exec(p); err != nil {
			t.Fatalf("pragma %q: %v", p, err)
		}
	}
	db := &DB{DB: sqlDB}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}
	var m016 migration
	for _, m := range migs {
		switch {
		case m.version <= 15:
			if err := db.applyMigration(m); err != nil {
				t.Fatalf("apply migration %d: %v", m.version, err)
			}
		case m.version == 16:
			m016 = m
		}
	}
	if m016.version != 16 {
		t.Fatal("migration 016 not found")
	}

	// Legacy v15 state: an unknown-artist bucket (empty keys), its unknown-album
	// bucket, and three tracks — untitled (→ filename), a real title (preserved),
	// and a multi-dot filename (only the final extension stripped).
	mustExec(t, db, `INSERT INTO artists (id, name, norm_name, created_at) VALUES (1, '', '', 1)`)
	mustExec(t, db, `INSERT INTO albums (id, artist_id, title, norm_title, created_at) VALUES (1, 1, '', '', 1)`)
	seedLegacyTrack := func(id int64, hash, filename string, title any) {
		mustExec(t, db, `INSERT INTO files (id, hash, byte_size, mime_type, storage_backend, object_key, created_at)
			VALUES (?, ?, 1, 'audio/flac', 'local', ?, 100)`, id, hash, hash+"/"+filename)
		mustExec(t, db, `INSERT INTO file_uploads (file_id, filename, uploaded_at) VALUES (?, ?, 100)`, id, filename)
		mustExec(t, db, `INSERT INTO media_metadata (file_id, title, extracted_at, artist_id, album_id) VALUES (?, ?, 100, 1, 1)`, id, title)
	}
	seedLegacyTrack(1, "h1", "Track 01.flac", nil)
	seedLegacyTrack(2, "h2", "whatever.mp3", "Real Title")
	seedLegacyTrack(3, "h3", "Album.Disc.2.mp3", nil)

	if err := db.applyMigration(m016); err != nil {
		t.Fatalf("apply migration 016: %v", err)
	}

	// Bucket display columns relabeled (dedup keys stay '' until the Go fold pass).
	var name, norm string
	if err := db.QueryRow(`SELECT name, norm_name FROM artists WHERE id = 1`).Scan(&name, &norm); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Unknown artist" || norm != "" {
		t.Errorf("artist = (%q,%q), want (Unknown artist, \"\")", name, norm)
	}
	var title, ntitle string
	if err := db.QueryRow(`SELECT title, norm_title FROM albums WHERE id = 1`).Scan(&title, &ntitle); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if title != "Other" || ntitle != "" {
		t.Errorf("album = (%q,%q), want (Other, \"\")", title, ntitle)
	}

	// Titles backfilled per rule.
	for _, tc := range []struct {
		fileID int64
		want   string
	}{
		{1, "Track 01"},     // untitled → filename, extension stripped
		{2, "Real Title"},   // real tag preserved
		{3, "Album.Disc.2"}, // only the final .mp3 stripped
	} {
		var got string
		if err := db.QueryRow(`SELECT title FROM media_metadata WHERE file_id = ?`, tc.fileID).Scan(&got); err != nil {
			t.Fatalf("read title %d: %v", tc.fileID, err)
		}
		if got != tc.want {
			t.Errorf("file %d title = %q, want %q", tc.fileID, got, tc.want)
		}
	}

	// Empty-guard triggers installed.
	var nTriggers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE '%_nonempty_%'`).Scan(&nTriggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if nTriggers != 4 {
		t.Errorf("nonempty triggers = %d, want 4", nTriggers)
	}
}

func TestOpen_IdempotentMigrations(t *testing.T) {
	db := openMem(t)

	if err := db.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rows != 21 {
		t.Errorf("schema_migrations row count = %d, want 21 after re-run", rows)
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

	if _, found, err := db.HardDeleteFileByHash(ctx, hash); err != nil || !found {
		t.Fatalf("HardDeleteFileByHash: found=%v err=%v", found, err)
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
