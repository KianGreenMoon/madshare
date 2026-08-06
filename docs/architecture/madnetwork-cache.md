# The madnetwork cache — control now, forgetting later

The swarm (federation F3/F4) fetches other nodes' bytes into a cache directory
and never thinks about them again. This doc designs the missing half: a place to
**see** what is in that cache and **act** on it, and — planned, not built here —
a daemon that forgets by itself.

Two deliverables, deliberately staged:

1. **`/admin/cache`, a manual control page** — built now. Primarily a cleaning
   surface; occasionally a rescue one (materialize a cached blob into the
   library, or download it to the device).
2. **The retention daemon** — designed here, built later. Same shape as the
   planned trash reaper: a policy that deletes by age and by ceiling, shipped
   with both knobs **off**.

Reference for the cache's place in the transfer machinery:
[`federation.md`](federation.md) §Distribution.

---

## What the cache is today

Worth stating plainly, because the design follows from how little is there.

`<data_dir>/cache/madnetwork/` (`config.MadnetworkCacheDir`) is a **flat
directory of files named after their content hash**. `<hash>` is a verified,
complete blob; `<hash>.part` is one being fetched. That is the entire data
model — there is no table, no index, no metadata of any kind.

Consequences, all of them live today:

- **Nothing records when a blob was fetched or last read.** The filesystem's
  mtime is the only clock, and `atime` is not usable (`relatime`/`noatime` are
  the norm).
- **Nothing records what a blob *is*.** The origin filename arrives in
  `Content-Disposition` during the fetch and is dropped when the transfer object
  is deleted from `n.transfers`. After a restart, `completedTransfer` sets
  `filename = filepath.Base(path)` — which for a cache hit is *the hash*
  (`federation/transfer.go:142`). A cached file is a 64-hex name and nothing
  else.
