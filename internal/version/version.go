// Package version exposes the program's build metadata (release tag and commit
// hash) for display in the web UI's About box.
package version

import (
	"runtime/debug"
	"sync"
)

// Name is the program's display name.
const Name = "Madshare"

// Tag is the release tag, injected at build time via the Makefile:
//
//	-ldflags "-X 'daemonlord.ygg/madshare/internal/version.Tag=$(git describe --tags --always --dirty)'"
//
// Plain `go build`/`go run` leave it empty; Get then falls back to the commit
// hash that the Go toolchain embeds automatically in the build info, and to ""
// (unknown) if even that is unavailable.
var Tag string

// Info is the resolved build metadata shown in the About box.
type Info struct {
	Name    string // always Name ("Madshare")
	Version string // Tag if set, else the short commit hash, else "" (unknown)
	Commit  string // full commit hash from the build info, else ""
}

var (
	once   sync.Once
	cached Info
)

// Get resolves the build metadata once and caches it; the build info is constant
// for the life of the process.
func Get() Info {
	once.Do(func() { cached = resolve() })
	return cached
}

func resolve() Info {
	info := Info{Name: Name}

	var commit string
	var modified bool
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	info.Commit = commit

	switch {
	case Tag != "":
		info.Version = Tag // `git describe --dirty` already encodes a dirty tree
	case commit != "":
		v := shortHash(commit)
		if modified {
			v += "-dirty"
		}
		info.Version = v
	default:
		info.Version = "" // unknown at compile time
	}
	return info
}

func shortHash(h string) string {
	const n = 12
	if len(h) > n {
		return h[:n]
	}
	return h
}
