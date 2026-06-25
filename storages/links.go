package storages

import (
	"fmt"
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
