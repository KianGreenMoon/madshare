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
// The statfs fields have platform-dependent signedness and width — Bsize is
// int64 on Linux and uint64 on FreeBSD, and Bavail is unsigned on Linux but
// *signed* on FreeBSD, where it legitimately goes negative once a filesystem
// eats into its root reservation. [statCount] normalizes all of them, clamping
// a negative count to zero, so the arithmetic below has one shape everywhere.
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
	bsize := blocks64(st.Bsize)
	total = blocks64(st.Blocks) * bsize
	free = blocks64(st.Bavail) * bsize
	used = (blocks64(st.Blocks) - blocks64(st.Bfree)) * bsize
	return total, free, used, nil
}

// statCount is the set of types the various platforms give the statfs block
// counters and block size.
type statCount interface {
	~int32 | ~uint32 | ~int64 | ~uint64
}

// blocks64 widens a statfs counter to uint64, treating a negative value as zero.
func blocks64[T statCount](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