- **There is no eviction at all.** Stated in `federation.md` §Principals ("the
  madnetwork cache has no eviction") and in `config.go`'s own comment. The cache
  grows until the disk is full.
- **Orphaned `.part` files are never cleaned.** A failed fetch removes its own
  partial (`transfer.go:462`, `:495`), but a killed process cannot. Nothing
  sweeps them afterwards: `EvictCachedMadnetworkBlobs` skips non-digest names on
  purpose, and `cacheHoldings` skips them via the same shape check. They are
  permanent dead disk.
- **One sweep does exist**, and stays: `database.EvictCachedMadnetworkBlobs` runs
  at startup and drops cache copies of blobs the library also holds, because the
  duplicate would be seeded under the cache's rules instead of the recording's
  sharing scope (`.issues/open-issues.md`, "Cache seeding overrides a
  recording's sharing scope"). That is a correctness sweep, not a retention one.

And the fact that makes deletion a real decision rather than housekeeping: **the
cache is what this node seeds.** `seedableBlob`'s second branch serves cache
blobs to our community, and `GET /madnetwork/v0/holdings` advertises them.
Removing an entry is not only reclaiming disk — it withdraws a seed from the
swarm.

---

## The model: the directory is the truth, the index is derived

The page needs facts the directory cannot hold, so a small index comes with it.
The rule between them is one-directional and non-negotiable:

> **The files on disk are authoritative. The index describes them. A
> disagreement is resolved by believing the disk.**

That keeps every existing code path correct without modification — `EnsureBlob`,
`seedableBlob`, `cacheHoldings` and the eviction sweep all keep reading the
directory and know nothing about the table. An index row that outlives its file
is a stale row that reconciliation drops, never a phantom entry the swarm might
advertise.

### Migration 040 — `madnetwork_cache`

```sql
CREATE TABLE madnetwork_cache (
    hash         TEXT    PRIMARY KEY,          -- lowercase sha256 hex = the filename
    byte_size    INTEGER NOT NULL DEFAULT 0,
    filename     TEXT    NOT NULL DEFAULT '',  -- the blob's own name, for storage + download
    title        TEXT    NOT NULL DEFAULT '',  -- the file's OWN embedded tags (media.ExtractTags)
    artist       TEXT    NOT NULL DEFAULT '',
    album        TEXT    NOT NULL DEFAULT '',
    fetched_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);
CREATE INDEX idx_mncache_lru  ON madnetwork_cache(last_used_at);
CREATE INDEX idx_mncache_size ON madnetwork_cache(byte_size);
```

Eight columns, and every one of them earns its place:

- `last_used_at` is the whole reason the table exists. The chosen retention
  policy is *age since last use* plus a size ceiling, and neither a directory
  walk nor `stat()` can answer "when did anyone last read this".
- `title`/`artist`/`album` are **read out of the file itself** with
  `media.ExtractTags` — the same call the upload ingest makes — once, when the
  row is created. They are not a node's claim about the bytes; they are what the
  bytes say about themselves. Without them the list is a column of hex strings
  and the search bar has nothing to match.
- `filename` is the blob's own name, needed to write it into storage with a real
  extension on materialize and to save it under a sane name on download.
- `byte_size` makes the size ceiling and the "total cache" figure one indexed
  `SUM`, the same reasoning that put `byte_size` on `files` for the dashboard's
  storage breakdown rather than walking a tree.

**No provenance columns.** No source key, no node name, no per-holder byte
accounting, and no cached copy of what any source *claimed* about the bytes —
decided 2026-08-06 (owner). `TransferStats` collects the delivery detail during a
fetch and it is discarded when the transfer ends; it stays discarded. Who holds
what belongs on the future swarm/torrent admin page, not here.

**No pin column.** Decided 2026-08-06 (owner): removal is manual, and when the
daemon lands it evicts by policy alone with nothing exempt from it.

### Reconciliation

`database.ReconcileMadnetworkCache(ctx, repo, cacheDir) (added, dropped int, err error)`
walks the directory once and makes the index agree with it:

- a `<hash>` file with no row → insert, `byte_size` from `stat`, `fetched_at` and
  `last_used_at` both from mtime (the only evidence available), tags read from
  the file (`media.ExtractTags` reads a header, not the whole blob, so adopting a
  large existing cache is a bounded one-off cost);
- a row with no file → delete.

It runs **at startup, immediately after `EvictCachedMadnetworkBlobs`** (that
sweep deletes files, so reconciling first would only re-drop the rows a moment
later), and on demand from the page's **Rescan** button. It is idempotent and
safe to run at any time.

That covers the three ways a cache and its index can drift: a pre-existing cache
from before this feature, a process killed mid-fetch, and an operator deleting
files by hand.

Steady-state writes go through the one funnel every cached byte already passes:
a completed `runTransfer` rename. It is the only place a `<hash>` file is ever
created, so a row written there cannot be missed.

### What counts as "used"

**A read by a local user, and nothing else** (decided 2026-08-06, owner). A
member of the community pulling the blob out of our cache over the mesh does
**not** count as use.

So the cache's retention clock measures one thing: *does anyone here still want
this?* Seeding is a service we render with bytes we happen to hold, not a reason
to keep holding them — and a node whose disk fills with content nobody local ever
touches, kept alive purely by other people's traffic, is the outcome to avoid.

That means the touch points are both in `api`, and `federation` needs no
knowledge of them at all:

| Path | Where |
|---|---|
| Streaming relay, and a cache hit served to a browser | `api.madnetworkStream` |
| Materialize (fetch + stage into the library) | `api.runMadnetworkDownload` |

Touching is still **throttled in memory** — at most one write per hash per
`cacheTouchInterval` (5 min). A seeking browser issues many Range requests and
each one is its own `madnetworkStream` call; without the throttle, scrubbing
through a track would write a row per drag. Five minutes is far finer than any
retention window will ever be.

No `PeerStore` change and no new federation option, so there is nothing to
re-vet in `tests/mesh/`.

---

## A cache entry on screen

### Identity, and what a row can say about itself

A row **is a file**, addressed by its content hash — this is a files-scope page,
as it must be: the cache holds bytes, and the same bytes may be described
differently by every node that has them.

**A row describes itself from its own tags.** Title, artist and album come from
the index, which read them out of the blob with `media.ExtractTags` when the row
was created. Not from any node's catalog claim — the list runs **no join against
`federation_catalog` at all**, which is what keeps it a single indexed query no
matter how large the community's cached catalogs grow.

That also settles the awkward case cleanly: a blob whose source has left the
network is not a degraded row. It reads exactly like every other row, because
what it says about itself never depended on anyone still being online. When a
file carries no usable tags either, the row falls back to its filename, then to
the hash.

What sources *currently* claim about a hash is a separate, on-demand question,
answered by the **Claims…** action for one hash at a time.

Row payload:

```json
{
  "hash": "…64 hex…",
  "byte_size": 5242880,
  "filename": "03 - Track.flac",
  "title": "Track", "artist": "Someone", "album": "An Album",
  "fetched_at": 1754400000,
  "last_used_at": 1754480000,
  "seeding": true,
  "in_library": false
}
```

No holder count and no "who has it" of any kind (decided 2026-08-06, owner):
that view belongs on the future swarm/torrent admin page, which will own it
properly for both directions of transfer. Putting a half-version of it here would
be the thing that page later has to contradict.

`seeding` reflects the node policy (`seed_enabled && seed_cache`) so the page can
say plainly whether these bytes are being served. `in_library` should always be
false — the startup sweep guarantees it — and a `true` is a visible bug signal
rather than a silent inconsistency.

### Completeness — "how full is the cache, and how full is this file"

Both readings of the question get an answer, because they are different
questions.

**Per entry.** A finished blob is by definition 100%: the swarm verifies the
whole-file sha256 before renaming into place, so a `<hash>` file is never
partial. Incompleteness lives in `.part` files, and there are two kinds:

- **In flight** — a transfer running right now. Progress is live
  (`Transfer.Progress()` / `Size()`), and the page shows it as a progress row.
- **Orphaned** — a `.part` with no running transfer, left by a killed process.
  Dead disk, today invisible and unreclaimable. The page lists them and offers
  **Reclaim partials**, which is the first time anything in Madshare can delete
  them.

Live transfers come from a new `FederationNode.ActiveTransfers() []federation.TransferStats`
— the `n.transfers` map is already keyed by hash and every entry already has
`Stats()`; this only exposes it.

**In aggregate.** The summary strip carries entries, total bytes, in-flight
count, orphaned-partial count and bytes, and — once the daemon's knobs exist —
the fill against the configured ceiling.

---

## The dashboard's storage panel

The cache is a **fifth category** on `/admin`'s storage breakdown, beside audio,
review, trash and images (`GET /api/admin/storage`, `categoryUsage{Name: "cache"}`),
labelled **Madnetwork cache**.

It is folded into the panel's "Madshare total" like every other category, and it
has to be: those bytes sit under `data_dir` and occupy the volume exactly as the
library does. A footprint that omitted them would understate the disk by however
much the swarm had fetched — which on a busy node is the largest single thing
missing from the picture. That the content belongs to other people is a fact
about *what to do* with those bytes, not about whether they are there.

The figure is `SUM(byte_size)` over the index — indexed and instant, not a
directory walk, the same reasoning that put `byte_size` on `files` rather than
walking the tree (and the opposite of the images category, whose uncached
`DirSize` walk is a known scaling complaint).

`MadnetworkCacheBytes` sits on `database.Repository`, not on the madnetwork
store, deliberately: the cache outlives federation being switched off, and a node
that turned it off would otherwise stop reporting disk it is still occupying.

## The page — `/admin/cache`

A routed admin sub-page beside Prune, Data sources and Network: one entry in
`adminSubPages`, `webui/html/admin/cache.html`, `webui/static/js/admin/cache.js`,
one nav link in `partials.html`, one dashboard card.

It is *not* a lens inside `/admin/library`. Everything there curates **our**
library — recordings, appearances, blobs we publish. The cache is somebody else's
content that we are holding, governed by different rules (it seeds under
`seed_cache`, never under a recording's `share_depth`), and nothing in it is
editable. Folding it in would blur the one line those lenses keep sharp.

### Summary strip

```
1 284 files · 41.2 GB cached      3 downloading (12.4 MB / 58.1 MB)
seeding to the community: on      2 abandoned partials · 91 MB   [Reclaim]
```

### The list

A **scope over `webui/static/js/file-list.js`** — the existing file-management
component — not a new list. It already provides, over one paged envelope, every
thing asked for here:

| Requirement | What provides it |
|---|---|
| Simulated infinite scroll | `createVirtualList` windowing + `fetchMorePage` |
| Search bar | the persistent `<input type=search>` + server `q`/`field` |
| Per-row checkbox and a bulk bar | `columns: ['check', …]` + `bulkActions` |
| "Select all N" beyond the loaded page | the `select-all-banner` → `bulkActions[].runAll(filter)` |
| Play | `onPlay(file, files)` + the page's preview player |
| Icon row actions | `rowActions` |

The scope contract it must satisfy is `loadPage({limit, offset, q, field, sort})`
→ `{items, total, selectable_total}`, which the endpoint below returns verbatim.
Nothing about the component needs changing.

Configured off: the Browse (artist/album) view — no `scope.browse`, so the view
switch never appears; the tag editors — no `editPatchURL`, no `bulkApply`, no
`accessEditable`, because **nothing here is editable**. These are another node's
tags about another node's bytes; the moment you want to change them you are
materializing into the library, where the real editors live.

Columns: `check · title · artist · album · size · fetched · last used · actions`.

Sorts (server-side): **newest fetched** (default) · oldest fetched · **least
recently used** · largest · smallest. The LRU sort matters beyond convenience —
it is a live preview of exactly what the daemon will delete first.

Search matches title, artist, album, filename and a hash prefix, entirely over
the index — one table, no joins, and every cached blob is findable whether or not
anyone still advertises it.

### Row actions

Icon-only, per the admin row-action convention:

| Action | Gate | What it does |
|---|---|---|
| **Play** | — | `GET /api/madnetwork/stream/{hash}` in the page's preview player. Cache hit → served straight off disk with Range support. This is the "what *is* this" button. |
| **Materialize** | `file.upload` + `madnetwork.access` | `POST /api/madnetwork/download {hash}` — the existing path. The bytes are already local, so `EnsureBlob` short-circuits and only staging runs; it lands in **My uploads** like any upload. Always available (see below). |
| **Download** | — | `GET /api/madnetwork/stream/{hash}?download=1` — the file to your device, no library involvement. |
| **Claims…** | — | The rare one: every tagset currently claimed for this hash, by source. Overflow menu, not a button. |
| **Remove** | `file.delete` | Delete the cache file (and its row). Modal confirm. |

Materialize and Download are the acknowledged rare paths — `/madnetwork` browse
remains the good way to bring content in. They are here because when you are
*looking at* a cached file and decide you want to keep it, sending you elsewhere
to find it again is silly.

One gap to close for them: `madnetworkDownload` currently 404s with "no friend
advertises this content" when `MadnetworkEntryForHash` returns nil, and stages
using `entryToMetadata(entry, now)` — the remote catalog text. For a cache entry
nobody advertises any more, that refusal is simply wrong: the bytes are already
on this disk, and `EnsureBlob` would hand them straight back.

**A materialize needs no source at all.** It stages into the review bucket like
any upload, so it takes its metadata the way an upload does — from the file's own
tags. Concretely, when `entry == nil` the handler builds the metadata with
`tagsToMetadata(extractTagsOrEmpty(f, mimeType), now)` (`api/handlers.go:362`,
`:385`) instead of `entryToMetadata`, over the same `t.Open()` reader it already
holds. `InsertFile` fills a non-empty title from the filename when the tags are
empty too (migration 016), so even a completely untagged blob stages cleanly and
the uploader fixes it up in **My uploads** — the ordinary path, not a special
case.

The remote catalog text stays preferred **when a claim exists**: it is usually
richer than the file's tags, and matching what the network shows for a track is
the behaviour `/madnetwork` materialize already has. This only removes the
refusal.

### Bulk actions

- **Remove selected** — `run(hashes)`; modal confirm naming the count and the
  reclaimed bytes.
- **Remove all matching** — `runAll({q, field})`, the select-all-N path. The
  endpoint refuses an empty filter without `"all": true`, matching the guardrail
  every other bulk endpoint uses (`api/appearances_handlers.go:190`).
- **Materialize selected** — sequential, one transfer at a time, aggregate toast;
  the same discipline as `/madnetwork`'s bulk materialize.

---

## API

All under `/api/admin/cache`, in the admin route group (already wrapped in
`RequirePermission(file.delete)`); the two federation-shaped actions keep their
existing gates.

```
GET  /api/admin/cache?limit=&offset=&q=&field=&sort=
     → {ok, total, selectable_total, items:[…]}          the file-list envelope

GET  /api/admin/cache/summary
     → {ok, entries, bytes, seeding:{enabled,cache},
        in_flight:[{hash,size,progress,mode}],
        partials:{count,bytes}}

GET  /api/admin/cache/{hash}/claims
     → {ok, claims:[{source_key, source_name, title, artist, album, …}]}

POST /api/admin/cache/bulk
     {action:"remove", hashes:[…]}                       explicit set
     {action:"remove", all:true, filter:{q,field}}       whole matching set
     → {ok, removed, bytes}

POST /api/admin/cache/rescan     → {ok, added, dropped}
POST /api/admin/cache/partials/reap → {ok, removed, bytes}
```

Plus one small change outside the prefix: `GET /api/madnetwork/stream/{hash}`
gains `?download=1`, which sets `Content-Disposition: attachment` with a filename
from the index (falling back to a name synthesized from the tagset text and
codec, the way `runMadnetworkDownload` already does).

Removal is `os.Remove` + row delete, in that order, tolerating `ENOENT` at both
ends — the disk is the truth, so a file already gone is a success, not an error.
Removing bytes some request is mid-read is safe: POSIX keeps the open descriptor
alive across the unlink, which is the same property `EvictCachedBlob` already
relies on. Every removal is written to the audit log.

---

## What this touches that already exists

| Existing thing | Change |
|---|---|
| `EvictCachedMadnetworkBlobs` (startup) | unchanged; reconciliation now runs after it and drops the rows for what it deleted |
| `Node.EvictCachedBlob` | also deletes the index row (via the touch callback's sibling, or the caller in `api`) |
| `seedableBlob`, `cacheHoldings`, `EnsureBlob`, `handleBlob` | **unchanged** — they read the directory, and the directory stays the truth |
| `PeerStore`, federation options | **unchanged** — nothing to re-vet in `tests/mesh/`; the only addition to `FederationNode` is `ActiveTransfers()` |
| `file-list.js` | **unchanged** — this is a new scope, not a component change |
| `database_test.go` | migration count/table assertions need bumping for 040 (the standing gotcha) |
| `api` `fakeRepo` | gains the new `Repository` methods (the other standing gotcha) |

---

## Planned — the retention daemon (not built here)

Designed now because its storage requirement is what shaped migration 040; built
later, and off until an operator turns it on.

**Policy: age since last use, plus a size ceiling.** Both apply; either can be
disabled alone.

- `madnetwork.cache_max_age_days` — evict entries whose `last_used_at` is older
  than N days.
- `madnetwork.cache_max_bytes` — while the total exceeds the ceiling, evict
  least-recently-used first until it fits.

Both are **runtime settings in `MadnetworkPolicy`** (`GET/POST
/api/admin/settings/madnetwork`, a card on `/admin/settings`), not static config,
so an operator can change them without a restart — matching `seed_enabled`,
`seed_cache` and `autoapprove_downloads`.

**Both default to 0 = off.** The mechanism is built in full; the numbers are the
operator's. A guessed default here would silently delete other people's content
on every existing node the moment they upgrade — the worst possible way to learn
a feature exists.

Shape, matching the prune job (`prune/`, `docs/architecture/prune-job.md`) rather
than inventing a second pattern:

- One goroutine, one sweep at a time, started from `madshare.go` only when a knob
  is non-zero.
- Cadence hourly; a sweep is cheap (two indexed queries and some `unlink`s).
- **Never touches an in-flight transfer** — a hash in `n.transfers` is skipped,
  and `.part` files are not eviction candidates at all (orphaned ones are the
  Reclaim button's job, which the daemon may also perform on a longer fuse).
- Nothing is exempt: there is no pin (decided 2026-08-06). What the daemon
  deletes is re-fetchable from the swarm as long as a holder remains.
- Every sweep logs what it removed and why (age vs. ceiling), and reports its
  last run on the cache page.

**Relation to the trash reaper.** The trash quarantine
([`gc-model.md`](gc-model.md)) wants the same thing — delete by date, on a timer,
with the window configurable — and the two should end up sharing one small
"scheduled reaper" harness rather than each growing its own. They are not the
same policy, though, and must not be merged into one setting: trash holds *our*
content that a person deleted and may want back, while the cache holds *other
people's* content that we can simply fetch again. The consequences of being wrong
are not comparable, which is why the cache can be swept on a much shorter fuse
than any trash window.

---

## Permissions

The page sits in the admin route group, so reaching it already requires
`file.delete`. Beyond that:

- listing, summary, claims, rescan, remove, reap partials — `file.delete`
  (this is a surface for destroying local data);
- Materialize — the existing `POST /api/madnetwork/download` gates,
  `file.upload` + `madnetwork.access`; the button is hidden client-side without
  them, exactly as `mn-browse.js` hides it;
- Download — `madnetwork.access` via the existing stream endpoint.

No new permission. The planned `madnetwork.access` split into browse-vs-relay
(`federation.md` §Principals) would touch this page's Materialize and Download
when it happens, and nothing else here.

---

## Build plan

1. **Migration 040 + the index** — ✅ built. Table, `database/madnetwork_cache.go`
   (list / count / sum / hashes / touch / put / delete / reconcile),
   reconciliation wired into `madshare.go` after the eviction sweep, and the
   dashboard's `cache` storage category. Tests: reconcile adopts an existing
   cache (reading the file's own tags), skips `.part` and stray names, drops
   fileless rows, is idempotent; the listing's page/count/select-all set agree
   under every filter; the use clock is monotonic, scoped to indexed rows, and
   unmoved by a re-fetch.
2. **Write path** — the completion insert (tags read from the blob), the two
   local touch points and the throttle. Tests: a completed fetch is indexed with
   its own tags; a seeking browser's Range storm writes one touch; a seed serve
   writes none.
3. **API** — the five endpoints plus `?download=1`, and the no-claim fallback in
   `madnetworkDownload`. Tests: envelope shape, empty-filter guardrail,
   remove-tolerates-missing-file, an unadvertised cache entry materializes and
   carries its own tags.
4. **Page** — `cache.html`, `admin/cache.js` as a `file-list.js` scope, nav link,
   dashboard card, summary strip, partials reclaim.
5. **Daemon** — later, on its own, once the shape above has lived on a real node
   for a while.

Steps 1–4 are the deliverable. Step 5 is scheduled, not promised.

---

## Decisions

| Date | Decision | Why |
|---|---|---|
| 2026-08-06 | **No provenance, and no cached source claims.** Nothing records which node served the bytes or what it called them. | Owner call. `TransferStats` has the delivery detail during the fetch and it stays discarded. |
| 2026-08-06 | **A row describes itself from the file's own tags** (`media.ExtractTags` at index time). | Owner call, generalising the materialize answer: a cached blob falls back to tags-inside-the-file behaviour. Local, source-independent, and it is what makes the search bar work. |
| 2026-08-06 | **Materialize never needs a live claim.** No-claim → stage with the file's own tags. | Owner call. It goes through the upload/review path regardless, and that path already reads tags from the file. The 404 was protecting nothing. |
| 2026-08-06 | **Daemon evicts by last use + size ceiling**, both off by default. | Owner call. Fetch-date-only eviction deletes the track you replay weekly at the same rate as junk; the ceiling is what makes disk predictable. |
| 2026-08-06 | **No pin.** | Owner call. Removal stays manual; the daemon, when it lands, has nothing exempt from it. |
| 2026-08-06 | **"Used" = local reads only**, throttled to one write per hash per 5 min. | Owner call. Seeding is a service rendered with bytes we hold, not a reason to hold them; a disk kept full purely by other people's traffic is the outcome to avoid. |
| 2026-08-06 | **Live catalog claims** for the per-entry tagset view, no claim history. | No new growing table for a rarely-used button; truthful about now, which is what the button is for. |
| 2026-08-06 | **No holder count, no "who has it".** | Owner call: that belongs on the future swarm/torrent admin page, which will own it for both directions. A half-version here is something that page would have to contradict. It also removes the list's only join. |
| 2026-08-06 | **Directory authoritative, index derived.** | Every existing cache path keeps reading the directory unmodified; a stale row is a reconciliation problem, never a phantom the swarm could advertise. |
| 2026-08-06 | **Its own page, not a `/admin/library` lens.** | Those lenses curate content we publish under our sharing scopes. This is other people's content under `seed_cache`, and none of it is editable. |
| 2026-08-06 | **A `file-list.js` scope, not a new list.** | Infinite scroll, search, select-all-N, bulk bar and Play already exist there, over exactly the paged envelope this endpoint returns. |
