package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestNormalizeKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"The Beatles", "the beatles"},
		{"  The   Beatles  ", "the beatles"}, // trim + collapse internal whitespace
		{"DAFT PUNK", "daft punk"},           // lowercase
		{"Sigur\tRós", "sigur rós"},          // tab is whitespace; case-fold accent kept
		{"AC/DC", "ac/dc"},                   // slashes are not special
		{"é", "é"},                           // precomposed é stays é
		{"é", "é"},                          // decomposed e+combining-acute → NFC é
	}
	for _, c := range cases {
		if got := normalizeKey(c.in); got != c.want {
			t.Errorf("normalizeKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// countRows is a tiny test helper for asserting entity-table cardinality.
func countRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestResolveAlbumArtist_Idempotent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	tags := AlbumArtistTags{Artist: "Daft Punk", Album: "Discovery", Year: 2001}

	a1, _, al1, err := db.resolveAlbumArtist(ctx, tags)
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, _, al2, err := db.resolveAlbumArtist(ctx, tags)
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if a1 != a2 || al1 != al2 {
		t.Errorf("non-idempotent: (%d,%d) then (%d,%d)", a1, al1, a2, al2)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d, want 1", n)
	}
	if n := countRows(t, db, "albums"); n != 1 {
		t.Errorf("albums count = %d, want 1", n)
	}
}

func TestResolveAlbumArtist_CaseAndWhitespaceMerge(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	a1, _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "The Beatles", Album: "Abbey Road"})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "  the   BEATLES ", Album: "abbey road"})
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if a1 != a2 {
		t.Errorf("differently-cased spellings did not merge: %d vs %d", a1, a2)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d, want 1", n)
	}
	if n := countRows(t, db, "albums"); n != 1 {
		t.Errorf("albums count = %d, want 1", n)
	}
	// First spelling wins the display name.
	var name string
	if err := db.QueryRow(`SELECT name FROM artists WHERE id = ?`, a1).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "The Beatles" {
		t.Errorf("display name = %q, want %q (first spelling)", name, "The Beatles")
	}
}

func TestResolveAlbumArtist_VariousArtists(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Two tracks, different performers, shared album_artist + album. The album-artist
	// entity is the shared "Various Artists"; each track also gets its own performer
	// entity from its `artist` tag.
	aa1, p1, al1, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{
		Artist: "Performer One", AlbumArtist: "Various Artists", Album: "Comp",
	})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	aa2, p2, al2, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{
		Artist: "Performer Two", AlbumArtist: "Various Artists", Album: "Comp",
	})
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if aa1 != aa2 {
		t.Errorf("album_artist did not win: album-artist ids %d vs %d", aa1, aa2)
	}
	if al1 != al2 {
		t.Errorf("same album resolved to different ids: %d vs %d", al1, al2)
	}
	if p1 == p2 {
		t.Errorf("distinct performers collapsed to one entity: %d", p1)
	}
	if p1 == aa1 || p2 == aa1 {
		t.Errorf("performer entity equals the album-artist (Various Artists): p1=%d p2=%d aa=%d", p1, p2, aa1)
	}
	// Various Artists + Performer One + Performer Two = 3 distinct artists.
	if n := countRows(t, db, "artists"); n != 3 {
		t.Errorf("artists count = %d, want 3 (album-artist + two performers)", n)
	}
}

func TestResolveAlbumArtist_EmptyBuckets(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Empty artist → unknown-artist bucket; empty album → unknown-album bucket
	// under that artist. Two untagged tracks group together, not apart.
	a1, _, al1, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, _, al2, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{})
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if a1 != a2 || al1 != al2 {
		t.Errorf("untagged tracks did not share buckets: (%d,%d) vs (%d,%d)", a1, al1, a2, al2)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d, want 1", n)
	}
	if n := countRows(t, db, "albums"); n != 1 {
		t.Errorf("albums count = %d, want 1", n)
	}

	// A real album under the same (empty) artist is distinct from the unknown bucket.
	_, _, al3, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Album: "Has A Title"})
	if err != nil {
		t.Fatalf("resolve 3: %v", err)
	}
	if al3 == al1 {
		t.Errorf("titled album collided with unknown-album bucket")
	}
}

// mustExec runs a statement and fails the test on error.
func mustExec(t *testing.T, db *DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestResolveAlbumArtist_UnknownDefaults checks that untagged tracks resolve to
// the named default buckets ("Unknown artist" / "Other") with folded dedup keys,
// and that a track tagged literally that way converges on the same entities.
func TestResolveAlbumArtist_UnknownDefaults(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	aID, _, alID, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{})
	if err != nil {
		t.Fatalf("resolve untagged: %v", err)
	}
	var name, norm string
	if err := db.QueryRow(`SELECT name, norm_name FROM artists WHERE id = ?`, aID).Scan(&name, &norm); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != DefaultArtistName || norm != "unknown artist" {
		t.Errorf("artist = (%q,%q), want (%q,%q)", name, norm, DefaultArtistName, "unknown artist")
	}
	var title, ntitle string
	if err := db.QueryRow(`SELECT title, norm_title FROM albums WHERE id = ?`, alID).Scan(&title, &ntitle); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if title != DefaultAlbumTitle || ntitle != "other" {
		t.Errorf("album = (%q,%q), want (%q,%q)", title, ntitle, DefaultAlbumTitle, "other")
	}

	// Literal "Unknown Artist" / "OTHER" normalize onto the same bucket keys.
	aID2, _, alID2, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "Unknown Artist", Album: "OTHER"})
	if err != nil {
		t.Fatalf("resolve literal: %v", err)
	}
	if aID2 != aID || alID2 != alID {
		t.Errorf("literal did not converge: artist (%d vs %d), album (%d vs %d)", aID2, aID, alID2, alID)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d, want 1", n)
	}
}

