package database

import (
	"context"
	"database/sql"
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

	a1, al1, err := db.resolveAlbumArtist(ctx, tags)
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, al2, err := db.resolveAlbumArtist(ctx, tags)
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

	a1, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "The Beatles", Album: "Abbey Road"})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "  the   BEATLES ", Album: "abbey road"})
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

	// Two tracks, different performers, shared album_artist + album.
	a1, al1, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{
		Artist: "Performer One", AlbumArtist: "Various Artists", Album: "Comp",
	})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, al2, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{
		Artist: "Performer Two", AlbumArtist: "Various Artists", Album: "Comp",
	})
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if a1 != a2 {
		t.Errorf("album_artist did not win: artist ids %d vs %d", a1, a2)
	}
	if al1 != al2 {
		t.Errorf("same album resolved to different ids: %d vs %d", al1, al2)
	}
	if n := countRows(t, db, "artists"); n != 1 {
		t.Errorf("artists count = %d, want 1 (album_artist entity only)", n)
	}
}

func TestResolveAlbumArtist_EmptyBuckets(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Empty artist → unknown-artist bucket; empty album → unknown-album bucket
	// under that artist. Two untagged tracks group together, not apart.
	a1, al1, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	a2, al2, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{})
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
	_, al3, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Album: "Has A Title"})
	if err != nil {
		t.Fatalf("resolve 3: %v", err)
	}
	if al3 == al1 {
		t.Errorf("titled album collided with unknown-album bucket")
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
	_, alID, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y"})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if got := albumYear(t, db, alID); got.Valid {
		t.Errorf("year = %d, want NULL after yearless track", got.Int64)
	}

	// Second track supplies a year → it fills in.
	if _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y", Year: 1999}); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if got := albumYear(t, db, alID); !got.Valid || got.Int64 != 1999 {
		t.Errorf("year = %v, want 1999", got)
	}

	// Third track with a different year does NOT overwrite the representative one.
	if _, _, err := db.resolveAlbumArtist(ctx, AlbumArtistTags{Artist: "X", Album: "Y", Year: 2020}); err != nil {
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
			a, al, err := db.resolveAlbumArtist(ctx, tags)
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
		`UPDATE media_metadata SET artist_id = NULL, album_id = NULL`,
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

	// Every row now has both FKs set.
	var nullCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM media_metadata WHERE artist_id IS NULL OR album_id IS NULL`,
	).Scan(&nullCount); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("%d rows still have NULL FKs", nullCount)
	}

	// Two Discovery tracks share one album; Various Artists is its own artist;
	// the untagged track is the empty bucket → 3 distinct artists, 3 albums.
	if got := countRows(t, db, "artists"); got != 3 {
		t.Errorf("artists count = %d, want 3", got)
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

// fileEntityIDs reads the artist_id/album_id FKs of a file's metadata row.
func fileEntityIDs(t *testing.T, db *DB, hash string) (artistID, albumID sql.NullInt64) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT m.artist_id, m.album_id FROM media_metadata m
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

func TestListAlbumsByArtist_FiltersByNameReturnsEntityID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, "p3dd0001", "T1", "Boards of Canada", "Geogaddi", "")
	insertSearchFile(t, db, "p3dd0002", "T2", "Boards of Canada", "Geogaddi", "")
	insertSearchFile(t, db, "p3dd0003", "T3", "Someone Else", "Other Album", "")

	// Filter is by artist name (resolved to the entity), case-insensitively.
	albums, err := db.ListAlbumsByArtist(ctx, "boards of canada")
	if err != nil {
		t.Fatalf("ListAlbumsByArtist: %v", err)
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

	// Empty artist returns every album.
	all, err := db.ListAlbumsByArtist(ctx, "")
	if err != nil {
		t.Fatalf("ListAlbumsByArtist(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all albums = %d, want 2", len(all))
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
