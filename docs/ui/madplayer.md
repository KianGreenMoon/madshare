# Madplayer — the native client

> **Status: levels 2a and 1 are built.** The backend is embedded — madshare runs
> in the client's own process and owns the library, folders are its in-place data
> sources, and browse and search are its queries (the facade is
> `docs/architecture/embedding.md`). The client also **signs in to remote
> madshares** over HTTP, and this device's library and each server's are browsed
> as **one merged list** (§"Two libraries, one list"). Still open: the
> human-readable materialize target (§"Where the bytes live"), which nothing
> feeds until the mesh arrives at 2b.
>
> Behind it: F0–F8 all shipped, including the **F7 capability tokens** this client
> was named as the reason for (`docs/architecture/federation.md` §Principals &
> access), and the UI toolkit is settled on **Gio** (see *The UI toolkit*). Next
> is **level 2b, the mesh — designed 2026-08-09, not yet built**: the access half
> is `federation.md` §"The household", and what the client does with it is
> §"Level 2b, concretely" below.
>
> **madplayer now lives in its own repository** (split out at madshare v0.9.0),
> beside this one. It requires madshare as an ordinary Go module pinned to a
> released tag — which is what the tagging buys: the client pins a known-good
> server, and a server change reaches it when somebody chooses it rather than the
> moment it is written.
>
> **This document stays here**, because it is a cross-client contract rather than
> that program's private business: it is where the rules both UIs follow are
> written down, and a copy in the client's repo would be a second one to keep in
> agreement. A change to it is a madshare commit. Notes that concern only the
> client's own implementation live in that repository's `README.md`.

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

**Same engine, inverted primary path.** That one sentence is the whole concept.
A madshare server ingests by **upload**, into storage it manages (`files_dir`,
object storage later), and what lands there is content it owns and has moderated.
madplayer ingests by **scanning folders in place** — files that already exist,
where they already are, never written to — and reuses everything downstream. It
is not a smaller madshare or a fork of one; it is the same library engine
entered from the other end.

The parts already fit, which is the argument for reusing rather than rewriting:

| madplayer needs | madshare already has |
|---|---|
| add a music folder | `sources.Manager.Add` |
| scan it without touching anything | `sources/scan.go` — one symlink per hash, `LinkTarget` at the original |
| files playable at once, no review queue | scans land `ReviewApproved` **by design** — "they come from an admin behind the allow-list, not the public upload staging flow" |
| artists / albums / tracks / search | `ListArtists`, `ListAlbumsByArtistID`, `ListTracksByAlbumID`, `Search` |
| tags, entity resolution, charset repair | `media.ExtractTags`, the resolver, `SuggestCharset` + Fix charset |
| the whole madnetwork | federation, swarm, cached catalogs — the actual payoff |

One difference a player must handle that a server treats as an incident: **a
dangling link is normal here.** An unplugged drive, an ejected SD card, a folder
the user moved. `serveBlobs` already falls through on a broken link, but the UI
should say "that drive isn't connected" rather than reporting the track as
missing — on a server this means something has gone wrong, on a player it means
somebody unplugged something.

Locally it calls those methods **directly**, in-process, rather than over a
loopback HTTP hop — see §"Local is a function call". The HTTP API is for reaching
a *different* machine.

The level-0 build had its own scanner and index precisely because the backend was
not embedded yet. Those are **gone** as of level 2a. What survived is the UI, the
queue, the playback layer, and one display rule the server has no opinion about:
turning contiguous same-disc tracks into "Disc N" separators.

**The gap embedding needs:** `madshare.go` was ~700 lines in `package main`, so
none of the startup was importable — the reconciliation passes, the worker pools,
the listeners. Embedding requires a package that owns that startup and exposes
it, which is also the **facade** the client should call rather than reaching into
`database` and friends. It is a madshare change, and it is the right one: the
alternative is the client re-composing the startup itself and drifting from
`main` on every change. That package is
**`daemonlord.ygg/madshare/app`** — design, surface and the four decisions behind
it in `docs/architecture/embedding.md`; `madshare.go` is now a shim over the same
`app.Start` the client calls, which is what stops the two from drifting.

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
  for authorisation. That sign-in is the *whole* purpose of the HTTP client
  (§"Local is a function call"): it is how the device joins federation at all,
  and what it buys is that server's library plus madnetwork through it. It buys
  nothing in the other direction.
