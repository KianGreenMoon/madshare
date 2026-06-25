package storages_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/storages"
)

// sha256Hex is the lowercase hex SHA-256 of content — the hash a links entry is
// keyed by when it references a file holding exactly those bytes.
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

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

func TestLinker_LinkInfo(t *testing.T) {
	links := t.TempDir()
	external := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := storages.NewLinker(links)

	// No link yet.
	if _, exists, _, err := l.LinkInfo(hash64(21)); err != nil || exists {
		t.Errorf("LinkInfo(absent) = exists %v err %v, want absent", exists, err)
	}

	// Healthy link.
	h := hash64(22)
	if _, err := l.Link(h, "song.flac", external); err != nil {
		t.Fatal(err)
	}
	target, exists, present, err := l.LinkInfo(h)
	if err != nil || !exists || !present {
		t.Fatalf("LinkInfo(healthy) = (%q,%v,%v,%v)", target, exists, present, err)
	}
	if target != external {
		t.Errorf("recorded target = %q, want %q", target, external)
	}

	// Dangling link: target removed.
	if err := os.Remove(external); err != nil {
		t.Fatal(err)
	}
	_, exists, present, err = l.LinkInfo(h)
	if err != nil || !exists || present {
		t.Errorf("LinkInfo(dangling) = exists %v present %v err %v, want exists+not-present", exists, present, err)
	}
}

func TestLinker_VerifyLink(t *testing.T) {
	links := t.TempDir()
	// The content's real hash so VerifyLink can match it.
	content := []byte("real audio bytes")
	hash := sha256Hex(content)
	external := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(external, content, 0o644); err != nil {
		t.Fatal(err)
	}
	l := storages.NewLinker(links)
	if _, err := l.Link(hash, "song.flac", external); err != nil {
		t.Fatal(err)
	}

	ok, err := l.VerifyLink(hash)
	if err != nil || !ok {
		t.Errorf("VerifyLink(intact) = (%v,%v), want true", ok, err)
	}

	// Mutate the external original: VerifyLink must now report a mismatch.
	if err := os.WriteFile(external, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.VerifyLink(hash); err != nil || ok {
		t.Errorf("VerifyLink(tampered) = (%v,%v), want false", ok, err)
	}
}

func TestLinker_Usage(t *testing.T) {
	links := t.TempDir()
	l := storages.NewLinker(links)

	// Empty links tree → zero usage.
	if u, err := l.Usage(); err != nil || u != (storages.LinksUsage{}) {
		t.Fatalf("Usage(empty) = (%+v,%v), want zero", u, err)
	}

	ext1 := filepath.Join(t.TempDir(), "a.flac")
	if err := os.WriteFile(ext1, []byte("12345"), 0o644); err != nil { // 5 bytes
		t.Fatal(err)
	}
	ext2 := filepath.Join(t.TempDir(), "b.flac")
	if err := os.WriteFile(ext2, []byte("678"), 0o644); err != nil { // 3 bytes
		t.Fatal(err)
	}
	if _, err := l.Link(hash64(31), "a.flac", ext1); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Link(hash64(32), "b.flac", ext2); err != nil {
		t.Fatal(err)
	}
	// A dangling link contributes to Count + Broken but not ExternalBytes.
	if _, err := l.Link(hash64(33), "gone.flac", filepath.Join(t.TempDir(), "missing.flac")); err != nil {
		t.Fatal(err)
	}

	u, err := l.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Count != 3 || u.Broken != 1 || u.ExternalBytes != 8 {
		t.Errorf("Usage = %+v, want count=3 broken=1 external=8", u)
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
