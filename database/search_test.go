package database

import (
	"context"
	"database/sql"
	"testing"
)

// ---- helpers -----------------------------------------------------------------

// insertSearchFile inserts a file with specific title, artist, album, and
// albumArtist metadata and returns the file id.
func insertSearchFile(t *testing.T, db *DB, hash, title, artist, album, albumArtist string) int64 {
	t.Helper()
	f := newFile(hash)
	meta := &MediaMetadata{
		Title:       title,
		ExtractedAt: 1700000000,
	}
	if artist != "" {
		meta.Artist = sql.NullString{String: artist, Valid: true}
	}
	if album != "" {
		meta.Album = sql.NullString{String: album, Valid: true}
	}
	if albumArtist != "" {
		meta.AlbumArtist = sql.NullString{String: albumArtist, Valid: true}
	}
	if err := db.InsertFile(context.Background(), f, newUpload(hash[:8]+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	return f.ID
}

// ---- DB.Search ---------------------------------------------------------------

func TestSearch_EmptyQuery_ReturnsEmpty(t *testing.T) {
	db := openMem(t)
	// Insert a file so the DB is non-empty — an empty q must still return nothing.
	insertSearchFile(t, db, hash64("s1"), "Title One", "Artist One", "Album One", "")

	cases := []string{"", "   ", "\t"}
	for _, q := range cases {
		t.Run("q="+q, func(t *testing.T) {
			res, err := db.Search(context.Background(), q)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			if res == nil {
				t.Fatal("Search returned nil result")
			}
			if len(res.Artists) != 0 || len(res.Albums) != 0 || len(res.Tracks) != 0 {
				t.Errorf("Search(%q) returned non-empty results: artists=%d albums=%d tracks=%d",
					q, len(res.Artists), len(res.Albums), len(res.Tracks))
			}
		})
	}
}

func TestSearch_MatchesTrackTitle(t *testing.T) {
	db := openMem(t)
	// "Moonlight Sonata" contains "Moonlight"
	insertSearchFile(t, db, hash64("trk1"), "Moonlight Sonata", "Beethoven", "Piano Sonatas", "")
	insertSearchFile(t, db, hash64("trk2"), "Ode to Joy", "Beethoven", "Symphony No. 9", "")

	res, err := db.Search(context.Background(), "Moonlight")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(res.Tracks))
	}
	if res.Tracks[0].Title != "Moonlight Sonata" {
		t.Errorf("track title = %q, want Moonlight Sonata", res.Tracks[0].Title)
	}
	// The non-matching track must not appear.
	if len(res.Tracks) > 1 {
		t.Errorf("got extra tracks: %v", res.Tracks)
	}
}

func TestSearch_MatchesAlbumBySubstring(t *testing.T) {
	// Regression: album-by-substring was the bug fixed in the previous commit.
	db := openMem(t)
	insertSearchFile(t, db, hash64("alb1"), "Track A", "Artist A", "Greatest Hits", "")
	insertSearchFile(t, db, hash64("alb2"), "Track B", "Artist B", "Live at the Bowl", "")

	res, err := db.Search(context.Background(), "Greatest")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Albums) != 1 {
		t.Fatalf("albums = %d, want 1", len(res.Albums))
	}
	if res.Albums[0].Title != "Greatest Hits" {
		t.Errorf("album title = %q, want Greatest Hits", res.Albums[0].Title)
	}
}

func TestSearch_MatchesArtistBySubstring(t *testing.T) {
	db := openMem(t)
	// Two artists; only one contains "Beatles"
	insertSearchFile(t, db, hash64("ar1"), "Come Together", "The Beatles", "Abbey Road", "The Beatles")
	insertSearchFile(t, db, hash64("ar2"), "Stairway to Heaven", "Led Zeppelin", "Led Zeppelin IV", "Led Zeppelin")

	res, err := db.Search(context.Background(), "Beatles")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Artists) != 1 {
		t.Fatalf("artists = %d, want 1", len(res.Artists))
	}
	if res.Artists[0].Name != "The Beatles" {
		t.Errorf("artist name = %q, want The Beatles", res.Artists[0].Name)
	}
}

func TestSearch_PercentSign_TreatedAsLiteral(t *testing.T) {
	// A query of "%" must NOT match all files — it is a LIKE metachar that should
	// be escaped to "\%", which matches only files with "%" in their tags.
	db := openMem(t)
	insertSearchFile(t, db, hash64("pct1"), "Normal Track", "Normal Artist", "Normal Album", "")
	insertSearchFile(t, db, hash64("pct2"), "100% Pure", "Some Band", "100% Album", "")

	// Searching for "%" should only match the file that literally contains "%".
	res, err := db.Search(context.Background(), "%")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Only the "100% Pure" track and "100% Album" album should match.
	// Crucially, this must NOT return all tracks.
	for _, tr := range res.Tracks {
		if tr.Title == "Normal Track" {
			t.Errorf("'%%' query matched Normal Track — metachar not escaped")
		}
	}
	for _, al := range res.Albums {
		if al.Title == "Normal Album" {
			t.Errorf("'%%' query matched Normal Album — metachar not escaped")
		}
	}
}

