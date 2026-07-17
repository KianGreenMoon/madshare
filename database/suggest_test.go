package database

import (
	"context"
	"database/sql"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"daemonlord.ygg/madshare/media"
)

// mojibake1251 returns s as it looks after the classic mis-decode: cp1251
// bytes read as ISO-8859-1 (what ingest stores for a mis-declared file).
func mojibake1251(t *testing.T, s string) string {
	t.Helper()
	raw, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode fixture %q: %v", s, err)
	}
	out, ok := media.DecodeWith("iso-8859-1", raw)
	if !ok {
		t.Fatal("iso-8859-1 decode failed")
	}
	return out
}

func insertMojibakeFile(t *testing.T, db *DB, seed, title, artist, album string) (fileID, tagsetID int64) {
	t.Helper()
	ctx := context.Background()
	f := newFile(hash64(seed))
	meta := &MediaMetadata{
		Title:       title,
		Artist:      sql.NullString{String: artist, Valid: true},
		Album:       sql.NullString{String: album, Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(ctx, f, newUpload(seed+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	var ts int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id = ?`, f.ID).Scan(&ts); err != nil {
		t.Fatalf("tagset lookup: %v", err)
	}
	return f.ID, ts
}

func TestRecodeTagsetsText(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	title, artist, album := "Группа крови", "Виктор Цой", "Кино"
	_, ts1 := insertMojibakeFile(t, db, "rc1", mojibake1251(t, title), mojibake1251(t, artist), mojibake1251(t, album))
	_, ts2 := insertMojibakeFile(t, db, "rc2", mojibake1251(t, "Пачка сигарет"), mojibake1251(t, artist), mojibake1251(t, album))

	recode := func(s string) (string, bool) { return media.ReencodeLatin1(s, "windows-1251") }

	affected, notFound, err := db.RecodeTagsetsText(ctx, []int64{ts1, ts2, 9999}, sql.NullInt64{}, recode)
	if err != nil {
		t.Fatalf("RecodeTagsetsText: %v", err)
	}
	if affected != 2 || len(notFound) != 1 || notFound[0] != 9999 {
		t.Fatalf("affected=%d notFound=%v, want 2 / [9999]", affected, notFound)
	}

	// The stored text is fixed and the identity change re-resolved the entities.
	var gotTitle, gotArtist, gotAlbum, artistName string
	if err := db.QueryRow(`
		SELECT t.title, t.artist, t.album, a.name
		FROM tagsets t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = ?`, ts1).Scan(&gotTitle, &gotArtist, &gotAlbum, &artistName); err != nil {
		t.Fatalf("read recoded tagset: %v", err)
	}
	if gotTitle != title || gotArtist != artist || gotAlbum != album {
		t.Errorf("tagset = %q/%q/%q, want %q/%q/%q", gotTitle, gotArtist, gotAlbum, title, artist, album)
	}
	if artistName != artist {
		t.Errorf("resolved artist entity = %q, want %q (identity re-resolve)", artistName, artist)
	}

	// Idempotent: correct Cyrillic no longer fits Latin-1, so a second pass
	// changes nothing.
	affected, _, err = db.RecodeTagsetsText(ctx, []int64{ts1, ts2}, sql.NullInt64{}, recode)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if affected != 0 {
		t.Errorf("second pass affected = %d, want 0 (idempotent)", affected)
	}
}

func TestRecodeTagsetsText_OwnerScope(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	owner, err := db.CreateUser(ctx, "recode-owner", "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	stranger, err := db.CreateUser(ctx, "recode-stranger", "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	moji := mojibake1251(t, "Кино")
	f := newFile(hash64("rcown"))
	f.ReviewState = ReviewDraft
	f.UploadedBy = sql.NullInt64{Int64: owner, Valid: true}
	meta := &MediaMetadata{Title: moji, ExtractedAt: 1700000000}
	if err := db.InsertFile(ctx, f, newUpload("own.mp3"), meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	var ts int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id = ?`, f.ID).Scan(&ts); err != nil {
		t.Fatal(err)
	}

	recode := func(s string) (string, bool) { return media.ReencodeLatin1(s, "windows-1251") }

	// A different user's owner-scoped call must not touch the row.
	affected, notFound, err := db.RecodeTagsetsText(ctx, []int64{ts}, sql.NullInt64{Int64: stranger, Valid: true}, recode)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 0 || len(notFound) != 1 {
		t.Fatalf("foreign owner: affected=%d notFound=%v, want 0 / [ts]", affected, notFound)
	}

	// The owner's call does.
	affected, _, err = db.RecodeTagsetsText(ctx, []int64{ts}, sql.NullInt64{Int64: owner, Valid: true}, recode)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("owner: affected = %d, want 1", affected)
	}
}
