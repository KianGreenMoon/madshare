# Variants (derived media) — DRAFT / INCOMPLETE

> **STATUS: partial.** Three layers, at three maturities:
> 1. The dedicated `variants/` **directory** + the `CacheDir`→`SpoolDir` rename are
>    **BUILT** (data-sources P3.5).
> 2. The cover **source/derivative split & full-hash keying** (original →
>    `files/images`, recipe-keyed variants, audio asymmetry) is **DESIGNED below,
>    not yet built** — a later phase.
> 3. The audio-variant **engine** (transcode recipes, the LRU cache, eviction) is
>    an early **sketch** — do not implement from it.
>
> See `docs/architecture/data-sources.md` for storages.

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

## Storage layout — a dedicated `variants/` directory (DECIDED — dir built, split designed)

All derived media — **permanent and cache alike** — lives under one owned
top-level tree, separate from sources. `files/` holds only *sources* (content-
addressed by hash); `variants/` holds only *derivatives*.

```
<data_dir>/
  files/      SOURCES (owned, content-addressed by hash)
    audio/    <hash>/<filename>                     audio source blobs (catalog / files table)
    images/   <original_hash>/original<ext>         cover originals = regenerate seeds — NEVER served
  links/      symlink sources (audio only; covers are read-once-derived)
  variants/   DERIVED media (owned)                 variants_dir (default <data_dir>/variants)
    images/   <original_hash>/<recipe><ext>         permanent cover sizes — refcount GC; served at /images
    audio/    <source_hash>/<variant_hash>/<file>   permanent audio variants (future) — refcount GC
    cache/    <source_hash>/<variant_hash>/<file>   EVICTABLE audio-variant tier (future) — LRU GC
```

> **What's built vs designed.** **P3.5 (built)** created the dedicated `variants/`
> tree and relocated the whole cover bundle there under the legacy 16-char
> `base_key` (`variants/images/<base_key>/{original,<variant>}`, original + sizes
> together). The **source/derivative split** drawn above — cover original back out
> to `files/images/<full_hash>`, full-hash keying, recipe-keyed variants — is the
> next refinement, **designed below, not yet built**.

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

### Cover source/derivative split & keying (DESIGN — refines P3.5, not built)

Decided with the owner 2026-06-25. Brings the cover pipeline to a fully content-
addressed source/derivative split that mirrors the audio model.

- **Original = source → `files/images/<original_hash>/original<ext>`**, addressed
  by the **full** `sha256` of the original bytes (supersedes the 16-char
  `base_key`). Uploaded and embedded covers alike land here under the canonical
  name `original<ext>` (embedded art has no real filename). The original is the
  regenerate seed.
- **Variants = derivatives → `variants/images/<original_hash>/<recipe><ext>`**,
  **recipe-keyed** (`thumb_crop`, `small_fit`, … — today's recipe set), nested
  under the same original hash. The hash is the source→derivative link, and the
  same cover bytes auto-share one variant set across every album/artist that uses
  them.
- **The cover original is NEVER served.** The current UI has no full-size cover
  view (largest shown is 600px — the original is too big to be useful), so
  `files/images/<hash>` is a pure regenerate seed. **`/images` serves only
  `variants/`** — a source is never exposed there.

**Why recipe-keyed, not a per-variant hash.** A cover variant's content is fully
determined by `(original_hash, recipe)` — same source + same recipe → byte-
identical output. So a `<variant_hash>` level is *derivable* info we'd have to
**track in the DB to use**, replacing today's zero-DB static path map
(`/images/<key>/<recipe>.jpg`, URL = path). And `original_hash` already changes
whenever the cover bytes change, so the paths are already cache-immutable except
across a *recipe* change — handle that with a single global recipe-version bump,
not a hash per file. (The audio cache is the opposite case — see below — which is
why its layout above *does* carry `<variant_hash>`.)

**Owned covers keep their source; linked covers don't (DECIDED).** Uploaded *and*
embedded covers store the original at `files/images/<original_hash>/original<ext>` —
for an embedded cover, extract the picture once and keep the (few-KB) copy, so the
invariant "every owned cover has a source file in `files/images`" holds with no
empty-hash-dir special case, and regeneration never has to re-find or re-read the
audio blob. Only the **linked-import** case stays read-once-derive (no stored
original — see the decision below): that choice avoided a `links/images` *symlink
tree* with broken-link health, a cost that simply doesn't apply to a tiny owned
copy. Clean rule: **owned content keeps its cover source; linked content re-scans.**

**Migration.**
- Re-key `base_key` (16-char) → full original hash; relocate
  `variants/images/<base_key>/{original,<variant>}` →
  `files/images/<full_hash>/original<ext>` + `variants/images/<full_hash>/<recipe><ext>`.
  One idempotent startup pass (extends `RelocateImageVariants`) plus a DB column
  change (`album_images`/`artist_images` `base_key` → `image_hash`).
- A phase **beyond P3.5** — sequence after it.

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

### Implementation cost & sequencing

> **STATUS: the relocation + `SpoolDir` rename are BUILT** (data-sources P3.5,
> commits `80ab0b3` + `5528de0`): `variants_dir`, `storage.RelocateImageVariants`
> (`files/images` → `variants/images`), and `CacheDir` → `SpoolDir`. What remains
> is the *source/derivative split & full-hash keying* above (a separate, later
> phase). The cost notes below described the relocation step.

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

### Audio is NOT symmetric with covers — serve the source AND the variants

The defining difference from the cover pipeline (where the original is an *unserved*
regenerate seed): for audio, **both the source and its variants are reachable**.

- The audio **source** (full-quality original) is the catalog blob at
  `files/audio/<hash>` and is served at `/files/<hash>` today — unchanged. Users
  who want full quality get the source.
- Audio **variants** (transcodes) are served too: the client **picks a quality**
  and the chosen rendition is streamed. So unlike a cover (seed never exposed), an
  audio source *and* all its variants are first-class served objects.
- This is why audio variants are **content-hash-keyed**
  (`variants/audio/<source_hash>/<variant_hash>/…`, `variants/cache/…` likewise)
  rather than recipe-keyed: the recipe space is large (codec × bitrate ×
  container), they're DB-indexed in the cache anyway, and immutable/evictable
  content-addressed keys are exactly right. This is the one place `<variant_hash>`
  earns its keep (contrast the cover decision above).

### Frontend surface (don't forget)

Whatever the keying, the API must hand the frontend what it needs to *use* a
variant without knowing the storage layout:

- **Covers:** deterministic variant URLs (already `media.VariantURL` →
  `/images/<key>/<recipe><ext>`); browse DTOs / the image-status endpoint carry the
  key + `variants_ready`.
- **Audio:** the renditions / quality list — already `GET /api/tracks/{hash}/renditions`
  (recordings P4), the player's *Auto* + per-rendition in-place source swap. **Audio
  variants slot into this same list**, so when the engine lands it extends an
  existing surface rather than inventing one.

Undesigned (do not assume): the recipe/key scheme detail (codec, bitrate,
container); the transcode worker (an ffmpeg pool mirroring `imageproc`/`mediaproc`);
eviction policy details and accounting; whether cache variants count toward storage
stats; per-source or per-quality policy. (The cache *location* is no longer open —
it is `variants/cache/`, distinct from the upload `spool`.) All TBD in a later pass.

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