// TestFoldUnknownBuckets simulates the post-migration-016, pre-fold state (bucket
// keys still ”) with a pre-existing literal that already holds the target key,
// and checks the fold relabels the bucket and merges the literal into it.
func TestFoldUnknownBuckets(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO artists (id, name, norm_name, created_at) VALUES
		(1, 'Unknown artist', '', 1),
		(2, 'Unknown Artist', 'unknown artist', 1)`)
	mustExec(t, db, `INSERT INTO albums (id, artist_id, title, norm_title, created_at) VALUES
		(10, 1, 'Other', '', 1),
		(11, 2, 'Other', 'other', 1)`)

	if err := db.FoldUnknownBuckets(ctx); err != nil {
		t.Fatalf("FoldUnknownBuckets: %v", err)
	}

	if n := countRows(t, db, "artists"); n != 1 {
		t.Fatalf("artists count = %d, want 1 after fold", n)
	}
	var name, norm string
	if err := db.QueryRow(`SELECT name, norm_name FROM artists`).Scan(&name, &norm); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Unknown artist" || norm != "unknown artist" {
		t.Errorf("folded artist = (%q,%q), want (Unknown artist, unknown artist)", name, norm)
	}
	if n := countRows(t, db, "albums"); n != 1 {
		t.Fatalf("albums count = %d, want 1 after fold", n)
	}
	var ntitle string
	if err := db.QueryRow(`SELECT norm_title FROM albums`).Scan(&ntitle); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if ntitle != "other" {
		t.Errorf("folded album norm_title = %q, want other", ntitle)
	}

	// Idempotent: a second run finds no '' keys and is a no-op.
	if err := db.FoldUnknownBuckets(ctx); err != nil {
		t.Fatalf("FoldUnknownBuckets re-run: %v", err)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d after re-run, want 1", n)
	}
}

// TestRequiredNames_RejectEmpty checks the migration-016 enforcement: the
// artists/albums triggers reject ” names, and media_metadata.title rejects
// ” and NULL.
func TestRequiredNames_RejectEmpty(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO artists (name, norm_name, created_at) VALUES ('', 'x', 1)`); err == nil {
		t.Error("empty artists.name insert: want error, got nil")
	}
	mustExec(t, db, `INSERT INTO artists (name, norm_name, created_at) VALUES ('A', 'a', 1)`)
	if _, err := db.Exec(`UPDATE artists SET name = '' WHERE norm_name = 'a'`); err == nil {
		t.Error("empty artists.name update: want error, got nil")
	}
	if _, err := db.Exec(
		`INSERT INTO albums (artist_id, title, norm_title, created_at)
		 SELECT id, '', 'x', 1 FROM artists WHERE norm_name = 'a'`); err == nil {
		t.Error("empty albums.title insert: want error, got nil")
	}

	// media_metadata.title CHECK / NOT NULL (needs a files row; nil meta leaves
	// no media_metadata row to collide with).
	f := newFile("ee00000000000000000000000000000000000000000000000000000000000000")
	if err := db.InsertFile(ctx, f, newUpload("z.mp3"), nil); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO media_metadata (file_id, title, extracted_at) VALUES (?, '', 1)`, f.ID); err == nil {
		t.Error("empty media_metadata.title: want CHECK error, got nil")
	}
	if _, err := db.Exec(`INSERT INTO media_metadata (file_id, title, extracted_at) VALUES (?, NULL, 1)`, f.ID); err == nil {
		t.Error("null media_metadata.title: want NOT NULL error, got nil")
	}
}

// albumYear reads the (nullable) representative year of an album row.
func albumYear(t *testing.T, db *DB, albumID int64) sql.NullInt64 {
	t.Helper()
	var y sql.NullInt64
	if err := db.QueryRow(`SELECT year FROM albums WHERE id = ?`, albumID).Scan(&y); err != nil {
		t.Fatalf("read album year: %v", err)
	}
	return y
}

func TestResolveAlbumArtist_YearFillNotOverwritten(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// First track has no year → album.year stays NULL.
	_, _, alID, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y"})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if got := albumYear(t, db, alID); got.Valid {
		t.Errorf("year = %d, want NULL after yearless track", got.Int64)
	}

	// Second track supplies a year → it fills in.
	if _, _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y", Year: 1999}); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if got := albumYear(t, db, alID); !got.Valid || got.Int64 != 1999 {
		t.Errorf("year = %v, want 1999", got)
	}

	// Third track with a different year does NOT overwrite the representative one.
	if _, _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y", Year: 2020}); err != nil {
		t.Fatalf("resolve 3: %v", err)
	}
	if got := albumYear(t, db, alID); !got.Valid || got.Int64 != 1999 {
		t.Errorf("year = %v, want 1999 (not overwritten)", got)
	}
}

func TestResolveAlbumArtist_Concurrent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	tags := AlbumArtistTags{Artist: "Concurrent Band", Album: "Race", Year: 2010}

	const n = 16
	ids := make([][2]int64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			a, _, al, err := db.resolveAlbumArtist(ctx, tags)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			ids[i] = [2]int64{a, al}
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got %v, want %v (single winner)", i, ids[i], ids[0])
		}
	}
	if got := countRows(t, db, "artists"); got != 1 {
		t.Errorf("artists count = %d, want 1", got)
	}
	if got := countRows(t, db, "albums"); got != 1 {
		t.Errorf("albums count = %d, want 1", got)
	}
}

func TestBackfillEntities(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Seed rows, then reset them to the legacy (pre-entity) state: InsertFile now
	// resolves entities inline (Phase 2), so to exercise the backfill we have to
	// strip the FKs and the entities it created, simulating rows imported before
	// the overlay existed.
	insertSearchFile(t, db, "hashaaa1", "T1", "Daft Punk", "Discovery", "")
	insertSearchFile(t, db, "hashaaa2", "T2", "Daft Punk", "Discovery", "") // same album
	insertSearchFile(t, db, "hashaaa3", "T3", "Performer", "Comp", "Various Artists")
	insertSearchFile(t, db, "hashaaa4", "T4", "", "", "") // fully untagged

	for _, stmt := range []string{
		`UPDATE media_metadata SET album_artist_id = NULL, artist_id = NULL, album_id = NULL`,
		`DELETE FROM albums`,
		`DELETE FROM artists`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("reset to legacy state (%q): %v", stmt, err)
		}
	}

	n, err := db.BackfillEntities(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 4 {
		t.Errorf("backfilled = %d, want 4", n)
	}

	// Every row now has all three FKs set.
	var nullCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM media_metadata WHERE album_artist_id IS NULL OR artist_id IS NULL OR album_id IS NULL`,
	).Scan(&nullCount); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("%d rows still have NULL FKs", nullCount)
	}

	// Two Discovery tracks share one (album-)artist + album; the comp track adds the
	// "Various Artists" album-artist AND a distinct "Performer" performer entity; the
	// untagged track is the unknown bucket → 4 distinct artists (Daft Punk, Various
	// Artists, Performer, Unknown artist), 3 albums (Discovery, Comp, Other).
	if got := countRows(t, db, "artists"); got != 4 {
		t.Errorf("artists count = %d, want 4", got)
	}
	if got := countRows(t, db, "albums"); got != 3 {
		t.Errorf("albums count = %d, want 3", got)
	}

	// Idempotent: a second pass resolves nothing.
	again, err := db.BackfillEntities(ctx)
	if err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	if again != 0 {
		t.Errorf("second backfill updated %d rows, want 0", again)
	}
}

