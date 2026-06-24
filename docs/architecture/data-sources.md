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
| Cover images | Link sidecar covers too; resized **variants stay owned/local**. |
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
    audio/   <hash>/<filename>           owned blobs
    images/  <base_key>/<variant>        owned cover variants — ALL storages' variants live here
  links/                                 the single 'links' storage (shared by every symlink source)
    audio/   <hash>/<filename>  ->  /srv/music/album/song.flac    symlink
    images/  <base_key>/<orig>  ->  /srv/music/album/cover.jpg    symlink (cover regenerate-source)
```

One `links/` dir, content-addressed by hash exactly like `files/`. `data_dir`
default `./data` reproduces the historical defaults; `database.path` / `files_dir`,
if set, override their derived value (nothing breaks for existing installs).

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

-- Cover-image entities: which storage the linked ORIGINAL came from (variants stay local).
ALTER TABLE album_images  ADD COLUMN storage_backend TEXT NOT NULL DEFAULT 'local';
ALTER TABLE album_images  ADD COLUMN link_target      TEXT;
ALTER TABLE artist_images ADD COLUMN storage_backend TEXT NOT NULL DEFAULT 'local';
ALTER TABLE artist_images ADD COLUMN link_target      TEXT;
```

No `storage_order`/`storage_default` settings in v0 — precedence is the fixed
constant `local` > `links` (see *Storage precedence*; configurable ordering ships
with S3). No per-file `source_id`: with a shared links dir, per-source file
attribution is not tracked in v0 (sources show their **scan-time summary**; link
health is reported over the whole `links` storage).

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
   via `http.ServeFile` (native HEAD/range; `links` follows the symlink to the
   original). ≤2 cheap stats.
3. A dangling/missing copy returns `ok=false`, so resolution **falls through** to
   the next storage that has the hash — a duplicate elsewhere transparently covers
   a broken link. No hit anywhere → 404.

`hash` is regex-validated before any join (as today); a link may legitimately
resolve outside `data_dir` — that is the mechanism, gated at *creation*.
`storage_backend` is only an origin hint; serving does not depend on it.

`/images/*` is **unchanged** — a static `http.FileServer` rooted at
`files_dir/images`, since all variants are owned/local regardless of storage.

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
  (`storage_backend='links'`, `link_target`, `review_state='approved'`,
  `uploaded_by`; if a row already exists for the hash, reuse it — content-addressed
  catalog), `file_uploads`, `media_metadata`, enqueue analysis, resolve the
  recording. Sidecar covers: symlink the original into `links/images/<base_key>/`,
  generate variants into the **local** `files_dir/images/<base_key>/`, attach the
  album image (`storage_backend='links'`). Record a scan summary; `status='active'`.
- **`GET /api/admin/sources`** — list the symlink sources (root, scan summary,
  external bytes) and `links`-storage health (broken-link count), plus the `local`
  store summary. No reorder/default control (v0 precedence is fixed).
- **`DELETE /api/admin/sources/{id}`** — *deferred / best-effort* (delete flagged
  "later, not necessary"): mark the source inactive / forget the record;
  reclaiming shared links is a later **manual** tool since attribution isn't
  tracked.

Recordings/duplicates run unchanged through the link (see
`docs/architecture/recordings.md`).

## Configuration

```toml
data_dir = "./data"          # default root for db + files + links; "./data" = today's defaults

[database]
#path = "./data/madshare.db" # defaults to <data_dir>/madshare.db

[storage]
#files_dir = "./data/files"  # defaults to <data_dir>/files
# … existing storage keys unchanged …

[sources]
# Roots a 'symlink' source may reference. Empty (default) = symlink sources disabled
# (the /admin/sources Add form is hidden; the local storage still works).
symlink_roots = ["/srv/music", "/mnt/nas/audio"]
```

