#!/bin/sh
# Madshare deb/rpm post-install. Deliberately does NOT enable or start the
# service: a fresh install has an unreviewed config and no bootstrap password,
# so starting it would only produce a failed unit. The remaining steps are
# printed instead, and are the same ones `make install` prints.
#
# Argument conventions differ — dpkg passes "configure <old-version>", rpm
# passes 1 (install) or 2 (upgrade) — so a first install is "no old version".
set -e

USER=madshare
GROUP=madshare
HOMEDIR=/var/lib/madshare
CONFDIR=/etc/madshare

case "$1" in
	configure) [ -z "${2:-}" ] && FIRST_INSTALL=yes || FIRST_INSTALL=no ;;
	1) FIRST_INSTALL=yes ;;
	2) FIRST_INSTALL=no ;;
	*) FIRST_INSTALL=no ;;
esac

# The package ships the directory, but an upgrade over a hand-made install may
# find it owned by someone else; only the ownership of the directory itself is
# touched, never what is inside it.
if [ -d "$HOMEDIR" ]; then
	chown "$USER:$GROUP" "$HOMEDIR" 2>/dev/null || true
	chmod 0750 "$HOMEDIR" 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "$FIRST_INSTALL" = yes ]; then
	cat <<EOF

Madshare is installed but not enabled. Remaining setup:

  1. Review $CONFDIR/madshare.toml. Data paths in it are resolved relative to
     the service's working directory ($HOMEDIR), so the defaults land in
     $HOMEDIR/data. The shipped config listens on 127.0.0.1:3000.

  2. Set the first-run admin password. It is consumed only while no user
     account exists and ignored forever after:
       echo 'MADSHARE_INITIAL_ADMIN_PASSWORD=choose-a-strong-password' \\
         > $CONFDIR/madshare.env
       chmod 600 $CONFDIR/madshare.env

  3. systemctl enable --now madshare

Madshare speaks plain HTTP; put it behind a TLS-terminating reverse proxy.
Example nginx configurations: /usr/share/doc/madshare/nginx/

EOF
fi

exit 0