// fileEntityIDs reads the album_artist_id/album_id FKs of a file's metadata row
// (the album-grouping artist, not the per-track performer).
func fileEntityIDs(t *testing.T, db *DB, hash string) (artistID, albumID sql.NullInt64) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT m.album_artist_id, m.album_id FROM media_metadata m
		 JOIN files f ON f.id = m.file_id WHERE f.hash = ?`, hash,
	).Scan(&artistID, &albumID); err != nil {
		t.Fatalf("read entity ids for %s: %v", hash, err)
	}
	return artistID, albumID
}

func TestInsertFile_ResolvesEntitiesInline(t *testing.T) {
	db := openMem(t)

	// Two tracks of the same album resolve to one shared album entity at import.
	insertSearchFile(t, db, "phase2aa1", "T1", "Daft Punk", "Discovery", "")
	insertSearchFile(t, db, "phase2aa2", "T2", "Daft Punk", "Discovery", "")

	a1, al1 := fileEntityIDs(t, db, "phase2aa1")
	a2, al2 := fileEntityIDs(t, db, "phase2aa2")
	if !a1.Valid || !al1.Valid {
		t.Fatalf("track 1 has NULL FKs: artist=%v album=%v", a1, al1)
	}
	if a1 != a2 || al1 != al2 {
		t.Errorf("same album/artist not shared: (%v,%v) vs (%v,%v)", a1, al1, a2, al2)
	}
	if got := countRows(t, db, "artists"); got != 1 {
		t.Errorf("artists count = %d, want 1", got)
	}
	if got := countRows(t, db, "albums"); got != 1 {
		t.Errorf("albums count = %d, want 1", got)
	}

	// The artist FK points at the right entity.
	var name string
	if err := db.QueryRow(`SELECT name FROM artists WHERE id = ?`, a1.Int64).Scan(&name); err != nil {
		t.Fatalf("read artist name: %v", err)
	}
	if name != "Daft Punk" {
		t.Errorf("artist name = %q, want %q", name, "Daft Punk")
	}
}

func TestUpdateFileMetadata_ReResolvesOnRename(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "phase2bb1", "T1", "Artist X", "Album Y", "")
	_, alBefore := fileEntityIDs(t, db, "phase2bb1")

	// Rename the album → album_id should move to a new entity titled "Album Z".
	if _, err := db.UpdateFileMetadata(ctx, "phase2bb1", MetadataPatch{Album: strPtr("Album Z")}); err != nil {
		t.Fatalf("patch album: %v", err)
	}
	_, alAfter := fileEntityIDs(t, db, "phase2bb1")
	if !alAfter.Valid {
		t.Fatalf("album_id is NULL after rename")
	}
	if alAfter == alBefore {
		t.Errorf("album_id did not change after rename (%v)", alAfter)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM albums WHERE id = ?`, alAfter.Int64).Scan(&title); err != nil {
		t.Fatalf("read album title: %v", err)
	}
	if title != "Album Z" {
		t.Errorf("new album title = %q, want %q", title, "Album Z")
	}
}

func TestUpdateFileMetadata_TitleOnlyKeepsEntities(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "phase2cc1", "T1", "Artist X", "Album Y", "")
	aBefore, alBefore := fileEntityIDs(t, db, "phase2cc1")

	// Patching only the track title must not re-resolve artist/album entities.
	if _, err := db.UpdateFileMetadata(ctx, "phase2cc1", MetadataPatch{Title: strPtr("Renamed Track")}); err != nil {
		t.Fatalf("patch title: %v", err)
	}
	aAfter, alAfter := fileEntityIDs(t, db, "phase2cc1")
	if aAfter != aBefore || alAfter != alBefore {
		t.Errorf("entities changed on title-only patch: (%v,%v) → (%v,%v)",
			aBefore, alBefore, aAfter, alAfter)
	}
}

// ---- Phase 3: entity-backed listings ----------------------------------------

func TestListArtists_StableIDsAndEntityBacked(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "p3aa0001", "T1", "Daft Punk", "Discovery", "")
	insertSearchFile(t, db, "p3aa0002", "T2", "Aphex Twin", "Drukqs", "")

	first, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("artists = %d, want 2", len(first))
	}
	// Every entry carries its entity id, and ordering is by lowercased name
	// (Aphex Twin before Daft Punk).
	if first[0].Name != "Aphex Twin" || first[1].Name != "Daft Punk" {
		t.Fatalf("order = [%q,%q], want [Aphex Twin, Daft Punk]", first[0].Name, first[1].Name)
	}
	for _, a := range first {
		if a.ID == 0 {
			t.Errorf("artist %q has zero ID", a.Name)
		}
	}

	// IDs are stable across calls.
	second, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists 2: %v", err)
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("artist %q ID changed: %d → %d", first[i].Name, first[i].ID, second[i].ID)
		}
	}
}

