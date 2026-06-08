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

	// Seed pre-entity rows (InsertFile leaves artist_id/album_id NULL).
	insertSearchFile(t, db, "hashaaa1", "T1", "Daft Punk", "Discovery", "")
	insertSearchFile(t, db, "hashaaa2", "T2", "Daft Punk", "Discovery", "") // same album
	insertSearchFile(t, db, "hashaaa3", "T3", "Performer", "Comp", "Various Artists")
	insertSearchFile(t, db, "hashaaa4", "T4", "", "", "") // fully untagged

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
