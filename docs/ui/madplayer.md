# Madplayer — the native client

> **Status: all three levels are built** — an offline player, an API client and
> a mesh node, one program. madplayer lives in **its own repository** (split out
> at madshare v0.8.6), beside this one, and requires madshare as an ordinary Go
> module pinned to a released tag.
>
> **The client's design moved to that repository on 2026-08-17** (owner's call):
> it is `docs/design.md` there — upstream
> <https://github.com/KianGreenMoon/madplayer> — and changing it is a madplayer
> commit. What stays HERE is the basic shape plus the parts madshare is
> answerable for: the server-computed rules no client may re-derive, and the
> access-model reasoning this repo's own code cites (`app/library.go`,
> `app/network.go` point at sections of this file by name). The behavioural
> contracts both UIs follow never moved at all — `docs/ui/library-page.md`,
> `docs/ui/player-and-queue.md`, `docs/ui/artists-and-performers.md`,
> `docs/ui/madnetwork-page.md` — and a change to any of those is a madshare
> commit, exactly as before.

## What this is

A **separate, native GUI client** for desktop and mobile (Gio, pure Go) — *not*
a reuse of the web UI. It **embeds madshare as a library**: the whole backend
runs in the client's own process, called directly through the embedder facade
(`docs/architecture/embedding.md`), and the library work — scanning, entity
resolution, charset repair, sort order, which file to play — is never
reimplemented client-side. **Same engine, inverted primary path**: a server
ingests by upload into storage it manages; the player ingests by scanning
folders in place, files that already exist, where they already are, never
written to.

Three levels, deliberately distinct, all built:

0. **An offline player.** Scans folders on this device and plays them. No
   server, no account. The baseline product, not a stepping stone.
1. **An API client.** Signs in to a server over HTTP and browses that server's
   library **merged with the device's own** into one list.
2. **A node.** Embeds a yggdrasil node too: swarm fetch from many holders,
   seeding back what it fetched, a capability token as its standing.

The deliberate consequence is **two UIs that solve the same task**, and the cost
stays bounded by one discipline: **push logic into the server, not the
clients.** The docs named above are the shared behavioural spec; both UIs follow
them rather than inventing behaviour, and anything a second client would have to
duplicate is a candidate to compute server-side and return.

## Federation: madplayer is a listener node

Defined in `docs/architecture/federation-access.md` §Principals & access; the
short form, because it is the contract half:

- **It signs in to a home server with user credentials**, not by friending it.
  The account's own rights decide what it may see. The key is for the mesh, the
  account is for authorisation.
- **It publishes nothing.** The device's library is never catalogued or
  advertised: it is unmoderated personal content and the network cannot vouch
  for it. The single route from the device into the network is an ordinary
  upload to the home server, through the review bucket.
- **It swarms fully all the same** — fetching chunks from many holders and
  seeding back what it fetched. Who it seeds *to* is `federation-access.md`
  §"The household": exactly what its home server vouches for — that server and
  that server's other devices.
- **It needs a capability token** to do that: to its home server's friends the
  device is a stranger, placeable by no graph walk. The token flow is
  `federation-access.md` §"The capability token".

### Why "publishes nothing" needs no setting

Sharing scope does not apply to a listener node, and it is worth understanding
why rather than reaching for `share_depth` — the reflex answer, and the wrong
one.

**A listener node never exchanges keys.** It does not friend anyone and nobody
friends it; its access comes from a normal node granting it a capability token,
which is a different flow entirely. So no community ever forms *around* it, and
that is what decides the question: `serveAudience` walks friend → member →
token → guest, and on a peerless node all four fail. There are no friend rows.
`MemberKeys` over an empty peer set places nobody. `tokenAudience` needs an
issuer *this* node can place, and it can place none. The guest fallback is
`madnetwork.serve_guests`, which is **off by default**.

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

## What the server already computes — do not re-implement it

This is the section to read before writing any list code, in this client or the
next one. Each of these rules is decided **server-side**, in SQL or in a
handler, and a client that renders what the endpoint returns gets it for free.
Re-deriving any of them client-side produces a UI that quietly disagrees with
the web UI about what the library contains. (Against the embedded backend the
same rules arrive as Go method results — the rule was never about HTTP, it is
about who decides.)

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
  madplayer ported it to Go, case for case against the same worked examples.
  Porting the *rules* is fine; inventing different ones is not — two clients
  that disagree about what shuffle does cannot share a queue.
- **Like-key normalisation** (`ts:<tagset_id>` vs `mn:<hash>`, `favorites.js`).
  Small, but it is the identity two surfaces must agree on; reconciling it is
  part of playlist sync, not before.

The one place a client MAY sort is re-ordering a **merged** list — N lists that
each arrived ordered do not concatenate into an ordered list — and it sorts by
the server's own keys, never by a rule of its own.

## One wording rule

Fetching remote content into a **server's** library is *Materialize*; onto a
person's own device it is *Keep on this device*. The rule turns on where the
content lands and is recorded in `docs/ui/madnetwork-page.md`; the split exists
because on a server the question is what enters a moderated catalogue, and on a
player it is whether you still have the song on the train.

## Everything else

The client's own design — the three directories and where kept music lands, the
merged browse, why a remote track is a download, the token flow from the
client's side, level 2b concretely with its measurements, the UI toolkit
decision, the roadmap — is **`docs/design.md` in the madplayer repository**,
which begins by naming what still lives here and why.
