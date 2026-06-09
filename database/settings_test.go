package database

import (
	"context"
	"testing"
)

func TestTrashRestorePolicy(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Default when unset.
	if p, err := db.GetTrashRestorePolicy(ctx); err != nil || p != TrashReuploadRestores {
		t.Fatalf("default = (%q, %v), want (%q, nil)", p, err, TrashReuploadRestores)
	}

	// Set + get roundtrip.
	if err := db.SetTrashRestorePolicy(ctx, TrashInform); err != nil {
		t.Fatalf("set inform: %v", err)
	}
	if p, _ := db.GetTrashRestorePolicy(ctx); p != TrashInform {
		t.Fatalf("get after set = %q, want inform", p)
	}

	// Invalid value is rejected.
	if err := db.SetTrashRestorePolicy(ctx, "bogus"); err == nil {
		t.Fatal("set bogus: want error, got nil")
	}

	// A corrupt stored value reads back as the default.
	if err := db.SetSetting(ctx, settingTrashRestorePolicy, "bogus"); err != nil {
		t.Fatalf("seed corrupt value: %v", err)
	}
	if p, _ := db.GetTrashRestorePolicy(ctx); p != TrashReuploadRestores {
		t.Fatalf("corrupt value reads %q, want default", p)
	}
}
