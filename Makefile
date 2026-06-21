# Madshare build helpers.
#
# The version stamp is injected at build time so the web UI's About box can show
# the release tag. `git describe` yields the tag on a tagged commit (e.g.
# v0.2.1), or a short commit hash otherwise, with a "-dirty" suffix for an
# unclean tree. Plain `go build`/`go run` (without these targets) still work —
# they just fall back to the commit hash the Go toolchain embeds automatically,
# or to nothing when VCS info is unavailable. See docs/building.md.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -X 'daemonlord.ygg/madshare/internal/version.Tag=$(VERSION)'

.PHONY: build build-nowebui source-archive run test vet clean

# AGPL Corresponding Source, embedded into release binaries so /source works
# with no working tree. `git archive HEAD` packages the tracked files at the
# built commit; source_embed.go //go:embed's it under -tags embedsource. The
# tarball is a generated, gitignored artifact, regenerated on every build.
source-archive:
	git archive --format=tar.gz -o source.tar.gz HEAD

# Full-stack server binary (API + web UI + admin) with the version baked in
# and the source archive embedded for AGPL §13 compliance.
build: source-archive
	go build -tags embedsource -ldflags "$(LDFLAGS)" -o madshare ./

# Pure-API binary (no web UI); see docs/building.md "Build variants".
build-nowebui: source-archive
	go build -tags "nowebui embedsource" -ldflags "$(LDFLAGS)" -o madshare ./

# Run straight from source (template paths are resolved relative to the CWD).
# No source embedding: /source falls back to git ls-files in the CWD.
run:
	go run -ldflags "$(LDFLAGS)" ./

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f madshare source.tar.gz
