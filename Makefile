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

.PHONY: build build-nowebui run test vet clean

# Full-stack server binary (API + web UI + admin) with the version baked in.
build:
	go build -ldflags "$(LDFLAGS)" -o madshare ./

# Pure-API binary (no web UI); see docs/building.md "Build variants".
build-nowebui:
	go build -tags nowebui -ldflags "$(LDFLAGS)" -o madshare ./

# Run straight from source (template paths are resolved relative to the CWD).
run:
	go run -ldflags "$(LDFLAGS)" ./

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f madshare
