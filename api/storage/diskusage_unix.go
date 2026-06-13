//go:build unix

package storage

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

// diskUsage statfs(2)es path and returns df-style figures for the filesystem
// that holds it:
//
//	total = capacity of the filesystem
//	free  = bytes available to an unprivileged process (df "Avail", Bavail)
//	used  = total minus all free blocks incl. root-reserved (df "Used", matches
//	        what df prints, which is why used+avail < total on a typical fs)
//
// The base directory may not exist yet on a fresh install (it is created on the
// first upload), so an ENOENT walks up to the nearest existing ancestor — free
// space is a property of the mount, so any ancestor on the same filesystem
// yields the same numbers.
//
// Block count fields are unsigned; Bsize can be signed on some platforms, so it
// is converted via uint64 after a guard.
func diskUsage(path string) (total, free, used uint64, err error) {
	var st syscall.Statfs_t
	for {
		err = syscall.Statfs(path, &st)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, 0, 0, err
		}
		parent := filepath.Dir(path)
		if parent == path { // reached the root and still nothing — give up
			return 0, 0, 0, err
		}
		path = parent
	}
	bsize := uint64(st.Bsize)
	if st.Bsize < 0 {
		bsize = 0
	}
	total = st.Blocks * bsize
	free = st.Bavail * bsize
	used = (st.Blocks - st.Bfree) * bsize
	return total, free, used, nil
}
