#!/bin/sh
# Madshare deb/rpm post-remove. The library, the database and the service
# account are deliberately kept: `apt remove` / `dnf remove` must never delete
# somebody's media collection. What survives is named, so removing it stays a
# deliberate act.
set -e

case "$1" in
	0|remove|purge) REMOVING=yes ;;
	*) REMOVING=no ;;
esac

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	[ "$REMOVING" = yes ] && systemctl reset-failed madshare >/dev/null 2>&1 || true
fi

if [ "$REMOVING" = yes ] && [ -d /var/lib/madshare ]; then
	echo "Madshare data kept at /var/lib/madshare (database, uploaded media)."
	echo "The 'madshare' user and group were kept; remove them with 'userdel madshare' if unwanted."
fi

exit 0
