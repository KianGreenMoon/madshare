package database

import (
	"context"
	"errors"
	"testing"
)

func strPtr(s string) *string { return &s }

const metaHash = "abcd000000000000000000000000000000000000000000000000000000000000"

// TestUpdateFileMetadata_PartialPatch verifies only the supplied fields change
// and the others are left untouched.
func TestUpdateFileMetadata_PartialPatch(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3") // Title="A Song", Artist="An Artist", Album="An Album"

	got, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{Title: strPtr("Renamed")})
	if err != nil {
		t.Fatalf("UpdateFileMetadata: %v", err)
	}
	if got.Title != "Renamed" {
		t.Errorf("title = %q, want %q", got.Title, "Renamed")
	}
	if got.Artist.String != "An Artist" {
		t.Errorf("artist = %q, want unchanged %q", got.Artist.String, "An Artist")
	}
	if got.Album.String != "An Album" {
		t.Errorf("album = %q, want unchanged %q", got.Album.String, "An Album")
	}
}

// TestUpdateFileMetadata_ClearVsAbsent verifies an empty-string pointer clears a
// field (stored NULL) while an absent (nil) field is left alone.
func TestUpdateFileMetadata_ClearVsAbsent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3")

	// Clear album (-> NULL), leave artist absent.
	got, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{Album: strPtr("")})
	if err != nil {
		t.Fatalf("UpdateFileMetadata: %v", err)
	}
	if got.Album.Valid {
		t.Errorf("album should be NULL after clearing, got %q (valid=%v)", got.Album.String, got.Album.Valid)
	}
	if got.Artist.String != "An Artist" {
		t.Errorf("artist = %q, want unchanged %q", got.Artist.String, "An Artist")
	}
}

// TestUpdateFileMetadata_EmptyPatchEchoes verifies an empty patch is a no-op
// that still returns the current row.
func TestUpdateFileMetadata_EmptyPatchEchoes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3")

	got, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{})
	if err != nil {
		t.Fatalf("UpdateFileMetadata: %v", err)
	}
	if got.Title != "A Song" {
		t.Errorf("title = %q, want %q", got.Title, "A Song")
	}
}

// TestUpdateFileMetadata_ClearTitleRederivesFilename verifies that clearing the
// title (required non-empty) re-derives it from the filename instead of NULL/”.
func TestUpdateFileMetadata_ClearTitleRederivesFilename(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "My Song.mp3") // newMeta sets Title="A Song"

	got, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{Title: strPtr("")})
	if err != nil {
		t.Fatalf("UpdateFileMetadata: %v", err)
	}
	if got.Title != "My Song" {
		t.Errorf("cleared title = %q, want %q (filename default)", got.Title, "My Song")
	}
}

// TestUpdateFileMetadata_ExtendedFields verifies the rich tags (string + numeric)
// round-trip, and that a blank numeric clears the column to NULL.
func TestUpdateFileMetadata_ExtendedFields(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3")

	got, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{
		Genre:       strPtr("Jazz"),
		Composer:    strPtr("Bill Evans"),
		Comment:     strPtr("live"),
		TrackNumber: strPtr("3"),
		TrackTotal:  strPtr("10"),
		DiscNumber:  strPtr("1"),
		Year:        strPtr("1961"),
	})
	if err != nil {
		t.Fatalf("UpdateFileMetadata: %v", err)
	}
	if got.Genre.String != "Jazz" || got.Composer.String != "Bill Evans" || got.Comment.String != "live" {
		t.Errorf("string tags = (%q,%q,%q), want (Jazz, Bill Evans, live)", got.Genre.String, got.Composer.String, got.Comment.String)
	}
	if got.TrackNumber.Int64 != 3 || got.TrackTotal.Int64 != 10 || got.DiscNumber.Int64 != 1 || got.Year.Int64 != 1961 {
		t.Errorf("numeric tags = (%d,%d,%d,%d), want (3,10,1,1961)", got.TrackNumber.Int64, got.TrackTotal.Int64, got.DiscNumber.Int64, got.Year.Int64)
	}

	// Blank clears the numeric to NULL; an absent field is left untouched.
	got, err = db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{TrackNumber: strPtr("")})
	if err != nil {
		t.Fatalf("clear track_number: %v", err)
	}
	if got.TrackNumber.Valid {
		t.Errorf("track_number should be NULL after clearing, got %d", got.TrackNumber.Int64)
	}
	if got.Year.Int64 != 1961 {
		t.Errorf("year = %d, want unchanged 1961", got.Year.Int64)
	}
}

// TestUpdateFileMetadata_InvalidNumber rejects a non-numeric / negative numeric
// field with ErrInvalidMetadata (mapped to 400 at the API layer).
func TestUpdateFileMetadata_InvalidNumber(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3")

	for _, bad := range []string{"abc", "-1", "3.5"} {
		_, err := db.UpdateFileMetadata(ctx, metaHash, MetadataPatch{TrackNumber: strPtr(bad)})
		if !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("track_number=%q: err = %v, want ErrInvalidMetadata", bad, err)
		}
	}
}

// TestFileMetadataByHash round-trips a stored row and reports ErrFileNotFound for
// an unknown hash.
func TestFileMetadataByHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, metaHash, "song.mp3")

	got, err := db.FileMetadataByHash(ctx, metaHash)
	if err != nil {
		t.Fatalf("FileMetadataByHash: %v", err)
	}
	if got.Title != "A Song" {
		t.Errorf("title = %q, want %q", got.Title, "A Song")
	}

	if _, err := db.FileMetadataByHash(ctx, "nope"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("unknown hash err = %v, want ErrFileNotFound", err)
	}
}

