#!/bin/sh
# Build the Madshare release artifacts: one static binary per target, wrapped as
# a .deb, an .rpm, a FreeBSD .pkg and a plain tarball. Invoked by `make release`;
# runnable on its own. Documented in docs/building.md "Release packages".
#
# Everything is produced on the build host, for every target, with no root and
# no distro tooling: Go cross-compiles the binaries (pure Go, CGO_ENABLED=0),
# nfpm writes the deb/rpm, and packaging/cmd/fbsdpkg writes the FreeBSD package.
#
# Knobs (all optional):
#   VERSION=v1.2.3     override the version stamp (default: git describe)
#   DIST=dist          output directory
#   TARGETS="os/arch…" which platforms to build
#   FREEBSD_ABI=14     FreeBSD major version the .pkg declares compatible
#   ALLOW_DIRTY=1      permit a release from a dirty tree (see below)
#   GO=go NFPM_VERSION=v2.43.0
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

: "${DIST:=dist}"
: "${TARGETS:=linux/amd64 linux/arm64 linux/armhf freebsd/amd64 freebsd/arm64}"
: "${FREEBSD_ABI:=14}"
# armhf is a PACKAGING name, not a GOARCH: it means 32-bit ARM with hardware
# floating point, which Go spells GOARCH=arm plus a GOARM level. 7 is what
# "armhf" means to Debian and Fedora (ARMv7-A + VFPv3), and it is the level the
# artifacts are named for. Set explicitly rather than left to the toolchain's
# default, which is 7 today but has not always been. A Raspberry Pi 1 / Zero is
# ARMv6 and needs ARMHF_GOARM=6 — that binary still runs on ARMv7, so it is the
# safe choice for a mixed fleet, at the cost of the newer instructions.
: "${ARMHF_GOARM:=7}"
: "${GO:=go}"
: "${NFPM_VERSION:=v2.43.0}"
NFPM="$GO run github.com/goreleaser/nfpm/v2/cmd/nfpm@${NFPM_VERSION}"

MAINTAINER="Kian Seibel <kian.eugen.seibel@gmail.com>"
HOMEPAGE="https://github.com/KianGreenMoon/madshare"
COMMENT="Self-hosted audio and media sharing server"

WORK="$DIST/work"

say() { printf '\n==> %s\n' "$*"; }

# ── Version ──────────────────────────────────────────────────────────────────
# A release ships its own Corresponding Source (AGPL §13): the binary embeds
# `git archive HEAD`, so uncommitted changes would produce a binary whose
# /source endpoint does not match it. That is a licensing defect, not a taste
# question, hence the refusal — ALLOW_DIRTY=1 is for local trial runs.
if [ -n "$(git status --porcelain 2>/dev/null)" ] && [ "${ALLOW_DIRTY:-0}" != 1 ]; then
	echo "release: the working tree has uncommitted changes." >&2
	echo "  A release binary embeds 'git archive HEAD' as its AGPL Corresponding" >&2
	echo "  Source, which would then not match the binary. Commit first, or set" >&2
	echo "  ALLOW_DIRTY=1 for a throwaway build." >&2
	exit 1
