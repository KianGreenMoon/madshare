package storages_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/storages"
)

// hash64 is a valid-looking 64-char lowercase hex digest for path tests. It
// need not be the real digest of any content — Locate only checks the hash
// shape and the filesystem, not that bytes hash to it.
func hash64(seed byte) string {
	b := make([]byte, 64)
	const hexd = "0123456789abcdef"
	for i := range b {
		b[i] = hexd[(int(seed)+i)%16]
	}
	return string(b)
}

// writeBlob creates <root>/audio/<hash>/<name> with content and returns its path.
func writeBlob(t *testing.T, root, hash, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// linkBlob creates <root>/audio/<hash>/<name> as a symlink to target.
func linkBlob(t *testing.T, root, hash, name, target string) string {
	t.Helper()
	dir := filepath.Join(root, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolve_LocalHit(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := hash64(1)
	want := writeBlob(t, files, h, "song.flac", "audio")
	reg := storages.New(files, links)

	got, id, ok := reg.Resolve(h)
	if !ok {
		t.Fatal("Resolve = not ok, want local hit")
	}
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if id != storages.Local {
		t.Errorf("storageID = %q, want %q", id, storages.Local)
	}
}

func TestResolve_LinksHit_FollowsSymlink(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "original.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(2)
	link := linkBlob(t, links, h, "original.flac", external)
	reg := storages.New(files, links)

	got, id, ok := reg.Resolve(h)
	if !ok {
		t.Fatal("Resolve = not ok, want links hit through a live symlink")
	}
	if got != link {
		t.Errorf("path = %q, want the symlink path %q (ServeFile follows it)", got, link)
	}
	if id != storages.Links {
		t.Errorf("storageID = %q, want %q", id, storages.Links)
	}
}

func TestResolve_DanglingLink_FallsThroughTo404(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := hash64(3)
	// Symlink to a target that does not exist → os.Stat fails → not present.
	linkBlob(t, links, h, "gone.flac", filepath.Join(t.TempDir(), "missing.flac"))
	reg := storages.New(files, links)

	if _, _, ok := reg.Resolve(h); ok {
		t.Error("Resolve = ok, want miss (a dangling link is not present)")
	}
}

func TestResolve_DanglingLink_FallsThroughToLocal(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	h := hash64(4)
	linkBlob(t, links, h, "gone.flac", filepath.Join(t.TempDir(), "missing.flac"))
	want := writeBlob(t, files, h, "song.flac", "audio") // a local duplicate covers it
	reg := storages.New(files, links)

	got, id, ok := reg.Resolve(h)
	if !ok || got != want || id != storages.Local {
		t.Errorf("Resolve = (%q,%q,%v), want local duplicate %q to cover the dangling link", got, id, ok, want)
	}
}

// When a hash exists in BOTH storages, local wins (fixed precedence).
func TestResolve_Precedence_LocalBeatsLinks(t *testing.T) {
	files, links := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "original.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := hash64(5)
	wantLocal := writeBlob(t, files, h, "song.flac", "audio")
	linkBlob(t, links, h, "original.flac", external)
	reg := storages.New(files, links)

	got, id, ok := reg.Resolve(h)
	if !ok || id != storages.Local || got != wantLocal {
		t.Errorf("Resolve = (%q,%q,%v), want local to outrank links", got, id, ok)
	}
}

func TestResolve_NoHit(t *testing.T) {
	reg := storages.New(t.TempDir(), t.TempDir())
	if _, _, ok := reg.Resolve(hash64(6)); ok {
		t.Error("Resolve on empty storages = ok, want miss")
	}
}

func TestResolve_InvalidHash_NeverEscapes(t *testing.T) {
	reg := storages.New(t.TempDir(), t.TempDir())
	for _, bad := range []string{"", "..", "../../etc/passwd", "ABC", strings.Repeat("g", 64)} {
		if _, _, ok := reg.Resolve(bad); ok {
			t.Errorf("Resolve(%q) = ok, want rejection of a non-hash", bad)
		}
	}
}

func TestRegistry_OrderAndLookup(t *testing.T) {
	reg := storages.New(t.TempDir(), t.TempDir())
	order := reg.Storages()
	if len(order) != 2 || order[0].ID() != storages.Local || order[1].ID() != storages.Links {
		t.Fatalf("Storages order = %v, want [local, links]", order)
	}
	if reg.Get(storages.Local) == nil || reg.Get(storages.Links) == nil {
		t.Error("Get(local)/Get(links) should both be non-nil")
	}
	if reg.Get("s3") != nil {
		t.Error("Get of an unknown storage should be nil")
	}
}