// TestUnifiedArtistBrowse_PerformerOnCompilation exercises the track-performer
// split: a "Various Artists" compilation surfaces its per-track performers as
// first-class, browsable artists, and the artist drill-down unions the
// album-artist and performer roles.
func TestUnifiedArtistBrowse_PerformerOnCompilation(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// A compilation: one album_artist ("Various Artists"), two different performers.
	insertSearchFile(t, db, "uc000001", "Halo", "Beyonce", "Greatest Comp", "Various Artists")
	insertSearchFile(t, db, "uc000002", "Hello", "Adele", "Greatest Comp", "Various Artists")
	// Plus a normal single-artist album by Beyonce.
	insertSearchFile(t, db, "uc000003", "Pretty Hurts", "Beyonce", "Beyonce", "")

	beyID, _, _ := db.LookupArtistID(ctx, "Beyonce")
	adeID, _, _ := db.LookupArtistID(ctx, "Adele")

	// 1. ListArtists includes the album-artist VA AND both performers, each with its
	//    union track_count (a row matching in both roles counts once).
	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	counts := map[string]int{}
	for _, a := range artists {
		counts[a.Name] = a.TrackCount
	}
	if counts["Various Artists"] != 2 { // album-artist of both comp tracks
		t.Errorf("Various Artists track_count = %d, want 2", counts["Various Artists"])
	}
	if counts["Beyonce"] != 2 { // 1 comp performance + 1 own album track
		t.Errorf("Beyonce track_count = %d, want 2", counts["Beyonce"])
	}
	if counts["Adele"] != 1 {
		t.Errorf("Adele track_count = %d, want 1", counts["Adele"])
	}

	// 2. A pure performer's drill-down lists the comp it appears on, counting only
	//    its own track on that album.
	adeAlbums, err := db.ListAlbumsByArtistID(ctx, adeID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID(Adele): %v", err)
	}
	if len(adeAlbums) != 1 || adeAlbums[0].Title != "Greatest Comp" || adeAlbums[0].TrackCount != 1 {
		t.Errorf("Adele albums = %+v, want one 'Greatest Comp' with 1 track", adeAlbums)
	}

	// 3. An album-artist who also performs on a comp sees both: their own album
	//    (full count) and the comp (their track only).
	beyAlbums, err := db.ListAlbumsByArtistID(ctx, beyID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID(Beyonce): %v", err)
	}
	beyCounts := map[string]int{}
	for _, al := range beyAlbums {
		beyCounts[al.Title] = al.TrackCount
	}
	if beyCounts["Beyonce"] != 1 || beyCounts["Greatest Comp"] != 1 {
		t.Errorf("Beyonce albums = %+v, want Beyonce:1 and Greatest Comp:1", beyCounts)
	}

	// 4. The comp's track list shows each track's performer, not the album-artist.
	compID, _, _ := db.LookupAlbumID(ctx, "Various Artists", "Greatest Comp")
	tracks, err := db.ListTracksByAlbumID(ctx, compID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumID: %v", err)
	}
	perf := map[string]string{}
	for _, tr := range tracks {
		perf[tr.Title] = tr.ArtistName
	}
	if perf["Halo"] != "Beyonce" || perf["Hello"] != "Adele" {
		t.Errorf("performers = %v, want Halo:Beyonce Hello:Adele", perf)
	}
}

func TestListArtists_MergesCaseVariants(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Two spellings that differ only by case/whitespace collapse to one entity,
	// so they list as a single artist with the combined track count.
	insertSearchFile(t, db, "p3bb0001", "T1", "The Beatles", "Abbey Road", "")
	insertSearchFile(t, db, "p3bb0002", "T2", "the  beatles", "Abbey Road", "")

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 {
		t.Fatalf("artists = %d, want 1 (case variants merged)", len(artists))
	}
	if artists[0].TrackCount != 2 {
		t.Errorf("track_count = %d, want 2", artists[0].TrackCount)
	}
	if artists[0].Name != "The Beatles" {
		t.Errorf("name = %q, want %q (first spelling)", artists[0].Name, "The Beatles")
	}
}

func TestListArtists_ExcludesOrphanedByRename(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "p3cc0001", "T1", "Old Name", "Album", "")
	// Rename the artist; the old entity now has no tracks.
	if _, err := db.UpdateFileMetadata(ctx, "p3cc0001", MetadataPatch{Artist: strPtr("New Name")}); err != nil {
		t.Fatalf("patch artist: %v", err)
	}

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "New Name" {
		t.Fatalf("artists = %v, want only [New Name] (orphan excluded)", artists)
	}
	// The orphan entity row still exists but is invisible to the listing.
	if got := countRows(t, db, "artists"); got != 2 {
		t.Errorf("artists table rows = %d, want 2 (orphan retained)", got)
	}
}

func TestListAlbumsByArtistID_FiltersByIDReturnsEntityID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "p3dd0001", "T1", "Boards of Canada", "Geogaddi", "")
	insertSearchFile(t, db, "p3dd0002", "T2", "Boards of Canada", "Geogaddi", "")
	insertSearchFile(t, db, "p3dd0003", "T3", "Someone Else", "Other Album", "")

	// Filter is by the artist's stable surrogate id.
	bocID, found, err := db.LookupArtistID(ctx, "boards of canada")
	if err != nil || !found {
		t.Fatalf("LookupArtistID: found=%v err=%v", found, err)
	}
	albums, err := db.ListAlbumsByArtistID(ctx, bocID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID: %v", err)
	}
	if len(albums) != 1 {
		t.Fatalf("albums = %d, want 1", len(albums))
	}
	if albums[0].Title != "Geogaddi" || albums[0].TrackCount != 2 {
		t.Errorf("album = %q (%d tracks), want Geogaddi (2)", albums[0].Title, albums[0].TrackCount)
	}
	if albums[0].ID == 0 {
		t.Error("album entry has zero ID")
	}
	if albums[0].ArtistName != "Boards of Canada" {
		t.Errorf("artist_name = %q, want %q", albums[0].ArtistName, "Boards of Canada")
	}

	// A different artist's id yields only that artist's albums.
	otherID, _, _ := db.LookupArtistID(ctx, "Someone Else")
	other, err := db.ListAlbumsByArtistID(ctx, otherID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID(other): %v", err)
	}
	if len(other) != 1 || other[0].Title != "Other Album" {
		t.Errorf("other artist albums = %+v, want 1 (Other Album)", other)
	}

	// An unknown id yields no albums (not an error).
	none, err := db.ListAlbumsByArtistID(ctx, 999999)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID(unknown): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown artist id albums = %d, want 0", len(none))
	}
}

