package storages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"daemonlord.ygg/madshare/api/storage"
)

// Linker is the write side of the shared 'links' storage: it creates and removes
// the symlinks that reference external originals, content-addressed by hash under
// <root>/audio/<hash>/<filename> (mirroring the local audio tree). It is the
// kind-aware counterpart to api/storage.Local — every operation touches only the
// link inside <root>/links, NEVER the external target it points at. This upholds
// the data-sources INVARIANT: Madshare never writes, moves, or deletes a file it
// does not own (see docs/architecture/data-sources.md, Lifecycle & safety).
type Linker struct {
	root string // the links storage root; the audio tree is <root>/audio
}

// NewLinker builds a Linker for the links storage rooted at linksDir
// (<data_dir>/links). The directory need not exist; it is created on first link.
func NewLinker(linksDir string) *Linker {
	return &Linker{root: linksDir}
}

// hashDir is the per-hash directory under the links audio tree.
func (l *Linker) hashDir(hash string) string {
	return filepath.Join(l.root, storage.AudioSubdir, hash)
}

// Has reports whether the links storage already holds a link for hash — i.e. the
// hash dir exists and is non-empty. It lists the directory WITHOUT following the
// symlink, so a dangling link still counts as present (one link per hash; we
// never overwrite). A malformed hash, or a missing dir, reads as absent.
func (l *Linker) Has(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("invalid hash %q", hash)
	}
	entries, err := os.ReadDir(l.hashDir(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read links hash dir: %w", err)
	}
	return len(entries) > 0, nil
}

// Link creates a symlink <root>/audio/<hash>/<filename> pointing at target (an
// absolute path to the external original). created is false when the links
// storage already holds this hash (Has) — the existing link is left untouched,
// honouring the one-link-per-hash rule. filename is reduced to a base name so it
// can never escape the hash dir. target is never opened, statted, or modified.
func (l *Linker) Link(hash, filename, target string) (created bool, err error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("invalid hash %q", hash)
	}
	has, err := l.Has(hash)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	name := filepath.Base(filepath.Clean(filename))
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return false, fmt.Errorf("invalid link filename %q", filename)
	}
	dir := l.hashDir(hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir links hash dir: %w", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		return false, fmt.Errorf("symlink %s: %w", hash, err)
	}
	return true, nil
}

// firstLinkEntry returns the on-disk path of the single symlink the links storage
// holds for hash, and whether one exists. It lists the hash dir WITHOUT following
// the symlink (so a dangling link is still found). A malformed hash or a
// missing/empty dir reports exists=false.
func (l *Linker) firstLinkEntry(hash string) (path string, exists bool, err error) {
	if !validHash.MatchString(hash) {
		return "", false, fmt.Errorf("invalid hash %q", hash)
	}
	dir := l.hashDir(hash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read links hash dir: %w", err)
	}
	for _, e := range entries {
		return filepath.Join(dir, e.Name()), true, nil
	}
	return "", false, nil
}

// LinkInfo inspects the links-storage entry for hash for prune broken-link
// detection (data-sources P5). It returns the symlink's recorded target (the
// literal os.Readlink value, to compare against the stored link_target), whether
// a link entry exists at all, and whether that link's target currently stats
// (through the symlink) as a regular file. It never modifies the target.
func (l *Linker) LinkInfo(hash string) (target string, exists, targetPresent bool, err error) {
	p, exists, err := l.firstLinkEntry(hash)
	if err != nil || !exists {
		return "", exists, false, err
	}
	target, err = os.Readlink(p)
	if err != nil {
		// The entry exists but is not a symlink (or is unreadable) — treat as a
		// present link with no resolvable target so the caller flags it.
		return "", true, false, nil
	}
	if info, statErr := os.Stat(p); statErr == nil && info.Mode().IsRegular() {
		targetPresent = true
	}
	return target, true, targetPresent, nil
}

// VerifyLink reports whether the link's target still hashes to hash — the deep
// (integrity) check, the links counterpart of storage.Local.VerifyBlob. Opening
// the link path follows the symlink to the external original, which is only read,
// never modified. A missing link or unreadable target reports false (not healthy).
func (l *Linker) VerifyLink(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("invalid hash %q", hash)
	}
	p, exists, err := l.firstLinkEntry(hash)
	if err != nil || !exists {
		return false, err
	}
	f, err := os.Open(p) // follows the symlink to the external original (read-only)
	if err != nil {
		return false, nil // dangling / unreadable → not verifiable, treat as not intact
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("hash link target %s: %w", hash, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == hash, nil
}

// LinksUsage is the links-storage accounting/health snapshot (data-sources P5):
// the number of link entries, how many are broken (dangling — target gone), and
// the external bytes referenced (stat-through-symlink sizes of the live targets).
// External bytes are physically outside data_dir, so callers report them
// separately and never fold them into the on-disk library footprint.
type LinksUsage struct {
	Count         int    `json:"count"`
	Broken        int    `json:"broken"`
	ExternalBytes uint64 `json:"external_bytes"`
}

// Usage walks the links storage once (links/audio/<hash>/*) and tallies a
// LinksUsage. A missing links tree (nothing imported yet) yields the zero value.
// It only reads symlink entries and stat-follows their targets; nothing is
// modified. A target that fails to stat (dangling) counts as Broken and adds no
// bytes — so importing 200 GB in place still adds 0 to the on-disk footprint.
func (l *Linker) Usage() (LinksUsage, error) {
	var u LinksUsage
	audioRoot := filepath.Join(l.root, storage.AudioSubdir)
	hashDirs, err := os.ReadDir(audioRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return u, nil
		}
		return u, fmt.Errorf("read links audio dir: %w", err)
	}
	for _, hd := range hashDirs {
		if !hd.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(audioRoot, hd.Name()))
		if err != nil {
			return u, fmt.Errorf("read links hash dir %s: %w", hd.Name(), err)
		}
		for _, e := range entries {
			u.Count++
			p := filepath.Join(audioRoot, hd.Name(), e.Name())
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				u.ExternalBytes += uint64(info.Size())
			} else {
				u.Broken++
			}
		}
	}
	return u, nil
}

// Remove deletes the link(s) for hash by removing the hash dir. os.RemoveAll
// unlinks a symlink entry without following it, so the external original is never
// touched — but the kind-aware delete path operates on the link by storage kind
// deliberately, never invoking the local DeleteAll on a path that resolves
// through a link. A missing dir is not an error.
func (l *Linker) Remove(hash string) error {
	if !validHash.MatchString(hash) {
		return fmt.Errorf("invalid hash %q", hash)
	}
	if err := os.RemoveAll(l.hashDir(hash)); err != nil {
		return fmt.Errorf("remove links hash dir: %w", err)
	}
	return nil
}
