# Storage backends & capacity reporting

Uploaded blobs are content-addressed and persisted through the `storage.Storage`
interface (`api/storage/storage.go`). Today there is one implementation,
`storage.Local` (`api/storage/local.go`), which lays files out as
`<files_dir>/audio/<hash>/<filename>` — under an `audio/` subdir
(`storage.AudioSubdir`) so the served blob tree is a sibling of, not a parent
of, the cover-images tree (`<files_dir>/images`); the `/files` server therefore
cannot reach images. The interface is the seam for a future object store (S3);
everything below is written so that backend drops in without changing callers.

> **Planned (roadmap):** the cover-images tree relocates out of `<files_dir>` into
> a dedicated owned `variants/` directory (`<variants_dir>/images`), so `files/`
> holds only source blobs and all derived media lives under `variants/`. See
> `docs/architecture/variants.md`. Not yet implemented.

On startup the server relocates any pre-split blobs (hash dirs sitting directly
under `<files_dir>`) into `audio/` via `storage.RelocateLegacyBlobs` — a
one-time, idempotent migration.

## The `Storage` interface

`Put` / `Exists` / `DeleteAll` / `BlobPresent` / `VerifyBlob` cover the
content lifecycle (see the interface doc comments). Capacity reporting is one
more method:

```go
Stats() (Stats, error)
```

`Stats` is deliberately backend-neutral:

```go
type Stats struct {
    Backend    string // "local" today, "s3" later
    Location   string // base dir (local) or bucket (object store)
    HasVolume  bool   // true when the byte fields reflect a real filesystem
    TotalBytes uint64 // filesystem capacity      (valid only when HasVolume)
    FreeBytes  uint64 // available to an unprivileged user, df "Avail"
    UsedBytes  uint64 // df-style used: capacity − all free blocks
}
```

- **Local disk** fills the byte fields from a `statfs(2)` of its base directory
  and sets `HasVolume=true`. The `statfs` call is OS-specific, so it lives in
  build-tagged files: `diskusage_unix.go` (`//go:build unix`, the real
  implementation) and `diskusage_other.go` (`//go:build !unix`, returns
  `errNoStatfs` → `Stats` reports `HasVolume=false` instead of failing). The
  deployment target is Linux, so the unix path is what runs.
- **Object stores (future S3)** have no fixed capacity. Such a backend sets
  `HasVolume=false` and leaves the byte fields zero. The app's *own* footprint
  is then the only meaningful figure, and it comes from the database, not the
  backend (next section).

## App footprint vs. volume

Two different numbers, sourced separately:

