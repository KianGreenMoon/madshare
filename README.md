# Madshare

Madshare is a self-hosted audio/media sharing server (primarily for music). A
single Go process serves a JSON API, file storage/streaming, and a bundled web
UI.

> ⚠️ **Federation is not implemented yet — it is only a plan.** Server-to-server
> federation (the "madnetwork": trusted-peer key exchange, cross-server
> upload/download/streaming, library sharing) is a *future phase*, not part of the
> current code. Today Madshare runs as a **standalone single-server instance**.
> The federation concept is described in [`madshare.org`](madshare.org) for
> reference only.

> Concept and roadmap: [`madshare.org`](madshare.org). Architecture docs live in
> [`docs/architecture/`](docs/architecture/).

## Features

- Upload, browse, stream and download audio (MP3 / OGG / FLAC / WAV / MP4 / M4A
  / AAC / OPUS), de-duplicated by content hash, with ID3/MP4/FLAC/OGG tag
  extraction and async cover-art variant generation.
- Two bundled web UIs: a Jellyfin-style drill-down browser at `/` and a
  cmus-style 3-panel view at `/cmus`, plus an `/admin` page. The listening
  pages share a persistent shell, so playback continues across navigation.
- Per-user playlists and favorites (`/playlists`): like tracks from the
  library or the player bar, edit the play queue in place (reorder / remove /
  play-next), save it as a playlist, and resume it after a reload. Player and
  queue behavior (shuffle, repeat, persistence):
  [`docs/ui/player-and-queue.md`](docs/ui/player-and-queue.md).
- Authentication (session cookies + API tokens), role-based permissions, and
  per-file/-group content access control (default-deny for anonymous visitors).
- Upload moderation: every upload stages privately in the uploader's
  "My uploads" area until a moderator approves it into the library (or returns
  it with a note, or discards it to Trash); moderators' own uploads
  self-approve. [`docs/architecture/moderation.md`](docs/architecture/moderation.md).
- Same-audio **recordings**: files that are the same audio in different encodings
  (e.g. a FLAC master and a 320 kbps MP3) are grouped by acoustic fingerprint. A
  moderator `/admin/duplicates` page compares renditions by quality, removes
  redundant copies (per-row or bulk "Select non-best"), and the player can switch
  rendition quality. A possible duplicate is flagged in moderation and never
  auto-approved. Needs the optional `ffprobe`/`fpcalc` tools (see Requirements);
  without them it degrades to a tag-based duplicate check.
  [`docs/architecture/recordings.md`](docs/architecture/recordings.md).
- One process, one HTTP listener per configured socket; the web UI can be
  compiled out for a pure-API binary.

## Requirements

- **Go 1.26+** to build — all a plain `go build` requires.
- **git** is needed only by `make build`, which embeds the AGPL source archive
  (`git archive`) and stamps the version (`git describe`); a plain `go build`
  works without it.
- SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — pure
  Go, so **no cgo / no system SQLite** is required.
- Disk for the database and uploaded blobs (see `[storage]`).
- *Optional runtime tools* for the recordings feature, looked up on `PATH` at
  startup (a missing tool only logs a warning): **`ffprobe`** (FFmpeg — fills
  audio tech columns) and **`fpcalc`** (Chromaprint — acoustic fingerprint).
  Neither is a build dependency.

## Build & run

> Full build reference (variants, build tags, cross-compilation, tests):
> [`docs/building.md`](docs/building.md).

```bash
# Build everything
go build ./...

# Build the server binary
go build -o madshare ./

# Or run directly (must be run from the project root on first build so the
# embedded assets resolve; runtime data paths are resolved relative to CWD)
go run madshare.go
```

The [`Makefile`](Makefile) wraps these and bakes in the version stamp so the web
UI's **About** box (header → *About* → *Version*) shows the release tag:

```bash
make build   # -> ./madshare, with `git describe --tags --always --dirty` injected
make run     # go run from source, version stamped
make test    # go test ./...
```

A plain `go build` / `go run madshare.go` still works — the About box just falls
back to the commit hash the Go toolchain embeds automatically (or nothing if VCS
info is unavailable). Cross-compiling honors the usual `GOOS`/`GOARCH` env vars
(pure Go, no cgo). Full reference: [`docs/building.md`](docs/building.md).

Pure-API build (no web UI):

```bash
go build -tags nowebui -o madshare ./
```

A `nowebui` binary aborts startup if a listener still asks to `serve = ["webui"]`.

### Splitting the web UI and the API (server-only / UI-only)

> ⚠️ **Supported but not yet regularly tested.** The bundled, same-origin setup
> (one process serving everything) is the well-trodden path. The split below is a
> deliberate feature, but exercise it carefully and report issues.

There are two independent axes — the **route groups** a listener serves (runtime)
and the **`nowebui` build tag** (compile-time):

**1. Server side only.** Two ways:

