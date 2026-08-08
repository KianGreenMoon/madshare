# Data sources & storages

Madshare separates **where bytes physically live** (a *storage*) from **where
library content was brought in from** (a *source*). This lets us reference an
existing on-disk music directory by **symlinks** — never copying — as a new
source that writes into a shared `links` storage, and leaves a clean seam for an
object store (S3) later.

It activates a column dormant since day one: `files.storage_backend` (migration
001, `TEXT NOT NULL DEFAULT 'local'`), carried through `database/models.go`,
every `files` query, and the upload path — but **always hardcoded `"local"`**. It
becomes an **origin hint** (see *Resolver* — serving no longer depends on it).

## Two concepts

**Storage** — a physical backend that holds bytes, content-addressed by hash:

| Storage | What | Written by | Count |
|---|---|---|---|
| `local` | Owned blobs under `files_dir/audio` (today). | uploads | one (config) |
| `links` | One shared dir of **symlinks** to originals; no copying. | symlink imports | one |
| `s3` *(future)* | Object store. **Designed-for, not built.** | — | — |

**Source** — a logical origin that *populates* a storage. Uploads are the
implicit source of `local`. A **symlink source** is an external directory you
scan in; **many symlink sources share the one `links` storage** (no per-source
directory — see *Layout*). Sources are recorded in `data_sources` for
listing/health.

The library is **content-addressed**: the `files` table is the catalog (one
logical row per hash). A hash may physically exist in **more than one storage**
(a duplicate) — allowed, not auto-managed. The **resolver** decides which copy to
serve by trying storages in a **fixed precedence** (`local` before `links`; see
*Storage precedence*).

> **Scope (YAGNI).** Build the seam; implement `local` + `links` only. The
> `Storage` interface is shaped so `s3` drops in, but no S3 code or plugin
> framework is written now.

## Decisions (agreed)

