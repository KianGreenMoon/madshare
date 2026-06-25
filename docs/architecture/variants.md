# Variants (derived media) — DRAFT / INCOMPLETE

> **STATUS: partial.** The image half, and the storage **layout** (a dedicated
> `variants/` directory — *decided* below), are settled; the audio-variant
> **engine** (transcode recipes, the LRU cache, eviction policy) is still an early
> sketch and is deliberately under-specified. Do not implement the audio engine
> from this doc. The layout decision and the `CacheDir`→`SpoolDir` rename are a
> **roadmap** — recorded here, **not yet implemented**. See
> `docs/architecture/data-sources.md` for storages.

A **variant** is a *derived* representation of a source file — a resized cover
image today, a transcoded/down-bitrate audio stream tomorrow. Variants are not
catalog content of their own; they are computed from a source blob and a recipe.

## Principles (the part I'm confident about)

1. **A variant is always *owned*.** It is produced by Madshare, so it lives in an
   owned location — the `variants/` tree (permanent) or its `variants/cache/`
   subtree (evictable) — **never** in a source storage (`links`, future `s3`). The
   *source* may be anywhere (a local blob or a linked original); the *derivative*
   is always ours. This is already true for images and should stay the rule.

2. **A variant is reproducible, so persistence is a *policy*, not a requirement.**
   Given the source + the recipe, any variant can be regenerated; losing one is a
   recompute, not data loss. That unlocks two storage modes, which the layout below
   makes physical:
   - **Materialized / permanent** — kept under `variants/`, reclaimed by
     reference-counted GC. Right for *cheap, small, hot* derivatives (cover
     variants).
   - **Cache / ephemeral** — kept under `variants/cache/`, regenerated on demand,
     evicted under pressure (LRU). Right for *expensive, large* derivatives (audio
     transcodes).
   The mode is a property of the variant *type* (and may be admin-configurable).

3. **Cleanup is reference-counted, never inline.** A variant is keyed by content
   (`base_key` for images; a source-hash+recipe key for audio) and is **shared**
   across catalog entries (many albums, one cover). A single file delete therefore
   cannot safely remove a variant — another entry may still use it. Removal is a
   GC sweep that drops a variant only when nothing references its key.

## Storage layout — a dedicated `variants/` directory (DECIDED, roadmap)

All derived media — **permanent and cache alike** — lives under one owned
top-level tree, separate from sources. Images move out of `files/images/`; the
audio cache gets a home alongside them.

```
<data_dir>/
  files/      sources only — owned audio blobs (files/images/ goes away)
  links/      symlink sources (audio only; covers are read-once-derived)
  variants/   ALL derived media (owned)            variants_dir (default <data_dir>/variants)
    images/   <base_key>/<variant>                 permanent — refcount GC (ReconcileImageOrphans)
    audio/    <key>/<variant>                      permanent audio variants (future) — refcount GC
    cache/    <key>/<variant>                      EVICTABLE audio-variant tier (future) — LRU GC
```

`variants_dir` derives from `data_dir` (default `<data_dir>/variants`) with an
optional override, exactly like `files_dir`/`database.path` (data-sources P0). The
cache is the `variants/cache/` subtree — no separate config knob in v0; it derives
from `variants_dir`.

**Why a dedicated tree.**
- Makes the source-vs-derivative split *physical*: `files/` is purely originals;
  `variants/` is purely regenerable, owned, GC-able derived data (Principle 1).
- One subtree to account as the "derived footprint", and one you can **wipe and
  rebuild without ever touching a source** — a clean backup-exclude boundary
  ("everything under `variants/` is regenerable").
- Unifies the permanent and cache tiers under one home (distinguished by their
  parent dir + retention policy) instead of images-under-`files/` and an
  audio-cache-elsewhere.
- Removes an existing wart: `/images/*` is served today from a *sibling of the
  audio tree* under `files/`; a dedicated root decouples them.

**Why the cache is its own subtree (safety, not just tidiness).** The LRU evictor
is rooted at `variants/cache/`, so it is **physically incapable** of deleting a
permanent, refcounted variant under `variants/images/` or `variants/audio/`. The
two retention policies — refcount-GC vs LRU-evict — never share a directory.

### The `CacheDir` → `SpoolDir` rename (terminology hygiene)

Today the upload path has a thing called `CacheDir` (`api.Deps.CacheDir`,
`madshare.go` defaults it to `os.TempDir()` — system temp, no TOML key). It is
**not a cache**: it is transient per-upload staging where a large upload is
*spooled* to a temp file while its hash is computed (`api/storage/hash.go`, above
`memBufferLimit`), cleaned up after each request. It is a **spool** in the classic
Unix sense (print/mail spool).

To stop "cache" being overloaded, rename it `SpoolDir`/`spool` (≈6 internal sites:
`api.Deps.CacheDir`, `handlers.cacheDir`, `NewRouter` param, `HashUpload` param,
`madshare.go` wiring — **no TOML/DB/user-facing surface**). Result: three short,
accurate, non-overlapping names —

- **`spool`** — transient upload staging (system temp).
- **`variants/`** — permanent derived media.
- **`variants/cache/`** — evictable derived media.

### Implementation cost & sequencing (when this lands)

Small and precedented; the image-dir path is derived in only **two** sites
(`madshare.go` → reconcile + imageproc pool; `api/api.go` → the `/images/*` mount
and every cover read/write via `h.imagesDir`). Everything else takes the dir as a
parameter, so it is already path-agnostic.