func TestSearch_Underscore_TreatedAsLiteral(t *testing.T) {
	// A query of "_" is a LIKE metachar (single-char wildcard) — must be escaped.
	db := openMem(t)
	insertSearchFile(t, db, hash64("un1"), "Short Title", "Short Artist", "Short Album", "")
	// A file whose title literally contains "_"
	insertSearchFile(t, db, hash64("un2"), "Track_One", "Artist_Two", "Album_Three", "")

	res, err := db.Search(context.Background(), "_")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// After escaping, "_" matches only the literal underscore character.
	// Positive: "Track_One" must appear (it contains a literal "_").
	found := false
	for _, tr := range res.Tracks {
		if tr.Title == "Track_One" {
			found = true
		}
		if tr.Title == "Short Title" {
			t.Errorf("'_' query matched 'Short Title' — underscore not escaped")
		}
	}
	if !found {
		t.Errorf("'_' query did not return 'Track_One' — literal underscore not matched")
	}
}

func TestSearch_CaseInsensitive(t *testing.T) {
	db := openMem(t)
	insertSearchFile(t, db, hash64("ci1"), "The Dark Side of the Moon", "Pink Floyd", "The Dark Side of the Moon", "Pink Floyd")

	for _, q := range []string{"dark side", "DARK SIDE", "Dark Side", "dArK sIdE"} {
		t.Run(q, func(t *testing.T) {
			res, err := db.Search(context.Background(), q)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			if len(res.Tracks) != 1 {
				t.Errorf("Search(%q) tracks = %d, want 1 (case-insensitive match)", q, len(res.Tracks))
			}
		})
	}
}

func TestSearch_SoftDeletedFileExcluded(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("del1")
	insertSearchFile(t, db, h, "Deleted Track", "Ghost Artist", "Ghost Album", "")

	if _, _, err := db.SoftDeleteFileByHash(ctx, h); err != nil {
		t.Fatalf("SoftDeleteFileByHash: %v", err)
	}

	res, err := db.Search(ctx, "Deleted")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Tracks) != 0 {
		t.Errorf("soft-deleted track appeared in search results: %v", res.Tracks)
	}
	if len(res.Artists) != 0 {
		t.Errorf("soft-deleted artist appeared in search results: %v", res.Artists)
	}
	if len(res.Albums) != 0 {
		t.Errorf("soft-deleted album appeared in search results: %v", res.Albums)
	}
}

func TestSearch_NoMatchReturnsEmptySlices(t *testing.T) {
	db := openMem(t)
	insertSearchFile(t, db, hash64("nm1"), "Something Else", "Some Other Band", "Other Album", "")

	res, err := db.Search(context.Background(), "zzznomatchzzz")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res == nil {
		t.Fatal("Search returned nil")
	}
	// Callers should not need to nil-check the slices; we expect them to be nil
	// or empty (the handler coerces nil to [] before JSON encoding).
	if len(res.Artists) != 0 {
		t.Errorf("artists = %d, want 0", len(res.Artists))
	}
	if len(res.Albums) != 0 {
		t.Errorf("albums = %d, want 0", len(res.Albums))
	}
	if len(res.Tracks) != 0 {
		t.Errorf("tracks = %d, want 0", len(res.Tracks))
	}
}

// ---- DB.SearchGuest ----------------------------------------------------------

func TestSearchGuest_GuestFileVisibleToAnon(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("gf1")
	insertSearchFile(t, db, h, "Guest Track", "Public Artist", "Public Album", "")

	// Mark as guest-playable.
	if found, err := db.SetGuestPlayable(ctx, h, true); err != nil || !found {
		t.Fatalf("SetGuestPlayable: found=%v err=%v", found, err)
	}

	res, err := db.SearchGuest(ctx, "Guest Track")
	if err != nil {
		t.Fatalf("SearchGuest: %v", err)
	}
	if len(res.Tracks) != 1 {
		t.Errorf("tracks = %d, want 1 (guest file should be visible to anon)", len(res.Tracks))
	}
}

func TestSearchGuest_NonGuestFileHiddenFromAnon(t *testing.T) {
	// A file that is NOT guest-playable must not appear in anonymous searches.
	db := openMem(t)
	ctx := context.Background()

	// Default: guest_playable = 0.
	insertSearchFile(t, db, hash64("ng1"), "Private Track", "Private Artist", "Private Album", "")

	res, err := db.SearchGuest(ctx, "Private")
	if err != nil {
		t.Fatalf("SearchGuest: %v", err)
	}
	if len(res.Tracks) != 0 {
		t.Errorf("tracks = %d, want 0 (non-guest file must be hidden from anonymous)", len(res.Tracks))
	}
	if len(res.Artists) != 0 {
		t.Errorf("artists = %d, want 0 (non-guest artist must be hidden from anonymous)", len(res.Artists))
	}
	if len(res.Albums) != 0 {
		t.Errorf("albums = %d, want 0 (non-guest album must be hidden from anonymous)", len(res.Albums))
	}
}

func TestSearchGuest_EmptyQuery_ReturnsEmpty(t *testing.T) {
	db := openMem(t)
	insertSearchFile(t, db, hash64("sf1"), "Some Track", "Some Artist", "Some Album", "")

	res, err := db.SearchGuest(context.Background(), "")
	if err != nil {
		t.Fatalf("SearchGuest: %v", err)
	}
	if len(res.Artists) != 0 || len(res.Albums) != 0 || len(res.Tracks) != 0 {
		t.Errorf("empty q should return zero results; got artists=%d albums=%d tracks=%d",
			len(res.Artists), len(res.Albums), len(res.Tracks))
	}
}
