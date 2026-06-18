package storage

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
	"testing/fstest"
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

// vanishingEntry wraps a DirEntry whose Info() reports ErrNotExist, simulating a
// file removed between the directory read and the stat — the race that prune /
// cover-replace can lose to while the walk runs.
type vanishingEntry struct{ fs.DirEntry }

func (e vanishingEntry) Info() (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "lstat", Path: e.Name(), Err: fs.ErrNotExist}
}

// vanishingFS makes exactly one path's entry vanish (Info → ErrNotExist) on
// read, deterministically reproducing the mid-walk-deletion race.
type vanishingFS struct {
	fstest.MapFS
	vanish string
}

func (v vanishingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := v.MapFS.ReadDir(name)
	if err != nil {
		return entries, err
	}
	for i, e := range entries {
		if path.Join(name, e.Name()) == v.vanish {
			entries[i] = vanishingEntry{e}
		}
	}
	return entries, nil
}

// A file that disappears mid-walk must be skipped, not collapse the whole total
// to zero (regression guard for the ErrNotExist over-broad swallow).
func TestDirSizeVanishingEntry(t *testing.T) {
	fsys := vanishingFS{
		MapFS: fstest.MapFS{
			"keep.bin":   {Data: make([]byte, 100)},
			"vanish.bin": {Data: make([]byte, 900)},
		},
		vanish: "vanish.bin",
	}
	got, err := dirSizeFS(fsys)
	if err != nil {
		t.Fatalf("dirSizeFS: unexpected error %v", err)
	}
	if got != 100 {
		t.Errorf("dirSizeFS = %d, want 100 (surviving file only, total not collapsed)", got)
	}
}

// An unreadable subdir is skipped (a slight undercount), not a hard error that
// 500s the whole storage panel. Sibling files outside it still count.
func TestDirSizePermDeniedSubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readable.bin"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden.bin"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // so TempDir cleanup can recurse

	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize with an unreadable subdir: unexpected error %v", err)
	}
	if got != 250 {
		t.Errorf("DirSize = %d, want 250 (readable sibling only; locked subtree skipped)", got)
	}
}
