#!/bin/sh
# Madshare deb/rpm pre-remove: stop the service, but only when the package is
# really going away. An upgrade must leave it running — dpkg says "upgrade",
# rpm says 1 (and 0 for the last copy being removed).
set -e

case "$1" in
	0|remove|purge) REMOVING=yes ;;
	*) REMOVING=no ;;
esac

if [ "$REMOVING" = yes ] && command -v systemctl >/dev/null 2>&1; then
	systemctl --quiet is-active madshare 2>/dev/null && systemctl stop madshare || true
	systemctl --quiet is-enabled madshare 2>/dev/null && systemctl disable madshare || true
fi

exit 0