// ---- Phase 4: cover re-key + backfill ---------------------------------------

func TestBackfillCoverEntities(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// A track materializes the artist/album entities the covers resolve onto.
	insertSearchFile(t, db, "cover001", "T1", "Pink Floyd", "Animals", "")
	albumID, found, err := db.LookupAlbumID(ctx, "Pink Floyd", "Animals")
	if err != nil || !found {
		t.Fatalf("LookupAlbumID: found=%v err=%v", found, err)
	}
	artistID, found, err := db.LookupArtistID(ctx, "Pink Floyd")
	if err != nil || !found {
		t.Fatalf("LookupArtistID: found=%v err=%v", found, err)
	}

	// Seed legacy string-keyed cover rows into the *_old tables that migration
	// 014 set aside (still present and empty after Open).
	if _, err := db.Exec(
		`INSERT INTO album_images_old (album_artist, album_title, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES ('Pink Floyd', 'Animals', 'bk/original.jpg', 'image/jpeg', 1000, 'bk', '.jpg', 1)`,
	); err != nil {
		t.Fatalf("seed album_images_old: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO artist_images_old (artist_name, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES ('Pink Floyd', 'art.png', 'image/png', 2000, NULL, NULL, 0)`,
	); err != nil {
		t.Fatalf("seed artist_images_old: %v", err)
	}
	// A cover whose album has no entity (no tracks) — must be dropped, not crash.
	if _, err := db.Exec(
		`INSERT INTO album_images_old (album_artist, album_title, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES ('Ghost', 'Nobody', 'x/original.jpg', 'image/jpeg', 3000, 'x', '.jpg', 0)`,
	); err != nil {
		t.Fatalf("seed orphan cover: %v", err)
	}

	if err := db.BackfillCoverEntities(ctx); err != nil {
		t.Fatalf("BackfillCoverEntities: %v", err)
	}

	// The album cover survived onto its entity id, preserving the variant fields.
	bk, ext, ready, foundC, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || !foundC {
		t.Fatalf("GetAlbumCoverStatus: found=%v err=%v", foundC, err)
	}
	if bk != "bk" || ext != ".jpg" || !ready {
		t.Errorf("migrated album cover = (%q,%q,ready=%v), want (bk,.jpg,true)", bk, ext, ready)
	}
	// The artist cover survived onto its entity id.
	if _, _, foundA, err := db.GetArtistImage(ctx, artistID); err != nil || !foundA {
		t.Errorf("artist cover not migrated: found=%v err=%v", foundA, err)
	}
	// The unresolved (orphan) cover was dropped, not migrated.
	if n := countRows(t, db, "album_images"); n != 1 {
		t.Errorf("album_images rows = %d, want 1 (orphan dropped)", n)
	}
	// The *_old leftovers are gone.
	if ok, _ := db.tableExists(ctx, "album_images_old"); ok {
		t.Error("album_images_old not dropped after backfill")
	}
	if ok, _ := db.tableExists(ctx, "artist_images_old"); ok {
		t.Error("artist_images_old not dropped after backfill")
	}

	// Idempotent: a second run is a clean no-op (tables already gone).
	if err := db.BackfillCoverEntities(ctx); err != nil {
		t.Fatalf("BackfillCoverEntities re-run: %v", err)
	}
}

func TestAlbumImage_RoundTripByID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	if err := db.UpsertAlbumImage(ctx, albumID, "obj.jpg", "image/jpeg", 1000); err != nil {
		t.Fatalf("UpsertAlbumImage: %v", err)
	}
	key, mime, found, err := db.GetAlbumImage(ctx, albumID)
	if err != nil || !found {
		t.Fatalf("GetAlbumImage: found=%v err=%v", found, err)
	}
	if key != "obj.jpg" || mime != "image/jpeg" {
		t.Errorf("got (%q,%q), want (obj.jpg, image/jpeg)", key, mime)
	}
	// A different album id has no image.
	other, err := db.ResolveAlbumID(ctx, "Artist", "Other")
	if err != nil {
		t.Fatalf("resolve other: %v", err)
	}
	if _, _, found, _ := db.GetAlbumImage(ctx, other); found {
		t.Error("unrelated album reported an image")
	}
}

// ---- Phase 5: rename --------------------------------------------------------

func TestRenameArtist_TracksAndCoverFollow(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "ren00001", "T1", "Old Name", "Album", "")
	artistID, _, err := db.LookupArtistID(ctx, "Old Name")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := db.UpsertArtistImage(ctx, artistID, "art.png", "image/png", 1); err != nil {
		t.Fatalf("upsert artist image: %v", err)
	}

	if err := db.RenameArtist(ctx, artistID, "New Name"); err != nil {
		t.Fatalf("RenameArtist: %v", err)
	}

	// Same id resolves under the new name; the old name no longer resolves.
	if id, found, _ := db.LookupArtistID(ctx, "New Name"); !found || id != artistID {
		t.Errorf("new name resolves to id=%d found=%v, want %d/true", id, found, artistID)
	}
	if _, found, _ := db.LookupArtistID(ctx, "Old Name"); found {
		t.Error("old name still resolves after rename")
	}
	// The cover (keyed by artist id) is still attached.
	if _, _, found, _ := db.GetArtistImage(ctx, artistID); !found {
		t.Error("artist cover lost after rename")
	}
	// The listing shows the new display name with the track still attached.
	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "New Name" || artists[0].TrackCount != 1 {
		t.Errorf("listing = %+v, want one [New Name, 1 track]", artists)
	}
}

