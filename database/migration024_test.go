package database

import (
	"database/sql"
	"testing"
)

// TestMigration024_TagsetRoundTrip exercises the recording-tagsets P0 migration
// on a seeded pre-024 database (docs/architecture/recording-tagsets.md, test
// plan "Migration round-trip"): every file's descriptive tags land on exactly
// one tagset (column-level equality), review/trash state moves onto it, license
// and guest access collapse onto the recording using the best rendition's
// values, every file gains a recording, media_metadata keeps only tech columns,
// and the NOT NULL trigger guards future inserts.
func TestMigration024_TagsetRoundTrip(t *testing.T) {
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
	var m024 migration
	for _, m := range migs {
		switch {
		case m.version <= 23:
			if err := db.applyMigration(m); err != nil {
				t.Fatalf("apply migration %d: %v", m.version, err)
			}
		case m.version == 24:
			m024 = m
		}
	}
	if m024.version != 24 {
		t.Fatal("migration 024 not found")
	}

	// ── Legacy v23 state ────────────────────────────────────────────────────
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, created_at) VALUES (7, 'kian', 'x', 1)`)
	mustExec(t, db, `INSERT INTO artists (id, name, norm_name, created_at) VALUES (1, 'Artist', 'artist', 1)`)
	mustExec(t, db, `INSERT INTO albums (id, artist_id, title, norm_title, created_at) VALUES (1, 1, 'Album', 'album', 1)`)

	seed := func(id int64, hash, state string, deletedAt, submittedAt any, note any, lic any, guest, manual int) {
		mustExec(t, db, `INSERT INTO files (id, hash, byte_size, mime_type, storage_backend, object_key,
			created_at, uploaded_by, review_state, review_note, submitted_at, deleted_at,
			license, guest_playable, guest_playable_manual)
			VALUES (?, ?, 10, 'audio/flac', 'local', ?, 100, 7, ?, ?, ?, ?, ?, ?, ?)`,
			id, hash, hash+"/f.flac", state, note, submittedAt, deletedAt, lic, guest, manual)
		mustExec(t, db, `INSERT INTO file_uploads (file_id, filename, uploaded_at) VALUES (?, 'f.flac', 100)`, id)
	}
	seedMeta := func(fileID int64, title string, codec any, bitrate any) {
		mustExec(t, db, `INSERT INTO media_metadata (file_id, title, artist, album, album_artist, genre, year,
			track_number, track_total, disc_number, composer, comment, codec, bitrate, extracted_at,
			artist_id, album_artist_id, album_id)
			VALUES (?, ?, 'Artist', 'Album', 'Artist', 'Rock', 2001, 3, 12, 1, 'Comp', 'Note', ?, ?, 100, 1, 1, 1)`,
			fileID, title, codec, bitrate)
	}

	// 1: live approved, unresolved recording, explicit guest grant + license.
	seed(1, "h1", "approved", nil, nil, nil, "CC0-1.0", 1, 1)
	seedMeta(1, "Live Track", "flac", nil)
	// 2: approved-then-trashed (deleted_at set) — must become a trashed tagset
	//    on a live file row.
	seed(2, "h2", "approved", 555, nil, nil, nil, 0, 0)
	seedMeta(2, "Trashed Track", "flac", nil)
	// 3: pending submission with a moderator note.
	seed(3, "h3", "returned", nil, 444, "fix the tags", nil, 0, 0)
	seedMeta(3, "Pending Track", "flac", nil)
	// 4+5: two renditions of one recording with conflicting license values —
	//      the FLAC (best by ladder) must win the collapse and the primary flag.
	mustExec(t, db, `INSERT INTO recordings (id, created_at) VALUES (9, 100)`)
	seed(4, "h4", "approved", nil, nil, nil, "MIT", 1, 0)
	seedMeta(4, "Song", "flac", nil)
	seed(5, "h5", "approved", nil, nil, nil, "WRONG", 0, 0)
	seedMeta(5, "Song", "mp3", 320000)
	mustExec(t, db, `UPDATE files SET recording_id = 9 WHERE id IN (4, 5)`)
	// 6: a file with no media_metadata row at all → filename-derived tagset.
	seed(6, "h6", "approved", nil, nil, nil, nil, 0, 0)

	if err := db.applyMigration(m024); err != nil {
		t.Fatalf("apply migration 024: %v", err)
	}

	// ── Every file: non-null recording_id + exactly one primary-per-recording ─
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE recording_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count null recordings: %v", err)
	}
	if n != 0 {
		t.Errorf("%d files left without recording_id, want 0", n)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM recordings r WHERE
		(SELECT COUNT(*) FROM tagsets t WHERE t.recording_id = r.id AND t.is_primary = 1) <> 1`).Scan(&n); err != nil {
		t.Fatalf("count primaries: %v", err)
	}
	if n != 0 {
		t.Errorf("%d recordings without exactly one primary tagset, want 0", n)
	}

	// ── Column-level tag equality on file 1's single tagset ────────────────
	var (
		title, artist, album, albumArtist, genre, composer, comment, state string
		year, track, total, disc, createdBy, artistID, albumID             int64
		deletedAt, submittedAt, note                                       sql.NullInt64
		noteText                                                           sql.NullString
		isPrimary                                                          int
	)
	_ = note
	row := db.QueryRow(`SELECT t.title, t.artist, t.album, t.album_artist, t.genre, t.year,
		t.track_number, t.track_total, t.disc_number, t.composer, t.comment,
		t.review_state, t.review_note, t.submitted_at, t.created_by, t.deleted_at,
		t.artist_id, t.album_id, t.is_primary
		FROM tagsets t WHERE t.origin_file_id = 1`)
	if err := row.Scan(&title, &artist, &album, &albumArtist, &genre, &year,
		&track, &total, &disc, &composer, &comment,
		&state, &noteText, &submittedAt, &createdBy, &deletedAt,
		&artistID, &albumID, &isPrimary); err != nil {
		t.Fatalf("read tagset 1: %v", err)
	}
	if title != "Live Track" || artist != "Artist" || album != "Album" || albumArtist != "Artist" ||
		genre != "Rock" || year != 2001 || track != 3 || total != 12 || disc != 1 ||
		composer != "Comp" || comment != "Note" {
		t.Errorf("tagset 1 tags differ from the legacy media_metadata row: %q %q %q %q %q %d %d %d %d %q %q",
			title, artist, album, albumArtist, genre, year, track, total, disc, composer, comment)
	}
	if state != "approved" || createdBy != 7 || deletedAt.Valid || artistID != 1 || albumID != 1 || isPrimary != 1 {
		t.Errorf("tagset 1 lifecycle = (%s, by %d, deleted %v, artist %d, album %d, primary %d)",
			state, createdBy, deletedAt, artistID, albumID, isPrimary)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tagsets WHERE origin_file_id = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("file 1 has %d tagsets (err %v), want exactly 1", n, err)
	}

	// ── Trashed file → trashed tagset, file row live ────────────────────────
	var fileDeleted, tagsetDeleted sql.NullInt64
	if err := db.QueryRow(`SELECT f.deleted_at, t.deleted_at FROM files f
		JOIN tagsets t ON t.origin_file_id = f.id WHERE f.id = 2`).Scan(&fileDeleted, &tagsetDeleted); err != nil {
		t.Fatalf("read trashed pair: %v", err)
	}
	if fileDeleted.Valid {
		t.Error("file 2 row still trashed; the Trash should have moved to its tagset")
	}
	if !tagsetDeleted.Valid || tagsetDeleted.Int64 != 555 {
		t.Errorf("file 2 tagset deleted_at = %v, want 555", tagsetDeleted)
	}

	// ── Pending state + note + submitted_at moved ───────────────────────────
	if err := db.QueryRow(`SELECT t.review_state, t.review_note, t.submitted_at FROM tagsets t
		WHERE t.origin_file_id = 3`).Scan(&state, &noteText, &submittedAt); err != nil {
		t.Fatalf("read pending tagset: %v", err)
	}
	if state != "returned" || noteText.String != "fix the tags" || submittedAt.Int64 != 444 {
		t.Errorf("pending tagset = (%s, %q, %v), want (returned, fix the tags, 444)", state, noteText.String, submittedAt)
	}

	// ── License collapse: best rendition (FLAC) wins on the shared recording ─
	var lic sql.NullString
	var guest int
	if err := db.QueryRow(`SELECT license, guest_playable FROM recordings WHERE id = 9`).Scan(&lic, &guest); err != nil {
		t.Fatalf("read recording 9: %v", err)
	}
	if lic.String != "MIT" || guest != 1 {
		t.Errorf("recording 9 license/guest = (%q, %d), want (MIT, 1) from the FLAC rendition", lic.String, guest)
	}
	var primaryOrigin int64
	if err := db.QueryRow(`SELECT origin_file_id FROM tagsets WHERE recording_id = 9 AND is_primary = 1`).Scan(&primaryOrigin); err != nil {
		t.Fatalf("read recording 9 primary: %v", err)
	}
	if primaryOrigin != 4 {
		t.Errorf("recording 9 primary tagset originates from file %d, want 4 (the FLAC)", primaryOrigin)
	}

	// ── File without metadata still got a (filename-derived) tagset ─────────
	if err := db.QueryRow(`SELECT t.title FROM tagsets t WHERE t.origin_file_id = 6`).Scan(&title); err != nil {
		t.Fatalf("read bare tagset: %v", err)
	}
	if title != "f" {
		t.Errorf("bare file tagset title = %q, want %q (filename minus extension)", title, "f")
	}

	// ── media_metadata reduced to tech columns; tech values survived ────────
	var hasDescriptive int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media_metadata')
		WHERE name IN ('title','artist','album','album_artist','genre','year',
		               'track_number','track_total','disc_number','composer','comment',
		               'artist_id','album_artist_id','album_id')`).Scan(&hasDescriptive); err != nil {
		t.Fatalf("inspect media_metadata: %v", err)
	}
	if hasDescriptive != 0 {
		t.Errorf("media_metadata still carries %d descriptive column(s), want 0", hasDescriptive)
	}
	var codec string
	var bitrate int64
	if err := db.QueryRow(`SELECT codec, bitrate FROM media_metadata WHERE file_id = 5`).Scan(&codec, &bitrate); err != nil {
		t.Fatalf("read tech row: %v", err)
	}
	if codec != "mp3" || bitrate != 320000 {
		t.Errorf("tech row = (%s, %d), want (mp3, 320000)", codec, bitrate)
	}

	// ── files lost the moved lifecycle/access columns ────────────────────────
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('files')
		WHERE name IN ('review_state','review_note','submitted_at',
		               'license','guest_playable','guest_playable_manual')`).Scan(&n); err != nil {
		t.Fatalf("inspect files: %v", err)
	}
	if n != 0 {
		t.Errorf("files still carries %d moved column(s), want 0", n)
	}

	// ── The NOT NULL trigger guards future inserts ───────────────────────────
	if _, err := db.Exec(`INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, created_at)
		VALUES ('h7', 1, 'audio/flac', 'local', 'h7/x', 1)`); err == nil {
		t.Error("insert without recording_id succeeded, want trigger ABORT")
	}
}