// TestUpdateFileMetadata_UnknownHash returns ErrFileNotFound for a hash with no
// files row, even for an empty patch.
func TestUpdateFileMetadata_UnknownHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	_, err := db.UpdateFileMetadata(ctx, "nope", MetadataPatch{Title: strPtr("x")})
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
	_, err = db.UpdateFileMetadata(ctx, "nope", MetadataPatch{})
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("empty-patch err = %v, want ErrFileNotFound", err)
	}
}

// Tags an embedder brings for a file whose bytes do not carry them.
//
// The case is madplayer's "keep on this device": bytes copied out of a library
// that knew what they were, into one that can only read the file. Untagged WAVs
// are the extreme — this reader has no WAV tag dialect at all, so the whole
// album arrives as Unknown artist / Other — and the ordinary case is an album
// artist that only ever existed as an edit, since metadata here is an overlay
// and is never written back into the file.
func TestFillMissingTags_DescribesAnUntaggedFile(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("fill1")
	f := newFile(h)
	if err := db.InsertFile(ctx, f, newUpload("01 - Plague Awake Here.wav"),
		&MediaMetadata{ExtractedAt: 1700000000}); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	// What the scan alone makes of it: nothing but the filename.
	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != DefaultArtistName {
		t.Fatalf("artists = %v, want the %q bucket before the tags arrive",
			names(artists), DefaultArtistName)
	}

	if _, err := db.FillMissingTags(ctx, h, MetadataPatch{
		Title:       strPtr("Plague Awake Here"),
		Artist:      strPtr("Vasily Kashnikov"),
		AlbumArtist: strPtr("Pathologic 2"),
		Album:       strPtr("Pathologic 2 OST"),
		TrackNumber: strPtr("1"),
	}); err != nil {
		t.Fatalf("FillMissingTags: %v", err)
	}

	artists, err = db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "Pathologic 2" {
		t.Fatalf("artists = %v, want [Pathologic 2] — the album artist groups it", names(artists))
	}
	albums, err := db.ListAlbumsByArtistID(ctx, artists[0].ID)
	if err != nil {
		t.Fatalf("ListAlbumsByArtistID: %v", err)
	}
	if len(albums) != 1 || albums[0].Title != "Pathologic 2 OST" {
		t.Fatalf("albums = %+v, want the album's own title, not %q", albums, DefaultAlbumTitle)
	}
	tracks, err := db.ListTracksByAlbumID(ctx, albums[0].ID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumID: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Title != "Plague Awake Here" {
		t.Fatalf("tracks = %+v, want the catalogue's title rather than the filename", tracks)
	}
	if tracks[0].ArtistName != "Vasily Kashnikov" {
		t.Errorf("performer = %q, want Vasily Kashnikov", tracks[0].ArtistName)
	}
}

// The file's own tags win. This fills gaps; it does not import somebody else's
// idea of what a file is over the tags in it — the same posture as the rest of
// the overlay, from the other direction.
func TestFillMissingTags_LeavesWhatTheFileAlreadySays(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("fill2")
	insertSearchFile(t, db, h, "Darkness", "Theodor Bastard", "Utopia", "")

	got, err := db.FillMissingTags(ctx, h, MetadataPatch{
		Title:       strPtr("Something Else"),
		Artist:      strPtr("Somebody Else"),
		AlbumArtist: strPtr("Pathologic 2"),
		Album:       strPtr("Another Album"),
	})
	if err != nil {
		t.Fatalf("FillMissingTags: %v", err)
	}
	if got.Title != "Darkness" || got.Artist.String != "Theodor Bastard" || got.Album.String != "Utopia" {
		t.Errorf("tags = %q/%q/%q, want the file's own", got.Title, got.Artist.String, got.Album.String)
	}
	// The album-artist tag was the one thing the file did not carry, so it is
	// the one thing that changed — and it regroups the track, which is exactly
	// the "this is the Pathologic 2 album, not a Theodor Bastard album" case.
	if got.AlbumArtist.String != "Pathologic 2" {
		t.Fatalf("album artist = %q, want the gap filled", got.AlbumArtist.String)
	}
	artists, err := db.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "Pathologic 2" {
		t.Errorf("artists = %v, want [Pathologic 2]", names(artists))
	}
}

// Knowing nothing must not erase anything, and an unknown hash is the ordinary
// answer for a caller that has just handed a file to a scanner.
func TestFillMissingTags_BlankValuesAndUnknownHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("fill3")
	insertSearchFile(t, db, h, "Darkness", "Theodor Bastard", "Utopia", "")
	got, err := db.FillMissingTags(ctx, h, MetadataPatch{
		Title:       strPtr(""),
		AlbumArtist: strPtr("   "),
		TrackNumber: strPtr(""),
	})
	if err != nil {
		t.Fatalf("FillMissingTags: %v", err)
	}
	if got.Title != "Darkness" || got.AlbumArtist.Valid {
		t.Errorf("blank values changed the row: %q / %q", got.Title, got.AlbumArtist.String)
	}

	if _, err := db.FillMissingTags(ctx, hash64("nosuch"), MetadataPatch{
		Artist: strPtr("Anybody"),
	}); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("unknown hash gave %v, want ErrFileNotFound", err)
	}
}
