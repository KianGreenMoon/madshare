package database

import (
	"context"
	"strings"
	"testing"
)

// The two rules the A-Z library view and the search box are built on, stated as
// tests because both are cross-client contracts
// (docs/architecture/artist-album-model.md §"Browse by album-artist, search
// across both roles", docs/ui/artists-and-performers.md):
//
//  1. The ARTIST of a row is the album-artist tag when there is one. A performer
//     is shown separately only when they have releases of their own — i.e. when
//     they are the album-artist of something outside the album they guest on.
//  2. Searching folds case, and folds it for every alphabet — not only ASCII.
//
// The neighbouring file entities_test.go covers the compilation shape (one
// album_artist, many performers). What is pinned here is the OTHER shape, the
// one that reaches a client as a bug report: an album-artist tag and no artist
// tag at all.

// A file tagged with an album artist and NOTHING in its artist tag is the
// ordinary shape of a well-tagged single-artist release — plenty of taggers
// write TPE2 and leave TPE1 empty. It must be filed under that album artist,
// and its album must keep its title. Landing in the Unknown-artist bucket, or
// in that artist's "Other" album, is the failure this pins.
func TestListArtists_AlbumArtistTagIsTheArtist(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, hash64("aa1"), "Sweet Unrest", "", "The Devil's Walk", "Apparat")
	insertSearchFile(t, db, hash64("aa2"), "Song of Los", "", "The Devil's Walk", "Apparat")

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 {
		t.Fatalf("artists = %v, want exactly [Apparat]", names(artists))
	}
	if artists[0].Name != "Apparat" {
		t.Fatalf("artist = %q, want %q — an album-artist tag is the artist",
			artists[0].Name, "Apparat")
	}
	if artists[0].TrackCount != 2 {
		t.Errorf("track_count = %d, want 2", artists[0].TrackCount)
	}

	albums, err := db.ListAlbumsByArtistID(ctx, artists[0].ID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID: %v", err)
	}
	if len(albums) != 1 || albums[0].Title != "The Devil's Walk" {
		t.Fatalf("albums = %+v, want one %q — an album tag is not the %q bucket",
			albums, "The Devil's Walk", DefaultAlbumTitle)
	}

	// The track's own performer resolves to the album artist too (rule 2 of the
	// identity rules), so the row a client renders never reads as anonymous.
	tracks, err := db.ListTracksByAlbumID(ctx, albums[0].ID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumID: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
	for _, tr := range tracks {
		if tr.ArtistName != "Apparat" {
			t.Errorf("track %q performer = %q, want Apparat (it falls back to the album artist)",
				tr.Title, tr.ArtistName)
		}
	}
}

// The same album, searched. An album artist with no artist tag anywhere on the
// record must still answer to its own name in all three sections — the artist
// row (either role matches), the album, and the tracks (whose performer entity
// IS the album artist).
func TestSearch_AlbumArtistWithNoArtistTagIsFindable(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, hash64("aas1"), "Sweet Unrest", "", "The Devil's Walk", "Apparat")

	res, err := db.Search(ctx, "apparat")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Artists) != 1 || res.Artists[0].Name != "Apparat" {
		t.Errorf("artists = %+v, want [Apparat]", res.Artists)
	}
	if len(res.Tracks) != 1 || res.Tracks[0].ArtistName != "Apparat" {
		t.Errorf("tracks = %+v, want the one track credited to Apparat", res.Tracks)
	}
}

// An album artist that owns the album, and a performer who has releases of
// their own: both are artists in the list. The album-artist scope decides the
// GROUPING; it does not decide who exists.
func TestListArtists_APerformerWithReleasesOfTheirOwnIsListedToo(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// A split record: the album artist is the label's name, the two tracks are
	// performed by two different acts.
	insertSearchFile(t, db, hash64("sp1"), "First Half", "Nine Inch Nails", "Split", "Some Label")
	insertSearchFile(t, db, hash64("sp2"), "Second Half", "Coil", "Split", "Some Label")
	// Only one of them has a record of their own.
	insertSearchFile(t, db, hash64("sp3"), "Closer", "Nine Inch Nails", "The Downward Spiral", "")

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	listed := map[string]int{}
	for _, a := range artists {
		listed[a.Name] = a.TrackCount
	}
	if _, ok := listed["Some Label"]; !ok {
		t.Errorf("the album artist is missing from the list: %v", names(artists))
	}
	if listed["Nine Inch Nails"] != 1 {
		t.Errorf("Nine Inch Nails = %d tracks, want 1 — a performer with an album of "+
			"their own is listed, counting only what they are the album artist of", listed["Nine Inch Nails"])
	}
	if _, ok := listed["Coil"]; ok {
		t.Errorf("Coil has no release of its own and must not be in the A-Z list: %v", names(artists))
	}

	// …and the one the list leaves out is still an artist: search finds it, and
	// it drills into the record it plays on. A hit is never a dead end.
	res, err := db.Search(ctx, "Coil")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Artists) != 1 || res.Artists[0].Name != "Coil" {
		t.Fatalf("search artists = %+v, want [Coil]", res.Artists)
	}
	albums, err := db.ListAlbumsByArtistID(ctx, res.Artists[0].ID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID(Coil): %v", err)
	}
	if len(albums) != 1 || albums[0].Title != "Split" {
		t.Errorf("Coil's albums = %+v, want the split it performs on", albums)
	}
}