- **It publishes nothing.** The device's library is never catalogued or
  advertised: it is unmoderated personal content and the network cannot vouch
  for it. The single route from the device's library into the network is an
  ordinary **upload** to the home server, through the review bucket, which needs
  `file.upload` — and what the network then sees belongs to the *server*.
- **It swarms fully all the same** — fetching chunks from many holders and
  seeding back what it fetched. Serving a hash claims possession of bytes, never
  an identity, so seeding asserts nothing anyone must trust. That is why one-way
  publication and two-way swarming do not contradict each other.

  Who it seeds *to* is narrower than "anyone", and the answer is
  `federation.md` §"The household": **it serves exactly what its home server
  vouches for** — that server, and that server's other devices. A member of the
  wider community cannot pull from it, which is the price of never appearing in
  anybody's friend list.
- **It needs a capability token** to do that: to its home server's friends the
  device is a stranger, placeable by no graph walk, because it publishes no
  friend list and appears in nobody else's. That token is now built — the flow is
  §"The capability token, concretely".

### Why "publishes nothing" needs no setting

Sharing scope does not apply to a listener node, and it is worth understanding
why rather than reaching for `share_depth` — the reflex answer, and the wrong
one.

**A listener node never exchanges keys.** It does not friend anyone and nobody
friends it; its access comes from a normal node granting it a capability token,
which is a different flow entirely (§"The capability token, concretely"). So no
community ever forms *around* it, and that is what decides the question:
`serveAudience` walks friend → member → token → guest, and on a peerless node all
four fail. There are no friend rows. `MemberKeys` over an empty peer set places
nobody. `tokenAudience` needs an issuer *this* node can place, and it can place
none. The guest fallback is `madnetwork.serve_guests`, which is **off by
default**.

Nothing serving means the scope check is never reached. `share_depth`,
`DepthPrivate` and the whole publishing model exist in the binary — madshare is a
dependency, all of it comes along — and are simply **inert here**. That is
normal and not worth engineering around: dead code in a dependency is the price
of reusing it, and cheaper than a fork.

The one switch that *would* expose the library is `madnetwork.serve_guests`, and
a player has no reason to offer it.

> An earlier revision of this doc claimed the opposite — that an unset
> `madnetwork.default_share_depth` (which does resolve to `DepthUnlimited`) means
> a freshly-started embedded node advertises its owner's library. That is true of
> a node **with friends**, and a listener node has none. The default is inert
> rather than dangerous. Recorded because the worry is easy to re-derive.

### Where the bytes live: three directories, two of them technical

A server ingests by **upload** into storage it manages, and nobody browses
`files_dir` by hand — it is hash-named on purpose. A player inverts that: its
primary path is scanning folders that already exist, and the person using it
opens a file manager. So the directories divide by *who reads them*:

| Directory | Who reads it | Layout |
|---|---|---|
| `links/` | the program | one symlink per hash, target = the original file. Purely technical, exactly as on a server. |
| `cache/madnetwork/` | the program | hash-named blobs the swarm fetched, and the only thing this node seeds. Opaque by design; needs no layout and gets none. |
| **the music directory** | **a person** | `Artist/Album/NN - Title.ext`. Browsable, backup-able, indistinguishable from the rest of the collection. |

**Materializing writes into the third one**, laid out from the tags. This is the
one place the player should *not* copy the server's behaviour: on a server,
fetching remote content stages it through the review bucket into managed storage,
because it is becoming part of a moderated catalogue. On a player it is becoming
part of *your music*, and music you pulled off the network should sit next to
music you already had — same folders, same naming, no second class of file that
only the app can find.

The cache is deliberately left alone. It is scratch space for the swarm, its
contents come and go, and giving it a human layout would invite people to treat
it as a library it is not.

Open, and worth settling when this is built:

- **Does a materialized file get registered directly, or picked up by the next
  scan?** Registering it immediately is better — a track that vanishes until a
  rescan is a bug report — but it has to land as an ordinary links-backed row so
  there is exactly one kind of library entry.
- **Naming and collisions.** Tags are dirty; `/` in a title, an empty artist, two
  different recordings with identical tags. The Unknown-artist and Other buckets
  already name the empty cases (`docs/architecture/artist-album-model.md`), so
  the directory layout should reuse them rather than invent placeholders.
