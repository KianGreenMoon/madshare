# Madshare

Madshare is a federated, self-hosted audio/media sharing server (primarily for
music). A single Go process serves a JSON API, file storage/streaming, and a
bundled web UI; federation between trusted servers is a planned later phase.

> Concept and roadmap: [`madshare.org`](madshare.org). Architecture docs live in
> [`docs/architecture/`](docs/architecture/).

## Features

- Upload, browse, stream and download audio (MP3 / OGG / FLAC / WAV / MP4 / M4A
  / AAC / OPUS), de-duplicated by content hash, with ID3/MP4/FLAC/OGG tag
  extraction and async cover-art variant generation.
- Two bundled web UIs: a Jellyfin-style drill-down browser at `/` and a
  cmus-style 3-panel view at `/cmus`, plus an `/admin` page.
- Authentication (session cookies + API tokens), role-based permissions, and
  per-file/-group content access control (default-deny for anonymous visitors).
- One process, one HTTP listener per configured socket; the web UI can be
  compiled out for a pure-API binary.

## Requirements

- **Go 1.26+** to build (the only build dependency).
- SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — pure
  Go, so **no cgo / no system SQLite** is required.
- Disk for the database and uploaded blobs (see `[storage]`).

## Build & run

```bash
# Build everything
go build ./...

# Build the server binary
go build -o madshare ./

# Or run directly (must be run from the project root on first build so the
# embedded assets resolve; runtime data paths are resolved relative to CWD)
go run madshare.go
```

Pure-API build (no web UI):

```bash
go build -tags nowebui -o madshare ./
```

A `nowebui` binary aborts startup if a listener still asks to `serve = ["webui"]`.

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
relative URLs, so there is no `public_url`. Validation rejects an empty
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
(delete/edit + full library), **uploader** (upload + full library), **listener**
(play/download the full library). Any *authenticated* user therefore sees the
whole library; **anonymous** (not-logged-in) visitors are **default-deny** and
see only files explicitly marked guest-playable (or free-licensed). Per-group /
per-file grants exist for custom restricted roles. Manage users and access from
the `/admin` page. Details: [`docs/architecture/auth.md`](docs/architecture/auth.md).

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
`GET /source` and the license text at `GET /license`.
