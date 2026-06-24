# Variants (derived media) — DRAFT / INCOMPLETE

> **STATUS: incomplete draft.** The image half describes what exists today plus
> the one fix linked imports require; the audio half is an early sketch of a
> *future* feature and is deliberately under-specified. Do not implement from the
> audio section yet. See `docs/architecture/data-sources.md` for storages.

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

## Image variants (today + the linked-import fix)

Today (`media/images.go`, `imageproc/`, `database/reconcile.go`):

- `media.ProcessImage` makes 8 square variants (crop/fit × 64/150/300/600) under
  `files_dir/images/<base_key>/`, drained from `image_processing_jobs`.
- `ReconcileImageOrphans` (startup) removes a `<base_key>/` dir when no
  `album_images`/`artist_images` row and no active job references the key.

**What linked imports change.** A linked cover keeps its *original* as a symlink
in `links/images/<base_key>/…` (the regenerate-source) while the resized variants
stay owned in `files_dir/images/<base_key>/`. So a `base_key` going unreferenced
must clean **two** places:

| Location | What | How removed |
|---|---|---|
| `files_dir/images/<base_key>/` | owned variant dir | `os.RemoveAll` (ours) |
| `links/images/<base_key>/` | linked-original **symlink** | `os.Remove` the symlink only — **never the external original** (the data-sources INVARIANT) |

→ **v0 work:** extend `ReconcileImageOrphans` (or add a links-aware sibling) to
also sweep the `links/images` tree by the same `base_key` reference check, using
`os.Remove` on the symlink. (See the matching bullet in
`docs/architecture/data-sources.md` → *Lifecycle & safety*.)

**Note:** the sweep is startup-only. Fine for small owned images; a more active GC
matters more for the audio cache below.

### OPEN DECISION — image original persistence (to discuss, not decided)

For a **linked** cover, do we keep the external original around (as a symlink) or
only the derived variants?

- **(A) Read-once-derive** — generate variants from the cover, keep **no** link to
  the original. Variants are fully self-contained in `local`: a single cleanup
  location, no `links/images` tree, no broken-image-link health. Cost: if the
  variant recipe ever changes (new sizes/crops) and the original cover is gone, we
  can't re-derive. Simplifies the whole data-sources linked-cover path.
- **(B) Persist linked original** — symlink the original in `links/images` as a
  regenerate-source *plus* owned variants. Enables future re-derivation, but adds
  the second cleanup location + broken-image-link health (the "stinky" part).

**Nuance for the discussion:** *embedded* cover art (ID3/MP4/FLAC pictures) has no
separate original file — it is bytes inside the audio blob/link — so it is
inherently read-once-derive; only **sidecar** images (`cover.jpg` next to the
tracks) even *have* an original file to persist. So (B) only buys re-derivation for
sidecar covers, while (A) treats sidecar and embedded art uniformly. That
asymmetry leans toward (A).

**Argument that (B) buys ~nothing:** the persisted symlink is redundant with the
source directory itself. If the variant recipe ever changes, re-derive by
**re-scanning the source** (the `cover.jpg` is still in the external dir — and a
rescan is wanted anyway, the future sync). The *only* case (B)'s symlink would help
— recipe changed **and** the source dir is gone — is exactly the case where the
linked **audio** is also gone, so the album is unplayable and its cover is moot.
Net: (A) loses nothing real and drops the second cleanup path, the `links/images`
tree, and the broken-image health surface. Owner parked it; revisit before P4.
(Mirror of the data-sources "image-original persistence" open question.)

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
