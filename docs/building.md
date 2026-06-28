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

The Go module path is `daemonlord.ygg/madshare`; the entry point is
`madshare.go` at the repository root.

### Optional runtime tools (not build-time deps)

Two external binaries enable ingest **media analysis** (see
[`architecture/recordings.md`](architecture/recordings.md)). Both are looked up
on `PATH` at startup and are **entirely optional** — neither affects the build,
and a missing tool only logs a warning and disables its own output:

- **`ffprobe`** (from FFmpeg) — fills the audio tech columns (duration, bitrate,
  sample rate, channels, codec, bit depth). Absent → those columns stay empty.
- **`fpcalc`** (Chromaprint) — computes the acoustic fingerprint used for
  same-audio (recording) identity. Absent → fingerprinting is disabled and
  duplicate detection degrades to a tag-based check.

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

  Both can be installed at once (e.g. under `DESTDIR` for a package that
  targets both); on a real host only the one whose tool is present fires.

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

## Build variants

`nowebui` is the only build tag you choose by hand; the server-only / UI-only
split is otherwise a *runtime* choice driven by each listener's `serve` route
groups. The two axes are independent. (A second tag, `embedsource`, bakes the
AGPL source archive into the binary, but the `Makefile` manages it for every
`make build` — you don't set it manually. See the `/source` note above.)

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
go test -tags nowebui ./...   # verify the pure-API variant still compiles/passes
```

There are no JS/CSS build steps — the web UI is hand-written vanilla
HTML/CSS/ES-modules served as-is. If you edit assets under `webui/`, **rebuild
the binary** to pick them up (they are embedded), then restart the server.

## Cross-compilation

Because the build is pure Go, cross-compiling is just `GOOS`/`GOARCH`:

```bash
# Linux arm64 (e.g. a Raspberry Pi / ARM server)
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -o madshare-linux-arm64 ./

# Linux amd64, API-only
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -tags nowebui -o madshare-api-linux-amd64 ./

# macOS arm64
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o madshare-darwin-arm64 ./
```

A smaller binary (strip debug info):

```bash
go build -ldflags="-s -w" -o madshare ./
```
