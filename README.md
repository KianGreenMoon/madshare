# Madshare

Madshare is a self-hosted audio/media sharing server (primarily for music). A
single Go process serves a JSON API, file storage/streaming, and a bundled web
UI.

> **Federation — the "madnetwork" — is built and off by default.** A node can
> peer with other madshare servers over an embedded [Yggdrasil](https://yggdrasil-network.github.io)
> mesh and browse, stream and fetch from their libraries. It stays disabled until
> you turn it on: see [Deploying a madnetwork node](#deploying-a-madnetwork-node),
> and read that section's posture note before you do — sharing with your
> community means sharing with people you never individually approved.

> Concept and roadmap: [`madshare.org`](madshare.org). Architecture docs live in
> [`docs/architecture/`](docs/architecture/).

## Features

- Upload, browse, stream and download audio (MP3 / OGG / FLAC / WAV / MP4 / M4A
  / AAC / OPUS), de-duplicated by content hash, with ID3/MP4/FLAC/OGG tag
  extraction and async cover-art variant generation.
- A bundled web UI: a Jellyfin-style drill-down browser at `/library`, plus an
  `/admin` page. `/` is a front door rather than a page: it forwards to
  `/madnetwork` on a node with federation enabled, and to `/library` otherwise.
  The listening pages share a persistent shell, so playback continues across
  navigation.
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
- **Federation (madnetwork)**, off by default: peer with other madshare servers
  over an embedded Yggdrasil mesh — no TUN device, no root, no open host port —
  and browse their libraries at `/madnetwork` beside your own — the landing view
  leads with your local library and a set of lanes, and every node has a page of
  its own (`/madnetwork/node/<key>`, a link you can send). Content is fetched
  from *whoever has the bytes* (multi-source chunked transfer with per-chunk
  verification), streams through a cache while it downloads, and lands in the
  review queue like any other upload. Per-recording sharing scope (Local /
  Direct friends / Madnetwork), and a network map of who your friends' friends
  are. The network also feeds back into your own library: a review card shows
  what other nodes call the audio being approved and warns when the tags and the
  fingerprint disagree, and `/admin/upgrades` lists renditions out there that
  rank above the copies you hold — additive, so nothing of yours is ever
  replaced without you saying so.
  [Deploying a madnetwork node](#deploying-a-madnetwork-node) ·
  [`docs/architecture/federation.md`](docs/architecture/federation.md).
- One process, one HTTP listener per configured socket; the web UI can be
  compiled out for a pure-API binary, and federation for a standalone one.

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
  **`fpcalc` stops being optional once federation is enabled** — the server
  refuses to start without it, because a federated node re-fingerprints what it
  downloads instead of trusting what a peer says about it. See
  [Deploying a madnetwork node](#deploying-a-madnetwork-node).

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
#   webui  -> /, /library, /playlists, /madnetwork*, /static/*  (bundled UI)
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

Two further sections are omitted above because they are opt-in features with
their own walkthroughs: `[federation]`
([Deploying a madnetwork node](#deploying-a-madnetwork-node)) and `[sources]`
(in-place symlink imports,
[`docs/architecture/data-sources.md`](docs/architecture/data-sources.md)).
`madshare.toml.example` carries both, commented, with every knob annotated.

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

### Reachable from anywhere, without a reverse proxy

The shortest path to a server you can reach from outside your LAN. Madshare
embeds an [Yggdrasil](https://yggdrasil-network.github.io) node (userspace
netstack — **no TUN device and no root**), and a `[[listen_mesh]]` block serves
the web UI and API on **that node's own address**:

```toml
[[listen]]                          # local admin
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]

[[listen_mesh]]                     # you, from anywhere
enabled = true                      # required; defaults to false
port    = 80
serve   = ["api", "webui", "admin"]

[yggdrasil]
enabled = true
peers   = ["tls://peer.example:12345"]   # any public peer; see the list below
```

Pick a peer from
[publicpeers.neilalexander.dev](https://publicpeers.neilalexander.dev/), start
the server, and it logs the address to hand out:

```
yggdrasil: mesh up — address 201:abcd:… (key file ./data/federation.key)
listening on mesh [201:abcd:…]:80 serving [api webui admin]
```

Anyone running Yggdrasil then reaches you at `http://[201:abcd:…]/` — brackets
required, no port, behind any NAT, on any network that lets you out at all. **No
certificate, no domain, no port forwarding, no `setcap`**: port 80 is free
because the netstack is not a kernel socket, the overlay encrypts end to end,
and the address is derived from the node's public key, so it is self-certifying
the way a `.onion` is.

Three things to know:

- **`enabled = true` is required on the block** — it defaults to false, and
  turning federation on does not switch it on. Unlike a `[[listen]]` address you
  type yourself, this one is derived from the node's key and its audience is the
  whole mesh, so it never arrives by copy-paste or inheritance. A block left off
  says so at startup rather than doing nothing quietly.
- **That address is not reachable from the server itself** (there is no TUN
  device). Keep the loopback `[[listen]]` for local administration — hence both
  blocks above.
- **Its audience is the whole Yggdrasil network**, not a chosen list.
  Authentication is the only gate, exactly as on a LAN listener; serving `admin`
  there is supported (that is the point — administering your node from your
  phone) and warns at startup so the exposure is never a surprise.
- **This does not federate you.** `[yggdrasil]` is the transport;
  [`[federation]`](#deploying-a-madnetwork-node) is the madnetwork feature on top
  of it, and enabling it later keeps the same key and therefore the same address.

Full reference: [`docs/architecture/listeners-and-config.md`](docs/architecture/listeners-and-config.md)
§4.3c.

### Behind a reverse proxy

Madshare itself speaks plain HTTP and can also run behind a reverse proxy that
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
  the overlay encrypts the link). Only needed with a *system* yggdrasil daemon;
  `[[listen_mesh]]` above reaches the mesh with no proxy and no root at all.

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

## Deploying a madnetwork node

Federation is off by default. Turning it on makes this server a node on the
**madnetwork**: an overlay of madshare servers that each had to be deliberately
friended in by somebody, joined transitively. Nothing here is exposed to the
public internet — the mesh is the only transport, every requester is
authenticated by a key it must possess, and a node outside your community is
served nothing at all.

> ⚠️ **Read this before enabling.** With the shipped default scope, your library
> is published to **your whole community** — every node reachable through a chain
> of friendships, at any distance, with no radius limit. That means your friends'
> friends' friends, people you will never meet or approve individually. This is
> the deal by design, not an oversight, and the two things that make it
> defensible are that nothing leaves the community and that any branch of it can
> be cut in one action. If you want a narrower line, set the node default to
> **Direct friends** on `/admin/settings`, and/or pin individual recordings to
> **Local** so they never travel at all — per recording in the Recordings lens of
> `/admin/library`, or in bulk from the All Appearances lens there.

### 1. Prerequisites

- **A binary with federation compiled in.** The default build has it; only
  `-tags nofederation` strips it, and such a binary aborts at startup if the
  config still enables federation.
- **`fpcalc` (Chromaprint) on `PATH`.** Required, not recommended: the server
  refuses to start otherwise. A federated node re-fingerprints downloaded audio
  locally before it joins a recording, because a peer's claim about what a file
  *is* is a hint and never a fact. `allow_missing_fingerprinting = true`
  federates anyway, at the cost of importing and re-publishing content this node
  cannot verify.
- **`ffprobe` (FFmpeg)** is genuinely optional here, but recommended: without it
  your published catalog carries no quality facts, so friends cannot rank your
  renditions against their own.
- **At least one Yggdrasil peer to dial.** A node with no `peers` and no
  `listen` joins nothing. Public peers are listed at
  [publicpeers.neilalexander.dev](https://publicpeers.neilalexander.dev/); if you
  already run Yggdrasil, its own peers work here too. Madshare embeds the mesh
  (userspace gVisor netstack) rather than using a system TUN, so it needs **no
  root and no `VpnService`** — and note it runs its *own* node identity even on a
  host that already has Yggdrasil installed.

### 2. Enable it

```toml
[federation]
enabled = true
name    = "my madshare"                          # display only; identity is the key
peers   = ["tls://peer.example:12345"]           # outbound; join the mesh
#listen = ["tls://0.0.0.0:12345"]                # only if others peer INTO you
```

Restart. On first federated start the node writes its identity to
`<data_dir>/federation.key` (PEM ed25519, mode 0600) and logs the address it came
up on:

```
federation: madnetwork node up — mesh address 2xx:… (key file ./data/federation.key)
```

That address is in Yggdrasil's `200::/7` range and is derived from the key, so it
is stable across restarts and is what a friend's node card will resolve to.

**No host port is opened.** The madnetwork protocol is served on port 1314 of the
node's *mesh* address, reachable only from inside the mesh. `listen` is a
different thing — it accepts incoming **underlay** peerings, which only backbone
nodes with a stable public address need. A node behind NAT dials out via `peers`
and needs no inbound anything.

**Back up `federation.key`.** The mesh address derives from it, so the key *is*
the node's identity: lose it and you come back as a stranger who has to be
re-friended by everyone. Back it up with your database, and never copy it to a
second node — two hosts holding one key are one address as far as the mesh is
concerned, so they collide rather than share the load.

### 3. Make a friend

Friendship is deliberate on both sides and takes an out-of-band exchange — there
is no discovery-by-search and no way to be added without acting.

1. Both admins open **`/admin/network`** (needs `federation.manage`) and copy or
   download their **node card** — a small JSON blob with the node's name and
   public key.
2. Send it to the other admin through any channel you already trust. The card is
   not secret; the point of the out-of-band step is that you know whose it is.
3. Each side pastes the other's card into the import form.
4. Both sides flip to **friend** once each has acted. If you both imported, the
   handshake completes on its own. If only one of you did, the other sees an
   **incoming request** on `/admin/network` and clicks **Accept** — importing a
   card and accepting a request are the same decision arriving from opposite
   directions, and a friendship is never made by one side alone.

Peers you no longer want go the other way on the same page: **Remove** forgets
the node, **Block** additionally cuts its whole branch out of your community and
publishes the block with its reason. Both take effect on the next request.

Catalogs then sync on a 15-minute cadence (with a nudge on first friendship), so
give it a few minutes before expecting a friend's library to appear.

Beyond your direct friends, the node also pulls a few **members'** catalogs per
cycle, rotating outward through the community — that is how a network you never
individually friended becomes browsable. `discovery_budget = -1` turns that off
and keeps the node looking at its own friends only, while still *serving* its
whole community.

### 4. Let people use it

Browsing the madnetwork is gated by **`madnetwork.access`**, which admins hold by
default. For everyone else it comes from the built-in **`madnetwork`** role,
which carries nothing else and so stacks onto whatever role a user already has —
grant it on `/admin/users` and a `/madnetwork` link appears in that user's
header. Fetching a remote track into the local library additionally needs
`file.upload`, and the result lands in the review queue like any other upload
rather than publishing silently.

### 5. Tune the policy

Runtime toggles live on **`/admin/settings`** (no restart):

| Setting | Default | What it decides |
|---|---|---|
| `default_share_depth` | Madnetwork | The scope a recording inherits when not pinned. **Direct friends** narrows the whole node. |
| `seed_enabled` | on | Whether this node serves blobs at all. Off = consume without serving. |
| `seed_cache` | on | Whether blobs *downloaded* from others are re-served and advertised. Off if you would rather not re-serve content you did not publish. |
| `serve_guests` | **off** | Whether nodes outside your community get guest-playable content. For deliberately public archive nodes. |
| `hide_unavailable` | on | Whether tracks held only by nodes that look offline are hidden from the browse. |
| `autoapprove_downloads` | off | Whether fetched tracks land approved instead of staged for review. |

Static knobs in `[federation]` (restart required) cover the resource side:
`seed_rate_kib` caps outbound seeding for everyone, while `member_rate_kib` /
`per_member_rate_kib` / `member_max_transfers` / `per_member_max_transfers` bound
what nodes you have *no direct relationship with* may cost you, per requester and
across all of them together. All default to unlimited — a handful of friends
needs none of it. **Direct friends bypass all four**, so the nodes you chose
never queue behind the ones the graph let in.

**Disk.** Blobs fetched from the network are cached under
`<data_dir>/cache/madnetwork/` and **there is no eviction yet** — the directory
grows with everything you stream or materialize. Watch it, and clear it by hand
if it outgrows the disk; cached blobs are rebuildable from the network and
nothing local references them. `discovery_cap` bounds the *catalog* side (foreign
catalogs kept, coldest evicted).

### Turning it off

Set `enabled = false` and restart. Friendships, cached catalogs and the identity
key survive, so re-enabling resumes where you left off. To leave for good, also
remove `federation.key` and the `cache/madnetwork` directory — and tell your
friends, since to them a node that simply stopped answering is indistinguishable
from one that is briefly down.

Full design, threat model and the reasoning behind every default:
[`docs/architecture/federation.md`](docs/architecture/federation.md).

## Endpoints (overview)

| Path | Group | Notes |
|---|---|---|
| `GET /healthz` | api | Health check. |
| `GET /api/*` | api | Library browse (artists/albums/tracks/search), auth, UI config. |
| `POST /files/upload`, `GET /files/*` | api | Upload (gated `file.upload`) and stream/download. |
| `GET /images/*` | api | Cover images. |
| `GET /api/madnetwork/*` | api | Federated browse, streaming relay and fetch-to-library (gated `madnetwork.access`; present only when federation runs). |
| `GET /`, `/static/*` | webui | Bundled web UI. |
| `GET /madnetwork`, `/madnetwork/nodes`, `/madnetwork/node/{key}` | webui | The federated browse, the directory of libraries behind it, and one node's own page (addressed by its public key). |
| `GET /admin`, `/api/admin/*` | admin | Admin page + destructive/management ops (incl. `/admin/network`, the peer and map surface, and `/admin/upgrades`, the better-renditions findings). |
| `GET /source` | api | AGPL §13: `tar.gz` of the git-tracked source. |
| `GET /license` | api | The project license. |

## License

Madshare is licensed under the **GNU AGPL-3.0** (see [`LICENSE.md`](LICENSE.md)).
For §13 network-use compliance the running server publishes its own source at
`GET /source` and the license text at `GET /license`. `make build` embeds both
into the binary (the source archive via `-tags embedsource`), so an installed
binary serves its Corresponding Source with no working tree present.
