# Embedding madshare — the `app` facade

> **Status: the embedder surface for madplayer level 2a** (2026-08-08).
> Package `daemonlord.ygg/madshare/app`. The client that motivated it is
> `docs/ui/madplayer.md`; this doc is madshare's side of that boundary and holds
> for any embedder, not only that one.

## The problem it solves

`madshare.go` was ~700 lines in `package main`: a dozen reconciliation passes, the
entity and cover backfills, the first-admin bootstrap, four worker pools, the
prune manager, the data-source manager, the `api.Deps` assembly, the mesh, the
madnetwork node, the traffic flusher, then the listeners. **None of it was
importable.** A program wanting the same backend in-process had exactly one
option: re-compose that startup itself, and drift from `main` on every change to
it.

So the facade exists for a reason that is not "tidiness":

> **One code path.** `madshare.go` is a shim over the same `app.Start` an embedder
> calls. A facade *beside* `main` would be a second startup to keep in agreement
> forever, which is the failure it was built to prevent.

## The surface

```go
inst, err := app.Start(ctx, cfg, app.WithLogger(lg))
if err != nil { … }
defer inst.Stop(context.Background())

// Server only: bind the [[listen]] and [[listen_mesh]] entries.
if err := inst.Serve(); err != nil { … }

// Embedder: the library, in-process, no listener and no port.
arts, err := inst.Library().Artists(ctx)
```

| Call | What it does |
|---|---|
| `app.Start(ctx, cfg, opts…)` | Everything that makes a madshare **node**: opens the DB, runs every reconciliation/backfill pass in its existing order, bootstraps the admin, starts the worker pools and the reaper, builds `api.Deps`, brings up the mesh and the madnetwork node when configured. |
| `inst.Serve()` | Everything that makes it **reachable**: one `http.Server` per `[[listen]]`, plus the `[[listen_mesh]]` blocks over the netstack. |
| `inst.Library()` | The browse/playback method set (below). |
| `inst.Network()` | The mesh method set, and whether there is one (below). |
| `inst.Sources()` | The `sources.Manager`, for folders-as-data-sources. |
| `inst.Stop(ctx)` | Graceful shutdown: workers, listeners, traffic flush, node/mesh, in-flight prune. |

Options carry what belongs to the *program* rather than the node:

| Option | Default | Why it is an option |
|---|---|---|
| `WithLogger(*log.Logger)` | `log.Default()` | A GUI client has no terminal; startup chatter has to be routable into its own log view. |
| `WithUIConfig(*config.UIConfig)` | `config.DefaultUIConfig()` | `webui.toml` is a file `main` chose to read; an embedder without a web UI has none. |
| `WithLicenseText([]byte)` | none → `/license` 404s | `//go:embed LICENSE.md` is `main`'s embed. |
| `WithSourceArchive([]byte)` | none | The `-tags embedsource` AGPL archive, likewise `main`'s. |
| `WithSourceRoot(string)` | none → `/source` 404s | Derived from the CWD, which is a property of how the program was launched. |

### `Start` / `Serve` is the split that matters

The split is **what a node is** versus **who can reach it**, and both halves are
ordinary parts of the facade: an embedder that wants to be reachable calls
`Serve`, and gets the whole HTTP surface — middleware, `auth.Identify`, the
permission gates — exactly as the server does. Nothing here treats serving as
second-class.

What the split buys is that reachability is a **separate, explicit choice**. A
program that only calls `Start` has a complete library with no port open, and it
never had to opt out of anything to get there.

> **madplayer is that case** — it never calls `Serve` (owner decision,
> 2026-08-08). That is a decision about *that client*, not a rule about
> embedding: it holds one person's own music on their own machine, so a listener
> would add an attack surface in exchange for nothing it needs. A different
> embedder — a headless box, a household server with a native UI on top — may
> serve, and the rest of this doc says where that changes things.

Two consequences of being a library rather than a `main`:

- **`log.Fatalf` becomes a returned error.** Every fatal in the old startup is now
  `fmt.Errorf`; the informational `log.Printf`s stay, on the injected logger.
- **A failed `Start` tears itself down.** It cannot leave a DB handle open or a
  pool goroutine running behind an error return, so the error paths after the
  first successful step unwind through the same `Stop` machinery.

## The library surface returns `database` row types

Decided 2026-08-08. `Library()` is a **small method set over the existing row
types**, not a set of facade-owned DTOs:

```go
type Library interface {
    Artists(ctx context.Context) ([]*database.ArtistEntry, error)
    AlbumsByArtist(ctx context.Context, artistID int64) ([]*database.AlbumEntry, error)
    TracksByAlbum(ctx context.Context, albumID int64) ([]*database.TrackEntry, error)
    Search(ctx context.Context, q string) (*database.SearchResults, error)
    Renditions(ctx context.Context, tagsetID int64) ([]database.DuplicateRendition, error)
    BlobPath(objectKey string) (path string, ok bool)
}
```

