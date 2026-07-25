# Madshare .pkg post-install: seed the live configuration from the shipped
# samples, the FreeBSD convention — the package owns *.sample, the admin owns
# the file next to it, so an upgrade refreshes the sample and never touches
# local edits. A config can hold the first-run admin password, so the live copy
# is readable by the service account only.
for f in madshare.toml webui.toml; do
	target="${PKG_ROOTDIR}%%CONFDIR%%/${f}"
	sample="${target}.sample"
	[ -f "${sample}" ] || continue
	if [ ! -f "${target}" ]; then
		cp -p "${sample}" "${target}" || exit $?
		echo "Created ${target} from ${f}.sample"
	fi
	chown root:madshare "${target}" 2>/dev/null || true
	chmod 0640 "${target}" 2>/dev/null || true
done
