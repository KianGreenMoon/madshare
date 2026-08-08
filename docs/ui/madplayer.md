# Madplayer — the native client

> **Status: level 0 built — an offline player that needs no server.** This doc
> opened for a year with "waits until federation is designed and landed". That
> wait is over — F0–F8 all shipped, including the **F7 capability tokens** this
> client was named as the reason for (`docs/architecture/federation.md`
> §Principals & access). The UI toolkit is settled on **Gio** (see *The UI
> toolkit*), and the offline player it was chosen for now scans folders, indexes
> them and plays. Next is embedding the backend (§"Three levels").
>
> Work happens on the temporary `madplayer` branch, in a directory of its own with
> its own Go module, and is kept strictly separate from server commits so a
> server-side fix can reach the main branch without dragging client code with it.

## What this is

A **separate, native GUI client** for desktop and mobile — *not* a reuse of the
web UI. By default it is "just another music player" running against its own
**embedded** madshare backend, with the federation features layered on so it can
also act as a peer to other madshare nodes.

The deliberate consequence is **two UIs that solve the same task**: the existing
HTML/CSS/JS web UI for the browser, and this native client for desktop/mobile.
That cost is accepted on purpose — see *Why two UIs*.

## Three levels, and the first one has no server in it

The word "madplayer" covers programs of quite different ambition, and conflating
them is the fastest way to make this look impossible:

0. **An offline player.** It scans folders on this device, indexes what it finds
   and plays it. **No server, no account, nothing to sign in to.** This is the
   baseline product and the default posture — "just another music player" — not
   a stepping stone that gets replaced.
1. **An API client.** It signs in to a server with an account, browses that
   server's library, and plays. It needs **no mesh, no node key and no
   capability token** — including for madnetwork content, because
   `GET /api/madnetwork/stream/{hash}` is a cache-through relay the server
   already runs on the client's behalf. Every endpoint this needs exists today
   over plain HTTP, and the Capacitor Android app is exactly this program in a
   WebView.
2. **A node.** It embeds the backend *and* a yggdrasil node in-process, holds its
   own library, fetches chunks from many holders at once and seeds back what it
   fetched. This is where the netstack, the capability token, the mobile process
   lifecycle and the audio-decode work all live.

