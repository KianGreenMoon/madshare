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

.PHONY: build build-nowebui source-archive run test vet clean install uninstall

# Installation layout (POSIX systems only — see the note on `install` below).
# GNU-style overrides, plus DESTDIR staging for packagers:
#   make install PREFIX=/usr            # binary -> /usr/bin/madshare
#   make install DESTDIR=/tmp/pkg       # staged install (packaging); no systemctl
#   make install SYSCONFDIR=/usr/local/etc
DESTDIR          ?=
PREFIX           ?= /usr/local
BINDIR           ?= $(PREFIX)/bin
SYSCONFDIR       ?= /etc
CONFDIR          ?= $(SYSCONFDIR)/madshare
SYSTEMD_UNIT_DIR ?= /etc/systemd/system
OPENRC_INITD_DIR ?= /etc/init.d
OPENRC_CONFD_DIR ?= /etc/conf.d
INSTALL          ?= install

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

# Install the full-stack binary, seed config under $(CONFDIR), and — depending
# on the detected init system — drop in the systemd unit (systemctl present)
# and/or the OpenRC service (rc-update present), with their paths rewritten for
# the chosen PREFIX/CONFDIR. Builds first only if ./madshare is missing.
#
# It never clobbers a live config: the *.example files are always refreshed,
# but madshare.toml / webui.toml are created only when absent. It does NOT
# create the service user or enable the unit (those need root + decisions and
# are not idempotent) — the commands are printed at the end.
#
# `install` is POSIX-only. Windows has no /usr/local or systemd and `make` is
# uncommon there: just `go build -o madshare.exe ./` and run the binary with a
# madshare.toml beside it (or pass `-config <path>`).
install:
	@test -x ./madshare || $(MAKE) build
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m 0755 madshare "$(DESTDIR)$(BINDIR)/madshare"
	$(INSTALL) -d "$(DESTDIR)$(CONFDIR)"
	$(INSTALL) -m 0644 madshare.toml.example "$(DESTDIR)$(CONFDIR)/madshare.toml.example"
	$(INSTALL) -m 0644 webui.toml.example    "$(DESTDIR)$(CONFDIR)/webui.toml.example"
	@if [ ! -f "$(DESTDIR)$(CONFDIR)/madshare.toml" ]; then \
		echo "  CREATE  $(DESTDIR)$(CONFDIR)/madshare.toml (from example)"; \
		$(INSTALL) -m 0644 madshare.toml.example "$(DESTDIR)$(CONFDIR)/madshare.toml"; \
	else echo "  KEEP    $(DESTDIR)$(CONFDIR)/madshare.toml (already present)"; fi
	@if [ ! -f "$(DESTDIR)$(CONFDIR)/webui.toml" ]; then \
		echo "  CREATE  $(DESTDIR)$(CONFDIR)/webui.toml (from example)"; \
		$(INSTALL) -m 0644 webui.toml.example "$(DESTDIR)$(CONFDIR)/webui.toml"; \
	else echo "  KEEP    $(DESTDIR)$(CONFDIR)/webui.toml (already present)"; fi
	@if [ -n "$(DESTDIR)" ] || command -v systemctl >/dev/null 2>&1; then \
		echo "  INSTALL $(DESTDIR)$(SYSTEMD_UNIT_DIR)/madshare.service"; \
		$(INSTALL) -d "$(DESTDIR)$(SYSTEMD_UNIT_DIR)"; \
		sed -e 's|/usr/local/bin|$(BINDIR)|g' -e 's|/etc/madshare|$(CONFDIR)|g' \
			contrib/systemd/madshare.service > "$(DESTDIR)$(SYSTEMD_UNIT_DIR)/madshare.service"; \
		chmod 0644 "$(DESTDIR)$(SYSTEMD_UNIT_DIR)/madshare.service"; \
	else echo "  SKIP    systemd unit (systemctl not found; set DESTDIR to stage anyway)"; fi
	@if [ -n "$(DESTDIR)" ] || command -v rc-update >/dev/null 2>&1; then \
		echo "  INSTALL $(DESTDIR)$(OPENRC_INITD_DIR)/madshare"; \
		$(INSTALL) -d "$(DESTDIR)$(OPENRC_INITD_DIR)"; \
		sed -e 's|/usr/local/bin|$(BINDIR)|g' -e 's|/etc/madshare|$(CONFDIR)|g' \
			contrib/openrc/madshare.initd > "$(DESTDIR)$(OPENRC_INITD_DIR)/madshare"; \
		chmod 0755 "$(DESTDIR)$(OPENRC_INITD_DIR)/madshare"; \
		$(INSTALL) -d "$(DESTDIR)$(OPENRC_CONFD_DIR)"; \
		if [ ! -f "$(DESTDIR)$(OPENRC_CONFD_DIR)/madshare" ]; then \
			echo "  CREATE  $(DESTDIR)$(OPENRC_CONFD_DIR)/madshare (from example)"; \
			sed -e 's|/etc/madshare|$(CONFDIR)|g' \
				contrib/openrc/madshare.confd > "$(DESTDIR)$(OPENRC_CONFD_DIR)/madshare"; \
			chmod 0644 "$(DESTDIR)$(OPENRC_CONFD_DIR)/madshare"; \
		else echo "  KEEP    $(DESTDIR)$(OPENRC_CONFD_DIR)/madshare (already present)"; fi; \
	else echo "  SKIP    OpenRC service (rc-update not found; set DESTDIR to stage anyway)"; fi
	@echo
	@echo "Installed. Remaining one-time setup (needs root; not done automatically):"
	@echo "  1. useradd --system --home /var/lib/madshare --shell /usr/sbin/nologin madshare"
	@echo "     install -d -o madshare -g madshare /var/lib/madshare"
	@if command -v systemctl >/dev/null 2>&1; then \
		echo "  2. echo 'MADSHARE_INITIAL_ADMIN_PASSWORD=...' | tee $(CONFDIR)/madshare.env >/dev/null"; \
		echo "     chmod 600 $(CONFDIR)/madshare.env   # ignored once the first admin exists"; \
		echo "  3. Review $(CONFDIR)/madshare.toml, then: systemctl daemon-reload && systemctl enable --now madshare"; \
	elif command -v rc-update >/dev/null 2>&1; then \
		echo "  2. Uncomment & export MADSHARE_INITIAL_ADMIN_PASSWORD in $(OPENRC_CONFD_DIR)/madshare"; \
		echo "     (ignored once the first admin exists)"; \
		echo "  3. Review $(CONFDIR)/madshare.toml, then: rc-update add madshare default && rc-service madshare start"; \
	else \
		echo "  2. Export MADSHARE_INITIAL_ADMIN_PASSWORD in the environment before first start."; \
		echo "  3. Review $(CONFDIR)/madshare.toml, then run:"; \
		echo "     $(BINDIR)/madshare -config $(CONFDIR)/madshare.toml -webui-config $(CONFDIR)/webui.toml"; \
	fi

# Remove the binary, the service units (systemd + OpenRC), and the installed
# *.example files. Leaves the live config ($(CONFDIR)/*.toml), the OpenRC
# conf.d, and the data dir untouched on purpose — remove those by hand if you
# really mean to.
uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/madshare"
	rm -f "$(DESTDIR)$(SYSTEMD_UNIT_DIR)/madshare.service"
	rm -f "$(DESTDIR)$(OPENRC_INITD_DIR)/madshare"
	rm -f "$(DESTDIR)$(CONFDIR)/madshare.toml.example" "$(DESTDIR)$(CONFDIR)/webui.toml.example"
	rmdir "$(DESTDIR)$(CONFDIR)" 2>/dev/null || true
	@echo "Removed binary + units + examples. Kept $(CONFDIR)/*.toml, $(OPENRC_CONFD_DIR)/madshare and /var/lib/madshare (if any)."