| Topic | Decision |
|---|---|
| Trigger | Admin page + API (`/admin/sources`); server-side path. |
| Link type | **Symlink** (cross-fs; breaks honestly if the original disappears). |
| Cover images | **Read-once-derive** — covers (sidecar + embedded) decode into owned/local **variants** at scan time; **no linked original kept** (decided; see `docs/architecture/variants.md`). |
| Path safety | Config allowlist `[sources].symlink_roots`. |
| Storages dir | **One shared `links` storage** (`links/audio/<hash>/…`), mirroring `files/`. No per-source dirs. |
| **Resolver** | Serve the first storage that physically has the hash, **`local` before `links`** (fixed precedence). A dangling link reads as absent → falls through (free resilience). |
| **Storage precedence** | **Fixed in v0** (`local` > `links`; uploads → `local` only; `links` never outranks `local`). Configurable priority/default is an **S3-era** feature → `docs/architecture/s3-storage.md`. |
| Upload of an existing hash | **Dedup** — no re-store (no need to upload what a storage already has). |
| Duplicates across storages | **Allowed, not auto-managed**; a manual reconcile knob comes later. Within `links`: one link per hash (don't overwrite). |
| Default data location | `data_dir` (db + files + links default under it); `database.path`/`files_dir` are overrides. |
| Review state | Land `approved` (admin behind allowlist); duplicates still get the recordings flag. |

## Physical layout (`data_dir`)

```
<data_dir>/                              data_dir (config; default "./data" = today's defaults)
  madshare.db                            database.path  (default <data_dir>/madshare.db)
  files/                                 storage.files_dir (default <data_dir>/files) = 'local' storage
    audio/   <hash>/<filename>           owned source blobs (audio)
    images/  <image_hash>/original<ext>  owned cover SOURCE originals — regenerate seeds, NEVER served
  links/                                 the single 'links' storage (shared by every symlink source)
    audio/   <hash>/<filename>  ->  /srv/music/album/song.flac    symlink
                                           (no links/images: covers are read-once-derived, owned)
  variants/                              owned DERIVED media — variants_dir (default <data_dir>/variants)
    images/  <image_hash>/<recipe>       owned cover VARIANTS — served at /images; ALL storages' covers derive here
    cache/                               evictable audio-variant tier (future; LRU-GC'd)
```

One `links/` dir, content-addressed by hash exactly like `files/`. Derived media
(cover variants today, audio variants later) lives under its own `variants/` tree,
**not** under `files/` — see `docs/architecture/variants.md` for that decision.
`data_dir` default `./data` reproduces the historical defaults; `database.path` /
`files_dir` / `variants_dir`, if set, override their derived value (nothing breaks
for existing installs).

## Database

Migration `021_data_sources.sql` (DB version → 21; breaks the `database_test.go`
version/table assertions and the `api` `fakeRepo`; new `Repository` methods break
`fakeRepo` too):

```sql
CREATE TABLE data_sources (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL CHECK (kind IN ('symlink')),   -- 's3' later
  name         TEXT NOT NULL,
  root_path    TEXT NOT NULL,                               -- the external dir referenced
  status       TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','scanning','error')),
  summary_json TEXT,                                        -- last scan: linked/skipped/failed counts
  created_at   INTEGER NOT NULL,
  scanned_at   INTEGER
);

-- files.storage_backend already exists (default 'local'); now an ORIGIN HINT
-- ('local' | 'links'), NOT authoritative for serving. Add the original-path
-- pointer for link rows:
ALTER TABLE files ADD COLUMN link_target TEXT;             -- abs path of the original; NULL for local

-- No cover-image storage columns: covers are read-once-derived into owned/local
-- variants (see the Cover images decision + variants.md), so every
-- album_images/artist_images row is implicitly local. The storage-aware image
-- columns (storage_backend / link_target on album_images + artist_images) are
-- parked, unapplied, in database/migrations/pending/image_storage_columns.sql in
-- case the "persist linked original" option (B) is ever adopted.
```

No `storage_order`/`storage_default` settings in v0 — precedence is the fixed
constant `local` > `links` (see *Storage precedence*; configurable ordering ships
with S3).

Migration `023_source_files.sql` (DB version → 23) adds **per-source file
attribution**, required so a source can be removed safely (drop a record only when
no source still references it — see *Removing a source*). It was deliberately
omitted in v0 (a shared links dir reported only a scan summary); it is added now
that delete/refresh exist:

```sql
-- Which files each source references. Many-to-many on purpose: the same hash
-- found under two source roots is attributed to BOTH (so removing one leaves
-- the record, owned by the other). ON DELETE CASCADE on both sides keeps it
-- self-cleaning — a hard-deleted file or a removed source takes its rows with it.
CREATE TABLE source_files (
  source_id TEXT    NOT NULL REFERENCES data_sources(id) ON DELETE CASCADE,
  file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  PRIMARY KEY (source_id, file_id)
);
CREATE INDEX idx_source_files_file ON source_files(file_id);  -- the "any other source?" probe
```

A scan attributes **every** accepted file it touches — newly linked, an existing
catalog row it reuses, **and** a hash already in the links storage it skips (the
skip path still records that *this* source references the hash). Attribution is
`INSERT OR IGNORE`, so a re-scan of the same tree is a no-op. Sources still carry
their **scan-time summary**; link health is still reported over the whole `links`
storage.

## Storages registry & the resolver

An in-memory `storages.Registry` is built at startup from config: `local` (rooted
at `files_dir`) and `links` (rooted at `<data_dir>/links`).

```go
type Storage interface {
    ID() string
    // Locate returns the on-disk path of the file for hash, and whether this
    // storage actually has it. local: a regular file in the hash dir. links:
    // a symlink whose target stats as a regular file (a DANGLING link → false).
    Locate(hash string) (path string, ok bool)
}
```

**v0 precedence is fixed — `local` before `links`** (see *Storage precedence*).
There are only two storages, one each, and `links` can never outrank `local`, so
there is nothing to order. The general, configurable priority-ordered probe — the
machinery that only earns its keep once a second interchangeable storage exists —
lives in `docs/architecture/s3-storage.md`; v0 is its degenerate case.

**Serving `/files/*` (resolving handler, replaces the static mount):**

1. `fileAccessGuard` already loads the `files` row for ACL; keep that (one lookup
   for access + the canonical name).
2. **Try `local`, then `links`:** call `Locate(hash)` on each; serve the first hit
   via `os.Open` + `http.ServeContent` (native HEAD/range; `links` follows the
   symlink to the original). ≤2 cheap stats. We open the blob ourselves rather
   than calling `http.ServeFile` because `ServeFile` re-opens the path through
   `http.Dir`, which **404s file names that are not valid UTF-8** — and a `links`
   import keeps the external original's raw on-disk filename bytes (e.g. a
   Latin-encoded `ł`). `os.Open` is byte-based, so those blobs still serve.