func TestRenameArtist_CasingOnlyAllowed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "ren00002", "T1", "the beatles", "Abbey Road", "")
	id, _, _ := db.LookupArtistID(ctx, "the beatles")

	// Same normalized key, different display — allowed (not a self-conflict).
	if err := db.RenameArtist(ctx, id, "The Beatles"); err != nil {
		t.Fatalf("casing rename: %v", err)
	}
	var name string
	db.QueryRow(`SELECT name FROM artists WHERE id = ?`, id).Scan(&name)
	if name != "The Beatles" {
		t.Errorf("display name = %q, want The Beatles", name)
	}
}

func TestRenameArtist_Conflict(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "ren00003", "T1", "Artist A", "X", "")
	insertSearchFile(t, db, "ren00004", "T2", "Artist B", "Y", "")
	idA, _, _ := db.LookupArtistID(ctx, "Artist A")

	if err := db.RenameArtist(ctx, idA, "Artist B"); !errors.Is(err, ErrNameConflict) {
		t.Errorf("rename onto existing name = %v, want ErrNameConflict", err)
	}
}

func TestRenameArtist_NotFound(t *testing.T) {
	db := openMem(t)
	if err := db.RenameArtist(context.Background(), 99999, "Whoever"); !errors.Is(err, ErrEntityNotFound) {
		t.Errorf("rename missing id = %v, want ErrEntityNotFound", err)
	}
}

func TestRenameAlbum_CoverFollowsAndConflictScoped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "ren00005", "T1", "Artist", "Old Title", "")
	insertSearchFile(t, db, "ren00006", "T2", "Artist", "Other", "")
	albumID, _, _ := db.LookupAlbumID(ctx, "Artist", "Old Title")
	if err := db.UpsertAlbumImage(ctx, albumID, "cover.jpg", "image/jpeg", 1); err != nil {
		t.Fatalf("upsert album image: %v", err)
	}

	// Rename succeeds; the cover (album_id keyed) follows.
	if err := db.RenameAlbum(ctx, albumID, "New Title"); err != nil {
		t.Fatalf("RenameAlbum: %v", err)
	}
	if id, found, _ := db.LookupAlbumID(ctx, "Artist", "New Title"); !found || id != albumID {
		t.Errorf("new title resolves to %d/%v, want %d/true", id, found, albumID)
	}
	if _, _, found, _ := db.GetAlbumImage(ctx, albumID); !found {
		t.Error("album cover lost after rename")
	}

	// Renaming onto a sibling album's title (same artist) conflicts.
	if err := db.RenameAlbum(ctx, albumID, "Other"); !errors.Is(err, ErrNameConflict) {
		t.Errorf("rename onto sibling title = %v, want ErrNameConflict", err)
	}
}

func TestRenameAlbum_SameTitleDifferentArtistAllowed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "ren00007", "T1", "Artist One", "Greatest Hits", "")
	insertSearchFile(t, db, "ren00008", "T2", "Artist Two", "Misc", "")
	two, _, _ := db.LookupAlbumID(ctx, "Artist Two", "Misc")

	// "Greatest Hits" exists under Artist One, but the conflict is scoped to the
	// album's own artist, so renaming Artist Two's album to it is fine.
	if err := db.RenameAlbum(ctx, two, "Greatest Hits"); err != nil {
		t.Errorf("cross-artist same title rename = %v, want success", err)
	}
}

// ---- Phase 5: merge ---------------------------------------------------------

func tracksUnderAlbum(t *testing.T, db *DB, albumID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_metadata WHERE album_id = ?`, albumID).Scan(&n); err != nil {
		t.Fatalf("count tracks under album: %v", err)
	}
	return n
}

func TestMergeArtists_NonCollidingMovesEverything(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00a001", "T1", "Artist B", "B Album", "")
	insertSearchFile(t, db, "mg00a002", "T2", "Artist A", "A Album", "")
	bID, _, _ := db.LookupArtistID(ctx, "Artist B")
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")

	if err := db.MergeArtists(ctx, bID, aID); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	if _, found, _ := db.LookupArtistID(ctx, "Artist B"); found {
		t.Error("source artist survived the merge")
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists = %d, want 1", n)
	}
	// No track (in either role) or album still references the deleted source.
	var dangling int
	db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM media_metadata WHERE album_artist_id = ?1 OR artist_id = ?1)
		+ (SELECT COUNT(*) FROM albums WHERE artist_id = ?1)`, bID).Scan(&dangling)
	if dangling != 0 {
		t.Errorf("%d rows still reference the source artist", dangling)
	}
	// Target now owns both albums and both tracks.
	albums, _ := db.ListAlbumsByArtistID(ctx, aID)
	if len(albums) != 2 {
		t.Errorf("target albums = %d, want 2", len(albums))
	}
	artists, _ := db.ListArtists(ctx)
	if len(artists) != 1 || artists[0].TrackCount != 2 {
		t.Errorf("listing = %+v, want one artist with 2 tracks", artists)
	}
}

