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