3. A dangling/missing copy returns `ok=false`, so resolution **falls through** to
   the next storage that has the hash — a duplicate elsewhere transparently covers
   a broken link. No hit anywhere → 404.

`hash` is regex-validated before any join (as today); a link may legitimately
resolve outside `data_dir` — that is the mechanism, gated at *creation*.
`storage_backend` is only an origin hint; serving does not depend on it.

`/images/*` is **unchanged** — a static `http.FileServer` rooted at the owned
cover-variant tree (`files_dir/images` today; `variants/images` after the
variants-dir relocation — see `docs/architecture/variants.md`), since all variants
are owned/local regardless of storage.

## Ingest & duplicates

The library is content-addressed; what creates/locates a copy:

- **Upload** → `local`. If the hash already exists in **any** storage, **dedup**
  (no bytes stored). So an upload never creates a redundant big-byte copy.
- **Symlink import** → add a link in the `links` storage for each scanned file,
  **unless `links` already has that hash** (don't overwrite). It does not consult
  other storages, so a file that is also a `local` blob may end up duplicated
  across `local` + `links` — **allowed**; the resolver serves the `local` copy
  (`local` always precedes `links`), and the link is a tiny, resilient fallback.
- **Duplicates across storages** are never removed automatically. A deliberate
  (likely manual) reconcile tool is future work.

### Known behaviour (accepted)

A file uploaded when it exists **only** as a link is bound to that link (dedup,
no local copy). If the external dir later disappears the link dangles, nothing
else has the hash, and it 404s — accepted. A future **"adopt / materialize"**
action could copy a link's bytes into `local` on demand.

## Storage precedence — fixed in v0

With exactly two storages, one each, precedence is a constant, not a setting:

- **`local` always precedes `links`.** `links` can never outrank `local`.
- **Uploads only ever go to `local`.** `links` receives imports only — never an
  upload.
- So the resolver order is hardwired `[local, links]`; there is **no reorder or
  set-default control in v0**, and no `storage_order`/`storage_default` settings.

Configurable priority, a selectable upload default, an S3 cache that may be raised
above `local`, and ordering among multiple object stores only become meaningful
with a second uploadable/interchangeable storage. That logic — and why the rules
look convoluted — lives in `docs/architecture/s3-storage.md`, deferred with S3
itself.

## Adding & managing a source

Endpoints under `/api/admin/sources*` — admin group (`file.delete`) + gated
`content.moderate`.

- **`POST /api/admin/sources`** `{ kind:"symlink", name, root }` — validate `root`
  is absolute, `Clean`-stable, and under a `[sources].symlink_roots` entry
  (resolving symlinks in the input first); else 403. Insert `status='scanning'`,
  then **scan** (prune-job-style worker): walk `root`, pick `acceptedAudioTypes`
  extensions, hash, **skip if `links` already has the hash**, else `os.Symlink`
  into `links/audio/<hash>/<name>`, insert/locate the `files` row
  (`storage_backend='links'`, `link_target`, `uploaded_by`; if a row already
  exists for the hash, reuse it — content-addressed catalog) via `InsertFile`,
  which bundles it with its recording and an **approved appearance**
  (`tagsets.review_state='approved'` — review/trash live on the tagset since
  migration 024, `recording-tagsets.md`; symlink imports land approved because
  they come from an admin behind the allow-list, not the public staging flow)
  and the tech `media_metadata` row in one transaction, then `file_uploads`,
  enqueue analysis, resolve the recording. Covers (read-once-derive): decode the cover — sidecar `cover.jpg` or
  embedded picture — once into owned variants under the cover-variant tree
  (`variants/images/<image_hash>/`; see `docs/architecture/variants.md`) and attach
  the album image (owned/`local`); **no `links/images` symlink is kept**. Record a
  scan summary; `status='active'`.
- **`GET /api/admin/sources`** — list the symlink sources (root, scan summary,
  external bytes) and `links`-storage health (broken-link count), plus the `local`
  store summary. No reorder/default control (v0 precedence is fixed).
- **`POST /api/admin/sources/{id}/rescan`** — *Refresh.* Re-walk the source's
  existing root (re-validated against `symlink_roots`, in case the allow-list
  changed) and re-run the same scan, reusing the existing `data_sources` row
  (status → `scanning`, then a fresh summary). **Additive only:** it links newly
  appeared files and re-affirms attribution; it never removes files that have
  *disappeared* from the root (a temporarily-unmounted drive must not trigger a
  mass delete). Vanished originals surface as broken links on the Prune page; the
  explicit Remove action below is how you delete. Reuses the single scan slot
  (409 `busy` if a scan/removal is already running).
- **`GET /api/admin/sources/{id}/removal-preview`** — dry-run for Remove: returns
  `{ will_remove, will_keep }` so the UI can confirm before destroying anything
  (mirrors the album-merge preview).
- **`DELETE /api/admin/sources/{id}`** — *Remove,* relation-aware (see *Removing a
  source*). Drops the source and, for each file it **exclusively** references
  (no other source, and not an owned `local` blob), unlinks the symlink and
  removes the catalog row; shared or locally-owned records are kept. Returns the
  removed/kept counts. Reuses the single scan slot so it can never race a scan.

Recordings/duplicates run unchanged through the link (see
`docs/architecture/recordings.md`).

## Configuration

```toml
data_dir = "./data"          # default root for db + files + links + variants; "./data" = today's defaults

[database]
#path = "./data/madshare.db" # defaults to <data_dir>/madshare.db

[storage]
#files_dir    = "./data/files"     # defaults to <data_dir>/files (source blobs)
#variants_dir = "./data/variants"  # defaults to <data_dir>/variants (derived media; see variants.md)
# … existing storage keys unchanged …

[sources]
# Roots a 'symlink' source may reference. Empty (default) = symlink sources disabled
# (the /admin/sources Add form is hidden; the local storage still works).
symlink_roots = ["/srv/music", "/mnt/nas/audio"]
# allow_any = true drops the allow-list entirely — for an EMBEDDED madshare with
# no listener (docs/architecture/embedding.md), never for one that serves.
```

`config.Load`: derive effective `database.path` / `files_dir` / `variants_dir`
from `data_dir` unless overridden; the `links` storage root is `<data_dir>/links`;
validate `data_dir` non-empty and each `symlink_roots` entry absolute +
`Clean`-stable; a non-existent root is a `cfg.Warnings()` advisory; empty
`symlink_roots` disables the `symlink` kind. (`variants_dir` derivation arrives
with the variants-dir relocation — see `docs/architecture/variants.md`.)

`symlink_roots` is **deploy-time TOML and intentionally not UI-editable** (it is a
trust boundary — see `docs/architecture/configuration.md`). Symlink *sources*,
which live within the allow-list, are DB-backed (`data_sources`) and added from
the UI, so no TOML write-back is needed for this feature.

**`allow_any = true` drops the allow-list**, for the one deployment where it
guards nothing: an **embedded** madshare with no HTTP surface, whose owner is at
the keyboard (a native player — `docs/architecture/embedding.md`,
`docs/ui/madplayer.md`). What the allow-list actually protects is the fact that
the surface adding a source is *reachable*: without it, an admin session can
symlink `/etc/shadow` into the library and read it back through `/files/`. With no
listener there is nothing to reach and nothing to protect. Combining it with a
listener — or with `symlink_roots`, which it then ignores — warns at startup, and
`/admin/sources`' Add form still offers only configured roots, so the web surface
stays constrained even when the manager is not.

## Lifecycle & safety

> **INVARIANT — Madshare never touches a file it does not own.** Nothing in any
> code path may write, move, truncate, or delete a file under a symlink source's
> external directory. Madshare only ever removes the **symlink** that lives inside
> `<data_dir>/links/`. A linked file deleted in the admin panel removes our link
> and leaves the original exactly where it was. This is a tested guarantee, not a
> convention.

Every destructive path is storage-aware:

- **Admin hard delete** of a `links` copy → `os.Remove(<link path>)` only.
  `os.Remove` on a symlink unlinks the link itself, never following it to the
  target — that is precisely the behaviour we want. It is a kind-aware sibling of
  `storage.DeleteAll`; the generic `DeleteAll` (`os.RemoveAll` of a hash dir) is
  reserved for `local` blobs and must **never** be invoked on a path that resolves
  through a link or sits outside `<data_dir>`. The hash dir under `links/audio/`
  is a real directory we created, and the symlink is the *file* inside it, so even
  removing the hash dir unlinks only the symlink — but the delete path operates on
  the link, by storage kind, deliberately.
- **Prune broken-link detection** over the `links` storage: **dangling** (target
  gone), **retargeted** (resolves but ≠ `link_target`), **corrupt** (deep mode:
  target no longer hashes to `files.hash`). Each reported; the target is never
  touched. The prune summary distinguishes *missing blob* (local) from *broken
  link* (links); health surfaces on `/admin/sources`.
- **Derived cover variants.** Cover variants are owned/local and shared by
  `image_hash` across albums, so they are reclaimed by the **reference-counted**
  image reconcile (`ReconcileImageOrphans`) when no `album_images`/`artist_images`
  row references the key — never inline on a single file delete. Because covers are
  **read-once-derived** (no linked original kept — see *Cover images* and
  `docs/architecture/variants.md`), there is a **single** owned cleanup location
  per tree (`variants/images/<image_hash>/` + the source seed
  `files/images/<image_hash>/`, `os.RemoveAll`, ours) and **no `links/images` tree**
  to sweep, so the existing reconcile needs no change for linked imports.
  Full variant-storage design (incl. future audio variants):
  `docs/architecture/variants.md`.

### Removing a source

Backed by the `source_files` attribution table (migration 023). Removing a source
must never destroy a record another source — or a local upload — still relies on:

1. **Exclusive set.** Find the files referenced by *this* source and by **no
   other** source (`NOT EXISTS` over `source_files`). This is content-addressed by
   the `files` row, so a hash imported from two roots is excluded (kept).
2. **Per file in the set** — unlink the **symlink** for its hash (only-this-source,
   so the one link-per-hash is safe to drop; never touches the external target).
   Then: if the catalog row is a `links` row, hard-delete it (**removed**); if it
   is an owned `local` blob (a prior upload the scan merely re-linked), keep the
   row — only the stray link is reclaimed (**kept**). The local `DeleteAll` is
   never invoked, upholding the never-touch-what-we-don't-own invariant.
3. **Delete the `data_sources` row.** Its remaining `source_files` rows (the shared
   / locally-owned files we kept) cascade away; those records stay, now attributed
   only to their other source(s).

Removal reserves the **single scan slot** (409 `busy` while a scan/removal runs),
so attribution can't shift underneath it. The `GET …/removal-preview` endpoint
runs step 1 read-only to report `{will_remove, will_keep}` for a confirm dialog.

**Legacy backfill.** Sources created before migration 023 have no `source_files`
rows. At startup, `Manager.BackfillAttribution` attributes each such source's
linked files by matching `files.link_target` under the source's (symlink-resolved)
`root_path` — recovering single-source attribution so Remove works without a
forced rescan first. It is idempotent and skipped once a source has any rows. (A
hash that earlier scans *skipped* under a second root was never recorded anywhere;
a Refresh of that source re-attributes it via the skip path.)

## Accounting

`storageStats` reports per **storage**:

- `local` audio/review/trash byte sums come from the DB (`byte_size`) as today;
- `links` (**external**) bytes come from **walking the `links` storage**
  (stat-through-symlink sizes), shown separately and **not** added to on-disk
  `LibraryBytes`. A hash duplicated in both `local` and `links` shows in both
  figures — accurate to physical reality (a real blob plus a tiny reference).
  Importing 200 GB in-place adds 0 to the on-disk footprint.

## UI

`/admin/sources` ("Data sources", admin shell — built, P6): an **Imported
directories** section listing each symlink source (name, root, status badge, scan
summary, last-scan date), an **Add symlink source** form constrained to
`symlink_roots` (root dropdown + optional subfolder; hidden when none), scan
progress (polling `GET /api/admin/sources` while any source is `scanning`),
per-source **Refresh** (rescan, additive) and **Remove** (relation-aware delete,
gated behind a `removal-preview` confirm showing the will-remove/keep counts), and
a **Links storage** health line (count / external bytes / broken-link count); the
broken-link *detail* (which hashes, with reasons) lives on the Prune page, linked
from the health line. The dashboard's storage card shows external (linked) bytes
separately and a "scanning" badge. **No reorderable storage list in v0**: `links`
is not presented as a peer storage device, it *is* the imports section (the
reorderable storage view is an S3-era addition — see `s3-storage.md`). Reuses the
admin shell, `shared.js`, `toast.js`. Moderator-gated client-side
(`content.moderate`). `-tags nowebui`: page compiles out, API stays.

## Phasing

- **P0 — `data_dir`** config + derivation + overrides + validation. No behaviour
  change (default `./data` = today).
- **P1 — registry + schema.** Migration 021, `data_sources`, the `storages.Registry`
  (`local` + `links`) in the fixed precedence `local` > `links` (no settings —
  see *Storage precedence*). Only `local` populated → no behaviour change.
- **P2 — resolving `/files` handler** (try `local` then `links`; links-aware
  `Locate`; fall-through). `/images` unchanged. Regression-test HEAD/range.
- **P3 — `links` storage + symlink source. _(done)_** Shared links dir,
  symlink/kind-aware storage helpers (`storages.Linker` — `Has`/`Link`/`Remove`,
  link-only, never touches the target), `[sources].symlink_roots` allow-list,
  `data_sources` CRUD + `files.link_target` (origin hint `storage_backend='links'`),
  the `sources.Manager` scan engine (one scan at a time; walk/hash/skip-if-in-links/
  symlink/insert + tags + analysis + recordings; lands `approved`), and
  `GET/POST /api/admin/sources` (`content.moderate`; 503 disabled / 403 root not
  allowed / 409 busy). **No covers yet** (P4): a file's embedded/sidecar art is
  ignored at scan time. **No admin-delete storage-awareness yet:** a links row
  hard-deleted by an admin removes only its catalog row (the local-rooted
  `DeleteAll` can't reach the external original — the safety invariant holds), but
  leaves the symlink as a dangling orphan in `links/` for the future reclaim tool.
- **P3.5 — `variants/` directory relocation** *(pre-P4; independent of the links
  work — can land anytime before P4, but P4 depends on it).* Add `variants_dir`
  (derive from `data_dir`, default `<data_dir>/variants`, overridable); point the
  two image-dir derivation sites (`app.Start` reconcile + imageproc; `api/api.go`
  `/images/*` mount + cover read/write) at `variants/images`; one idempotent
  startup relocation of an existing `files/images` → `variants/images` (à la
  `storage.RelocateLegacyBlobs`). The `/images/*` URL is unchanged. Also rename the
  upload `CacheDir` → `SpoolDir`. Doing this **before P4** means P4 derives covers
  straight into the final location and fresh installs never put images under
  `files/`. Full design: `docs/architecture/variants.md`.
- **P4 — covers (read-once-derive). _(done)_** During a scan, each newly linked
  file's cover — a sidecar `cover/folder/front.{jpg,jpeg,png}` in its directory
  (preferred) or the embedded picture — is decoded once into an owned source
  original under `<files_dir>/images/<image_hash>/original<ext>` and its album
  cover filled if absent (`SetAlbumCoverIfAbsent`), with a variant job enqueued on
  the shared cover-variant pool (variants land in `variants/images`). Covers are
  always **owned/local** — there is no `links/images` tree, so the existing image
  reconcile is unchanged. Enabled via `Manager.WithCovers`; opt-out (unset) keeps
  the P3 no-cover behaviour. Capped at 10 MB like the upload path. See *Cover
  images* and `docs/architecture/variants.md`.
- **P5 — prune broken-link detection + per-storage accounting + storage-aware
  delete. _(done)_** The prune scan is storage-aware: each `files` row is probed
  against the storage its `storage_backend` names — a local blob (missing/corrupt,
  as before) or a links symlink (`dangling` target gone / `retargeted` ≠
  `link_target` / deep `corrupt` target no longer hashes), classified via
  `storages.Linker` and reported with its reason; a confirmed prune **unlinks the
  symlink only** (never the external target). Admin hard-delete is likewise
  storage-aware (`reclaimStorage`): a links row is unlinked, never `RemoveAll`'d.
  Accounting: `storageStats` reports `external_bytes` (the links storage walked
  stat-through-symlink) **separately** from `library_bytes`, which now excludes
  links rows (`storage_backend <> 'links'`) — importing in place adds 0 on disk.
  `GET /api/admin/sources` carries links health (`count` / `broken` / external
  bytes). The external original is never touched in any path.
- **P6 — `/admin/sources` page. _(done)_** Admin-shell page ("Data sources" nav
  link + dashboard card): an **Add symlink source** form constrained to
  `symlink_roots` (a root dropdown + optional subfolder; hidden when none
  configured), an **Imported directories** list (name, root, status badge,
  scan summary, last-scan date) that polls `GET /api/admin/sources` while any
  source is scanning, and a **Links storage** health line (count / external bytes
  / broken-link count, linking to Prune when broken). The dashboard's storage card
  also shows external (linked) bytes separately and a "scanning" badge. No
  reorderable storage list (that ships with S3). Moderator-gated client-side
  (`content.moderate`); compiles out under `-tags nowebui` (API stays).
- **P7 — source refresh + relation-aware remove. _(done)_** Migration 023
  (`source_files` attribution; DB → 23). The scan engine attributes every file it
  touches (link/reuse/skip). `Manager.Rescan` re-walks an existing source
  additively (reusing its row); `Manager.Remove` drops a source and, via the
  exclusive set, unlinks + hard-deletes only the records no other source/local
  blob relies on (shared/local kept), with a `removal-preview` dry-run;
  `Manager.BackfillAttribution` heals pre-023 sources at startup. Endpoints
  `POST …/{id}/rescan`, `GET …/{id}/removal-preview`, `DELETE …/{id}`
  (`content.moderate`; reuse the single scan slot → 409 busy). The `/admin/sources`
  page gains per-source **Refresh** and **Remove** (confirm with counts).
- **Future** — *full two-way* sync/rescan (auto-remove disappeared originals with
  an empty-root guard); duplicate-reconcile tool; the `s3` storage + configurable
  precedence (`docs/architecture/s3-storage.md`); "adopt/materialize" a link into
  `local`.

## Open questions

1. **Source deletion / reclaim. _(DONE — P7.)_** Resolved by the `source_files`
   attribution table: Remove is relation-aware (drop a record only when no other
   source — or local upload — still references it). See *Removing a source*.
2. **Two-way sync (`rescan`). _(Refresh DONE — P7; full sync deferred.)_** Refresh
   re-scans **additively** (links new files, re-affirms attribution; never removes
   vanished originals — that stays Prune's job, to avoid an unmounted-drive mass
   delete). A true two-way sync that auto-removes disappeared files (with an
   empty/unreadable-root guard) is still future.
3. **`s3` storage** — the configurable storage priority/default, S3-as-cache, and
   multi-store ordering are designed in `docs/architecture/s3-storage.md` (future,
   not built). **adopt/materialize** a link into `local` — also future.
4. **Image-original persistence (DECIDED — read-once-derive).** Covers (sidecar +
   embedded) decode once into owned/local variants; no linked original is kept, so
   there is no `links/images` tree and a single cleanup location. Rationale in
   `docs/architecture/variants.md`. The storage-aware `album_images`/`artist_images`
   columns are therefore left out of migration 021 and parked, unapplied, in
   `database/migrations/pending/image_storage_columns.sql` for the (B) contingency.
5. **UI config-editing model (PARKED — to discuss).** DB `settings` vs a live TOML
   editor. Not a v0 blocker (sources are DB-backed, the allow-list is operator-only
   TOML). Framed in `docs/architecture/configuration.md`.
