package database

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"daemonlord.ygg/madshare/media"
)

// insertTaggedFP inserts an approved file with real tags AND a fingerprint, so
// the resolver can group it. Returns the file id.
func insertTaggedFP(t *testing.T, db *DB, seed, title, artist, album, codec string, raw []uint32) int64 {
	t.Helper()
	ctx := context.Background()
	meta := &MediaMetadata{
		Title:       title,
		Artist:      sql.NullString{String: artist, Valid: artist != ""},
		Album:       sql.NullString{String: album, Valid: album != ""},
		Codec:       sql.NullString{String: codec, Valid: codec != ""},
		ExtractedAt: 1000,
	}
	f := newFile(hash64(seed))
	if err := db.InsertFile(ctx, f, newUpload(seed+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile(%s): %v", seed, err)
	}
	fp := media.Fingerprint{Algo: "chromaprint", Duration: 200, Raw: raw}
	if err := db.InsertAudioFingerprint(ctx, f.ID, fp, 1000); err != nil {
		t.Fatalf("fingerprint(%s): %v", seed, err)
	}
	if _, err := db.ResolveRecording(ctx, f.ID); err != nil {
		t.Fatalf("resolve(%s): %v", seed, err)
	}
	return f.ID
}

// TestVisibility_AppearancesPlayBestRendition is the P1 visibility & access
// matrix (docs/architecture/recording-tagsets.md, test plan): an N-tagset
// recording surfaces as N library tracks, each playing the recording's
// ladder-best blob; removing every rendition makes the recording dormant
// (appearances hidden, favorites drop out) and restoring one re-surfaces it.
func TestVisibility_AppearancesPlayBestRendition(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x77BB77BB, 120)

	// Two renditions of one recording, offered under two different albums:
	// an MP3 appearance on "Single" and a FLAC appearance on "Album".
	mp3ID := insertTaggedFP(t, db, "v1", "Song", "Band", "Single", "mp3", fp)
	flacID := insertTaggedFP(t, db, "v2", "Song", "Band", "Album", "flac", fp)

	var recMP3, recFLAC int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, mp3ID).Scan(&recMP3); err != nil {
		t.Fatalf("rec of mp3: %v", err)
	}
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, flacID).Scan(&recFLAC); err != nil {
		t.Fatalf("rec of flac: %v", err)
	}
	if recMP3 != recFLAC {
		t.Fatalf("setup: files not grouped (%d vs %d)", recMP3, recFLAC)
	}

	// Both appearances are library tracks — under their own albums — and BOTH
	// play the FLAC (the ladder-best rendition).
	var flacKey string
	if err := db.QueryRow(`SELECT object_key FROM files WHERE id=?`, flacID).Scan(&flacKey); err != nil {
		t.Fatalf("flac key: %v", err)
	}
	tracksOf := func(album string) []*TrackEntry {
		t.Helper()
		var albumID int64
		if err := db.QueryRow(`SELECT al.id FROM albums al WHERE al.title=?`, album).Scan(&albumID); err != nil {
			t.Fatalf("album %q: %v", album, err)
		}
		tracks, err := db.ListTracksByAlbumID(ctx, albumID)
		if err != nil {
			t.Fatalf("tracks of %q: %v", album, err)
		}
		return tracks
	}
	single, album := tracksOf("Single"), tracksOf("Album")
	if len(single) != 1 || len(album) != 1 {
		t.Fatalf("tracks = %d/%d, want 1 per album (one appearance each)", len(single), len(album))
	}
	if single[0].ObjectKey != flacKey || album[0].ObjectKey != flacKey {
		t.Errorf("appearances play %q / %q, want both %q (ladder best)",
			single[0].ObjectKey, album[0].ObjectKey, flacKey)
	}
	if single[0].TagsetID == 0 || single[0].TagsetID == album[0].TagsetID {
		t.Errorf("appearances must carry distinct tagset ids (got %d / %d)",
			single[0].TagsetID, album[0].TagsetID)
	}
	// The origin hash rides along for the admin/file ops.
	if single[0].Hash != hash64("v1") || album[0].Hash != hash64("v2") {
		t.Errorf("origin hashes = %s / %s, want v1 / v2", single[0].Hash, album[0].Hash)
	}

	// Removing every rendition (files.deleted_at — rendition removal) makes the
	// recording dormant: both appearances vanish from the library.
	if _, err := db.Exec(`UPDATE files SET deleted_at = 1 WHERE recording_id = ?`, recMP3); err != nil {
		t.Fatalf("remove renditions: %v", err)
	}
	if got := tracksOf("Single"); len(got) != 0 {
		t.Errorf("dormant recording still lists %d track(s)", len(got))
	}
	// Restoring one rendition re-surfaces both appearances, playing what survives.
	if _, err := db.Exec(`UPDATE files SET deleted_at = NULL WHERE id = ?`, mp3ID); err != nil {
		t.Fatalf("restore rendition: %v", err)
	}
	var mp3Key string
	if err := db.QueryRow(`SELECT object_key FROM files WHERE id=?`, mp3ID).Scan(&mp3Key); err != nil {
		t.Fatalf("mp3 key: %v", err)
	}
	single = tracksOf("Single")
	if len(single) != 1 || single[0].ObjectKey != mp3Key {
		t.Errorf("after restore: %d track(s), key %q, want the surviving MP3 %q",
			len(single), single[0].ObjectKey, mp3Key)
	}
}

