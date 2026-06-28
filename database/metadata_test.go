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

// TestBulkUpdateFileMetadata_AppliesAndSkipsMissing applies one patch across a
// set in a shared transaction: present files update (and reclassify), an unknown
// hash is reported in notFound rather than failing the batch.
func TestBulkUpdateFileMetadata_AppliesAndSkipsMissing(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	h1 := hash64("bmeta1")
	h2 := hash64("bmeta2")
	seedFile(t, db, h1, "a.mp3")
	seedFile(t, db, h2, "b.mp3")
	missing := hash64("bmeta-missing")

	affected, notFound, err := db.BulkUpdateFileMetadata(ctx,
		[]string{h1, h2, missing}, MetadataPatch{Artist: strPtr("New Artist")})
	if err != nil {
		t.Fatalf("BulkUpdateFileMetadata: %v", err)
	}
	if affected != 2 {
		t.Fatalf("affected = %d, want 2", affected)
	}
	if len(notFound) != 1 || notFound[0] != missing {
		t.Fatalf("notFound = %v, want [%s]", notFound, missing)
	}
	for _, h := range []string{h1, h2} {
		m, err := db.FileMetadataByHash(ctx, h)
		if err != nil {
			t.Fatalf("FileMetadataByHash(%s): %v", h, err)
		}
		if m.Artist.String != "New Artist" {
			t.Errorf("%s artist = %q, want New Artist", h, m.Artist.String)
		}
	}
}

// TestBulkUpdateFileMetadata_InvalidNumericAborts checks a patch-level bad value
// (identical for every file) surfaces as ErrInvalidMetadata, not a per-file skip.
func TestBulkUpdateFileMetadata_InvalidNumericAborts(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	h := hash64("bmeta-bad")
	seedFile(t, db, h, "a.mp3")

	if _, _, err := db.BulkUpdateFileMetadata(ctx, []string{h}, MetadataPatch{Year: strPtr("nope")}); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("err = %v, want ErrInvalidMetadata", err)
	}
}