**Level 0 first** (decided 2026-08-08, reversing an earlier "build the API client
first"). It is the only level that is a product with nothing else running, it
settles every UI question the other two also have to answer, and it is what makes
the client honest about its own framing: a player that cannot play your own files
without an account is not "just another music player".

### The embedded backend does the library work

This decides what belongs in the client at all: **the embedded backend does the
library work, and the client does not reimplement it.** Scanning folders in
place, resolving artists and albums, guessing a mis-decoded charset — madshare
already does all of it, and a second implementation in Go here would be a second
thing to keep in agreement forever. Anything the client appears to need that
madshare already decides is a reason to embed sooner, not to port.

Locally it calls those methods **directly**, in-process, rather than over a
loopback HTTP hop — see §"Local is a function call". The HTTP API is for reaching
a *different* machine.

The level-0 build that exists today has its own scanner and index precisely
because the backend is not embedded yet. Those are **provisional**, and they go
when it is. What survives is the UI, the queue, and the playback layer.

**The gap embedding needs:** `madshare.go` is ~700 lines in `package main`, so
none of the startup is importable — the reconciliation passes, the worker pools,
the listeners. Embedding requires a package that owns that startup and exposes
it, which is also the **facade** the client should call rather than reaching into
`database` and friends. It is a madshare change, and it is the right one: the
alternative is the client re-composing the startup itself and drifting from
`main` on every change.

## The decisions

### Two renderers over one shared contract

The madshare **HTTP API is the single shared contract.** Features land in the
backend once; each UI just paints them — exactly the relationship the web UI and
the (already-built) Capacitor app have today. The two clients are *renderers*, not
two products.

- **Web → keep HTML/CSS/JS.** JS is the right tool *for the browser*: the DOM,
  accessibility (screen readers), and web-native behaviours (find-in-page, text
  selection, deep links, native scrolling) are real advantages, and the existing
  web UI already delivers them. A canvas-rendered Go UI (Gio/Fyne compile to a
  WebAssembly `<canvas>`) was considered for web reuse and **rejected** — the
  canvas loses all of the above. So web stays JS.
- **Native → a separate pure-Go GUI**, in **Gio** (decided 2026-08-08 — see *The
  UI toolkit*). Not Flutter/Godot: those still leave a second language + an FFI
  seam, and don't reuse on the web the way you'd want anyway.

### The native client embeds the backend as a library

The native app is a **single Go binary**: the madshare server **and** a Yggdrasil
node compiled in as libraries and run **in-process**, with the Go UI on top.

- **In-process, not a sidecar.** Spawning the compiled server as a child process
  is the simplest model on desktop, but **iOS forbids spawning processes** and
  Android makes it awkward — so mobile forces the in-process library path, and we
  plan for that everywhere.
- **`modernc.org/sqlite` (pure Go, no CGo)** is what makes the mobile embed
  (`gomobile` / `c-archive`) feasible at all. This was already the project's DB
  driver — the choice pays off here.
- **No TUN, no root, no VPN entitlement.** The F0 spike confirmed a full mesh
  node over yggstack's gVisor netstack, and the server has shipped on it since;
  `[[listen_mesh]]` (below) is the same machinery serving HTTP on the node's own
  mesh address. Nothing here needs `VpnService` / `NetworkExtension`.
- **Local-first.** With the backend embedded, the app is a standalone music
  player against its *own* library — **no server required**, and no loopback hop
  either: it calls the backend's methods in-process (§"Local is a function
  call").

### Federation: madplayer is a listener node

The point of embedding the backend is to make every install a **node**. What
kind of node is settled: a **listener node**, defined in
`docs/architecture/federation.md` §Principals & access (decided 2026-07-26). The
short form, because it shapes nearly every screen in this client:

- **It signs in to a home server with user credentials**, not by friending it.
  The account's own rights decide what it may see. Being a node and being a
  principal are separate things here — the key is for the mesh, the account is
  for authorisation.
- **It publishes nothing.** The device's library is never catalogued or
  advertised: it is unmoderated personal content and the network cannot vouch
  for it. The single route from the device's library into the network is an
  ordinary **upload** to the home server, through the review bucket, which needs
  `file.upload` — and what the network then sees belongs to the *server*.
- **It swarms fully all the same** — fetching chunks from many holders and
  seeding back what it fetched, discovered like any other node. Serving a hash
  claims possession of bytes, never an identity, so seeding asserts nothing
  anyone must trust. That is why one-way publication and two-way swarming do not
  contradict each other.
- **It needs a capability token** to do that: to its home server's friends the
  device is a stranger, placeable by no graph walk, because it publishes no
  friend list and appears in nobody else's. That token is now built — the flow is
  §"The capability token, concretely".

### Playlists follow the person, not the device

A listener node has three pools of music at once — what is on the device, what
its home server holds, and what the network holds — and a playlist should
survive moving between them. Sync playlists and favourites with the home server.

The hard half is already solved server-side and should be reused rather than
reinvented: a playlist item is **either** a local tagset **or** a remote content
hash plus captured display text (migration 029), and listing rows carry an
availability flag so the UI can dim what nothing can currently serve. The
madplayer case is the mirror image of that — an item pointing into the *device's*
local-only library, which the home server cannot resolve — and it takes the same
shape: keep the captured text, mark it unresolvable *here*, and say so plainly.
"This one is only on your device" is a perfectly good answer.

The rule that matters: **an item you cannot resolve is displayed, never
dropped.** A sync that silently deletes whatever the other side can't see would
quietly eat the user's playlists, and it would do it worst to the people with the
largest local libraries. Captured text exists so an unresolvable row can still be
shown, searched and re-pointed later if the same audio turns up — which is what
`RepointRemotePlaylistItems` already does for remote rows that land locally.

Open: sync direction and conflict rules (two-way, last-writer-wins, or per-item
merge), and whether favourites follow the same path or ride along with playlists.

### Local is a function call; HTTP is for remote only

This doc argued for a year that the embedded backend should speak HTTP to
itself on loopback, so that "my library" and "a friend's node" differed only by
base URL and the client needed one networking layer. **That is not the design**
(decided 2026-08-08). It is a real idea with a real cost — serialising a library
listing, running a listener, minting a local account and a token, and holding a
port open — all to talk to code in the same process.

Instead:

| What | How |
|---|---|
| This device's library — browse, search, play, scan | **Direct Go calls** into the embedded madshare. No listener, no port, no local account. |
| The merged network catalog | Also direct calls: the embedded node does its own federation and already holds the cached catalogs. |
| Node-to-node federation | madshare's own business, already built (`docs/architecture/federation.md`). The client does not implement it. |
| **Signing in to somebody else's server** | The **HTTP API**, over the network, exactly as `internal/madshare` already does. |

So the HTTP client is not the client's data layer. It exists for the one thing
that genuinely crosses a machine boundary: **reaching a server that is not this
one**, and authenticating to it.

Two consequences, both of which are the price of this and neither of which should
be discovered later:

- **madshare's Go API becomes a contract for madplayer**, in a way its HTTP API
  already was and its packages never were. `database` alone exposes 300+ methods
  that change freely today. The client must therefore go through a **deliberate
  facade** — one package madshare exports *for embedders*, whose surface is
  small enough to keep stable — rather than reaching into `database`, `api` or
  `storages` directly. Without that, every internal refactor is a client break.
- **Direct calls bypass the permission layer.** Access control is not in the DB:
  the queries come in full and guest variants, and it is the HTTP handler
  (`guestListing`, reading the identity from the request context) that picks
  between them. A client calling the store directly is choosing for itself. For
  a single-user player on your own machine that is correct — you own the library
  — but it is a decision, not an accident, and it means the day madplayer serves
  anyone but its owner, the gate has to be reinstated deliberately.

### Versioned dependency, not a vendored copy

In its own repo, madplayer will `require daemonlord.ygg/madshare` at a **released
tag** and upgrade on purpose. That is what the release tagging is for: the client
pins a known-good server, and a server change lands in the client when someone
chooses it rather than the moment it is written. While the two still share this
repo, a `replace` directive stands in for that.

## What the server already computes — do not re-implement it

This is the section to read before writing any list code. Each of these rules is
decided **server-side**, in SQL or in a handler, and a client that renders what
the endpoint returns gets it for free. Re-deriving any of them client-side
produces a UI that quietly disagrees with the web UI about what the library
contains.

| Rule | Where it is decided | What the client does |
|---|---|---|
| **Which names are artists** (album artists, carrying their guest appearances; performers are search hits, not rows) | `/api/artists`, `/api/madnetwork/artists` | Render the rows. **Never group track rows into an artist list** — `docs/ui/artists-and-performers.md` §"The one way to get this wrong" |
| **Which file to play** for a track | `url` on every track row — the recording's ladder-best surviving rendition | Play `url`. Only the quality picker needs `/api/tagsets/{id}/renditions` |
| **Quality-ladder order** of renditions | `RankRenditions` (`docs/architecture/recordings.md`) | Show the list in the order given; index 0 is "Auto" |
| **Disc grouping** (untagged / 0 / N are distinct discs) | `disc_number` per row + the shared rule in `docs/architecture/disc-numbering.md` | Group by the documented rule; headers are visual only and must not shift track indices |
| **Sort order** incl. Unknown-artist / Other last | every browse query | Do not re-sort |
| **Availability** — hiding tracks no reachable node can serve | request-time filter on the madnetwork browse | Render what arrived. There is no client-side liveness logic, and no poll (`docs/ui/madnetwork-page.md` §Availability) |
| **Which version is the default pick** on a merged network row | `mergeVersions`, branch-weighted (one branch = one voice) | Act on `versions[0].renditions[0]` |
| **Re-pointing a remote playlist item** once its audio lands locally | `RepointRemotePlaylistItems`, server sweep | Nothing — the badge disappears on the next load |
| **Cover variants** (8 sizes per image) | `imageproc` pool | Request `?size=thumb\|small\|medium\|large` (`docs/api/cover-images.md`) |

Two things are **not** server-side today, and a second client is exactly the
pressure that should move them:

- **Queue index arithmetic** — shuffle permutation, insert-after-current, the
  original-order mirror (`webui/static/js/queue-ops.js`, pure and unit-tested).
  A native client needs a Go twin of this, or the semantics in
  `docs/ui/player-and-queue.md` re-implemented by hand. Porting the *rules* is
  fine; inventing different ones is not — two clients that disagree about what
  shuffle does share a queue in `localStorage`-shaped ways that will not survive
  the playlist sync above.
- **Like-key normalisation** (`ts:<tagset_id>` vs `mn:<hash>`, `favorites.js`).
  Small, but it is the identity two surfaces must agree on.

The standing rule from *Why two UIs* is the tiebreaker: **push logic into the
API, not the clients.** Anything a second client would have to duplicate is a
candidate to compute server-side and return.

## The API surface a player needs — from a REMOTE server

Everything below is plain HTTP, and it is the surface for talking to **a server
that is not this machine**. Against the embedded backend the client calls the
same capabilities as Go methods instead (§"Local is a function call"), so read
this table as "what signing in to somebody else's madshare requires", not as the
client's data layer.

It is still worth knowing in full for the local case, because the embedded
facade answers the same questions in the same shapes — the identity of a track is
still its `tagset_id`, the artist list is still album-artists-only — and because
the rules in §"What the server already computes" hold whichever way they are
called.

Gates are permissions (`docs/architecture/auth.md`); a request with no identity
is anonymous, and the endpoint decides.

| Purpose | Endpoints | Gate |
|---|---|---|
| **Sign in** | `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | — |
| **Credential for a non-browser client** | `GET/POST /api/auth/tokens`, `DELETE /api/auth/tokens/{id}`; then `Authorization: Bearer <token>` | signed in (`docs/api/tokens.md`) |
| **Browse the library** | `GET /api/artists[?limit=&cursor=]`, `GET /api/albums?artist_id=`, `GET /api/tracks?album_id=`, `GET /api/search?q=` | narrows without `content.access` — see below |
| **Play** | the `url` on a track row (`/files/<hash>/<name>`, native HTTP Range) | `content.access`, default-deny per blob |
| **Quality picker** | `GET /api/tagsets/{id}/renditions` | narrows |
| **Covers** | `GET /api/albums/{id}/image?size=`, `GET /api/artists/{id}/image?size=` | narrows |
| **Playlists & favourites** | `/api/playlists*`, `/api/favorites*`, `POST /api/favorites/remote/{hash}` | `content.access` (`docs/api/playlists.md`) |
| **Browse the network** | `/api/madnetwork/{summary,discover,lane,artists,albums,tracks,search,nodes/{key}}` | `madnetwork.access` |
| **Play network content** | `GET /api/madnetwork/stream/{hash}` — cache-through relay, seek-aware | `madnetwork.access` |
| **Materialize** (fetch into the server's library) | `POST /api/madnetwork/download`, `GET /api/madnetwork/transfers/{hash}` | `madnetwork.access` + `file.upload` |
| **Upload from the device** | `POST /files/upload`, then the staging flow under `/api/my/uploads*` | `file.upload` (`docs/api/upload.md`, `docs/architecture/moderation.md`) |
| **Vouch for this device on the mesh** | `POST /api/madnetwork/token` | `madnetwork.access` (level 2 only) |

Three shapes to know before writing the fetch layer:

- **The browse endpoints narrow, they do not refuse.** A caller without
  `content.access` — anonymous, or an account that lacks it — gets the **guest
  listing**: guest-playable content only, same response shape, no error. So an
  empty library is never evidence that sign-in failed; `GET /api/auth/me` is the
  question about identity. Playback is where the refusal actually happens
  (`/files/*` is default-deny, per blob).
- **`GET /api/artists` answers two ways.** Without `limit` it returns a bare
  array (everything); with `limit` it returns `{items, next_cursor}` and
  `next_cursor` is **opaque** — pass it back verbatim, never construct one. The
  network twin (`/api/madnetwork/artists`) is cursor-only.
- **A library track is addressed by `tagset_id`**, the *appearance*, not by file
  hash — that is what favourites, playlists and the renditions endpoint all key
  on. The `hash` on a row is the **origin blob**, for admin surfaces; a track
  whose origin blob is gone still plays, because `url` resolves to a surviving
  rendition. `docs/architecture/recording-tagsets.md` is the model.

## The capability token, concretely

Level 2 only. A listener node fetching bytes **directly** from its home server's
friends is a stranger to them: it publishes no friend list and appears in nobody
else's, so no graph walk can place it. The token is the one credential in
madnetwork, and it exists for exactly that shape.

```
POST /api/madnetwork/token          (to the HOME SERVER, ordinary authenticated call)
{ "node_key": "<the device's own 64-hex ed25519 public key>" }

→ 200 { "token": "<base64url JSON>", "issuer": "<home server key>",
        "bearer": "<device key>", "expires_at": …, "renew_after": … }
```

Then, on every mesh request the device makes to a *third* node:

```
Madnetwork-Token: <token>
```

What is worth knowing before building against it:

- **No card, no accept, no peer row.** This is a plain authenticated API call —
  there is no admin step anywhere in the flow, which is the whole point of the
  listener node being a third kind of participant. Issuing stores **nothing**: a
  token verifies from its own bytes.
- **A leaked token is worthless.** The verifier checks that the bearer key
  derives to the mesh address the request actually came from, so using one needs
  the bearer's private key.
- **Renew at `renew_after`, not at `expires_at`.** Lifetime is one hour, renewal
  is at the half-life, so a transient outage of the home server costs a retry
  rather than an interruption. There is a hard 2-hour ceiling on any token's
  claimed lifetime, validated without consulting a clock.
- **It grants membership, never friendship.** The device reaches what a member of
  the home server's community reaches — a recording scoped `DepthFriends` stays
  off a device this admin never chose. If the account lacks `content.access`, the
  token says `guest_only` and the restriction travels with it.
- **Revocation is the issuer's standing, not the clock.** Blocking a home server
  kills every token it ever issued, on the very next request. There is no
  revocation list, deliberately.
- **Bearers are not friends for quotas.** They draw on the member budget
  (`per_member_*` / `member_*`), which is what answers a home server enrolling a
  thousand devices.

Full design and the four verifier checks: `docs/architecture/federation.md`
§Principals & access, "The capability token".

## Before writing a client at all: `[[listen_mesh]]`

A person who wants their own library on their phone over the mesh does **not**
need this client. The server can serve its existing web UI and API on **its own
yggdrasil address** — no proxy, no certificate, no domain, no port forward, no
root, no TUN:

```toml
[[listen_mesh]]
enabled = true          # per-block, defaults FALSE — writing the block is not enough
port    = 80
serve   = ["api", "webui"]
```

This is worth stating in the client's own doc for two reasons. It is the honest
answer to "I just want to reach my music from outside", so a native client should
not be justified by that problem. And it is the shape the embedded case takes
anyway: same netstack, same route groups, a different address.

Two gotchas, both from `docs/plans/mesh-listener.md`: the mesh address is **not
reachable from the host itself** (there is no TUN), so keep a loopback
`[[listen]]` as well; and that address's audience is the **whole yggdrasil
network**, so auth is the only gate — `admin` on it warns at startup, and
`allow_from` CIDRs are near-useless against key-derived addresses.

## Why two UIs (and keeping the cost bounded)

Two UIs is real maintenance cost, accepted because each is best-in-class for its
target (JS for the web, native Go for desktop/mobile). The cost stays bounded by
discipline, not luck:

- **Push logic into the server, not the clients** — the rule and its current
  exceptions are §"What the server already computes". It is worth restating as
  *server* rather than *API* now that the native client reaches the same
  decisions by function call: the rule was never about HTTP, it was about who
  decides. Two clients that decide separately disagree, whichever way they ask.
- **Design docs are the shared behavioural spec.** `docs/ui/library-page.md`,
  `docs/ui/player-and-queue.md`, `docs/ui/artists-and-performers.md`,
  `docs/ui/madnetwork-page.md` define behaviour once; both UIs *follow* the spec
  rather than each inventing it. `docs/ui/README.md` says which docs are the
  contract and which are web-only implementation notes. Keep them authoritative:
  when a change makes one of the pinned tests in those docs fail, it is changing
  the contract, and the doc moves in the same commit.

## The native client's own burdens (not shared with web)

- **Audio playback/decoding.** No Flutter-style plugin ecosystem — this is the
  native client's job. Pure-Go decoders cover MP3/FLAC/Vorbis; **Opus/AAC/M4A**
  need cgo bindings or ffmpeg. Synergy: lean on the **embedded backend's** ffmpeg
  (the deferred recordings **P5** transcode work) to decode/transcode locally to a
  Go-playable format. Gapless / ReplayGain / crossfade are all manual.
- **Mobile background playback & process lifecycle.** The OS aggressively kills
  background work — the lesson already learned in the Capacitor app's
  `MediaPlaybackService` foreground service (`docs/architecture/android-app.md`).
  An embedded *server* backgrounding is harder still, and on iOS a persistent
  server effectively can't run backgrounded at all.
- **Intermittent mobile peers.** A phone serves over the mesh only while
  foregrounded, so federation must expect an **asymmetry**: a backbone of
  always-on desktop/server nodes plus flaky mobile peers. The availability model
  already assumes this and is tuned for it (two freshness windows, passive
  `last_seen`, fail-open) — but it is why a phone is a poor sole holder of
  anything, and why "favourite → replicate" keeps coming back as a later idea.
- **Offline is a first-class state, and one button already assumes it.**
  `/admin/cache`'s **Materialize** exists precisely for a madplayer with no
  connectivity adding a cached file to its own library: it makes no live claim
  and touches no network, reading the tags and the container out of the file the
  way an upload does (`docs/architecture/madnetwork-cache.md`). A client that
  routes that path through the network has broken the one case it was built for.

## Relationship to the Capacitor Android app

The Capacitor app (`docs/architecture/android-app.md`) is a thin WebView that
reuses the **web UI** same-origin and is already built and on-device-verified. It
is level 1 of §"Two levels of ambition", implemented the cheapest possible way.

This native Go client is a **more ambitious, different direction** for the mobile
slot: an embedded backend + a real federation peer, not a remote-URL shell. They
are alternatives — when this client is actually scheduled, decide whether the
Capacitor app remains the lightweight "connect to someone's server" option while
the native client is the "run your own node" option, or whether one supersedes the
other.

One inherited gotcha worth carrying over: the player JS ships **inside the server
binary** (`embed.FS`), so a Capacitor-style client picks up player changes only
when the server it points at is redeployed. A native client compiled from this
repo has the opposite property — it ships its own UI and can outrun the server it
talks to, which is why the API contract, not the shared code, is what keeps them
in agreement.

## The UI toolkit

**Gio** (decided 2026-08-08). The shortlist was Gio vs. Fyne, on the one tradeoff
that mattered — single-language Go end to end, no FFI seam, one build for desktop
and mobile, at the cost of weaker UI polish, a smaller widget ecosystem, and
owning audio playback yourself. A two-language stack with a Go core was rejected
for the seam and the lack of web reuse.

It was settled the way this section always said to settle it: build the same real
screen — a 5000-row track list and a player bar — in **both**, over identical
rows so nothing but the toolkit could differ, and look at it. Gio won on the look,
which was the stated criterion. The supporting numbers, on Linux/aarch64/Wayland:

| | Gio v0.10.1 | Fyne v2.8.0 |
|---|---|---|
| Binary, unstripped, all backends | **14.2 MiB** | 28.4 MiB |
| Native Wayland | yes | yes |
| Extra system dev packages | 2, both optional (each backend has a fallback) | 1, **mandatory** — will not link without it |

Two things were kept out of the comparison because they could not discriminate
between the candidates: **audio decoding** (the same oto/beep burden either way —
it remains real work, see *The native client's own burdens*) and **icons** (Fyne
ships a themed set, Gio does not; that is now a cost Gio has to pay).

**The phone half of "compile it to the actual target" is still owed.** It could
not be done on the development host: `gogio` resolves the NDK through an
`archNDK()` that panics on `linux/arm64`, the NDK ships no linux-aarch64 host
toolchain, and the 16 KB-page emulation wall from
`docs/architecture/android-app.md` sits behind both — the same split that app
already lives with, so the APK is built on x86_64 and installed over `adb`. Until
someone has actually looked at it on a phone, "one build for desktop and mobile"
is a claim this project has verified only on the desktop side.

## First steps when it's time

1. ~~**UI-toolkit prototype + decision.**~~ Done — Gio, see *The UI toolkit*. The
   one thing it still owes is a look at the phone build.
2. ~~**Level 0: the offline player.**~~ Done — folder scan, index, browse, search,
   queue, playback. `docs/ui/library-page.md`, `docs/ui/player-and-queue.md` and
   `docs/ui/artists-and-performers.md` were the spec; the queue and identity
   rules are ports of `queue-ops.js` and `database/entities.go`, pinned by tests
   against those docs' own worked examples. Its scanner and index are
   provisional — see §"The embedded backend is what level 1 talks to".
3. **Level 2a: embed the core** — and note it comes BEFORE level 1 now. madshare
   in-process, called **directly** as Go methods through the facade, with the
   provisional local scanner and index deleted. Needs the startup extraction
   named above. Doing this before level 1 means the client's own library work is
   gone before a second data path is added, rather than being reconciled with one
   afterwards.
4. **Level 1: remote servers.** Sign-in and browsing somebody else's library over
   HTTP — the one thing the API client is for. `internal/madshare` already exists
   for this.
5. **Level 2b: the mesh.** Node key, capability token from the home server,
   swarm fetch and seed-back. The trust model is `federation.md`; the token flow
   is §"The capability token, concretely".
6. **Playlist sync**, once the levels above exist and the unresolvable-item
   question has a real UI to be answered in.
