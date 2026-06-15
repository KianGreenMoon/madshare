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