fi

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)}
FILEVER=${VERSION#v}

# Split the git description into the version/release pair packagers expect:
# "v0.6.0" -> 0.6.0 release 1; "v0.6.0-51-gc3b1cd1" -> 0.6.0 release 51.gc3b1cd1
# (a dash is legal in neither an rpm Release nor a FreeBSD version). An untagged
# tree has no version at all, so it becomes 0.0.0 with the commit as release —
# which also sorts below every real tag.
case "$FILEVER" in
[0-9]*.[0-9]*)
	PKG_VERSION=${FILEVER%%-*}
	PKG_REST=${FILEVER#"$PKG_VERSION"}
	PKG_REST=${PKG_REST#-}
	if [ -n "$PKG_REST" ]; then
		PKG_RELEASE=$(printf '%s' "$PKG_REST" | tr '-' '.')
	else
		PKG_RELEASE=1
	fi
	;;
*)
	PKG_VERSION=0.0.0
	PKG_RELEASE=0.$(printf '%s' "$FILEVER" | tr '-' '.')
	;;
esac

# FreeBSD parses "name-version", so its version may not contain a dash; "_N" is
# the native suffix for a revision on top of an upstream version.
if [ "$PKG_RELEASE" = 1 ]; then
	FBSD_VERSION=$PKG_VERSION
else
	FBSD_VERSION="${PKG_VERSION}_${PKG_RELEASE}"
fi

LDFLAGS="-X daemonlord.ygg/madshare/internal/version.Tag=${VERSION} -s -w"

say "Madshare ${VERSION} (package version ${PKG_VERSION}, release ${PKG_RELEASE})"

rm -rf "$WORK"
mkdir -p "$DIST" "$WORK"

# AGPL Corresponding Source, embedded into every release binary via
# -tags embedsource (see the source-archive target in the Makefile).
say "Embedding Corresponding Source (git archive HEAD)"
git archive --format=tar.gz -o source.tar.gz HEAD

# subst <src> <dst> <mode> — expand the %%PATH%% markers the service templates
# carry. The paths differ per platform (FreeBSD keeps everything under
# /usr/local and puts state in /var/db), so the templates stay generic and the
# packaging decides.
subst() {
	sed \
		-e "s|%%PREFIX%%|${PREFIX}|g" \
		-e "s|%%BINDIR%%|${BINDIR}|g" \
		-e "s|%%CONFDIR%%|${CONFDIR}|g" \
		-e "s|%%DATADIR%%|${DATADIR}|g" \
		-e "s|%%RCDIR%%|${RCDIR}|g" \
		-e "s|%%DOCDIR%%|${DOCDIR}|g" \
		-e "s|%%VERSION%%|${VERSION}|g" \
		"$1" >"$2"
	chmod "$3" "$2"
}

for target in $TARGETS; do
	GOOS=${target%/*}
	GOARCH=${target#*/}

	# ARCH is the name in artifacts and package metadata; GOARCH/GOARM are what
	# the toolchain wants. They differ only for armhf, where "arm" alone would
	# name three incompatible ABIs (armel/armhf, v5/v6/v7) and tell a person
	# downloading it nothing. NFPM_ARCH is nfpm's own spelling, which it maps per
	# packager (arm7 -> deb armhf, rpm armv7hl).
	ARCH=$GOARCH
	GOARM=
	NFPM_ARCH=$GOARCH
	case "$GOARCH" in
	armhf)
		GOARCH=arm
		GOARM=$ARMHF_GOARM
		NFPM_ARCH=arm$ARMHF_GOARM
		;;
	esac

	case "$GOOS" in
	freebsd)
		PREFIX=/usr/local
		BINDIR=$PREFIX/bin
		CONFDIR=$PREFIX/etc/madshare
		DATADIR=/var/db/madshare
		RCDIR=$PREFIX/etc/rc.d
		DOCDIR=$PREFIX/share/doc/madshare
		;;
	*)
		PREFIX=/usr/local
		BINDIR=$PREFIX/bin
		CONFDIR=/etc/madshare
		DATADIR=/var/lib/madshare
		RCDIR=
		DOCDIR=$PREFIX/share/doc/madshare
		;;
	esac

	if [ -n "$GOARM" ]; then
		say "Building ${GOOS}/${ARCH} (GOARCH=${GOARCH} GOARM=${GOARM})"
	else
		say "Building ${GOOS}/${ARCH}"
	fi
	BIN="$WORK/bin/${GOOS}-${ARCH}/madshare"
	mkdir -p "$(dirname "$BIN")"
	CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOARM="$GOARM" \
		"$GO" build -tags embedsource -trimpath -ldflags "$LDFLAGS" -o "$BIN" ./

	# ── Plain tarball ────────────────────────────────────────────────────
	# Self-contained and prefix-agnostic: unpack it anywhere, or follow the
	# generated INSTALL.txt to land it under /usr/local.
	NAME="madshare-${FILEVER}-${GOOS}-${ARCH}"
	TARDIR="$WORK/tar/$NAME"
	mkdir -p "$TARDIR/contrib"
	install -m 0755 "$BIN" "$TARDIR/madshare"
	install -m 0644 madshare.toml.example webui.toml.example LICENSE.md README.md "$TARDIR/"
	cp -R contrib/nginx "$TARDIR/contrib/"
	case "$GOOS" in
	freebsd)
		mkdir -p "$TARDIR/contrib/freebsd"
		subst contrib/freebsd/madshare.in "$TARDIR/contrib/freebsd/madshare" 0755
		;;
	*)
		cp -R contrib/systemd contrib/openrc "$TARDIR/contrib/"
		;;
	esac
	subst "packaging/INSTALL-${GOOS}.txt" "$TARDIR/INSTALL.txt" 0644
	tar -czf "$DIST/${NAME}.tar.gz" -C "$WORK/tar" "$NAME"
	echo "    ${NAME}.tar.gz"

	case "$GOOS" in
	linux)
		# ── deb + rpm (nfpm) ─────────────────────────────────────────
		# The unit ships with /usr/local rewritten to the package
		# prefix; everything else in it is already package-correct.
		UNIT="$WORK/systemd/${ARCH}/madshare.service"
		mkdir -p "$(dirname "$UNIT")"
		sed -e "s|/usr/local/bin|/usr/bin|g" contrib/systemd/madshare.service >"$UNIT"

		# The ${MADSHARE_*} markers are filled in here rather than left to
		# nfpm's own environment expansion, which does not reach into
		# contents[].src.
		CFG="$WORK/nfpm-${ARCH}.yaml"
		sed \
			-e "s|\${MADSHARE_ARCH}|${NFPM_ARCH}|g" \
			-e "s|\${MADSHARE_VERSION}|${PKG_VERSION}|g" \
			-e "s|\${MADSHARE_RELEASE}|${PKG_RELEASE}|g" \
			-e "s|\${MADSHARE_BIN}|${BIN}|g" \
			-e "s|\${MADSHARE_UNIT}|${UNIT}|g" \
			packaging/nfpm.yaml >"$CFG"
		for packager in deb rpm; do
			$NFPM package --config "$CFG" \
				--packager "$packager" --target "$DIST" >/dev/null
		done
		echo "    madshare ${PKG_VERSION}-${PKG_RELEASE} ${ARCH}: .deb .rpm"
		;;
	freebsd)
		# ── FreeBSD .pkg ─────────────────────────────────────────────
		case "$GOARCH" in
		amd64) FBSD_ABI="FreeBSD:${FREEBSD_ABI}:amd64"; FBSD_ARCH="freebsd:${FREEBSD_ABI}:x86:64" ;;
		arm64) FBSD_ABI="FreeBSD:${FREEBSD_ABI}:aarch64"; FBSD_ARCH="freebsd:${FREEBSD_ABI}:aarch64:64" ;;
		*) echo "release: no FreeBSD ABI mapping for $ARCH (32-bit arm is a Linux target only)" >&2; exit 1 ;;
		esac

		STAGE="$WORK/stage/freebsd-$ARCH"
		rm -rf "$STAGE"
		mkdir -p "$STAGE$BINDIR" "$STAGE$RCDIR" "$STAGE$CONFDIR" "$STAGE$DOCDIR/nginx"
		install -m 0555 "$BIN" "$STAGE$BINDIR/madshare"
		subst contrib/freebsd/madshare.in "$STAGE$RCDIR/madshare" 0755
		# The .sample convention: pkg owns these, post-install copies
		# each to its live name once, upgrades never clobber the copy.
		install -m 0644 madshare.toml.example "$STAGE$CONFDIR/madshare.toml.sample"
		install -m 0644 webui.toml.example "$STAGE$CONFDIR/webui.toml.sample"
		install -m 0644 README.md LICENSE.md "$STAGE$DOCDIR/"
		install -m 0644 contrib/nginx/* "$STAGE$DOCDIR/nginx/"

		SCRIPTS="$WORK/freebsd-scripts/$ARCH"
		mkdir -p "$SCRIPTS"
		for s in pre-install post-install post-deinstall; do
			subst "packaging/freebsd/${s}.sh" "$SCRIPTS/${s}.sh" 0644
		done
		subst packaging/freebsd/pkg-message "$SCRIPTS/pkg-message" 0644

		PKGFILE="$DIST/madshare-${FBSD_VERSION}-freebsd${FREEBSD_ABI}-${ARCH}.pkg"
		"$GO" run -tags tools ./packaging/cmd/fbsdpkg \
			-stage "$STAGE" \
			-name madshare \
			-version "$FBSD_VERSION" \
			-origin audio/madshare \
			-abi "$FBSD_ABI" \
			-arch "$FBSD_ARCH" \
			-prefix "$PREFIX" \
			-comment "$COMMENT" \
			-desc-file packaging/pkg-descr \
			-message-file "$SCRIPTS/pkg-message" \
			-maintainer "$MAINTAINER" \
			-www "$HOMEPAGE" \
			-license AGPLv3 \
			-category audio \
			-user madshare \
			-group madshare \
			-dir "$CONFDIR:root:wheel:0755" \
			-dir "$DATADIR:madshare:madshare:0750" \
			-script "pre-install=$SCRIPTS/pre-install.sh" \
			-script "post-install=$SCRIPTS/post-install.sh" \
			-script "post-deinstall=$SCRIPTS/post-deinstall.sh" \
			-o "$WORK/freebsd-$ARCH.tar"
		zstd -19 -q -f -o "$PKGFILE" "$WORK/freebsd-$ARCH.tar"
		echo "    $(basename "$PKGFILE")"
		;;
	esac
done

say "Checksums"
(cd "$DIST" && rm -f SHA256SUMS &&
	find . -maxdepth 1 -type f ! -name SHA256SUMS -printf '%P\n' |
	sort | xargs sha256sum >SHA256SUMS)

rm -rf "$WORK"

say "Artifacts in $DIST"
ls -lh "$DIST" | tail -n +2
