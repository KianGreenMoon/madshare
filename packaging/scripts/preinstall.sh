#!/bin/sh
# Madshare deb/rpm pre-install: create the service account BEFORE the payload is
# unpacked, so the config files (root:madshare 0640) and the data directory
# (madshare:madshare 0750) land with the ownership the package declares — a
# group that does not exist yet at unpack time silently degrades to root.
set -e

USER=madshare
GROUP=madshare
HOMEDIR=/var/lib/madshare

# nologin sits in /usr/sbin on Debian and /sbin on Fedora/SUSE.
SHELL_NOLOGIN=/usr/sbin/nologin
[ -x "$SHELL_NOLOGIN" ] || SHELL_NOLOGIN=/sbin/nologin
[ -x "$SHELL_NOLOGIN" ] || SHELL_NOLOGIN=/bin/false

if ! getent group "$GROUP" >/dev/null 2>&1; then
	groupadd --system "$GROUP"
fi

if ! getent passwd "$USER" >/dev/null 2>&1; then
	useradd --system --gid "$GROUP" --home-dir "$HOMEDIR" \
		--no-create-home --shell "$SHELL_NOLOGIN" \
		--comment "Madshare server" "$USER"
fi

exit 0