func TestMergeArtists_CollidingAlbumsCollapse(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00b001", "TA", "Artist A", "Greatest Hits", "")
	insertSearchFile(t, db, "mg00b002", "TB", "Artist B", "greatest hits", "") // collides (norm)
	insertSearchFile(t, db, "mg00b003", "TC", "Artist B", "Other", "")         // non-colliding
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")
	bID, _, _ := db.LookupArtistID(ctx, "Artist B")
	aHits, _, _ := db.LookupAlbumID(ctx, "Artist A", "Greatest Hits")

	if err := db.MergeArtists(ctx, bID, aID); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	// Two albums under A: the collapsed "Greatest Hits" (2 tracks) + moved "Other".
	albums, _ := db.ListAlbumsByArtistID(ctx, aID)
	if len(albums) != 2 {
		t.Fatalf("target albums = %d, want 2 (collapsed + moved)", len(albums))
	}
	if got := tracksUnderAlbum(t, db, aHits); got != 2 {
		t.Errorf("Greatest Hits tracks = %d, want 2 (collapsed)", got)
	}
	artists, _ := db.ListArtists(ctx)
	if len(artists) != 1 || artists[0].TrackCount != 3 {
		t.Errorf("listing = %+v, want one artist with 3 tracks", artists)
	}
}

func TestMergeArtists_MovesCoversWhenTargetLacks(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00c001", "TA", "Artist A", "Shared", "")
	insertSearchFile(t, db, "mg00c002", "TB", "Artist B", "shared", "") // colliding album
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")
	bID, _, _ := db.LookupArtistID(ctx, "Artist B")
	aShared, _, _ := db.LookupAlbumID(ctx, "Artist A", "Shared")
	bShared, _, _ := db.LookupAlbumID(ctx, "Artist B", "shared")

	// Source has covers; target has none.
	db.UpsertArtistImage(ctx, bID, "bart.png", "image/png", 1)
	db.UpsertAlbumImage(ctx, bShared, "balb.jpg", "image/jpeg", 1)

	if err := db.MergeArtists(ctx, bID, aID); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	if _, _, found, _ := db.GetArtistImage(ctx, aID); !found {
		t.Error("target did not inherit the source artist cover")
	}
	if _, _, found, _ := db.GetAlbumImage(ctx, aShared); !found {
		t.Error("collapsed target album did not inherit the source album cover")
	}
}

func TestMergeArtists_TargetCoverWins(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00d001", "TA", "Artist A", "X", "")
	insertSearchFile(t, db, "mg00d002", "TB", "Artist B", "Y", "")
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")
	bID, _, _ := db.LookupArtistID(ctx, "Artist B")
	db.UpsertArtistImage(ctx, aID, "aart.png", "image/png", 1)
	db.UpsertArtistImage(ctx, bID, "bart.png", "image/png", 1)

	if err := db.MergeArtists(ctx, bID, aID); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	key, _, found, _ := db.GetArtistImage(ctx, aID)
	if !found || key != "aart.png" {
		t.Errorf("artist cover = (%q, found=%v), want the target's own (aart.png)", key, found)
	}
}

func TestMergeArtists_SelfAndNotFound(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertSearchFile(t, db, "mg00e001", "T1", "Artist A", "X", "")
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")

	if err := db.MergeArtists(ctx, aID, aID); !errors.Is(err, ErrMergeSelf) {
		t.Errorf("self-merge = %v, want ErrMergeSelf", err)
	}
	if err := db.MergeArtists(ctx, aID, 99999); !errors.Is(err, ErrEntityNotFound) {
		t.Errorf("missing target = %v, want ErrEntityNotFound", err)
	}
	if err := db.MergeArtists(ctx, 88888, aID); !errors.Is(err, ErrEntityNotFound) {
		t.Errorf("missing source = %v, want ErrEntityNotFound", err)
	}
}

func TestMergeAlbums_SameArtist(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00f001", "T1", "Artist", "Old Edition", "")
	insertSearchFile(t, db, "mg00f002", "T2", "Artist", "New Edition", "")
	oldID, _, _ := db.LookupAlbumID(ctx, "Artist", "Old Edition")
	newID, _, _ := db.LookupAlbumID(ctx, "Artist", "New Edition")
	db.UpsertAlbumImage(ctx, oldID, "old.jpg", "image/jpeg", 1) // source cover; target lacks

	if err := db.MergeAlbums(ctx, oldID, newID); err != nil {
		t.Fatalf("MergeAlbums: %v", err)
	}
	if _, found, _ := db.LookupAlbumID(ctx, "Artist", "Old Edition"); found {
		t.Error("source album survived")
	}
	if got := tracksUnderAlbum(t, db, newID); got != 2 {
		t.Errorf("target tracks = %d, want 2", got)
	}
	if _, _, found, _ := db.GetAlbumImage(ctx, newID); !found {
		t.Error("target did not inherit the source cover")
	}
}

func TestMergeAlbums_CrossArtistRepointsAlbumArtist(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "mg00g001", "T1", "Artist Y", "Dup", "")
	insertSearchFile(t, db, "mg00g002", "T2", "Artist Z", "Real", "")
	yDup, _, _ := db.LookupAlbumID(ctx, "Artist Y", "Dup")
	zReal, _, _ := db.LookupAlbumID(ctx, "Artist Z", "Real")
	yID, _, _ := db.LookupArtistID(ctx, "Artist Y")
	zID, _, _ := db.LookupArtistID(ctx, "Artist Z")

	if err := db.MergeAlbums(ctx, yDup, zReal); err != nil {
		t.Fatalf("MergeAlbums: %v", err)
	}
	// The moved track points at the target album and its album-artist (Z), but its
	// performer (artist_id) stays Y — moving a track between albums never rewrites
	// who performed it.
	var albumArtistID, performerID, albumID int64
	if err := db.QueryRow(
		`SELECT m.album_artist_id, m.artist_id, m.album_id FROM media_metadata m
		 JOIN files f ON f.id = m.file_id WHERE f.hash = ?`, "mg00g001").
		Scan(&albumArtistID, &performerID, &albumID); err != nil {
		t.Fatalf("read moved track: %v", err)
	}
	if albumArtistID != zID || albumID != zReal {
		t.Errorf("moved track = (album-artist %d, album %d), want (%d, %d)", albumArtistID, albumID, zID, zReal)
	}
	if performerID != yID {
		t.Errorf("performer = %d, want %d (unchanged by album merge)", performerID, yID)
	}
}