- **Which directory**, when several folders are scanned. Probably an explicit
  setting rather than a guess at "the main one".

### Two libraries, one list

A signed-in server does not get a second browser, a source picker or a tab. Its
artists, albums and tracks are **merged into the lists already on screen**
(decided 2026-08-08), with a small badge naming where a row lives. The person is
looking for music, and which machine holds it is a property of a row, not a mode
to be in.

The merge rule is **not the client's to invent**. It is the one the server
already applies on `/madnetwork` to fold catalogs from many nodes, because that
is the same problem — rows from different libraries sharing no id space:

| | Rule | Server's own |
|---|---|---|
| artist | `lower(name)`, Unknown-artist bucket last | `artistBucketLast` |
| album | `lower(title)` inside an artist, Other bucket last | `albumBucketLast` |
| track | disc + track number + `lower(title)` inside an album | `trackIdent` |

A client that re-derives those quietly disagrees with the web UI about what the
library contains, which is the standing rule of §"What the server already
computes" applied to a case no single server can answer — none of them can see
the others.

Four consequences, each of which is a decision rather than a detail:

- **A merged count is a lower bound**, rendered `23+`. Summing double-counts
  everything held in two places, which is exactly what a merged view is full of.
  The maximum is the one statement that is always true, because merging only
  ever folds rows and never invents them.
- **Every copy is kept, and the local one plays.** This is not an optimisation,
  it is the offline case working: a track this machine holds must play with the
  network unplugged, whichever server also has it. The converse falls out for
  free — a track whose drive is unplugged still plays from a server that has it,
  which is the merge earning its keep.
- **Ids are per-library.** 41 on one server is not 41 on another, so nothing is
  addressed by an id without its source beside it, and drilling asks each
  library the row came from with the id it has *there*.
- **One unreachable server is a footnote, not an error.** It is named above the
  rows and the rest of the music still lists. Only when *every* library fails is
  there a real error, because an empty list says "you own nothing", which is a
  much worse lie than "that server did not answer".

Re-ordering the merged list is the **one** place a client may sort: N lists that
each arrived ordered do not concatenate into an ordered list. It sorts by the
server's own keys, never by a rule of its own.

### A remote track is a download, not a stream

Worth stating because it looks like a shortcut and is not one. The pure-Go
decoders settle it: go-mp3 walks every frame header before it will report a
length (`ensureFrameStartsAndLength`), and beep's flac takes its seek path only
over an `io.ReadSeeker`. Both amount to "the whole file, on disk". Feeding a
decoder an HTTP-backed reader would issue a storm of Range requests to do what a
single sequential download does once.

So the client keeps a cache of fetched audio, **keyed by content hash** — the
same audio offered by two servers is one file, and a server changing address
orphans nothing. The directory is authoritative and there is no index, the rule
`docs/architecture/madnetwork-cache.md` settled for the same reason: an index is
a second thing to keep in agreement with the disk.

The ceiling is a **size cap with LRU eviction**, and it is **madshare's setting,
not a key in the client's config file** (decided 2026-08-08): reached through
`app.Instance.CacheCeiling` / `SetCacheCeiling` and edited on the client's
Settings panel. It is the same number a server's settings card writes, because
it is the same policy — a second copy beside it would be a number that can
disagree with itself.

One policy, one enforcer **per cache**: madshare sweeps the swarm's cache, the
client sweeps its own downloads. The ceiling is not a budget over their sum.

It has **three layers, here as on a server** (`docs/architecture/madnetwork-cache.md`
§"The retention ceiling"): a config default, a runtime override, and an unset
override meaning *inherit the default*. The client has no TOML file, so it fills
the config layer itself — `playerConfig` sets `Federation.CacheMaxMB = 2048`.

That last point is what makes the defaults differ meaningfully. A **server**
ships 0 (no limit), because a guessed number would start deleting other people's
content on a node that already has some. A **player's cache starts empty**, so it
has no such history to protect and a stated 2 GiB is a better answer than none.
Supplying it as *config* rather than writing it into the setting is the part that
matters: a value written once is indistinguishable from a value the person chose,
so clearing an override would land on "no limit" instead of back on the default.

Two things follow. Playback had to become **asynchronous**, since a download
cannot run on the goroutine that handled the click; and the next queue item is
**prefetched**, so only the first remote track in a run pays the gap.

