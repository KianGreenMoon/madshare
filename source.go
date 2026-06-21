//go:build embedsource

package main

import _ "embed"

// sourceArchive is the AGPL Corresponding Source baked into release builds.
// `make build` runs `git archive HEAD` to produce source.tar.gz (tracked files
// at the built commit), then compiles with `-tags embedsource`; the bytes are
// served verbatim at GET /source, so an installed binary carries its own source
// with no working tree present.
//
// Without the build tag this file is excluded, embeddedSourceTGZ (declared in
// madshare.go) stays nil, and /source falls back to building the archive from
// `git ls-files`. source.tar.gz is a generated, gitignored artifact referenced
// only here, so untagged `go build`/`go run`/`go test` never require it.
//
//go:embed source.tar.gz
var sourceArchive []byte

func init() { embeddedSourceTGZ = sourceArchive }
