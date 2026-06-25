package storages_test

import (
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/storages"
)

func TestLinker_LinkAndResolve(t *testing.T) {
	links := t.TempDir()
	external := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(11)
	l := storages.NewLinker(links)

	created, err := l.Link(h, "song.flac", external)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !created {
		t.Fatal("Link created = false, want true on first link")
	}

	// The link is resolvable through the read-side registry (follows the symlink).
	reg := storages.New(t.TempDir(), links)
	got, id, ok := reg.Resolve(h)
	if !ok || id != storages.Links {
		t.Fatalf("Resolve = (%q,%q,%v), want a links hit", got, id, ok)
	}
	info, err := os.Lstat(got)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("resolved path %q is not a symlink", got)
	}
}

func TestLinker_Has_SkipsOverwrite(t *testing.T) {
	links := t.TempDir()
	external := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(12)
	l := storages.NewLinker(links)

	if has, _ := l.Has(h); has {
		t.Error("Has = true before any link")
	}
	if _, err := l.Link(h, "song.flac", external); err != nil {
		t.Fatal(err)
	}
	if has, _ := l.Has(h); !has {
		t.Error("Has = false after linking")
	}
	// A second Link for the same hash is a no-op (one link per hash).
	created, err := l.Link(h, "other.flac", external)
	if err != nil {
		t.Fatalf("second Link: %v", err)
	}
	if created {
		t.Error("second Link created = true, want false (don't overwrite)")
	}
}

// A dangling link still counts as present so a re-scan does not duplicate it.
func TestLinker_Has_DanglingCounts(t *testing.T) {
	links := t.TempDir()
	h := hash64(13)
	l := storages.NewLinker(links)
	if _, err := l.Link(h, "gone.flac", filepath.Join(t.TempDir(), "missing.flac")); err != nil {
		t.Fatal(err)
	}
	if has, err := l.Has(h); err != nil || !has {
		t.Errorf("Has(dangling) = (%v,%v), want present", has, err)
	}
}

// Remove unlinks only the symlink; the external original is left intact.
func TestLinker_Remove_LeavesTargetIntact(t *testing.T) {
	links := t.TempDir()
	external := filepath.Join(t.TempDir(), "keep.flac")
	if err := os.WriteFile(external, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(14)
	l := storages.NewLinker(links)
	if _, err := l.Link(h, "keep.flac", external); err != nil {
		t.Fatal(err)
	}

	if err := l.Remove(h); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(links, storage.AudioSubdir, h)); !os.IsNotExist(err) {
		t.Errorf("hash dir still present after Remove: %v", err)
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "precious" {
		t.Errorf("external original disturbed by Remove: data=%q err=%v", data, err)
	}
}

func TestLinker_InvalidInputs(t *testing.T) {
	l := storages.NewLinker(t.TempDir())
	if _, err := l.Link("not-a-hash", "x.flac", "/tmp/x.flac"); err == nil {
		t.Error("Link with bad hash should error")
	}
	// A bare ".." reduces to nothing usable → rejected.
	if _, err := l.Link(hash64(15), "..", "/tmp/x.flac"); err == nil {
		t.Error("Link with a pure-traversal filename should error")
	}
}

// A traversal-laden filename is reduced to its base name and lands inside the
// hash dir — it can never escape, mirroring the upload path's sanitizeFilename.
func TestLinker_TraversalFilename_StaysContained(t *testing.T) {
	links := t.TempDir()
	external := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(16)
	l := storages.NewLinker(links)
	created, err := l.Link(h, "../../etc/escape.flac", external)
	if err != nil || !created {
		t.Fatalf("Link = (%v,%v), want a contained link", created, err)
	}
	want := filepath.Join(links, storage.AudioSubdir, h, "escape.flac")
	if _, err := os.Lstat(want); err != nil {
		t.Errorf("expected the link at %q (base-name only): %v", want, err)
	}
}
