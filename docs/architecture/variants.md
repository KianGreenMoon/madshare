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

**Open:** the sweep is startup-only. Fine for small owned images; a more active GC
matters more for the audio cache below. Also reconsider whether to persist the
linked-original image symlink at all — *read-once-derive* (don't keep the link)
would make image variants fully self-contained in `local`, removing the second
cleanup location and any broken-image-link health, at the cost of not being able
to re-derive variants if the recipe changes and the original is gone. (This is the
data-sources "image-original persistence" open question, viewed from the variant
side.)

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