- *Runtime:* run a normal binary whose listener serves only the API/admin groups —
  no UI is exposed even though it's compiled in:

  ```toml
  [[listen]]
  addr  = "0.0.0.0"
  port  = 3000
  serve = ["api", "admin"]   # no "webui"
  ```

- *Compile-time:* build with `-tags nowebui` for a smaller binary that physically
  omits the embedded templates/assets (and the `html/template` dependency). This
  is the right choice for an API-only deployment.

**2. Web UI only.** Run a (full, non-`nowebui`) binary whose listener serves just
the `webui` group, and point it at the API hosted elsewhere via `[webui].api_base`.
The bundled UI normally uses *relative, same-origin* URLs; `api_base` makes the
rendered pages target a remote API instead:

```toml
[[listen]]
addr  = "0.0.0.0"
port  = 8080
serve = ["webui"]

[webui]
api_base = "https://api.example.org"   # the separately hosted API origin
```

Notes:

- A `webui` listener with **no** `api` on the same origin *must* set `api_base`,
  or the page loads but its API calls 404.
- There is no UI-only *binary* build — the web UI is always part of the full
  binary; "UI only" is the runtime configuration above.
- Cross-origin UI→API calls work: the API sends permissive CORS headers. Put both
  behind one hostname (e.g. via the reverse proxy) if you'd rather keep it
  same-origin.

### First run — bootstrap the admin

On a **fresh database** Madshare creates the first admin account from the
`[auth]` config and then refuses to start if no password was supplied. Prefer the
environment variable for the password:

```bash
export MADSHARE_INITIAL_ADMIN_PASSWORD='choose-a-strong-password'
go run madshare.go
```

The admin is created with `password_change_required`, so you'll be prompted to
set a new password on first login (via the `/admin` page). After users exist the
initial-admin settings are ignored (and a warning is logged if still present).

Stop the server with `Ctrl-C` (SIGINT/SIGTERM trigger a graceful shutdown).

## Configuration

Madshare reads two optional TOML files. If neither exists, built-in defaults
apply (a single loopback listener serving the full stack). Both are gitignored.

| File | Purpose | Flag (default) |
|---|---|---|
| `madshare.toml` | Server config: listeners, database, storage, auth. | `-config ./madshare.toml` |
| `webui.toml` | UI-side upload controls, served to the browser at `GET /api/ui/config`. | `-webui-config ./webui.toml` |

```bash
go run madshare.go -config /etc/madshare/madshare.toml -webui-config /etc/madshare/webui.toml
```

Copy the examples to start:

```bash
cp madshare.toml.example madshare.toml
cp webui.toml.example     webui.toml
```

### `madshare.toml`

```toml
# One [[listen]] block per bound socket; each picks which route groups it serves:
#   api    -> /healthz, /api/* (library), /files/*, /images/*   (the product)
#   webui  -> /, /cmus, /static/*                               (bundled UI)
#   admin  -> /api/admin/* (delete, prune) and the /admin page  (destructive)
[[listen]]
addr  = "127.0.0.1"          # "" / "0.0.0.0" / "[::]" = all interfaces; or a specific IP
port  = 3000
serve = ["api", "webui", "admin"]
#allow_from = ["192.168.1.0/24"]   # optional per-listener source-IP allowlist (NOT auth)

#[webui]
#api_base = ""               # empty = bundled same-origin (relative URLs). Only set
                             # this for a SEPARATELY hosted UI pointing at a remote API.

[database]
path = "./data/madshare.db"

[storage]
files_dir = "./data/files"   # uploaded blobs; cover images go in <files_dir>/images
max_upload_mb = 500          # per-request upload cap, in MiB
#server_max_parallel_workers = 0   # total concurrent uploads, all users (0 = unlimited)
#user_max_parallel_workers   = 0   # concurrent uploads per user (0 = unlimited)
#image_processing_workers    = 0   # cover-resize goroutines; 0 = auto (NumCPU)

[auth]
initial_admin_user = "admin"
# Prefer the MADSHARE_INITIAL_ADMIN_PASSWORD env var over putting the password here.
#initial_admin_password = "..."
```

**Listeners and route groups** are *deployment topology, not access control.* The
listener/route split decides which URLs are reachable on which socket; it is not
authentication. The bundled web UI is served same-origin with the API and uses
relative URLs, so it needs no configured base address. Validation rejects an empty
`[[listen]]` list, bad ports, unknown `serve` groups, invalid `allow_from` CIDRs,
and conflicting binds (a wildcard bind collides with anything else on the same
port; identical specific binds are rejected; reusing a port across *different*
specific addresses is allowed). Non-fatal advisories (e.g. a `webui` listener
with no `api`) are logged at startup. Full model:
[`docs/architecture/listeners-and-config.md`](docs/architecture/listeners-and-config.md).

### `webui.toml`

```toml
[upload]
default_parallel_workers = 3    # initial value of the upload page's worker slider
max_parallel_workers     = 10   # ceiling the slider can be raised to
```

Served verbatim (and publicly) at `GET /api/ui/config` — the upload page needs it
before login.

