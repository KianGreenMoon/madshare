# The madnetwork cache — control, and forgetting

The swarm (federation F3/F4) fetches other nodes' bytes into a cache directory
and never thinks about them again. This doc designs the missing half: a place to
**see** what is in that cache and **act** on it, plus a daemon that forgets by
itself.

Two deliverables, deliberately staged — both now built:

1. **`/admin/cache`, a manual control page** — built now. Primarily a cleaning
   surface; occasionally a rescue one (materialize a cached blob into the
   library, or download it to the device).
2. **The retention daemon** — its **size ceiling is BUILT** (2026-08-08, see
   §"The retention ceiling"), and its **age half since 2026-08-13**. Both ship
   **off**, as designed.

   The age half was held back on 2026-08-08 (owner decision) because nothing had
   been reported as painful and — the reason specific to *this* half — deleting a
   cache entry does not only reclaim disk, it **withdraws a seed from the
   swarm**. See "what makes deletion a real decision" below. It is on now with
   that cost written into the settings card's own copy and the default left at 0.

   Still **not** shared with `gc-model.md`'s trash TTL: the two want the same
   "scheduled reaper" harness, and the two policies must not become one setting
   (trash holds *our* content somebody deleted and may want back; the cache holds
   *other people's* content we can fetch again). Merging the harness is a
   refactor waiting for the trash TTL to be built, not a gap in this feature.

Reference for the cache's place in the transfer machinery:
[`federation-swarm.md`](federation-swarm.md) §Distribution.

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
- **There is no eviction at all.** Stated in `federation-access.md` §Principals ("the
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
from before this feature, a process killed mid-fetch, and files removed by
something that is not this server — an operator's `rm`, a disk cleanup job, a
restored backup.

### Files deleted behind the server's back

Worth stating on its own, because it is the drift an operator will actually
cause, and because the two halves of the answer are very different:

- **Nothing dangerous happens, ever.** Seeding reads the directory
  (`cacheHoldings` re-lists it on every request), so a deleted file is **never
  advertised to a peer**. That is the whole payoff of making the directory
  authoritative: the index can be wrong without the swarm being wrong.
- **But the page must not go on counting bytes that are not there**, and it must
  not need a restart to notice. So the index **self-heals as it is read**:

| Where | What it does | Cost |
|---|---|---|
| The listing | stats the ≤100 rows on this page, drops the ones whose file is gone, and omits them | bounded; what you see is always real |
| The summary | drops every stale row in the index | free — it already lists the directory to count abandoned partials |
| `…/audio` | a request for missing bytes proves the row wrong, so it drops it | none |
| Startup / Rescan | full reconcile, both directions | the wholesale options |

Dropping is silent in the code but **logged once per sweep** — files vanishing
without the server doing it is either deliberate or worth knowing about.

No watcher, no inotify, no daemon: the index is only ever wrong between the
deletion and the next time somebody looks, and looking is what fixes it.

Steady-state writes go through one funnel: **`api.ensureBlob`**, the wrapper both
callers of `federation.EnsureBlob` now use (the streaming relay and
download-to-library — there are no others). It records the read, and arranges for
the blob to be indexed once the fetch lands.

The index deliberately does **not** live in `runTransfer`, even though that is
where the bytes physically arrive. Fetching does not require an index, so the
transfer engine has no business knowing about a database — and putting it there
would mean either a `PeerStore` change (re-vetting `tests/mesh/`) or a fourth
injected callback, plus a new `federation → media` dependency edge for the tag
read. Coverage is the same either way, because those two callers are the only
route into `EnsureBlob`.

Indexing happens in a **goroutine waiting on `t.Done()`**, not on the response
path: the fetch outlives the request that started it (cache-through streaming
keeps filling the cache after a browser disconnects), so waiting inline would tie
the row to whether someone stayed on the page. It is best-effort for the same
reason it can be — anything a crash loses between the rename and the insert is
adopted by the next reconcile.

Two kinds of finished transfer are **not** cache entries, and the indexer skips
both: one whose `Stats().Mode` is `"local"` (born complete — the bytes were
already here, and re-putting a cache hit would overwrite its real origin filename
with the hash, since `completedTransfer` names a transfer after its own path),
and one whose hash has no file under the cache dir (the library short-circuit —
`EnsureBlob` resolves the library before the cache).

### What counts as "used"

**A read by a local user, and nothing else** (decided 2026-08-06, owner). A
member of the community pulling the blob out of our cache over the mesh does
**not** count as use.

So the cache's retention clock measures one thing: *does anyone here still want
this?* Seeding is a service we render with bytes we happen to hold, not a reason
to keep holding them — and a node whose disk fills with content nobody local ever
touches, kept alive purely by other people's traffic, is the outcome to avoid.

That means the touch lives in `api`, and `federation` needs no knowledge of it at
all. Both local paths — the streaming relay (`madnetworkStream`) and
download-to-library (`runMadnetworkDownload`) — reach it through the single
`ensureBlob` wrapper, so the touch is tied to *asking for the bytes* rather than
to a successful response: a stream the browser abandons was still a person
wanting that track.

The seeding side is served entirely inside `federation.handleBlob`, which has no
route into this package. So "seed serves don't count" is **structural, not a
rule someone has to remember** — there is nothing to switch off.

Touching is **throttled in memory** — at most one write per hash per
`cacheTouchInterval` (5 min). A seeking browser issues many Range requests and
each one is its own relay call; without the throttle, scrubbing through a track
would write a row per drag. Five minutes is far finer than any retention window
will ever be. Evicting a blob clears its throttle memory too, so a re-fetch
inside the window cannot inherit a stale clock.

The write is monotonic in SQL (`WHERE last_used_at < ?`), so an out-of-order
touch — a throttled writer racing, or a clock stepping back — can never walk a
row *toward* eviction.

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
- **Abandoned** — a `.part` with no running transfer, left by a killed process.
  Dead disk that nothing could reclaim before this.

Live transfers come from a new `FederationNode.ActiveTransfers() []federation.TransferStats`
— the `n.transfers` map is already keyed by hash and every entry already has
`Stats()`; this only exposes it. It is not just a display convenience: **it is
what tells an abandoned partial from a live one**, and reaping a running fetch's
scratch file would be data loss mid-transfer.

**Abandoned partials are swept automatically, at startup, unconditionally**
(`database.ReapAbandonedPartials`, called from `app.Start` before the index
reconcile). The rule that makes this safe needs no age heuristic, no policy and
no knob: *a process that has just started is writing nothing*, so every `.part`
it finds is abandoned by definition — the live set is correctly `nil` there.

At runtime the same function backs the page's **Reclaim partials** button, and
must be passed the live set. This is deliberately **not** folded into
`ReconcileMadnetworkCache`, which the Rescan button can run while transfers are
in flight.

**In aggregate.** The summary strip carries entries, total bytes, in-flight
count, orphaned-partial count and bytes, and — once the daemon's knobs exist —
the fill against the configured ceiling.

---

## The dashboard's storage panel

The cache is a category on `/admin`'s storage breakdown, beside audio, review,
trash, images and database (`GET /api/admin/storage`, `categoryUsage{Name: "cache"}`),
labelled **Madnetwork cache**.

It is folded into the panel's "Madshare total" like every other category, and it
has to be: those bytes sit under `data_dir` and occupy the volume exactly as the
library does. A footprint that omitted them would understate the disk by however
much the swarm had fetched — which on a busy node is the largest single thing
missing from the picture. That the content belongs to other people is a fact
about *what to do* with those bytes, not about whether they are there.

The figure is `SUM(byte_size)` over the index — indexed and instant, not a
directory walk, the same reasoning that put `byte_size` on `files` rather than
walking the tree. Images followed the same route in migration 043
(`image_variants`, see `storage.md`), which was the last walked category; every
category on the panel is now an indexed sum.

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
| **Play** | `file.delete` | `GET /api/admin/cache/{hash}/audio` in the page's preview player. This is the "what *is* this" button. |
| **Materialize** | `file.upload` + a running node | `POST /api/madnetwork/download {hash}` — the existing path. The bytes are already local, so `EnsureBlob` short-circuits and only staging runs; it lands in **My uploads** like any upload. |
| **Download** | `file.delete` | The same audio endpoint with `?download=1` — the file to your device, no library involvement. |
| **Claims…** | `file.delete` | The rare one: every tagset currently claimed for this hash, by source. |
| **Remove** | `file.delete` | Delete the cache file and its row. Modal confirm. |

**Why Materialize is here at all**, when `/madnetwork` browse is the better way
to bring content in: it is the only way that works **offline**. The motivating
case is a madplayer somewhere with no connectivity (owner, 2026-08-06). The
browse page is a view of other people's catalogs and goes blank without them;
the cache is bytes already on this device. Deciding to keep one of those is a
purely local act, and it must not require the network that is missing.

That is what makes the no-claim path (below) load-bearing rather than a tidy-up:
offline, nothing advertises anything, so a materialize that needed a live claim
would fail in exactly the situation this button exists for. Everything the path
touches — the gates, the policy read, `EnsureBlob`'s cache short-circuit, the
tags, the analysis pipeline — is local.

Download is the smaller companion: the same bytes, to the device, no library
involvement.

**Play and Download do not go through the madnetwork streaming relay**, and this
is load-bearing rather than a preference. That relay is registered only when
`Deps.Federation != nil` — a running node — but *the cache outlives federation
being switched off*, which is exactly the situation in which someone opens this
page to reclaim disk. Asking the mesh for a file already on this disk would be
indirection that buys nothing and breaks in the one case that matters. So the
page reads the cache directory directly, through its own admin endpoint, gated
like the rest of the page. (Caught by running it: with federation off, every Play
and Download 404'd.)

**Materialize is the one action that does disappear** with federation off, since
`POST /api/madnetwork/download` is itself federation-gated. The summary reports
`federation: true|false` and the page omits the button rather than offering one
that 404s. That asymmetry is honest: playing, downloading and removing are
operations on a local file; materializing is a madnetwork feature.

One gap closed for them: `madnetworkDownload` used to 404 with "no friend
advertises this content" whenever `MadnetworkEntryForHash` returned nil. For a
cache entry nobody advertises any more that refusal was protecting nothing — the
bytes are already on this disk, and `EnsureBlob` hands them straight back.

**A materialize needs no source at all.** It stages into the review bucket like
any upload, so it takes its metadata the way an upload does — from the file's own
tags. Concretely, when `entry == nil` the handler builds the metadata with
`tagsToMetadata(extractTagsOrEmpty(f, mimeType), now)` (`api/handlers.go:362`,
`:385`) instead of `entryToMetadata`, over the same `t.Open()` reader it already
holds. `InsertFile` fills a non-empty title from the filename when the tags are
empty too (migration 016), so even a completely untagged blob stages cleanly and
the uploader fixes it up in **My uploads** — the ordinary path, not a special
case.

Only the *filename* still needs care, because the staged blob needs a real
extension. The chain is transfer name → the index's remembered origin name →
**the file's own container** (`media.Tags.FileType`, which the tag reader takes
from the bytes; `extForFileType`), and only then a refusal.

That third link is not a nicety — it is what carries the offline case, and it was
missing until it was tested for real. A blob **adopted** into the index has no
remembered filename (the origin's name was never recorded before there was
anywhere to put it), so *every cache on every node upgrading into the index
starts out that way*. Offline there is no claim either. Without asking the file
what it is, a whole existing cache would be unmaterializable on exactly the
devices with no other way to get the file.

Two things stay as they were. The remote catalog text is still **preferred when a
claim exists** — it is usually richer than the file's tags, and matching what the
network shows is the behaviour `/madnetwork` materialize already has. And a hash
we neither hold nor can find a holder for is still a 404.

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
        partials:{count,bytes}}      abandoned only — a live fetch's is not one

GET  /api/admin/cache/{hash}/audio[?download=1]
     → the bytes, straight off the cache dir (native Range; attachment when
       download=1). NOT the madnetwork relay — see "Row actions" above.

GET  /api/admin/cache/{hash}/claims
     → {ok, claims:[{source_key, source_name, title, artist, album, …}]}

POST /api/admin/cache/bulk
     {action:"remove", hashes:[…]}                       explicit set
     {action:"remove", all:true, filter:{q,field}}       whole matching set
     → {ok, removed, bytes}

POST /api/admin/cache/rescan     → {ok, added, dropped}
POST /api/admin/cache/partials/reap → {ok, removed, bytes}
```

```
GET  /api/admin/cache/summary  also reports  federation: true|false
```
— whether a node is running, which is what decides if Materialize is offered.

The download filename comes from the index, falling back to the file's own tags
(`Artist - Title`) and finally the hash. `http.ServeContent` types the response
from that name's extension and sniffs the bytes when it has none, so a blob
adopted from a pre-existing cache with no remembered name still plays (Go's
sniffer recognises an `ID3` prefix as `audio/mpeg`).

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
| `Node.EvictCachedBlob` | unchanged; its `api` caller (`h.evictCached`) now drops the index row after it — file first, description second |
| `Deps` | gains `MadnetworkCacheDir`, wired unconditionally like `Madnetwork` (the cache outlives federation being switched off) |
| `seedableBlob`, `cacheHoldings`, `EnsureBlob`, `handleBlob` | **unchanged** — they read the directory, and the directory stays the truth |
| `PeerStore`, federation options | **unchanged** — nothing to re-vet in `tests/mesh/`; the only addition to `FederationNode` is `ActiveTransfers()` |
| `file-list.js` | **unchanged** — this is a new scope, not a component change |
| `database_test.go` | migration count/table assertions need bumping for 040 (the standing gotcha) |
| `api` `fakeRepo` | gains the new `Repository` methods (the other standing gotcha) |

---

## The retention ceiling (ceiling built 2026-08-08, age half 2026-08-13)

**Policy: age since last use, plus a size ceiling.** Both apply; either can be
disabled alone. Both are now built, and both ship OFF.

- `madnetwork.cache_max_bytes` — while the total exceeds the ceiling, evict
  least-recently-used first until it fits.
- `madnetwork.cache_max_age_days` — evict entries whose `last_used_at` is older
  than N days.

**Age runs first, then the ceiling.** The order is not arbitrary: age asks an
absolute question ("does anyone here still want this?") while the ceiling asks
how much room is left, so running the ceiling first would evict by coldness
blobs the age rule was about to remove anyway — and report the wrong reason for
it. Each sweep logs its own count, because the two answers lead to different
knobs. Both evict through one `evictCacheEntry`, so the parts that are about the
cache rather than about the policy — sparing a hash in `ActiveTransfers()`,
tolerating one unremovable file, file-then-row order — cannot drift apart.

**The age knob has only TWO layers, deliberately**, where the ceiling has three.
The placement rule (`swarm-admin.md` §"Which layers a knob gets") says a config
layer is earned when a deployment without this UI must ship the value — and the
embedder story on this cache is a *size* (madplayer's 2 GiB), a bound on a
device's disk that an age cannot express. With no config layer there is no
"inherit" state to tell apart from 0, so the setting is a plain number, its API
field is a `*int64` (absent = unchanged, a number = set, 0 = off) rather than a
`json.RawMessage`, and the count of full three-layer knobs stays at three.

**What `last_used_at` means matters more here than for the ceiling.** It moves
on LOCAL reads only — a mesh peer pulling from our cache never counts, and
structurally cannot, since seeding is served in `federation.handleBlob` which
has no route into `api`. So this rule says *we* stopped using a blob, not that
nobody wants it, and switching it on can withdraw a seed the community is still
fetching. That is the operator's call, which is why the default is 0.

**Three layers, exactly like the swarm rate caps** (owner, 2026-08-08) — a
config default, a runtime override, and an "unset" that means inherit:

| Layer | Where | Meaning |
|---|---|---|
| Config default | `[federation] cache_max_mb` (MiB) | the ceiling when nothing overrides it; **0 = no limit**, and it is what ships |
| Runtime override | `madnetwork.cache_max_bytes` (bytes) | set on the settings card; a stored **0 is a real override** meaning "no limit" |
| Unset override | key absent | **inherit the config file** — the UI's *Default* |

`database.ResolveCacheCeiling(override, configDefault)` is the one function both
the sweep and the card resolve through, so they cannot disagree about what is in
force. Config in MiB and the setting in bytes is deliberate: MiB is the unit
somebody writing a config file thinks in (matching `storage.max_upload_mb`),
bytes is what the sweep compares, and `Config.CacheDefaultBytes()` is the single
conversion.

The runtime layer exists because a ceiling is adjusted while watching a disk
fill, and a knob that needs a config edit and a restart to move is a knob that
stays wrong. The config layer exists because an **embedder has no TOML file** and
still needs a default it chooses — madplayer sets 2 GiB there
(its `docs/design.md`), so its UI's *Default* is a real number rather than
"none".

**The shipped default is 0 = off.** The mechanism is built in full; the number is
the operator's. A guessed default here would silently delete other people's
content on every existing node the moment they upgrade — the worst possible way
to learn a feature exists.

The **API field is three-valued** (`cache_max_bytes`: absent = unchanged, `null`
= clear the override, a number = pin it) — the shape `share_depth` and the swarm
rates already use, decoded as `json.RawMessage` because a `*int64` collapses
"absent" and "null" into one nil and those are opposite instructions. GET reports
all three parts: `cache_max_bytes` (null when unset), `cache_default_bytes` and
`cache_effective_bytes`, because a UI choice called *Default* is meaningless
unless it can say what the default is. The card types **MiB** and an **empty
field is the null** — the same vocabulary `/admin/swarm`'s limits modal uses.

**Lowering it evicts in the same request**, and the reply carries `evicted` /
`freed_bytes` so the toast can say what went. Somebody who has just lowered a
ceiling is watching the disk; a number that takes an hour to mean anything reads
as a control that does not work.

Shape, matching the prune job (`prune/`, `docs/architecture/prune-job.md`) rather
than inventing a second pattern:

- One goroutine, one sweep at a time, started from `app.Start`
  **unconditionally**, re-reading the ceiling every pass. (The original design
  said "only when a knob is non-zero"; that was wrong and is corrected here —
  starting conditionally at boot would make switching a *runtime* setting on
  require a restart, which is the one property it was made runtime to avoid. An
  off ceiling costs one settings read an hour.)
- Cadence hourly; a sweep is cheap (two indexed queries and some `unlink`s).
- **File first, row second** (`database.SweepCacheCeiling`). The directory is the
  truth: a row without its file is stale and reconciliation drops it, while a
  file without its row is a blob that keeps being served and never counted. Only
  one of those two orders leaves a dangerous residue.
- One unremovable file is skipped, not fatal — the next-coldest blob frees the
  space just as well, and refusing to evict anything because of one file is how a
  ceiling silently stops being enforced.
- **An embedder shares the number, not the sweep.** `app.Instance.CacheCeiling`
  (effective / override / default) and `SetCacheCeiling` expose the setting, and a
  program with its own cache of remote audio enforces the same ceiling over its
  own directory — which is what madplayer does with its downloads
  (its `docs/design.md`). The ceiling therefore applies *per cache of remote
  audio a node keeps*, not to their sum.
- **Never touches an in-flight transfer** — a hash in `ActiveTransfers()` is
  skipped, which is also how it must call `ReapAbandonedPartials`. Abandoned
  partials are already swept unconditionally at startup and on demand, so the
  daemon only adds "without waiting for a restart"; they are never eviction
  *candidates* in the retention sense, because unverified bytes have no value to
  weigh in the first place.
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
(`federation-access.md` §Principals) would touch this page's Materialize and Download
when it happens, and nothing else here.

---

## Build plan

1. **Migration 040 + the index** — ✅ built. Table, `database/madnetwork_cache.go`
   (list / count / sum / hashes / touch / put / delete / reconcile),
   reconciliation wired into `app.Start` after the eviction sweep, and the
   dashboard's `cache` storage category. Tests: reconcile adopts an existing
   cache (reading the file's own tags), skips `.part` and stray names, drops
   fileless rows, is idempotent; the listing's page/count/select-all set agree
   under every filter; the use clock is monotonic, scoped to indexed rows, and
   unmoved by a re-fetch.
2. **Write path** — ✅ built. `api/madnetwork_cache.go`: the `ensureBlob` wrapper
   (touch + index-on-complete) adopted by both `EnsureBlob` callers, the
   background indexer reading the blob's own tags, the throttle, and index
   removal on eviction. Tests: a completed fetch is indexed with its own tags and
   the origin filename; a born-complete transfer and a library blob are both
   skipped; a Range storm writes one touch, and the throttle is per hash;
   eviction drops the row and its throttle memory.
3. **API** — ✅ built. `api/cache_handlers.go`: the six endpoints, `?download=1`
   on the stream relay, `federation.Node.ActiveTransfers`, the startup partial
   sweep, and the no-claim path through `madnetworkDownload`. Tests: the
   file-list envelope and its `selectable_total`; the empty-filter guardrail and
   `all:true`; removal takes the file, its row and nothing else, and tolerates a
   file already gone; the reaper spares a RUNNING transfer's partial and takes
   the abandoned one; an unadvertised cached blob materializes while an
   unreachable hash still 404s; the download name falls back index → tags → hash.
4. **Page** — ✅ built. `cache.html`, `admin/cache.js` as a `file-list.js` scope,
   the admin nav link, a dashboard card badged with the cache's footprint, the
   summary strip with Reclaim and Rescan, the claims panel, and the direct audio
   endpoint. `file-list.js` gained one option — `scope.sorts`, a scope-supplied
   sort vocabulary — because "least recently used" has no equivalent in the
   library's orders and it is the order that previews what a retention sweep
   would take. Verified live against a running server: adoption reads ID3v1 and
   ID3v2 tags off the blob, search matches text and hash prefixes, Range requests
   answer 206, removal takes file and row, the guardrail refuses an unqualified
   clear-all, and a restart reaped the abandoned partial by itself.
5. **Daemon** — ✅ built. The size half 2026-08-08 (hourly goroutine from
   `app.Start`, re-reading the ceiling each pass), the age half 2026-08-13:
   `database.SweepCacheAge`, the `madnetwork.cache_max_age_days` setting, its
   field on the settings card, and both rules applied in the save request as
   well as on the timer. Tests: the age rule evicts only what is past the window
   (never merely the coldest — that is the ceiling's job and the one thing the
   two could be confused for), 0 and below are off, an in-flight transfer is
   spared, and the setting round-trips including the negative clamp.

Steps 1–5 are all built. What is deliberately still not: sharing one reaper
harness with `gc-model.md`'s trash TTL, which waits for that TTL to exist.

---

## Decisions

| Date | Decision | Why |
|---|---|---|
| 2026-08-06 | **No provenance, and no cached source claims.** Nothing records which node served the bytes or what it called them. | Owner call. `TransferStats` has the delivery detail during the fetch and it stays discarded. |
| 2026-08-06 | **A row describes itself from the file's own tags** (`media.ExtractTags` at index time). | Owner call, generalising the materialize answer: a cached blob falls back to tags-inside-the-file behaviour. Local, source-independent, and it is what makes the search bar work. |
| 2026-08-06 | **Materialize never needs a live claim.** No-claim → stage with the file's own tags. | Owner call. It goes through the upload/review path regardless, and that path already reads tags from the file. The 404 was protecting nothing — and offline (the case the button exists for) nothing advertises anything, so requiring a claim would fail exactly when it is needed. |
| 2026-08-06 | **Materialize exists for the OFFLINE case**, not for convenience. | Owner, stating the motivating scenario: a madplayer with no connectivity, adding a cached file to its library. `/madnetwork` browse cannot serve that — it is a view of other people's catalogs. Keep this button and its no-claim path local end to end; do not "simplify" either into something that consults the network. |
| 2026-08-06 | **The staging filename falls back to the file's own container** (`media.Tags.FileType` → extension). | Found by running the offline case rather than reasoning about it. An adopted cache row has no remembered filename, so every upgrading node's whole cache was unmaterializable. Tested end to end against a node with no peers. |
| 2026-08-06 | **The index self-heals as it is read**, rather than only at startup and Rescan. | Owner asked what happens when files are deleted by the OS. Measured: the swarm was already correct (seeding reads the directory) but the page counted phantom bytes until a restart. Healing on read costs a bounded stat sweep on the listing and nothing at all on the summary, which already reads the directory. |
| 2026-08-06 | **Daemon evicts by last use + size ceiling**, both off by default. | Owner call. Fetch-date-only eviction deletes the track you replay weekly at the same rate as junk; the ceiling is what makes disk predictable. |
| 2026-08-08 | **The size half was built**, as a settings-panel control. | Owner call. A ceiling is adjusted while watching a disk fill; a knob needing a config edit and a restart is a knob that stays wrong. |
| 2026-08-13 | **The age half was built**, and got **two** layers where the ceiling has three. | The placement rule (`swarm-admin.md` §"Which layers a knob gets") answers it: a config layer is earned by a deployment that must ship the value without this UI, and the embedder story on this cache is a SIZE — a bound on a device's disk, which an age cannot express. No config layer means no "inherit" state, so unset and 0 are the same answer and the setting is a plain number. |
| 2026-08-13 | **Age is swept before the ceiling**, and each logs its own count. | Age asks an absolute question, the ceiling asks how much room is left. The other order evicts by coldness what the age rule was about to take anyway, and then reports the wrong reason — and the reason is what tells an operator which knob to reach for. |
| 2026-08-08 | **Three layers, not two**: `[federation] cache_max_mb` as the default, a runtime override on the card, and empty = inherit. | Owner call, same day, correcting the settings-only first cut. It is the arrangement the swarm rates already use, and it is what lets an embedder with no TOML file (madplayer, 2 GiB) have a *Default* that means something. |
| 2026-08-08 | **The sweep goroutine starts unconditionally and re-reads the ceiling**, correcting this doc's own "start only when a knob is non-zero". | Starting conditionally at boot would make switching a *runtime* setting on require a restart — the one property it was made runtime to avoid. An off ceiling costs one settings read an hour. |
| 2026-08-08 | **An embedder shares the number, not the sweep** (`app.Instance.CacheLimit`/`SetCacheLimit`). | madplayer keeps its own cache of remote audio and enforces the same ceiling over its own directory. One policy number, one enforcer per cache — so the ceiling is per cache, not over their sum. |
| 2026-08-06 | **No pin.** | Owner call. Removal stays manual; the daemon, when it lands, has nothing exempt from it. |
| 2026-08-06 | **"Used" = local reads only**, throttled to one write per hash per 5 min. | Owner call. Seeding is a service rendered with bytes we hold, not a reason to hold them; a disk kept full purely by other people's traffic is the outcome to avoid. |
| 2026-08-06 | **Live catalog claims** for the per-entry tagset view, no claim history. | No new growing table for a rarely-used button; truthful about now, which is what the button is for. |
| 2026-08-06 | **No holder count, no "who has it".** | Owner call: that belongs on the swarm admin page, which owns it for both directions — **built 2026-08-06, `swarm-admin.md`**, where it is the per-row info panel. A half-version here is something that page would have had to contradict. It also removes the list's only join. |
| 2026-08-06 | **Directory authoritative, index derived.** | Every existing cache path keeps reading the directory unmodified; a stale row is a reconciliation problem, never a phantom the swarm could advertise. |
| 2026-08-06 | **Its own page, not a `/admin/library` lens.** | Those lenses curate content we publish under our sharing scopes. This is other people's content under `seed_cache`, and none of it is editable. |
| 2026-08-06 | **A `file-list.js` scope, not a new list.** | Infinite scroll, search, select-all-N, bulk bar and Play already exist there, over exactly the paged envelope this endpoint returns. |
| 2026-08-06 | **Abandoned partials are reaped by the system, not only by a button** — unconditionally at startup. | Owner call. A killed process cannot clean up after itself and nothing else swept them, so they were permanent dead disk. Startup needs no heuristic to be safe: a process that just started is writing nothing, so every `.part` is abandoned by definition. |
| 2026-08-06 | **Removal does not go through `federation.EvictCachedBlob`.** | The cache outlives federation being switched off, and must stay cleanable then. The handler unlinks from `cacheDir` itself — file first, index row second. |
| 2026-08-06 | **Play and Download read the cache directory directly**, not the madnetwork relay. | Same reason, found by running it: the relay only exists while a node runs, so with federation off every Play and Download 404'd — in exactly the situation where someone is here to reclaim disk. Materialize alone stays federation-gated, and the page hides it rather than offering a 404. |
| 2026-08-06 | **`file-list.js` gained `scope.sorts`** — a scope-supplied sort vocabulary. | "Least recently used" has no equivalent among the library's orders, and it is the sort that previews what a retention sweep would take first. Server-side only, so a scope using it must be paged. |
