package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func hash64(seed string) string {
	return (seed + strings.Repeat("0", 64))[:64]
}

// insertAccessFile inserts a file (with the standard metadata: artist "An
// Artist", album "An Album") and returns its id.
func insertAccessFile(t *testing.T, db *DB, hash string) int64 {
	t.Helper()
	f := newFile(hash)
	if err := db.InsertFile(context.Background(), f, newUpload("song.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	return f.ID
}

func mkUser(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	id, err := db.CreateUser(context.Background(), name, "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

func accessible(t *testing.T, db *DB, hash string, userID sql.NullInt64) bool {
	t.Helper()
	ok, err := db.FileAccessibleByHash(context.Background(), hash, userID)
	if err != nil {
		t.Fatalf("FileAccessibleByHash: %v", err)
	}
	return ok
}

func anon() sql.NullInt64        { return sql.NullInt64{} }
func uid(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }

func TestAccess_DefaultDeny(t *testing.T) {
	db := openMem(t)
	h := hash64("deny")
	insertAccessFile(t, db, h)
	u := mkUser(t, db, "u")

	if accessible(t, db, h, anon()) {
		t.Error("anonymous should be denied by default")
	}
	if accessible(t, db, h, uid(u)) {
		t.Error("user with no grant should be denied by default")
	}
}

func TestAccess_GuestPlayable(t *testing.T) {
	db := openMem(t)
	h := hash64("guest")
	insertAccessFile(t, db, h)

	if found, err := db.SetGuestPlayable(context.Background(), h, true); err != nil || !found {
		t.Fatalf("SetGuestPlayable: found=%v err=%v", found, err)
	}
	if !accessible(t, db, h, anon()) {
		t.Error("guest_playable file should be reachable anonymously")
	}
}

func TestAccess_Grants(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		grant     func(t *testing.T, db *DB, groupID, fileID int64)
		wantAllow bool
	}{
		{"all", func(t *testing.T, db *DB, g, _ int64) {
			db.AddContentGrant(ctx, g, ScopeAll, "", "", sql.NullInt64{})
		}, true},
		{"artist match", func(t *testing.T, db *DB, g, _ int64) {
			db.AddContentGrant(ctx, g, ScopeArtist, "An Artist", "", sql.NullInt64{})
		}, true},
		{"artist mismatch", func(t *testing.T, db *DB, g, _ int64) {
			db.AddContentGrant(ctx, g, ScopeArtist, "Someone Else", "", sql.NullInt64{})
		}, false},
		{"album match", func(t *testing.T, db *DB, g, _ int64) {
			db.AddContentGrant(ctx, g, ScopeAlbum, "An Artist", "An Album", sql.NullInt64{})
		}, true},
		{"file match", func(t *testing.T, db *DB, g, fileID int64) {
			db.AddContentGrant(ctx, g, ScopeFile, "", "", sql.NullInt64{Int64: fileID, Valid: true})
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openMem(t)
			h := hash64("grant")
			fileID := insertAccessFile(t, db, h)
			u := mkUser(t, db, "u")
			g, err := db.CreateAccessGroup(ctx, "grp")
			if err != nil {
				t.Fatalf("CreateAccessGroup: %v", err)
			}
			if err := db.AddGroupMember(ctx, g, u); err != nil {
				t.Fatalf("AddGroupMember: %v", err)
			}
			tc.grant(t, db, g, fileID)

			if got := accessible(t, db, h, uid(u)); got != tc.wantAllow {
				t.Errorf("accessible = %v, want %v", got, tc.wantAllow)
			}
			// A grant must not leak to a non-member.
			other := mkUser(t, db, "other")
			if tc.wantAllow && accessible(t, db, h, uid(other)) {
				t.Error("non-member should not inherit the grant")
			}
		})
	}
}

func TestAccess_UnknownHashDenied(t *testing.T) {
	db := openMem(t)
	if accessible(t, db, hash64("nope"), anon()) {
		t.Error("unknown hash should be denied")
	}
}