// TestBlobGate_RecordingLevel: a pending re-encode of already-published audio
// serves like its published siblings (the recording has an approved
// appearance), while a genuinely new pending upload stays private.
func TestBlobGate_RecordingLevel(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x600D600D, 120)

	insertTaggedFP(t, db, "g1", "Pub", "B", "A", "flac", fp) // approved
	// Same audio, pending review.
	metaPending := &MediaMetadata{Title: "Pub", ExtractedAt: 1000}
	f := newFile(hash64("g2"))
	f.ReviewState = ReviewSubmitted
	if err := db.InsertFile(ctx, f, newUpload("g2.mp3"), metaPending); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	if err := db.InsertAudioFingerprint(ctx, f.ID, media.Fingerprint{Algo: "chromaprint", Duration: 200, Raw: fp}, 1000); err != nil {
		t.Fatalf("fp: %v", err)
	}
	if _, err := db.ResolveRecording(ctx, f.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if visible, found, err := db.BlobPubliclyVisible(ctx, hash64("g2")); err != nil || !found || !visible {
		t.Errorf("pending sibling of published audio: visible=%v found=%v err=%v, want visible", visible, found, err)
	}

	// A genuinely new pending upload (own recording) stays private.
	f2 := newFile(hash64("g3"))
	f2.ReviewState = ReviewSubmitted
	if err := db.InsertFile(ctx, f2, newUpload("g3.mp3"), nil); err != nil {
		t.Fatalf("insert new pending: %v", err)
	}
	if visible, found, _ := db.BlobPubliclyVisible(ctx, hash64("g3")); !found || visible {
		t.Errorf("new pending upload: visible=%v found=%v, want hidden", visible, found)
	}
}

// TestMigration025_PlaylistItemsToTagsets seeds pre-025 playlist rows (file_id
// keyed) and verifies the rebuild maps each item 1:1 onto the file's offered
// tagset, preserving ids, order, and timestamps.
func TestMigration025_PlaylistItemsToTagsets(t *testing.T) {
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
	var m025 migration
	for _, m := range migs {
		switch {
		case m.version <= 24:
			if err := db.applyMigration(m); err != nil {
				t.Fatalf("apply migration %d: %v", m.version, err)
			}
		case m.version == 25:
			m025 = m
		}
	}
	if m025.version != 25 {
		t.Fatal("migration 025 not found")
	}

	// Pre-025 state: a user, two files (each with its P0 tagset via InsertFile),
	// and a playlist whose items reference the files.
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, created_at) VALUES (7, 'u', 'x', 1)`)
	var fileIDs, tagsetIDs []int64
	for i := 1; i <= 2; i++ {
		f := newFile(fmt.Sprintf("%064d", i))
		if err := db.InsertFile(context.Background(), f, newUpload(fmt.Sprintf("m%d.mp3", i)), newMeta()); err != nil {
			t.Fatalf("InsertFile %d: %v", i, err)
		}
		fileIDs = append(fileIDs, f.ID)
		var ts int64
		if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=?`, f.ID).Scan(&ts); err != nil {
			t.Fatalf("tagset %d: %v", i, err)
		}
		tagsetIDs = append(tagsetIDs, ts)
	}
	mustExec(t, db, `INSERT INTO playlists (id, user_id, name, kind, created_at, updated_at) VALUES (1, 7, 'L', 'regular', 1, 1)`)
	mustExec(t, db, `INSERT INTO playlist_items (id, playlist_id, file_id, position, added_at) VALUES (10, 1, ?, 1, 100)`, fileIDs[1])
	mustExec(t, db, `INSERT INTO playlist_items (id, playlist_id, file_id, position, added_at) VALUES (11, 1, ?, 2, 200)`, fileIDs[0])

	if err := db.applyMigration(m025); err != nil {
		t.Fatalf("apply migration 025: %v", err)
	}

	rows, err := db.Query(`SELECT id, tagset_id, position, added_at FROM playlist_items ORDER BY position`)
	if err != nil {
		t.Fatalf("read items: %v", err)
	}
	defer rows.Close()
	type item struct{ id, tagset, pos, at int64 }
	var got []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.tagset, &it.pos, &it.at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, it)
	}
	want := []item{{10, tagsetIDs[1], 1, 100}, {11, tagsetIDs[0], 2, 200}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("items after 025 = %+v, want %+v", got, want)
	}
}
