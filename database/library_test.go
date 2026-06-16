package database

import (
	"context"
	"database/sql"
	"testing"
)

// TestListTracksByAlbumID_DiscOrdering verifies a multi-disc album returns its
// tracks ordered by (disc, track) and carries disc_number out for the UI's
// "Disc N" grouping.
func TestListTracksByAlbumID_DiscOrdering(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := func(hash, title string, disc, track int64) {
		t.Helper()
		f := newFile(hash)
		meta := &MediaMetadata{
			Title:       title,
			ExtractedAt: 1700000000,
			Artist:      sql.NullString{String: "AC/DC", Valid: true},
			Album:       sql.NullString{String: "Live", Valid: true},
			DiscNumber:  sql.NullInt64{Int64: disc, Valid: true},
			TrackNumber: sql.NullInt64{Int64: track, Valid: true},
		}
		if err := db.InsertFile(ctx, f, newUpload(hash[:8]+".mp3"), meta); err != nil {
			t.Fatalf("InsertFile %s: %v", hash, err)
		}
	}
	// Insert deliberately out of disc/track order.
	seed("d2000001", "D2T2", 2, 2)
	seed("d1000002", "D1T2", 1, 2)
	seed("d2000003", "D2T1", 2, 1)
	seed("d1000004", "D1T1", 1, 1)

	albumID, _, _ := db.LookupAlbumID(ctx, "AC/DC", "Live")
	tracks, err := db.ListTracksByAlbumID(ctx, albumID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumID: %v", err)
	}

	var got []string
	for _, tr := range tracks {
		got = append(got, tr.Title)
	}
	want := []string{"D1T1", "D1T2", "D2T1", "D2T2"}
	if len(got) != len(want) {
		t.Fatalf("got %d tracks %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (disc then track)", got, want)
		}
	}

	if tracks[0].DiscNumber.Int64 != 1 || tracks[2].DiscNumber.Int64 != 2 {
		t.Errorf("disc numbers = (%v, %v), want first disc 1, third disc 2",
			tracks[0].DiscNumber, tracks[2].DiscNumber)
	}
}

// TestListTracksByAlbumID_DiscZeroAndUntagged verifies the accurate disc model
// (docs/architecture/disc-numbering.md): disc 0 is a real disc distinct from
// disc 1, and an untagged (NULL) disc sorts AFTER every numbered disc — not
// folded into disc 1. Ordering: 0 < 1 < untagged.
func TestListTracksByAlbumID_DiscZeroAndUntagged(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// disc is nil → NULL disc_number (untagged).
	seed := func(hash, title string, disc *int64, track int64) {
		t.Helper()
		f := newFile(hash)
		meta := &MediaMetadata{
			Title:       title,
			ExtractedAt: 1700000000,
			Artist:      sql.NullString{String: "AC/DC", Valid: true},
			Album:       sql.NullString{String: "Live", Valid: true},
			TrackNumber: sql.NullInt64{Int64: track, Valid: true},
		}
		if disc != nil {
			meta.DiscNumber = sql.NullInt64{Int64: *disc, Valid: true}
		}
		if err := db.InsertFile(ctx, f, newUpload(hash[:8]+".mp3"), meta); err != nil {
			t.Fatalf("InsertFile %s: %v", hash, err)
		}
	}
	d0, d1 := int64(0), int64(1)
	// Insert out of order: untagged, disc 1, disc 0.
	seed("00000001", "Untagged", nil, 1)
	seed("00000012", "D1T1", &d1, 1)
	seed("00000023", "D0T1", &d0, 1)

	albumID, _, _ := db.LookupAlbumID(ctx, "AC/DC", "Live")
	tracks, err := db.ListTracksByAlbumID(ctx, albumID)
	if err != nil {
		t.Fatalf("ListTracksByAlbumID: %v", err)
	}

	var got []string
	for _, tr := range tracks {
		got = append(got, tr.Title)
	}
	want := []string{"D0T1", "D1T1", "Untagged"}
	if len(got) != len(want) {
		t.Fatalf("got %d tracks %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (disc 0, disc 1, then untagged last)", got, want)
		}
	}
	// disc 0 must come through as a valid 0, not NULL; the untagged track stays NULL.
	if !tracks[0].DiscNumber.Valid || tracks[0].DiscNumber.Int64 != 0 {
		t.Errorf("first track disc = %v, want valid 0", tracks[0].DiscNumber)
	}
	if tracks[2].DiscNumber.Valid {
		t.Errorf("untagged track disc = %v, want NULL", tracks[2].DiscNumber)
	}
}