## Roles & content access (in brief)

Four built-in roles bundle capabilities: **admin** (everything), **moderator**
(upload/delete/edit + moderation + full library), **uploader** (upload + full
library), **listener** (play/download the full library). Any *authenticated*
user therefore sees the whole library; **anonymous** (not-logged-in) visitors
are **default-deny** and see only files explicitly marked guest-playable (or
free-licensed). Per-group / per-file grants exist for custom restricted roles.
Manage users and access from the `/admin` page; review staged uploads in
`/admin/library` (the Review tab) and reconcile same-audio duplicates on
`/admin/duplicates`. Details:
[`docs/architecture/auth.md`](docs/architecture/auth.md),
[`docs/architecture/moderation.md`](docs/architecture/moderation.md),
[`docs/architecture/recordings.md`](docs/architecture/recordings.md).

## Deployment

Madshare itself speaks plain HTTP and is meant to run behind a reverse proxy that
terminates TLS (or on an already-encrypted overlay like Yggdrasil). A typical
production layout binds Madshare to loopback and lets nginx face the network:

```toml
[[listen]]
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]
```

Ready-to-edit reverse-proxy examples are in
[`contrib/nginx/`](contrib/nginx/):

- `madshare-ssl.conf` — public deployment, terminates HTTPS, HTTP→HTTPS redirect.
- `madshare-yggdrasil.conf` — plain HTTP bound to a Yggdrasil address (no TLS;
  the overlay encrypts the link).

Key proxy concerns (covered in the examples): set `client_max_body_size` ≥
`storage.max_upload_mb`, and disable response buffering + forward `Range` for
audio streaming/seeking. See [`contrib/nginx/README.md`](contrib/nginx/README.md).

**Run as a service.** Example definitions are provided for both init systems:
[`contrib/systemd/madshare.service`](contrib/systemd/madshare.service) and
[`contrib/openrc/`](contrib/openrc/) (`madshare.initd` + `madshare.confd`). Both
run Madshare as a dedicated `madshare` system user with working directory
`/var/lib/madshare` (so the default relative data paths land there), read config
from `/etc/madshare/`, and take the first-run admin password from the
environment. The file headers list the full setup steps.

The file-placement parts are automated by `sudo make install` (builds if needed,
installs the binary to `/usr/local/bin`, seeds `/etc/madshare/{madshare,webui}.toml`
without clobbering an existing config, and — autodetecting the init system —
installs the systemd unit when `systemctl` is present and/or the OpenRC service
when `rc-update` is present; overridable via `PREFIX` / `SYSCONFDIR` / `DESTDIR`;
see [`docs/building.md`](docs/building.md#installing-make-install)). It
deliberately does **not** create the service user or enable the service; do
those by hand:

```bash
sudo make install                                  # binary + config + service
sudo useradd --system --home /var/lib/madshare --shell /usr/sbin/nologin madshare
sudo install -d -o madshare -g madshare /var/lib/madshare

# systemd:
echo 'MADSHARE_INITIAL_ADMIN_PASSWORD=...' | sudo tee /etc/madshare/madshare.env >/dev/null
sudo chmod 600 /etc/madshare/madshare.env
sudo systemctl daemon-reload && sudo systemctl enable --now madshare

# OpenRC (instead of the systemd lines): export the password in
# /etc/conf.d/madshare, then:
sudo rc-update add madshare default && sudo rc-service madshare start
```

`make uninstall` reverses the file placement, keeping your live config and data.

**Data & backups.** Everything stateful lives under the paths in `[database]` and
`[storage]` (by default `./data/`): the SQLite DB (`madshare.db*` — note the WAL
files) and the blob/image tree. Resolve these relative to the server's working
directory; back them up together. The web UI assets are embedded in the binary,
so the UI has no CWD dependency, but `database.path` / `storage.files_dir` do.

**Upgrades.** Database migrations run automatically on startup; just deploy the
new binary and restart.

## Endpoints (overview)

| Path | Group | Notes |
|---|---|---|
| `GET /healthz` | api | Health check. |
| `GET /api/*` | api | Library browse (artists/albums/tracks/search), auth, UI config. |
| `POST /files/upload`, `GET /files/*` | api | Upload (gated `file.upload`) and stream/download. |
| `GET /images/*` | api | Cover images. |
| `GET /`, `/cmus`, `/static/*` | webui | Bundled web UI. |
| `GET /admin`, `/api/admin/*` | admin | Admin page + destructive/management ops. |
| `GET /source` | api | AGPL §13: `tar.gz` of the git-tracked source. |
| `GET /license` | api | The project license. |

## License

Madshare is licensed under the **GNU AGPL-3.0** (see [`LICENSE.md`](LICENSE.md)).
For §13 network-use compliance the running server publishes its own source at
`GET /source` and the license text at `GET /license`. `make build` embeds both
into the binary (the source archive via `-tags embedsource`), so an installed
binary serves its Corresponding Source with no working tree present.
