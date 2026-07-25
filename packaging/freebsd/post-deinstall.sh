# Madshare .pkg post-deinstall: remove the seeded configuration only when it is
# still byte-identical to the sample (nothing of the admin's to lose), and never
# touch the library, the database or the account — deleting somebody's media on
# `pkg delete` would be indefensible. What is left behind is named explicitly.
for f in madshare.toml webui.toml; do
	target="${PKG_ROOTDIR}%%CONFDIR%%/${f}"
	sample="${target}.sample"
	if [ -f "${target}" ] && [ -f "${sample}" ] && cmp -s "${target}" "${sample}"; then
		rm -f "${target}"
	fi
done

if [ -d "${PKG_ROOTDIR}%%DATADIR%%" ]; then
	echo "==> Madshare data kept at %%DATADIR%% (database, uploaded media)."
fi
if [ -f "${PKG_ROOTDIR}%%CONFDIR%%/madshare.toml" ]; then
	echo "==> Modified configuration kept at %%CONFDIR%%/madshare.toml."
fi
echo "==> The 'madshare' user and group were kept; remove them with 'pw userdel madshare -r' if unwanted."
