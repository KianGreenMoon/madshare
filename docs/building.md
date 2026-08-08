# Building Madshare

How to build, test, and produce the different binary variants. For *running* and
configuring the result, see the top-level [`README.md`](../README.md); for the
listener/route-group model the variants rely on, see
[`architecture/listeners-and-config.md`](architecture/listeners-and-config.md).

## Prerequisites

- **Go 1.26+** (see the `go` directive in [`go.mod`](../go.mod)) — all a plain
  `go build` needs.
- **git** — required only by `make build`, which embeds the AGPL Corresponding
  Source via `git archive HEAD` and stamps the version via `git describe`. A
  plain `go build`/`go run` needs no git: it embeds no source archive, and
  `/source` then falls back to a runtime `git ls-files` (see below).
- **No C toolchain / no cgo.** SQLite is provided by the pure-Go
  [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) driver, so there
  is no system-SQLite or cgo requirement. Builds work with `CGO_ENABLED=0` and
  cross-compile cleanly (see below).
- **`zstd`** — only for `make release`, which compresses the FreeBSD `.pkg` with
  it. That target also fetches a pinned [nfpm](https://github.com/goreleaser/nfpm)
  through the module cache on its first run; nothing else needs the network.

The Go module path is `daemonlord.ygg/madshare`; the entry point is
`madshare.go` at the repository root.

### Optional runtime tools (not build-time deps)

Two external binaries enable ingest **media analysis** (see
[`architecture/recordings.md`](architecture/recordings.md)). Both are looked up
on `PATH` at startup, and neither affects the build:

- **`ffprobe`** (from FFmpeg) — fills the audio tech columns (duration, bitrate,
  sample rate, channels, codec, bit depth). Absent → those columns stay empty.
  Always optional; a missing binary only warns.
- **`fpcalc`** (Chromaprint) — computes the acoustic fingerprint used for
  same-audio (recording) identity. Absent → fingerprinting is disabled and
  duplicate detection degrades to a tag-based check. Optional for a standalone
  server, **required once `[federation].enabled` is set** (see below).

**Federation requires `fpcalc`.** A federated node re-fingerprints downloaded
audio locally before it joins a recording, because a peer's claims about it are
hints and never facts; without fpcalc that check cannot happen, so the node
would import and re-publish content it is unable to verify — a cost borne by its
peers, not just by itself. Startup therefore **refuses** when federation is
enabled and fpcalc is missing, naming the package to install. Setting
`[federation] allow_missing_fingerprinting = true` federates anyway. Installing
the tool and restarting repairs the existing library on its own: the startup
backfill re-analyses every file that still lacks a fingerprint.

`ffprobe` is deliberately *not* gated the same way. Without it a node's catalog
carries no quality facts, so friends cannot rank its renditions — worse output,
not unverified input — and it is by far the heavier install of the two.

The distribution packages list both as **weak** dependencies (`Recommends`),
which apt and dnf install by default: a requirement conditional on a config
setting is not something package metadata can express, so the rule lives at
runtime where the config is known.

## Standard build

```bash
# Compile every package (the usual "does it build?" check).
go build ./...

# Produce the server binary in the working directory.
go build -o madshare ./
```

Run it (or run straight from source):

```bash
./madshare
# or
go run madshare.go
```

The compiled binary is self-contained: the web UI templates and static assets
are **embedded at compile time** (`//go:embed` in
[`webui/webui.go`](../webui/webui.go)), so the binary has no dependency on the
source tree at runtime. Note the distinction:

- **Embedded assets** — baked in at build time; path-independent at runtime.
- **Runtime data paths** (`database.path`, `storage.files_dir`) — resolved
  relative to the **current working directory** when the server starts. Launch
  the process from a stable directory (or use absolute paths in the config; the
  systemd unit pins `WorkingDirectory`).
- **`GET /source` archive (AGPL §13)** — `make build` embeds it into the binary
  (`git archive HEAD` → `source.tar.gz`, baked in via `//go:embed` under
  `-tags embedsource`), so an installed binary serves its own Corresponding
  Source with no working tree. A plain `go build`/`go run` omits the tag and
  falls back to building the archive at runtime from `git ls-files` in the CWD
  (output only when started inside a git checkout, 404 otherwise). The bundled
  `LICENSE.md` served at `GET /license` is embedded unconditionally and always
  available. See [`docs/api/source.md`](api/source.md).

## Version stamping

The web UI's **About** box (header → *About* → *Version*) shows the build's
version and commit. Two pieces of data feed it:

- **Commit hash** — embedded automatically by the Go toolchain (`runtime/debug`
  build info, the `vcs.revision`/`vcs.modified` settings) on any ordinary
  `go build`/`go run` inside the checkout. Nothing extra is required.
- **Release tag** — *not* embedded automatically; it must be injected at build
  time via `-ldflags` into `internal/version.Tag`. The
  [`Makefile`](../Makefile) does this for you:

  ```bash
  make build          # full stack, version baked in   → ./madshare
  make build-nowebui  # pure-API variant, version baked in
  make run            # go run from source, version baked in
  ```

  `make build` resolves the version with `git describe --tags --always --dirty`,
  so it shows the tag on a tagged commit (`v0.2.1`), a short commit hash
  otherwise, and a `-dirty` suffix for an unclean tree. Equivalent manual
  command:

  ```bash
  go build -ldflags "-X 'daemonlord.ygg/madshare/internal/version.Tag=$(git describe --tags --always --dirty)'" -o madshare ./
  ```

A plain `go build ./` / `go run madshare.go` is still fine for development — the
About box just falls back to the embedded commit hash (and to an em-dash when
even that is unavailable, e.g. a `-buildvcs=false` build).

## Installing (`make install`)

On POSIX systems `make install` puts a built server in place system-wide:

```bash
sudo make install
```

It will:

- **build first only if `./madshare` is missing** (otherwise it installs the
  existing binary — run `make build` yourself to force a fresh one);
- install the binary to `$(BINDIR)` (`/usr/local/bin/madshare`, mode `0755`);
- create `$(CONFDIR)` (`/etc/madshare`) and copy `madshare.toml.example` /
  `webui.toml.example` there, then seed the live `madshare.toml` / `webui.toml`
  from them **only if absent** — re-running never clobbers an edited config;
- install a service definition for the detected init system, with its
  `/usr/local/bin` and `/etc/madshare` paths rewritten to match `BINDIR` /
  `CONFDIR`:
  - **systemd** (when `systemctl` is on `PATH`, or `DESTDIR` is set) —
    [`contrib/systemd/madshare.service`](../contrib/systemd/madshare.service) to
    `$(SYSTEMD_UNIT_DIR)` (`/etc/systemd/system`);
  - **OpenRC** (when `rc-update` is on `PATH`, or `DESTDIR` is set) —
    [`contrib/openrc/madshare.initd`](../contrib/openrc/madshare.initd) to
    `$(OPENRC_INITD_DIR)/madshare` (`/etc/init.d`, mode `0755`) plus a
    no-clobber [`madshare.confd`](../contrib/openrc/madshare.confd) seeded to
    `$(OPENRC_CONFD_DIR)/madshare` (`/etc/conf.d`). The init script uses
    `supervise-daemon` for restart-on-failure parity with the systemd unit.
  - **FreeBSD rc.d** (when `uname -s` is `FreeBSD`) —
    [`contrib/freebsd/madshare.in`](../contrib/freebsd/madshare.in) to
    `$(FREEBSD_RCD_DIR)/madshare` (`/usr/local/etc/rc.d`, mode `0755`), with its
    `%%PREFIX%%` / `%%CONFDIR%%` / `%%DATADIR%%` markers substituted. It wraps
    the server in `daemon(8)` for the pidfile, the privilege drop and syslog
    output; `madshare_user`, `madshare_chdir` and `madshare_config` are all
    overridable in `/etc/rc.conf.d/madshare`.

  Both Linux init systems can be installed at once (e.g. under `DESTDIR` for a
  package that targets both); on a real host only the one whose tool is present
  fires.

It intentionally does **not** create the `madshare` service user, write the
admin-password env file, or `daemon-reload`/`enable` the unit — those need root
decisions and aren't idempotent. The exact commands are printed at the end (and
listed in the [README deployment section](../README.md#deployment)).

Overridable variables (GNU-style, plus `DESTDIR` for staged/packaging installs):

| Variable | Default | Purpose |
|---|---|---|
| `PREFIX` | `/usr/local` | install root; `BINDIR` derives as `$(PREFIX)/bin` |
| `BINDIR` | `$(PREFIX)/bin` | where the binary lands |
| `SYSCONFDIR` | `/etc` | config root; `CONFDIR` derives as `$(SYSCONFDIR)/madshare` |
| `CONFDIR` | `$(SYSCONFDIR)/madshare` | config directory |
| `SYSTEMD_UNIT_DIR` | `/etc/systemd/system` | systemd unit destination |
| `OPENRC_INITD_DIR` | `/etc/init.d` | OpenRC init-script destination |
| `OPENRC_CONFD_DIR` | `/etc/conf.d` | OpenRC conf.d destination |
| `FREEBSD_RCD_DIR` | `$(PREFIX)/etc/rc.d` | FreeBSD rc.d destination |
| `DATADIR` | `/var/lib/madshare` | working directory baked into the rc.d script — pass `/var/db/madshare` on FreeBSD to match what the `.pkg` uses |
| `DESTDIR` | *(empty)* | staging prefix prepended to every path |
| `INSTALL` | `install` | the install(1) program |

```bash
make install PREFIX=/usr               # /usr/bin/madshare
make install DESTDIR=/tmp/pkg          # stage a package tree (forces unit install)
```

`make uninstall` removes the binary, the unit, and the installed `*.example`
files; it deliberately keeps the live `*.toml` config and any data directory.

There is **no Windows install target** — Windows has no `/usr/local` or systemd
and `make` is uncommon there. On Windows just `go build -o madshare.exe ./` and
run the binary with a `madshare.toml` beside it (or pass `-config <path>`).

## Release packages (`make release`)

```bash
make release          # everything, into ./dist
```

Builds the distributable artifacts for every supported platform **on one host**,
without root, without distro tooling and without a FreeBSD machine — the whole
matrix is cross-compiled, and each packager is a program that runs anywhere:

| Target | Artifacts | Package architecture |
|---|---|---|
| `linux/amd64` | `.deb`, `.rpm`, `.tar.gz` | `amd64` / `x86_64` |
| `linux/arm64` | `.deb`, `.rpm`, `.tar.gz` | `arm64` / `aarch64` |
| `linux/armhf` | `.deb`, `.rpm`, `.tar.gz` | `armhf` / `armv7hl` |
| `freebsd/amd64` | `.pkg`, `.tar.gz` | `freebsd:14:x86:64` |
| `freebsd/arm64` | `.pkg`, `.tar.gz` | `freebsd:14:aarch64` |

plus a `SHA256SUMS` over all of them.

**`armhf` is a packaging name, not a `GOARCH`.** It means 32-bit ARM with
hardware floating point, which Go spells `GOARCH=arm` plus a `GOARM` level, and
which deb and rpm each spell differently again — so the script keeps the label
(`armhf`, used in filenames and metadata) separate from the toolchain values.
`ARMHF_GOARM` picks the level: **7** by default, because that is what "armhf"
means to Debian and Fedora (ARMv7-A + VFPv3). A **Raspberry Pi 1 or Zero is
ARMv6** and needs `ARMHF_GOARM=6`; an ARMv6 build also runs on ARMv7, so it is
the safe choice for a mixed fleet at the cost of the newer instructions.

The 32-bit target is a full build, not a reduced one: federation included.
Verified under `qemu-arm-static` (this project's development host is Apple
silicon, which cannot execute 32-bit ARM at all) — SQLite, an 18 MB upload with
tag extraction, cover-variant generation, `ffprobe`/`fpcalc` analysis, the
yggdrasil transport, a `[[listen_mesh]]` bind on the node's own mesh address, and
an ordered shutdown all behave. Byte accounting across the codebase is `int64`
and every 64-bit atomic is a typed `atomic.Int64`, so neither of the two usual
32-bit hazards — truncated sizes, unaligned atomics — applies. The orchestration lives in
[`packaging/release.sh`](../packaging/release.sh); `make release` only calls it,
so the script is equally usable on its own.

Release binaries differ from `make build` in three ways: they are stripped
(`-s -w`), built with `-trimpath` (no local paths in the binary), and always
carry the embedded AGPL Corresponding Source (`-tags embedsource`).

**A dirty tree is refused.** The binary embeds `git archive HEAD` as its
Corresponding Source, so uncommitted changes would ship a binary whose `/source`
endpoint does not match it — a licensing defect rather than a matter of taste.
Use `make release ALLOW_DIRTY=1` for a throwaway build.

Knobs (environment variables): `VERSION`, `DIST` (default `dist`), `TARGETS`
(default: all five above), `ARMHF_GOARM` (default `7`), `FREEBSD_ABI` (default
`14`), `GO`, `NFPM_VERSION`.

```bash
make release TARGETS="linux/amd64"           # one platform
make release TARGETS="linux/armhf" ARMHF_GOARM=6   # Raspberry Pi 1 / Zero
make release FREEBSD_ABI=15                  # .pkg for FreeBSD 15
```

### Versions

`git describe` produces one string; packagers need a version/release pair, and
neither an RPM `Release` nor a FreeBSD version may contain a dash. The script
splits it:

| `git describe` | tarball / binary stamp | deb, rpm | FreeBSD |
|---|---|---|---|
| `v0.6.0` | `0.6.0` | `0.6.0-1` | `0.6.0` |
| `v0.6.0-51-gc3b1cd1` | `0.6.0-51-gc3b1cd1` | `0.6.0-51.gc3b1cd1` | `0.6.0_51.gc3b1cd1` |
| untagged (`c3b1cd1`) | `c3b1cd1` | `0.0.0-0.c3b1cd1` | `0.0.0_0.c3b1cd1` |

### What the packages do

All of them install the binary, the two configs, the platform's service
definition and the docs, create the `madshare` system user, and **stop there** —
nothing is enabled or started, because a fresh install has an unreviewed config
and no bootstrap password. The next steps are printed on install.

| | deb / rpm | FreeBSD pkg |
|---|---|---|
| binary | `/usr/bin/madshare` | `/usr/local/bin/madshare` |
| config | `/etc/madshare/` | `/usr/local/etc/madshare/` |
| data | `/var/lib/madshare` | `/var/db/madshare` |
| service | `/usr/lib/systemd/system/madshare.service` | `/usr/local/etc/rc.d/madshare` |
| docs | `/usr/share/doc/madshare/` | `/usr/local/share/doc/madshare/` |

The live `madshare.toml` / `webui.toml` are readable only by `root:madshare`
(they can carry the first-run admin password) and survive upgrades: on deb/rpm
as `conffiles` / `%config(noreplace)`, on FreeBSD via the `.sample` convention
(the package owns `madshare.toml.sample`, post-install copies it once). Removal
keeps the database, the media and the service account — `apt remove` must never
delete somebody's library — and says so.

### How each format is produced

- **deb + rpm** — [nfpm](https://github.com/goreleaser/nfpm) via
  `go run github.com/goreleaser/nfpm/v2/cmd/nfpm@<pinned>`, so no `dpkg-deb` or
  `rpmbuild` is needed. The recipe is
  [`packaging/nfpm.yaml`](../packaging/nfpm.yaml), a template whose
  `${MADSHARE_*}` markers `release.sh` substitutes (nfpm's own environment
  expansion does not reach `contents[].src`). Maintainer scripts are in
  [`packaging/scripts/`](../packaging/scripts/) and are written to work under
  both conventions — dpkg passes `configure`/`remove`, rpm passes `1`/`0`.
  Note that nfpm silently **drops** a content entry with an unknown `type`; the
  valid set is `symlink`, `ghost`, `config`, `config|noreplace`, `dir`, `tree`.
- **FreeBSD pkg** — [`packaging/cmd/fbsdpkg`](../packaging/cmd/fbsdpkg), a small
  build-tagged (`tools`) Go program. A `.pkg` is a zstd-compressed tar carrying
  `+COMPACT_MANIFEST` and `+MANIFEST` in front of an absolute-path payload, so
  it can be produced anywhere. The manifest shape was read off real packages
  from `pkg.freebsd.org` — including the two details that are easy to guess
  wrong: each `files` entry is an object (not a bare checksum), and its `sum` is
  the sha256 hex prefixed with `1$`. The `.pkg` declares an ABI
  (`FreeBSD:14:amd64`), and pkg(8) refuses a package built for a different major
  version, hence `FREEBSD_ABI`.
- **tarballs** — binary, config templates, `contrib/` for that platform, and a
  generated `INSTALL.txt` with the exact commands
  ([`packaging/INSTALL-linux.txt`](../packaging/INSTALL-linux.txt),
  [`packaging/INSTALL-freebsd.txt`](../packaging/INSTALL-freebsd.txt)).

## Build variants

Two build tags are yours to choose by hand — **`nowebui`** (drop the bundled web
UI) and **`nofederation`** (drop the madnetwork node) — and they are independent
of each other and of the server-only / UI-only split, which is a *runtime* choice
driven by each listener's `serve` route groups. Both tags subtract; the default
build has everything. (A third tag, `embedsource`, bakes the AGPL source archive
into the binary, but the `Makefile` manages it for every `make build` — you don't
set it manually. See the `/source` note above.)

### Full stack (default)

`go build ./` — one binary that can serve the API, the bundled web UI, and the
admin endpoints, in any combination per listener. This is what most deployments
want.

### Server only — pure API (`-tags nowebui`)

```bash
go build -tags nowebui -o madshare ./
```

Compiles the API/admin server **without** the web UI: no embedded templates or
static assets, and the `html/template` dependency is dropped, yielding a smaller
binary. The webui package compiles to a stub (`webui/webui_stub.go`,
`Available = false`) whose `Register*` functions are no-ops.

Guard rail: such a binary **aborts at startup** if any `[[listen]]` still asks to
`serve = ["webui"]` (it cannot honor it). Drop `webui` (and the `/admin` *page*,
which is part of the webui package — the `/api/admin/*` API stays) from those
listeners, or rebuild without the tag.

You can also get an API-only deployment from a *full* binary at runtime by simply
not serving the `webui` group; the `nowebui` tag is the choice when you want the
smaller artifact / no template dependency.

### Standalone — no federation (`-tags nofederation`)

```bash
go build -tags nofederation -o madshare ./
```

Compiles the server **without** the madnetwork node: the `federation` package
becomes a stub (`federation/node_stub.go`, `Available = false`) and the embedded
yggdrasil + gVisor netstack dependencies drop out of the binary entirely. That is
the real reason to reach for it — they are the heaviest dependencies in the tree,
and a node that will never federate has no use for them.

Guard rail, mirroring `nowebui`: such a binary **aborts at startup** if the config
still sets `[federation].enabled = true`, rather than starting up quietly
un-federated. Turn the setting off, or rebuild without the tag.

Federation is off by default in *any* build, so this tag is about artifact size
and dependency surface, not about disabling a feature — for that, just leave
`[federation].enabled` alone. See the README's
[Deploying a madnetwork node](../README.md#deploying-a-madnetwork-node).

### Web UI only (runtime, no separate build)

There is **no UI-only binary**. To host the UI apart from the API, run a *full*
(non-`nowebui`) binary whose listener serves only `webui`, and point it at the
remote API with `[webui].api_base`:

```toml
[[listen]]
addr  = "0.0.0.0"
port  = 8080
serve = ["webui"]

[webui]
api_base = "https://api.example.org"
```

See the README's "Splitting the web UI and the API" section for the full notes
(the `api_base` requirement, CORS). **This split is supported but not yet
regularly tested.**

## Tests & checks

```bash
go test ./...          # full test suite
go vet ./...           # static checks
go test -tags nowebui ./...        # verify the pure-API variant still compiles/passes
go build -tags nofederation ./...  # …and the standalone one
```

There are no JS/CSS build steps — the web UI is hand-written vanilla
HTML/CSS/ES-modules served as-is. If you edit assets under `webui/`, **rebuild
the binary** to pick them up (they are embedded), then restart the server.

## Cross-compilation

For the supported release targets this is already done for you — see
[Release packages](#release-packages-make-release). By hand: because the build
is pure Go, cross-compiling is just `GOOS`/`GOARCH`:

```bash
# Linux arm64 (e.g. a Raspberry Pi 3+ / ARM server)
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -o madshare-linux-arm64 ./

# Linux armhf — 32-bit ARM, hard float (ARMv7 boards, older Pi). Go 1.26 already
# defaults GOARM to 7; it is spelled out because that default has changed before,
# and because 6 is what an ARMv6 board (Pi 1 / Zero) needs.
CGO_ENABLED=0 GOOS=linux  GOARCH=arm GOARM=7 go build -o madshare-linux-armhf ./

# Linux amd64, API-only
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -tags nowebui -o madshare-api-linux-amd64 ./

# macOS arm64
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o madshare-darwin-arm64 ./

# FreeBSD amd64
CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -o madshare-freebsd-amd64 ./
```

A smaller binary (strip debug info):

```bash
go build -ldflags="-s -w" -o madshare ./
```
