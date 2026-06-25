package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestDataSourceCRUD(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	ds := &DataSource{
		ID:        "src1",
		Kind:      SourceKindSymlink,
		Name:      "NAS music",
		RootPath:  "/srv/music",
		Status:    SourceStatusScanning,
		CreatedAt: 1000,
	}
	if err := db.InsertDataSource(ctx, ds); err != nil {
		t.Fatalf("InsertDataSource: %v", err)
	}

	got, err := db.GetDataSource(ctx, "src1")
	if err != nil {
		t.Fatalf("GetDataSource: %v", err)
	}
	if got.Name != "NAS music" || got.Status != SourceStatusScanning || got.ScannedAt.Valid {
		t.Errorf("unexpected source after insert: %+v", got)
	}

	if err := db.FinishDataSourceScan(ctx, "src1", SourceStatusActive, `{"linked":3}`, 2000); err != nil {
		t.Fatalf("FinishDataSourceScan: %v", err)
	}
	got, _ = db.GetDataSource(ctx, "src1")
	if got.Status != SourceStatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if !got.SummaryJSON.Valid || got.SummaryJSON.String != `{"linked":3}` {
		t.Errorf("summary = %v, want the persisted JSON", got.SummaryJSON)
	}
	if !got.ScannedAt.Valid || got.ScannedAt.Int64 != 2000 {
		t.Errorf("scanned_at = %v, want 2000", got.ScannedAt)
	}

	list, err := db.ListDataSources(ctx)
	if err != nil {
		t.Fatalf("ListDataSources: %v", err)
	}
	if len(list) != 1 || list[0].ID != "src1" {
		t.Errorf("ListDataSources = %+v, want one row src1", list)
	}

	if _, err := db.GetDataSource(ctx, "nope"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("GetDataSource(missing) = %v, want ErrSourceNotFound", err)
	}
}

func TestResetStaleScans(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, st := range []struct {
		id, status string
	}{
		{"a", SourceStatusScanning},
		{"b", SourceStatusActive},
		{"c", SourceStatusScanning},
	} {
		if err := db.InsertDataSource(ctx, &DataSource{
			ID: st.id, Kind: SourceKindSymlink, Name: st.id, RootPath: "/srv/" + st.id,
			Status: st.status, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.ResetStaleScans(ctx)
	if err != nil {
		t.Fatalf("ResetStaleScans: %v", err)
	}
	if n != 2 {
		t.Errorf("reset %d rows, want 2", n)
	}
	a, _ := db.GetDataSource(ctx, "a")
	b, _ := db.GetDataSource(ctx, "b")
	if a.Status != SourceStatusError {
		t.Errorf("a status = %q, want error", a.Status)
	}
	if b.Status != SourceStatusActive {
		t.Errorf("b status = %q, want active (untouched)", b.Status)
	}
}

// TestInsertFile_LinkTarget verifies the link_target column round-trips on insert
// for a symlink import (and stays NULL for an owned local blob).
func TestInsertFile_LinkTarget(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	linked := &File{
		Hash: "aa" + strings.Repeat("0", 62), ByteSize: 10, MimeType: "audio/flac",
		StorageBackend: "links", ObjectKey: "aa.../song.flac",
		LinkTarget: sql.NullString{String: "/srv/music/song.flac", Valid: true},
		CreatedAt:  1,
	}
	if err := db.InsertFile(ctx, linked, &FileUpload{Filename: "song.flac"}, nil); err != nil {
		t.Fatalf("InsertFile(linked): %v", err)
	}
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT link_target FROM files WHERE id = ?`, linked.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.String != "/srv/music/song.flac" {
		t.Errorf("link_target = %v, want the external path", got)
	}
}
