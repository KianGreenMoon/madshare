# Covers over the madnetwork — plan

What this fixes: covers are invisible outside the owning server. Nothing about
a cover crosses the mesh (`CatalogEntry` has no image field, `seedableBlob`
seeds audio only), so a madnetwork browse row, a download-to-library, and a
listener client all end up with placeholder art for music another node has a
perfectly good cover for.

## Findings from the recheck (2026-08-21)

1. **Covers already belong to albums.** The track-level model is gone:
   embedded art claims the *stable album id* (`SetAlbumCoverIfAbsent(albumID,…)`,
   `api/upload_handlers.go`), reads are `GET /api/albums/{album_id}/image`.
   Nothing to migrate.
2. **"No endpoint for the original" is doctrine, not an accident**
   (`docs/architecture/variants.md`: the original is a regenerate seed, NEVER
   served — the UI needs ≤ ~480 px). The rule is about the *browser-facing*
   API. Node-to-node replication is a different audience, and for replication
   the original is exactly the right payload: canonical bytes, variants
   re-derivable locally, self-verifying by hash.
3. **Cross-node album identity is normalized text today.** The discover merge
   buckets by lowercased `(artist, album)` (`madnetwork_discover_handlers.go`);
   there is no global album id anywhere in the protocol.

## Design decisions

- **No global album id.** Album sameness stays a *claim* carried by text, like
  tagsets — the trust doctrine already says claims are weighed by independent
  voices and only become facts when bytes arrive. Sameness is *strengthened*,
  not defined, by shared recordings: two nodes whose albums share a content
  hash (or a fingerprint match) under the same normalized name are the same
  album for every purpose we have. Inventing a UUID would just move the
  question to "who mints it".
- **Cover identity is the image hash.** `media.ImageHash` is full sha256 —
  the same space `isBlobHash` accepts. A cover fetched by hash is
  self-verifying: the worst a wrong album-merge can do is show a *real* cover
  for a disputed name, never corrupt anything.
- **Cover choice on a merged row: the voices rule.** When holders disagree,
  the `cover_hash` claimed by the most independent branches wins — the same
  rule that picks the tagset leader. No new machinery, one shared doctrine.
- **The original stays unserved on `/api`.** The mesh serves it as a blob by
  full hash; browsers keep getting derived variants only.

## Work items — madshare

- **M1. Catalog carries the cover.** `CatalogEntry` += `cover_hash`,
  `cover_ext` (both `omitempty` — additive JSON, old nodes ignore it; no
  protocol bump). `store.PublishedCatalog` joins the album cover for each
  entry; rows whose cover is legacy (no full image hash) or not
  `variants_ready` publish nothing. The serial covers the new fields for free
  (it hashes canonical JSON).
- **M2. The mesh seeds cover originals.** `seedableBlob` (federation store
  side) learns a second lookup: hash → album cover original →
  `files/images/<hash>/original<ext>`. `handleBlob` needs no change — same
  hash space, same Range/HEAD, same quota. Covers are small (capped at
  `maxImageSize` at every upload boundary in the network), so single-shot
  fetches; no manifest/swarm work.
- **M3. Download-to-library attaches the cover.** When a madnetwork download
  is approved into an album whose cover is absent: `EnsureBlob(cover_hash)` →
  store as original → `SetAlbumCoverIfAbsent` → enqueue the variant job.
  Rides the existing pipeline end to end; hash keying makes re-downloads and
  shared covers dedup for free. Failure is soft — a missing cover must never
  fail an approval.
- **M4. Thin-client reach: browse rows + relay.** Madnetwork album/lane rows
  gain `cover_hash` (chosen by the voices rule). New
  `GET /api/madnetwork/cover/{hash}` — cache-through relay like
  `stream/{hash}`: EnsureBlob from a holder, serve from cache after.
  *v0 serves the cached original bytes*: this does not collide with the
  variants doctrine's reason (unbounded originals) because network covers are
  bounded by `maxImageSize`, and madplayer downsizes to 320 px on its side
  anyway. A derive-on-first-request variant step can join the
  `variants/cache` work (data-sources P3.5) later.
- **M5. Docs.** federation.md gains §Covers (catalog field, voices rule,
  identity stance); cover-images.md and variants.md get cross-references.

## Work items — madplayer

- **P1. Remote-library covers (home server).** Album rows already carry
  `HasImage`; fetch `GET /api/albums/{id}/image?size=medium` and extend
  `artwork.Cache` with remote entries (keyed by source+album id, async load,
  existing placeholder flow). Independent of everything above — can ship
  first.
- **P2. Madnetwork covers.** Rows carry `cover_hash` (M1/M4): fetch the blob
  from a mesh holder like audio (single GET, no swarm), fall back to the home
  server's relay (M4) when no holder is reachable.
- **P3. Materialize writes the cover.** A materialized album lands
  `cover.jpg` beside its tracks — `artwork` already looks for that name, so
  offline display costs nothing further.

## Order

M1+M2+M3 together (the mesh capability, one deploy), then M4; P1 any time;
P2/P3 after M1/M2/M4 reach the live server. No shared Go types cross the
repos — the player speaks HTTP — so no madshare tag/require dance.

## Verification

- meshlab scenario: node A holds album+cover, node B pulls catalog, downloads
  a track, asserts the cover attached and variants generated on B.
- Relay: thin client asks B's server for a cover only A holds.
- madplayer: madnetwork fake-server test asserting cover fetch + materialize
  writes `cover.jpg`.
