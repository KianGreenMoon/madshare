package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	// Nested files of known sizes; DirSize sums them across subdirectories.
	write := func(rel string, n int) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.bin", 100)
	write("sub/b.bin", 250)
	write("sub/deep/c.bin", 650)
	// An empty directory contributes nothing.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if got != 1000 {
		t.Errorf("DirSize = %d, want 1000", got)
	}
}

// A directory that does not exist is "zero bytes", not an error — a fresh
// install has no images/ (or video/) subtree until the first write.
func TestDirSizeMissingDir(t *testing.T) {
	got, err := DirSize(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("DirSize on missing dir: unexpected error %v", err)
	}
	if got != 0 {
		t.Errorf("DirSize on missing dir = %d, want 0", got)
	}
}

// A path whose prefix component is a regular file (ENOTDIR) is a real walk
// error and must propagate, not be swallowed as zero.
func TestDirSizeNotADir(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DirSize(filepath.Join(regular, "sub")); err == nil {
		t.Error("DirSize below a regular file: want error, got nil")
	}
}
