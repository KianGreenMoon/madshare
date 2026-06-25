# Variants (derived media) — DRAFT / INCOMPLETE

> **STATUS: incomplete draft.** The image half reflects what exists today plus the
> read-once-derive decision (below), under which linked imports need **no** change
> to the image path; the audio half is an early sketch of a *future* feature and is
> deliberately under-specified. Do not implement from the audio section yet. See
> `docs/architecture/data-sources.md` for storages.

A **variant** is a *derived* representation of a source file — a resized cover
image today, a transcoded/down-bitrate audio stream tomorrow. Variants are not
catalog content of their own; they are computed from a source blob and a recipe.

## Principles (the part I'm confident about)

1. **A variant is always *owned*.** It is produced by Madshare, so it lives in an
   owned location (`local`, or an owned cache dir) — **never** in a source storage
   (`links`, future `s3`). The *source* may be anywhere (a local blob or a linked
   original); the *derivative* is always ours. This is already true for images and
   should stay the rule.

2. **A variant is reproducible, so persistence is a *policy*, not a requirement.**
   Given the source + the recipe, any variant can be regenerated; losing one is a
   recompute, not data loss. That unlocks two storage modes:
   - **Materialized / permanent** — kept on disk, reclaimed by reference-counted
     GC. Right for *cheap, small, hot* derivatives (cover variants).
   - **Cache / ephemeral** — kept in an evictable cache dir, regenerated on
     demand, evicted under pressure. Right for *expensive, large* derivatives
     (audio transcodes).
   The mode is a property of the variant *type* (and may be admin-configurable).

3. **Cleanup is reference-counted, never inline.** A variant is keyed by content
   (`base_key` for images; a source-hash+recipe key for audio) and is **shared**
   across catalog entries (many albums, one cover). A single file delete therefore
   cannot safely remove a variant — another entry may still use it. Removal is a
   GC sweep that drops a variant only when nothing references its key.

## Image variants (today + linked imports)

Today (`media/images.go`, `imageproc/`, `database/reconcile.go`):

- `media.ProcessImage` makes 8 square variants (crop/fit × 64/150/300/600) under
  `files_dir/images/<base_key>/`, drained from `image_processing_jobs`.
- `ReconcileImageOrphans` (startup) removes a `<base_key>/` dir when no
  `album_images`/`artist_images` row and no active job references the key.

**What linked imports change: nothing here.** Per the read-once-derive decision
below, a linked source's cover (sidecar `cover.jpg` or embedded picture) is decoded
**once** at scan time into owned variants under `files_dir/images/<base_key>/`,
exactly like an uploaded cover. No original is kept in `links/`, so there is a
**single** cleanup location and the existing `ReconcileImageOrphans` sweep covers
it unchanged. Cover variants are always owned/local regardless of where the audio
came from, so the `album_images`/`artist_images` rows for a linked import are
`storage_backend='local'` like any other — no storage-aware image columns are
needed.

### DECISION — image original persistence: (A) read-once-derive

For a **linked** cover we keep **only the derived variants**, never the external
original. The cover — sidecar `cover.jpg` or embedded picture — is decoded once
into owned `local` variants; no symlink to the original is kept.

- **(A) Read-once-derive (CHOSEN)** — variants are fully self-contained in
  `local`: a single cleanup location, no `links/images` tree, no
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

## Audio variants (FUTURE — sketch only, not designed)

> Owner's idea, captured so it isn't lost; **not** for implementation now.

Goal: serve smaller/transcoded audio for streaming without permanently storing a
second copy of every track. Two admin-selectable modes:

- **Cache mode (default?)** — variants generated **on the fly** for streaming and
  written to an **evictable cache dir**; auto-pruned by rule, e.g. LRU eviction of
  least-recently-used variants once the cache passes a high-water mark (≈80%).
  Saves disk; cost is recompute on a cache miss.
- **Permanent mode** — admin opts to **materialize** chosen variants (e.g. one
  standard streaming bitrate), kept and GC'd by reference count like images.

Undesigned (do not assume): the recipe/key scheme (codec, bitrate, container);
where the cache dir lives (relation to the existing upload spool `CacheDir`?);
the transcode worker (an ffmpeg pool mirroring `imageproc`/`mediaproc`);
eviction policy details and accounting; whether cache variants count toward
storage stats; per-source or per-quality policy. All TBD in a later pass.

## IDEA — give variants their own directory (possible decision, not decided)

Today image variants live under `files_dir/images/<base_key>/` — i.e. *inside* the
local source tree. An alternative worth weighing: move **all** derived artifacts,
**permanent and cache alike**, out of `files/` into a dedicated top-level tree:

```
<data_dir>/
  files/      sources only (audio blobs)         <- files/images/ goes away
  links/      symlink sources
  variants/   ALL derived media (owned)
    images/   <base_key>/<variant>               (permanent)
    audio/    <key>/<variant>                     (permanent and/or cache)
```

(or a sibling `cache/` for the ephemeral tier, if LRU-evicting a whole subtree is
simpler than per-entry).

**Why it's attractive:**
- Makes the source-vs-derivative split *physical*: `files/` is purely originals;
  `variants/` is purely regenerable, owned, GC-able derived data (Principle 1).
- One subtree to account as the "derived footprint", and one you can wipe and
  rebuild without ever touching a source.
- Unifies the permanent and cache tiers under one home (distinguished by a
  retention-policy attribute) instead of images-under-`files/` and
  audio-cache-elsewhere.
- Removes an existing wart: `/images/*` is served today from a sibling of the
  audio tree under `files/`; a dedicated root decouples them.

**Costs:**
- Relocates existing image variants (`files/images/` → `variants/images/`) — a
  one-time idempotent startup move (à la `RelocateLegacyBlobs`), plus repointing
  the `/images/*` server root and the reconcile/`imagesDir` paths.
- It's a refactor of working code for a conceptual win — weigh against the cheaper
  "leave images where they are, only give *audio* its own/cache dir" middle ground.

Lean: clearly right for audio; for images it's a tidy-up whose only real cost is the
one-time relocation. Deferred with the rest of variants storage.

## Cross-cutting open questions

- **Active vs startup GC.** Image GC is startup-only; an audio cache needs
  continuous eviction. Likely a shared "variant store" abstraction with a
  pluggable retention policy (keep-and-refcount vs LRU-evict).
- **One variant abstraction for both?** Images and audio share: derived, owned,
  content-keyed, ref-counted, regenerable. A common `variants` layer (store +
  recipe + GC policy) may be worth it once audio is real — or premature now.
- **Keying & sharing** across linked + local sources (same content hash, one
  variant set) — should already fall out of content-addressing, confirm.
```
