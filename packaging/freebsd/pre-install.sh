# Madshare .pkg pre-install: create the service account before the payload is
# unpacked, so the data directory the manifest declares can be created owned by
# it. Runs under sh(1) with pkg's environment; PKG_ROOTDIR is set when pkg is
# installing into an alternate root, which pw(8) must be told about.
if [ -n "${PKG_ROOTDIR}" ] && [ "${PKG_ROOTDIR}" != "/" ]; then
	PW="/usr/sbin/pw -R ${PKG_ROOTDIR}"
else
	PW=/usr/sbin/pw
fi

if ! ${PW} groupshow madshare >/dev/null 2>&1; then
	echo "Creating group 'madshare'"
	${PW} groupadd madshare || exit $?
fi

if ! ${PW} usershow madshare >/dev/null 2>&1; then
	echo "Creating user 'madshare'"
	${PW} useradd madshare -g madshare -d %%DATADIR%% -s /usr/sbin/nologin \
		-c "Madshare server" || exit $?
fi