func TestMergeAlbums_SelfAndNotFound(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertSearchFile(t, db, "mg00h001", "T1", "Artist", "A", "")
	id, _, _ := db.LookupAlbumID(ctx, "Artist", "A")

	if err := db.MergeAlbums(ctx, id, id); !errors.Is(err, ErrMergeSelf) {
		t.Errorf("self-merge = %v, want ErrMergeSelf", err)
	}
	if err := db.MergeAlbums(ctx, id, 99999); !errors.Is(err, ErrEntityNotFound) {
		t.Errorf("missing target = %v, want ErrEntityNotFound", err)
	}
}

// ---- merge preview (read-only dry-run) --------------------------------------

func TestMergeArtistsPreview_CountsAndNoMutation(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "pv00a001", "TA", "Artist A", "Greatest Hits", "")
	insertSearchFile(t, db, "pv00a002", "TB", "Artist B", "greatest hits", "") // collides (norm)
	insertSearchFile(t, db, "pv00a003", "TC", "Artist B", "Solo", "")           // non-colliding
	aID, _, _ := db.LookupArtistID(ctx, "Artist A")
	bID, _, _ := db.LookupArtistID(ctx, "Artist B")
	// Source has a cover, target does not → the cover would move.
	if err := db.UpsertArtistImage(ctx, bID, "b.png", "image/png", 1); err != nil {
		t.Fatalf("UpsertArtistImage: %v", err)
	}

	p, err := db.MergeArtistsPreview(ctx, bID, aID)
	if err != nil {
		t.Fatalf("MergeArtistsPreview: %v", err)
	}
	if p.TracksMoved != 2 {
		t.Errorf("tracks_moved = %d, want 2", p.TracksMoved)
	}
	if p.AlbumsCollapsed != 1 || len(p.CollapsedTitles) != 1 || p.CollapsedTitles[0] != "greatest hits" {
		t.Errorf("collapsed = %d %v, want 1 [greatest hits]", p.AlbumsCollapsed, p.CollapsedTitles)
	}
	if p.AlbumsMoved != 1 {
		t.Errorf("albums_moved = %d, want 1 (Solo)", p.AlbumsMoved)
	}
	if !p.SourceHasCover || p.TargetHasCover {
		t.Errorf("cover flags = src %v tgt %v, want src true tgt false", p.SourceHasCover, p.TargetHasCover)
	}
	if p.FromLabel != "Artist B" || p.IntoLabel != "Artist A" {
		t.Errorf("labels = %q→%q, want Artist B→Artist A", p.FromLabel, p.IntoLabel)
	}

	// The preview must not have mutated anything.
	if n := countRows(t, db, "artists"); n != 2 {
		t.Errorf("artists after preview = %d, want 2 (no mutation)", n)
	}
	if _, found, _ := db.LookupArtistID(ctx, "Artist B"); !found {
		t.Error("source artist gone after preview (mutated)")
	}
}

func TestMergeArtistsPreview_SelfAndNotFound(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertSearchFile(t, db, "pv00b001", "T1", "Artist", "A", "")
	id, _, _ := db.LookupArtistID(ctx, "Artist")

	if _, err := db.MergeArtistsPreview(ctx, id, id); !errors.Is(err, ErrMergeSelf) {
		t.Errorf("self = %v, want ErrMergeSelf", err)
	}
	if _, err := db.MergeArtistsPreview(ctx, id, 99999); !errors.Is(err, ErrEntityNotFound) {
		t.Errorf("missing target = %v, want ErrEntityNotFound", err)
	}
}

func TestMergeAlbumsPreview_OrphanAndNoMutation(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Artist B is a pure album-artist label (its one track is performed by someone
	// else), so it has no performer credit to survive the move. Merging its only
	// album cross-artist therefore orphans B. (A single-artist album-artist would
	// keep its performer credit and NOT orphan — see TestMergeAlbums_Cross... above.)
	insertSearchFile(t, db, "pv00c001", "T1", "Performer C", "Old", "Artist B")
	insertSearchFile(t, db, "pv00c002", "T2", "Artist A", "New", "")
	fromID, _, _ := db.LookupAlbumID(ctx, "Artist B", "Old")
	intoID, _, _ := db.LookupAlbumID(ctx, "Artist A", "New")

	p, err := db.MergeAlbumsPreview(ctx, fromID, intoID)
	if err != nil {
		t.Fatalf("MergeAlbumsPreview: %v", err)
	}
	if p.TracksMoved != 1 {
		t.Errorf("tracks_moved = %d, want 1", p.TracksMoved)
	}
	if !p.SourceArtistOrphaned {
		t.Error("source_artist_orphaned = false, want true (Artist B left empty)")
	}
	if p.FromLabel != "Old" || p.IntoLabel != "New" {
		t.Errorf("labels = %q→%q, want Old→New", p.FromLabel, p.IntoLabel)
	}

	// No mutation: the source album still exists with its track.
	if _, found, _ := db.LookupAlbumID(ctx, "Artist B", "Old"); !found {
		t.Error("source album gone after preview (mutated)")
	}
	if got := tracksUnderAlbum(t, db, fromID); got != 1 {
		t.Errorf("source album tracks after preview = %d, want 1", got)
	}
}

func TestMergeAlbumsPreview_SameArtistNotOrphaned(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Two albums of the same artist: merging one into the other never orphans.
	insertSearchFile(t, db, "pv00d001", "T1", "Artist", "Old", "")
	insertSearchFile(t, db, "pv00d002", "T2", "Artist", "New", "")
	fromID, _, _ := db.LookupAlbumID(ctx, "Artist", "Old")
	intoID, _, _ := db.LookupAlbumID(ctx, "Artist", "New")

	p, err := db.MergeAlbumsPreview(ctx, fromID, intoID)
	if err != nil {
		t.Fatalf("MergeAlbumsPreview: %v", err)
	}
	if p.SourceArtistOrphaned {
		t.Error("source_artist_orphaned = true, want false (same artist keeps the target album)")
	}
}
