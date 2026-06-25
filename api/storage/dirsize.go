package storage

import (
	"errors"
	"io/fs"
	"os"
)

// DirSize returns the total size in bytes of all regular files under dir,
// walking subdirectories. It is the per-category disk-usage measure for the
// admin storage panel: audio (<files_dir>/audio), images (<variants_dir>/images),
// and any future category subtree are each sized by a DirSize of their root.
//
// It is deliberately tolerant, because it sizes a live tree that other
// goroutines mutate concurrently (the image pool writes cover variants; prune
// and delete remove them) and reports a best-effort advisory figure, not a
// transactional one:
//
//   - A missing dir yields (0, nil): a fresh install has no images/ (or video/)
//     directory until the first write, and "absent" means "zero bytes".
//   - An entry that vanishes mid-walk (fs.ErrNotExist) is skipped, not treated
//     as a wrong total. Without this, a single cover deleted while the walk runs
//     would otherwise collapse the entire reported size.
//   - An unreadable entry (fs.ErrPermission) is skipped rather than failing the
//     whole panel; the figure is then a slight undercount, which is preferable
//     to a 500 for an advisory metric.
//
// Any other error — notably a path component that is a regular file (ENOTDIR),
// which signals misconfiguration, not a transient race — is returned so the
// caller can surface it instead of reporting a wrong total.
func DirSize(dir string) (uint64, error) {
	return dirSizeFS(os.DirFS(dir))
}

// dirSizeFS is the fs.FS-backed core, split out so the tolerant error handling
// (vanished/unreadable entries) can be exercised deterministically with a
// synthetic filesystem in tests.
func dirSizeFS(fsys fs.FS) (uint64, error) {
	var total uint64
	err := fs.WalkDir(fsys, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil // missing root, vanished entry, or unreadable dir — skip
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil // entry removed between readdir and stat — skip
			}
			return err
		}
		total += uint64(info.Size())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