### The credential is a token, and it belongs to that server

A player must survive a restart still signed in, and storing the password to
achieve that is the wrong answer: it opens every door the account has, including
changing itself. The client spends the password once — log in, `POST
/api/auth/tokens`, drop the session — and keeps the token, which that server
lists by name and can revoke. This is the flow `docs/api/tokens.md` documents as
the credential for a non-browser client.

Two refusals must stay distinguishable, because their answers differ: a wrong
password is retyped, while an account under a forced password change can only be
fixed on that server's own web UI. The server already separates them — the
latter is a 403 carrying `X-Password-Change-Required` — so a client that
flattens both into "sign-in failed" is discarding information it was handed.

A bare host typed into the address field means **http**, not https. A madshare is
reached at a yggdrasil address or a box on the LAN, neither of which has a
certificate (`docs/ui/clipboard.md` records the same fact from the other side).

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

Built, and the split held: `internal/library` puts both behind one `Source`
interface, so the merge and every screen above it cannot tell a function call
from a request. The difference that survives is the one that is real — a
remote source can fail, and a local one is what still answers when it does.

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

### There is no local account, and no local login

madshare refuses to start on a fresh database without an initial admin. A player
must therefore create that identity **silently, at first run** — a generated
credential the user never sees, never types and never needs. Locally it is
bookkeeping: an actor id for data sources and uploads, and the owner of the
playlists. Direct calls bypass the permission layer anyway (§"Local is a function
call"), so it is not guarding anything on this machine.

That splits settings in two, and the split is what the user actually sees:

- **App settings** — theme, folders, playback. Local, always present, and they
  are *preferences*. There is no "change password" here, because there is no
  password.

  One thing on that panel is **not** a preference and is worth telling apart: the
  download ceiling is a *policy*, and it lives in the embedded backend's settings
  rather than in the client's config file (§"A remote track is a download").
  The rule the split follows is where the value belongs, not which panel shows
  it: preferences are this program's, policy is madshare's.
- **Account settings** — password, API tokens, the things
  `docs/ui/user-settings.md` describes. These appear **only when signed in to
  another node**, and they belong to *that* node: they are the remote server's
  user settings, surfaced in this client for the account you signed in with.

Getting this wrong in the obvious way — shipping the web UI's settings page
wholesale — would ask a person to manage credentials for a database on their own
phone that nothing else can reach.

### Versioned dependency, not a vendored copy

madplayer `require`s `daemonlord.ygg/madshare` at a **released tag** and upgrades
on purpose (done at v0.9.0, when it left this repo). That is what the release
tagging is for: the client pins a known-good server, and a server change lands in
the client when someone chooses it rather than the moment it is written.

The require is resolved today by a `replace` to a checkout beside it, and that is
a stand-in rather than the end state. **madshare's module path is
`daemonlord.ygg/madshare`** — a Yggdrasil-only name that cannot serve go-import
metadata over the public internet — and Go requires a replacement module's
`go.mod` to declare the path it is required as, so pointing the replace at the
GitHub repository is rejected outright rather than merely awkward. Renaming the
module to its public path is what turns the two directives into one `require`
line, and it is a decision about madshare's public identity rather than a
packaging detail, which is why it has not been made in passing.

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
  Now ported to Go, case for case against the same worked examples. Porting the
  *rules* is fine; inventing different ones is not — two clients that disagree
  about what shuffle does share a queue in `localStorage`-shaped ways that will
  not survive the playlist sync above.
- **Like-key normalisation** (`ts:<tagset_id>` vs `mn:<hash>`, `favorites.js`).
  Small, but it is the identity two surfaces must agree on. The native client
  has its own row key today (a path, or a URL) because it has no favourites yet;
  reconciling the two is part of playlist sync, not before.

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

## Level 2b, concretely (designed 2026-08-09, not built)

The access model is `federation.md` §"The household" and is not restated here.
This is what *this program* does with it, and the order matters because each step
is what makes the next one possible.

**The device becomes a node.** `playerConfig` enables the transport and
madnetwork, so the embedded backend generates and persists a node key exactly as a
server does. Three settings differ from a server's and each is a decision:
`madnetwork.default_share_depth = Local` (nothing this device owns is ever served,
whatever else goes wrong), `[yggdrasil] multicast = true` (a phone finding its
home server on the wifi is the zero-configuration case this client is for), and
peers, below.

**Signing in also enrols the device**, so there is no second flow and no screen
called "join the mesh". The sign-in that already exists gains three calls:
`POST /api/madnetwork/token` with this device's node key — whose response carries
`issuer`, which is how the device learns its home server's key and records it as a
home node — and `GET /api/madnetwork/peering`, whose URIs are added to the
yggdrasil core. The token is renewed at `renew_after`, not at expiry.

**Getting onto the mesh is three paths, not one**, because they fail in different
situations: the peering info above, multicast on the local network, and a typed
peer list in Settings for the person who has one. A device on none of them is not
broken — it is a level-1 client, which is a working program.

**Playing network content prefers the swarm and falls back to the relay.** The
holders come from the browse row being rendered (`versions[].holders[].key`) or
from `GET /api/madnetwork/holders/{hash}` when the row is not to hand — a playlist
item, a queue restored from disk. If no holder answers, the home server's
`GET /api/madnetwork/stream/{hash}` is still there and still correct; level 2b
adds a faster path, it does not remove the one that works. This is also the answer
to "what happens on a phone with no fpcalc": that build never reaches this
paragraph, and everything else still runs.

**Seeding is a consequence, not a feature.** The cache the level-1 downloads
already fill is what gets served, the audience is the home server and its other
devices, and the only new work is telling the server what is in it
(`POST /api/madnetwork/holdings`, on the same cadence the token is renewed).
Nothing about the device's own library is involved, which is the whole point.

**Materializing finally has a caller.** Level 2a deferred the human-readable
music directory (§"Where the bytes live") because nothing produced bytes to write
into it. The swarm does, so the writer is built here and the three questions that
section left open get answered against a real caller rather than a guessed one.

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
  Where those bytes then land differs from the server — a human-readable music
  directory rather than managed storage; see §"Where the bytes live".

## Relationship to the Capacitor Android app

The Capacitor app (`docs/architecture/android-app.md`) is a thin WebView that
reuses the **web UI** same-origin and is already built and on-device-verified. It
is level 1 of §"Three levels", implemented the cheapest possible way — and now
that the native client has its own level 1, the two overlap for the first time.
They still differ in the thing that matters: the WebView browses **one** server,
the native client merges that server's library with the device's own.

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
3. **Level 2a: embed the core** — mostly done, and it came BEFORE level 1 on
   purpose: the client's own library work is gone before a second data path is
   added, rather than being reconciled with one afterwards. madshare is
   in-process, called **directly** as Go methods through the facade
   (`docs/architecture/embedding.md`), and the provisional scanner and index are
   deleted. ~~Silent provisioning~~ (§"There is no local account"), ~~folders as
   data sources~~ and ~~browse and playback off the embedded store~~ are built.
   **Still owed: the human-readable materialize target** (§"Where the bytes
   live"), which is deliberately last — nothing produces bytes to materialize
   until the mesh arrives at 2b, so building the writer before there is anything
   to write would be guessing at its caller.
4. ~~**Level 1: remote servers.**~~ Done — sign-in over HTTP, and the server's
   library merged into this device's rather than shown beside it
   (§"Two libraries, one list"). The credential is a minted API token
   (§"The credential is a token"), and remote audio is downloaded into a
   size-capped cache because the decoders leave no choice (§"A remote track is a
   download"). Verified end to end against a running madshare, including the
   case that motivates the merge — the same album on both sides folding to one
   row whose local copy plays — and the case that motivates the error handling:
   a server on a closed port leaves the device's own music listed.
   Still owed here: **cover art from a remote server** (the endpoints exist,
   `GET /api/albums/{id}/image?size=`; nothing renders images yet, on either
   side), and the **quality picker** for a remote track, which is
   `/api/tagsets/{id}/renditions` against the right server.
5. **Level 2b: the mesh** — designed 2026-08-09, not built. Node key, capability
   token from the home server, swarm fetch and seed-back. The access half is
   `federation.md` §"The household" (which is also where the three things that
   turned out to be missing are written down); the client's half is §"Level 2b,
   concretely"; the token flow is §"The capability token, concretely".
6. **Playlist sync**, once the levels above exist and the unresolvable-item
   question has a real UI to be answered in. Level 1 sharpened the case for it:
   the client now has two pools of music at once and no way to save a list
   across them.