- New config: `variants_dir` derived from `data_dir`; point the two derivation
  sites at `variants_dir/images` instead of `files_dir/images`.
- One-time **idempotent startup relocation** of an existing `files/images/` →
  `variants/images/` (à la `storage.RelocateLegacyBlobs`), for existing installs.
- The **`/images/*` URL is unchanged** — it is a URL prefix
  (`media/imagestore.go`), not tied to the filesystem root; only the FS root moves.
  Zero client/UI/API churn.
- **Sequencing:** this is **data-sources P3.5** (a pre-P4 step — see
  `docs/architecture/data-sources.md` → *Phasing*). Landing it before the
  read-once-derive cover step (P4) means P4 derives covers **straight into
  `variants/images/`** so fresh installs never accumulate images under `files/`;
  the relocation pass is then only ever needed for pre-existing deployments. It is
  independent of the links/symlink work, so it can land anytime before P4.

## Image variants (today + linked imports)

Today (`media/images.go`, `imageproc/`, `database/reconcile.go`) image variants are
written under `files_dir/images/<base_key>/`; per the layout decision above they
**relocate to `variants/images/<base_key>/`** (paths below name the post-move
location). The behaviour is otherwise unchanged:

- `media.ProcessImage` makes 8 square variants (crop/fit × 64/150/300/600),
  drained from `image_processing_jobs`.
- `ReconcileImageOrphans` (startup) removes a `<base_key>/` dir when no
  `album_images`/`artist_images` row and no active job references the key.

**What linked imports change: nothing here.** Per the read-once-derive decision
below, a linked source's cover (sidecar `cover.jpg` or embedded picture) is decoded
**once** at scan time into owned variants under the variants image tree, exactly
like an uploaded cover. No original is kept in `links/`, so there is a **single**
cleanup location and the existing `ReconcileImageOrphans` sweep covers it
unchanged. Cover variants are always owned/local regardless of where the audio came
from, so the `album_images`/`artist_images` rows for a linked import are
`storage_backend='local'` like any other — no storage-aware image columns are
needed.

### DECISION — image original persistence: (A) read-once-derive

For a **linked** cover we keep **only the derived variants**, never the external
original. The cover — sidecar `cover.jpg` or embedded picture — is decoded once
into owned `local` variants; no symlink to the original is kept.

- **(A) Read-once-derive (CHOSEN)** — variants are fully self-contained in the
  owned variants tree: a single cleanup location, no `links/images` tree, no
  broken-image-link health. Cost: if the variant recipe ever changes (new
  sizes/crops) and the original cover is gone, we can't re-derive — but see "buys
  ~nothing" below.
- **(B) Persist linked original** *(rejected)* — symlink the original in
  `links/images` as a regenerate-source *plus* owned variants. Adds a second
  cleanup location + broken-image-link health for no real gain.

**Why (A).** *Embedded* cover art (ID3/MP4/FLAC pictures) has no separate original
file — it is bytes inside the audio blob/link — so it is inherently
read-once-derive; only **sidecar** images even *have* an original to persist. (B)
would therefore special-case sidecar covers while (A) treats sidecar and embedded
art uniformly. And (B) buys ~nothing: the persisted symlink is redundant with the
source directory itself — if the variant recipe ever changes, re-derive by
**re-scanning the source** (the `cover.jpg` is still in the external dir, and a
rescan is wanted anyway for the future sync). The only case (B) would help —
recipe changed **and** the source dir gone — is exactly when the linked **audio**
is also gone, so the album is unplayable and its cover moot. (A) loses nothing
real and drops the second cleanup path, the `links/images` tree, and the
broken-image health surface.

## Audio variants (FUTURE — engine sketch only, not designed)

> Owner's idea, captured so it isn't lost; **not** for implementation now. The
> *home* for these is decided (`variants/audio/` permanent, `variants/cache/`
> evictable — see the layout above); the *engine* below is not.

Goal: serve smaller/transcoded audio for streaming without permanently storing a
second copy of every track. Two admin-selectable modes:

- **Cache mode (default?)** — variants generated **on the fly** for streaming and
  written to `variants/cache/`; auto-pruned by rule, e.g. LRU eviction of the
  least-recently-used variants once the cache passes a high-water mark (≈80%).
  Saves disk; cost is recompute on a cache miss.
- **Permanent mode** — admin opts to **materialize** chosen variants (e.g. one
  standard streaming bitrate) under `variants/audio/`, kept and GC'd by reference
  count like images.

Undesigned (do not assume): the recipe/key scheme (codec, bitrate, container); the
transcode worker (an ffmpeg pool mirroring `imageproc`/`mediaproc`); eviction
policy details and accounting; whether cache variants count toward storage stats;
per-source or per-quality policy. (The cache *location* is no longer open — it is
`variants/cache/`, distinct from the upload `spool`.) All TBD in a later pass.

## Cross-cutting open questions

- **Active vs startup GC.** Image GC is startup-only; an audio cache needs
  continuous eviction. Likely a shared "variant store" abstraction with a
  pluggable retention policy (keep-and-refcount under `variants/` vs LRU-evict
  under `variants/cache/`).
- **One variant abstraction for both?** Images and audio share: derived, owned,
  content-keyed, ref-counted, regenerable. A common `variants` layer (store +
  recipe + GC policy) may be worth it once audio is real — or premature now.
- **Keying & sharing** across linked + local sources (same content hash, one
  variant set) — should already fall out of content-addressing, confirm.