Why not DTOs: `ArtistEntry`, `AlbumEntry` and `TrackEntry` **are** the HTTP
contract already — the handlers marshal them straight out — so a parallel set of
structs plus a mapper would duplicate the one thing `docs/ui/madplayer.md` forbids
duplicating, and would have to be kept in agreement with the row types forever.

The stability promise is therefore on the **method set**, deliberately: `database`
has 300+ methods that change freely, and an embedder reaching into them directly
is what makes every internal refactor a client break. That promise is the whole
point of the facade — if an embedder needs something new, it is added here, named
and documented, rather than borrowed from `database`.

**`BlobPath` is the one method with no HTTP equivalent.** Over the wire a track
row's `url` is `/files/<hash>/<name>`; in-process the caller needs a *path* to
hand a decoder. It probes the same `storages.Registry` in the same precedence
`api.serveBlobs` does, so a linked import resolves through `links/` exactly as it
would over HTTP. **Which file to play is still the server's decision** — the row's
`ObjectKey` is already the recording's ladder-best surviving rendition, and an
embedder that re-derives that has re-implemented the ladder
(`docs/ui/madplayer.md` §"What the server already computes").

## The mesh surface exists for one participant (built 2026-08-09)

`Network()` is `Library()`'s counterpart for the madnetwork, and the same
bargain: a small named method set promised to stay, rather than a licence to
reach into `federation.Node`.

It differs from `Library()` in one way that is worth the second return value.
Every instance has a library; a mesh is something a configuration can simply not
include, so `Network() (Network, bool)` reports it **up front** — a program that
cannot use the mesh should find that out once at startup rather than from a call
that fails.

What is on it is not a general mesh API, and reading it as one will make it look
arbitrary. It is the **listener node's** surface
(`docs/architecture/federation.md` §"The household"): a server needs none of
these, because it discovers its own holders, is placed by other people's graph
walks, and has nothing to sign in to. Each method substitutes for one of the
things a device cannot have.

| Method | Stands in for |
|---|---|
| `Key()` / `Address()` | the identity a device names when asking for a token, and the address that token binds |
| `SetToken(tok)` | the standing it cannot earn — the vouch its home server signed |
| `AddHome` / `RemoveHome` / `Homes` | the community it is not in: whose word it will take about who a stranger is |
| `Fetch(ctx, hash, size, holders)` | the holder discovery its empty catalog tables can never do |
| `Holdings()` | the advertisement nobody would otherwise make on its behalf |

Three shapes worth knowing before calling it:

- **`SetToken` is safe at any time, from any goroutine**, and that is the point
  rather than a courtesy: a token is renewed at its half-life, which is to say
  while transfers are in flight. It is an `atomic.Pointer` read on the outbound
  path, so a server — which never stores anything into it — pays nothing
  measurable for the facility.
- **`Fetch` takes public keys**, because that is what a home server's browse rows
  and its holders endpoint carry. A key that is not 64 hex characters is dropped
  rather than refused: a holder list arrives from another machine, and one bad
  entry should cost that holder and not the download. With nothing left it
  answers `federation.ErrNoHolder`, which is also what "we asked everyone and
  nobody had it" answers — the same thing from the caller's side.
- **`Holdings()` reads the cache directory, not an index.** A device advertising
  from an index could advertise a blob it has already swept, and the swarm reads
  a holder that refuses as a holder that is broken.

`federation.Transfer` and `federation.HomeNode` come back as they are, on the
same reasoning as the `database` row types above: they are the shapes the mesh
already speaks, and a twin here would be one more thing to keep in agreement.

### Direct calls bypass the permission layer

Access control is not in the DB. The browse queries come in **full and guest
variants**, and it is the HTTP handler (`guestListing`, reading the identity off
the request context) that picks between them. `Library()` calls the **full**
variants, unconditionally.

For a single-user player on its owner's machine that is correct — you own the
library — but it is a decision, not an oversight.

The good news is that it does not have to be re-made by hand: **an embedder that
serves gets the gate back for free**, because it lives in the handler stack
`Serve` builds, not in something the embedder must remember. Only the *direct*
caller chooses for itself. So the rule is about which door the request came in
through, not about who embedded what: through `Serve`, the identity decides;
through `Library()`, the embedder has already decided.

## A config built in code

An embedder has no TOML file, and before this it could not build a `Config`
either: `defaults()` was unexported and every resolve step was an unexported
method, so the derived paths (`database.path`, `files_dir`, `variants_dir`, the
node key) could not be filled from outside the package.

```go
cfg := config.Default()
cfg.DataDir = filepath.Join(xdgData, "madplayer")
cfg.Listen = nil                    // serve nothing
cfg.Sources.AllowAny = true
cfg.Auth.InitialAdminUser = "owner"
cfg.Auth.InitialAdminPassword = secret   // random, discarded — see below
cfg, err := cfg.Prepare()           // resolve derived paths, then validate
```