- **Volume** (whole disk): from `Storage.Stats()`. Local only.
- **Library footprint, by category**: a per-category size — `audio`, `review`,
  `trash`, `images` (and future `video`) — summing to `library_bytes`. This
  figure is backend-agnostic: it is the meaningful capacity number for an object
  store too.

  Sizing is **hybrid**, because the categories have different cheapest truthful
  sources:

  - **audio / review / trash** come from the DB in one query:
    `Repository.StorageByteBreakdown` (`database/files.go`) — a single
    `SUM(byte_size)` over the files table, partitioned by state with `CASE`:
    - `audio` = approved & not soft-deleted (the live library);
    - `review` = not deleted, `review_state <> 'approved'` (staged uploads,
      `docs/architecture/moderation.md`);
    - `trash` = soft-deleted, awaiting prune.

    The state (approved / review / trash) is read from each file's
    **representative appearance** — its `tagsets` row (primary, else oldest),
    since review/trash moved onto the tagset in migration 024
    (`docs/architecture/recording-tagsets.md`); the query joins that appearance
    via `reprTagset`, so a blob carrying several appearances still counts once.
    The three are **mutually exclusive** (trash takes precedence over review
    state) and together equal the whole files-table byte total, so they never
    double-count. Files are content-addressed (one row per hash), so each bucket
    is a deduplicated blob total. It is an **indexed sum: instant and always
    fresh**, so these figures need no caching. (They do *not* count un-pruned
    orphan audio blobs that exist on disk without a row — those are the Verify &
    Prune view's job, `docs/architecture/prune-job.md`.)
  - **images** (and future video) are **walked on disk** with `storage.DirSize`
    (`api/storage/dirsize.go`), because cover images have **no byte-size column**
    in the DB (the 8 variants per cover are derived files). The image set is small
    (few files), so the walk is cheap and is **not cached** — it re-reads the tree
    on every request. The walk is a **best-effort advisory** measure, deliberately
    tolerant of the tree being mutated under it (the image pool writes variants;
    prune/delete removes them, concurrently with a dashboard load): a missing
    subtree (a fresh install has no `images/` yet) reads as zero; an entry that
    vanishes mid-walk, or an unreadable subdir, is **skipped** rather than failing
    the request — so a transient race can momentarily undercount but never 500s
    the panel or collapses the total. Only a structural error (a path component
    that is a regular file, `ENOTDIR`) is surfaced as an error.

  **Sizes are logical, not filesystem-allocated.** Both sources report logical
  bytes — `SUM(byte_size)` and `DirSize`'s `stat` `st_size` — i.e. the apparent
  file size, *not* the compressed on-disk allocation (`du` without `--apparent-size`,
  `st_blocks × 512`). Filesystem-compression-aware sizing is **intentionally out
  of scope**: the payload is already-compressed media (MP3/FLAC/Opus/AAC) and
  images (JPEG/PNG), so transparent FS compression (ZFS/Btrfs) saves negligibly,
  and switching to allocated sizing would mean platform-specific stat-block
  accounting *and* abandoning the instant DB sum for audio — not worth it for this
  workload.

## Admin endpoint

`GET /api/admin/storage` (gated `file.delete`, matching the other
storage-management routes) merges the volume with the per-category breakdown:

```json
{
  "backend": "local",
  "location": "./data/files",
  "library_bytes": 12345678,
  "categories": [
    { "name": "audio",  "bytes": 11000000 },
    { "name": "review", "bytes": 0 },
    { "name": "trash",  "bytes": 0 },
    { "name": "images", "bytes": 1345678 }
  ],
  "volume": { "total_bytes": N, "free_bytes": N, "used_bytes": N, "used_percent": 42.0 }
}
```

`location` is the `files_dir` root (parent of the subtrees). `volume` is `null`
when the backend has no fixed capacity (the S3 shape); the admin dashboard then
drops the disk meter and shows only the per-category breakdown with a "no fixed
capacity" note. The dashboard (`webui/static/js/admin/dashboard.js`, panel in
`webui/html/admin/dashboard.html`) renders a usage bar with one colored segment
per category, then a rest-of-disk-used segment, plus "X free of Y"; the category
list is rendered **generically** (colors + display labels keyed by name, e.g.
`review` → "On review", `trash` → "In trash"), so a future `video` category
appears automatically once the server reports it.

## Deferred: split block devices / multi-volume storage

Today every category is a **sibling subtree of one `files_dir`** on a single
filesystem. That assumption is load-bearing: the volume figure comes from one
`statfs` of `files_dir`, and every category's bytes (the DB audio sum, the image
walk) describe space on that same disk. The whole-disk meter is therefore
meaningful because all categories share one disk.

A future deployment may want `audio/`, `images/`, and especially `video/` (large
files) on **separate disks / block devices** — e.g. bulk audio on a big spinning
volume, hot images on SSD. This is **deferred** (not implemented): there is no
real benefit until video makes physical separation worthwhile, and the single
`files_dir` layout is simpler and sufficient now.

What it would take when the need arises (recorded here so the current design
doesn't quietly assume one disk forever):

- **Per-category directory overrides** in config — e.g. an optional `dir` per
  category (`[storage.video].dir = "/mnt/bulk/video"`), defaulting to
  `<files_dir>/<category>` when unset, so categories can live on different mounts.
- **Per-volume capacity reporting**: with categories on different filesystems, a
  single whole-disk `statfs` no longer describes "the storage". `Storage.Stats`
  (or its caller) would report one volume per **distinct** mount (a `statfs` per
  category directory, de-duplicated by device), instead of one volume total.
- **UI**: group the per-category breakdown under its backing volume, with a
  free/used meter per volume rather than one global meter.

The per-category breakdown built now is the stepping stone: usage is already
sourced per category, so splitting those subtrees across devices later changes
only where each category lives and how many volumes are reported — not the shape
of the data.