// Search folds case for Cyrillic exactly as it does for ASCII. SQLite's own
// LOWER only touches A-Z, which is why every search predicate goes through the
// unicode_lower function — this is the test that says so for the alphabet the
// bug was reported in.
func TestSearch_CaseInsensitive_Cyrillic(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, hash64("cy1"), "Группа крови", "Виктор Цой", "Группа крови", "Кино")

	for _, q := range []string{"кино", "КИНО", "Кино", "кИнО"} {
		t.Run("artist/"+q, func(t *testing.T) {
			res, err := db.Search(ctx, q)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			if len(res.Artists) != 1 || res.Artists[0].Name != "Кино" {
				t.Errorf("Search(%q) artists = %+v, want [Кино]", q, res.Artists)
			}
		})
	}

	// The album and the track carry the same words; a mid-string, mixed-case
	// substring has to reach all three sections.
	for _, q := range []string{"группа", "ГРУППА", "ппа кров", "ЦОЙ", "виктор"} {
		t.Run("all/"+q, func(t *testing.T) {
			res, err := db.Search(ctx, q)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			if len(res.Tracks) != 1 {
				t.Errorf("Search(%q) tracks = %d, want 1", q, len(res.Tracks))
			}
		})
	}

	res, err := db.Search(ctx, "ГРУППА КРОВИ")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Albums) != 1 || res.Albums[0].Title != "Группа крови" {
		t.Errorf("albums = %+v, want [Группа крови]", res.Albums)
	}
}

// Two spellings of one Cyrillic name that differ only in case are ONE artist,
// the same way "The Beatles" and "the  beatles" are. The dedup key is
// normalizeKey, which lowercases the Unicode way; a byte-wise key would file
// the same band twice and split its albums between the two rows.
func TestListArtists_CyrillicCaseVariantsAreOneArtist(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertSearchFile(t, db, hash64("cyv1"), "Звезда по имени Солнце", "", "Звезда по имени Солнце", "Кино")
	insertSearchFile(t, db, hash64("cyv2"), "Пачка сигарет", "", "звезда по имени солнце", "КИНО")

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 {
		t.Fatalf("artists = %v, want one row — case variants of one Cyrillic name", names(artists))
	}
	if artists[0].Name != "Кино" || artists[0].TrackCount != 2 {
		t.Errorf("artist = %q with %d tracks, want Кино with 2", artists[0].Name, artists[0].TrackCount)
	}
	albums, err := db.ListAlbumsByArtistID(ctx, artists[0].ID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID: %v", err)
	}
	if len(albums) != 1 || albums[0].TrackCount != 2 {
		t.Errorf("albums = %+v, want one album holding both tracks", albums)
	}
}

// The A-Z list is alphabetical case-insensitively, and that has to hold for a
// non-ASCII alphabet too. A band spelled with a lower-case first letter belongs
// between its neighbours, not in a block of its own before or after them —
// which is what an ASCII-only LOWER() in the ORDER BY produces, since every
// capital Cyrillic letter sorts before every small one.
func TestListArtists_AlphabeticalOrderIsCaseInsensitiveForCyrillic(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Deliberately inserted out of order, and deliberately mixed in their first
	// letter's case: "аукцЫон" is spelled small, the rest are capitalised.
	for i, artist := range []string{"Пикник", "аукцЫон", "Кино", "Ленинград"} {
		insertSearchFile(t, db, hash64("ord"+string(rune('a'+i))), "T", "", "A", artist)
	}

	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	got := names(artists)
	want := []string{"аукцЫон", "Кино", "Ленинград", "Пикник"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("artist order = %v, want %v (alphabetical, case-folded)", got, want)
	}
}

// names is the artist list as a plain slice of names, for readable failures.
func names(artists []*ArtistEntry) []string {
	out := make([]string, 0, len(artists))
	for _, a := range artists {
		out = append(out, a.Name)
	}
	return out
}