`Load` is now `Default()` → decode the file → `Prepare()`, so a programmatic
config goes through **the same** resolution and validation the file does. One
requirement moved in the process:

> **"at least one `[[listen]]`" is a requirement of the config *file*, not of a
> madshare node.** It is enforced in `Load`, not in `validate`. A file describes a
> server, and a server that binds nothing is an operator mistake; a *program*
> embedding madshare may legitimately serve nothing at all.

## Silent provisioning

madshare refuses to start on an empty `users` table, so an embedder with no
operator to prompt must create that identity itself, at first run, without asking
anyone. The user row is bookkeeping: an actor id for data sources and uploads, and
the owner of the playlists.

**What happens to the generated password is the embedder's decision, and it
follows from whether that embedder serves.**

madplayer's answer (owner decision, 2026-08-08): **generated and thrown away** —
32 random bytes handed to `auth.Bootstrap`, never stored, never logged, never
shown. Since it never serves, there is nothing to log in to, so a stored
credential could only ever be a liability; discarding it means **nobody can
authenticate as the owner, including whoever reaches that machine later**. A
credential that exists nowhere cannot be replayed.

An embedder that *does* serve wants the opposite and should say so: an identity
somebody chose, through the same `[auth]` bootstrap the server uses
(`initial_admin_user` + a password from `MADSHARE_INITIAL_ADMIN_PASSWORD`), or its
own first-run prompt. Silent provisioning is a way to satisfy the empty-table
refusal without a human in the loop — not a recommendation to hide credentials
from a deployment that needs them.

## A boundary a listener-less deployment may drop: `[sources].allow_any`

`[sources].symlink_roots` gates where an in-place import may point. It exists
because the surface that adds a source is **HTTP-reachable**: without the
allow-list, an admin session (or anything that steals one) can symlink
`/etc/shadow` into the library and read it back over `/files/`. That is why
`docs/architecture/configuration.md` refuses it an override layer — a boundary a
web admin can move is not a boundary.

A deployment that serves nothing has no such surface: the person who clicks *Add
folder* is at the keyboard, on their own machine, with no `Serve()` running. For
that case the allow-list has nothing left to protect, and the honest way to say so
is a key that says it:

```toml
[sources]
allow_any = true   # no allow-list: this deployment has no HTTP surface
```

`allow_any` makes `SymlinkSourcesEnabled()` true with no roots and turns the
`withinAllowlist` check off. Two guards keep it from becoming a server footgun:

- Setting it **together with a listener** is a startup **warning** — the boundary
  it removes is exactly the one that listener would need.
- Setting it together with `symlink_roots` warns too: the roots are then dead
  config, and silently-ignored security config is worse than none.

It stays TOML-only, like the roots it replaces: deploy-time, file-owned, not
UI-editable. One deliberate asymmetry: `/admin/sources`' Add form is a dropdown of
the configured roots, so with `allow_any` it has none to offer and the **web
surface stays constrained even when the manager is not**. That is the right
default for a key whose whole premise is that no web surface exists.

## What stays in `main`

The `LICENSE.md` embed, the `-tags embedsource` source archive, flag parsing, the
CWD-derived source root, and the signal handler. Each is a property of *the
program* — how it was built, how it was launched, how it is asked to stop — not of
the node it starts. They arrive through options, which is what keeps `Start`'s
signature the same for a GUI with no flags and no CWD worth reading.

## Build tags for an embedder

Both existing tags keep working, and the facade compiles under either:

- **`-tags nowebui`** is recommended for a client that ships its own UI —
  otherwise the binary carries a second, unreachable interface. `Serve()` still
  refuses a listener that asks to `serve = ["webui"]`, as `main` did.
- **`-tags nofederation` is *not* recommended**, even for an embedder that does
  not federate yet: leaving it off means reaching the mesh later
  (madplayer level 2b) is wiring rather than a rebuild, at the cost of the
  yggdrasil and gVisor dependencies in the embedder's own `go.sum`.

## What to test

The extraction is meant to be **behaviour-neutral**, so its tests are mostly
about that:

- `app.Start` on a fresh temp data dir with no listeners: every pass runs, the
  admin is provisioned, `Library()` answers, `Stop` releases everything (no
  leaked goroutine, DB closed).
- `Start` twice on the same data dir: the second run provisions nothing and warns
  about nothing — the passes are idempotent, which they already had to be.
- A failing pass returns an error *and* leaves nothing running.
- `config.Default().Prepare()` is valid without listeners; `config.Load` still
  refuses a file that declares none.
- The old `madshare.go` behaviours that were `log.Fatalf` are still refusals:
  `webui` asked of a `nowebui` build, a mesh listener with no mesh, federation
  without `fpcalc`.
