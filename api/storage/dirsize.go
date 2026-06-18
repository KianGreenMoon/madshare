package storage

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// DirSize returns the total size in bytes of all regular files under dir,
// walking subdirectories. It is the per-category disk-usage measure for the
// admin storage panel: audio (<files_dir>/audio), images (<files_dir>/images),
// and any future category subtree are each sized by a DirSize of their root.
//
// A missing dir yields (0, nil): a fresh install has no images/ (or video/)
// directory until the first cover (or video) is written, and "absent" means
// "zero bytes", not an error. Any other walk error — e.g. an intermediate path
// component that is a regular file (ENOTDIR), or a permission failure — is
// returned so the caller can surface it rather than report a wrong total.
func DirSize(dir string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += uint64(info.Size())
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return total, nil
}
