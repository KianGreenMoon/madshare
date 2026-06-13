//go:build !unix

package storage

// diskUsage has no portable implementation off Unix (no statfs), so it reports
// errNoStatfs and Stats() falls back to HasVolume=false. The server targets
// Linux; this file only keeps the package building on other platforms.
func diskUsage(string) (total, free, used uint64, err error) {
	return 0, 0, 0, errNoStatfs
}