`config.Load`: derive effective `database.path` / `files_dir` from `data_dir`
unless overridden; the `links` storage root is `<data_dir>/links`; validate
`data_dir` non-empty and each `symlink_roots` entry absolute + `Clean`-stable; a
non-existent root is a `cfg.Warnings()` advisory; empty `symlink_roots` disables
the `symlink` kind.

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
- **Derived cover variants & linked-image originals.** Cover variants are
  owned/local and shared by `base_key` across albums, so they are reclaimed by the
  **reference-counted** image reconcile (`ReconcileImageOrphans`) when no
  `album_images`/`artist_images` row references the key — never inline on a single
  file delete. For a **linked** cover this spans two places: the owned variant dir
  `files_dir/images/<base_key>/` (`os.RemoveAll`, ours) **and** the linked-original
  symlink `<data_dir>/links/images/<base_key>/` (`os.Remove` the symlink only —
  **never the external original**). **v0 work:** extend the reconcile to also sweep
  `links/images`. Full variant-storage design (incl. future audio variants):
  `docs/architecture/variants.md`.

## Accounting

`storageStats` reports per **storage**:

- `local` audio/review/trash byte sums come from the DB (`byte_size`) as today;
- `links` (**external**) bytes come from **walking the `links` storage**
  (stat-through-symlink sizes), shown separately and **not** added to on-disk
  `LibraryBytes`. A hash duplicated in both `local` and `links` shows in both
  figures — accurate to physical reality (a real blob plus a tiny reference).
  Importing 200 GB in-place adds 0 to the on-disk footprint.

## UI

`/admin/sources` ("Data sources", admin shell): an **Imported directories**
section listing each symlink source (root, scan summary, external bytes, health
badge), an **Add symlink source** form constrained to `symlink_roots` (hidden if
none), scan progress (prune-status polling), and a broken-links detail view —
plus a read-only `local` store summary. **No reorderable storage list in v0**:
`links` is not presented as a peer storage device, it *is* the imports section
(the reorderable storage view is an S3-era addition — see `s3-storage.md`). Reuses
the admin shell, `toast.js`, shared player. `-tags nowebui`: page compiles out,
API stays.

## Phasing

- **P0 — `data_dir`** config + derivation + overrides + validation. No behaviour
  change (default `./data` = today).
- **P1 — registry + schema.** Migration 021, `data_sources`, the `storages.Registry`
  (`local` + `links`) ordered from `settings`. Only `local` populated → no
  behaviour change.
- **P2 — resolving `/files` handler** (try `local` then `links`; links-aware
  `Locate`; fall-through). `/images` unchanged. Regression-test HEAD/range.
- **P3 — `links` storage + symlink source.** Shared links dir, symlink/kind-aware
  storage helpers, `POST/GET /api/admin/sources` + scan engine (walk/hash/skip-if-
  in-links/symlink/insert + tags + analysis + recordings).
- **P4 — sidecar covers** + extend the image reconcile to sweep `links/images`
  (`os.Remove` the unreferenced linked-original symlink — see *Lifecycle & safety*
  and `docs/architecture/variants.md`).
- **P5 — prune broken-link detection + per-storage accounting.**
- **P6 — `/admin/sources` page** (imports section + add/scan/health; no storage
  reorder UI — that ships with S3).
- **Future** — source delete / duplicate-reconcile tool; two-way sync/rescan; the
  `s3` storage + configurable precedence (`docs/architecture/s3-storage.md`);
  "adopt/materialize" a link into `local`.

## Open questions

1. **Source deletion / reclaim.** Shared links dir + no per-file attribution → a
   deliberate (likely manual) tool; deferred.
2. **Two-way sync (`rescan`).** v0 is one-shot; sync needs a removed-original
   policy.
3. **`s3` storage** — the configurable storage priority/default, S3-as-cache, and
   multi-store ordering are designed in `docs/architecture/s3-storage.md` (future,
   not built). **adopt/materialize** a link into `local` — also future.
