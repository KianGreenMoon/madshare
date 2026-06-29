# Roadmap — deferred work

The per-feature plan docs were retired once their work shipped; their durable
design now lives in the reference docs (`docs/architecture/*`, `docs/api/*`,
`docs/ui/*`). This file is the one place that tracks what was **consciously
deferred** out of those efforts — forward-looking only, grouped by area. Granular
bug/cleanup items live in `.issues/open-issues.md` and `.issues/ui-issues.md`.

## Covers & images

- **Aura effect (cmus).** An ambient glow behind the player in `cmus.html`,
  derived from the current track's album cover: read the cover pixels on a hidden
  canvas, pick a representative colour, drive `--aura-h/s/l` CSS custom properties
  (transitioned between tracks), render as a `radial-gradient`/`box-shadow`.
  Purely client-side and decorative — skip silently when no cover or on a tainted
  canvas. Was the last (unbuilt) phase of the upload & covers work.
- **WebP cover support.** JPEG/PNG only today; WebP is rejected at the upload
  boundary (`POST /api/albums/{album}/image`, embedded extraction). A later
  addition is non-breaking: accept the type, add the `.webp` variant names, decode
  in the worker. No backfill needed (v0 stores zero WebP rows). See
  `docs/api/cover-images.md`.
- **Artist-cover variant pipeline.** Album covers generate eight square variants;
  artist images are uploaded and stored flat (`<base_key><ext>`) with **no**
  resize variants or worker job. Give artists the same pipeline when artist pages
  need sized thumbnails.

## Variants & derived-media storage (decided, not built)

- **Dedicated `variants/` directory** — *scheduled as data-sources **P3.5** (pre-P4;
  see `docs/architecture/data-sources.md` → Phasing).* Move all *derived* media out
  of `files/` into an owned top-level `variants/` tree (`variants/images/` permanent
  + `variants/cache/` evictable), so `files/` holds only source blobs. Config:
  `variants_dir` derived from `data_dir` (default `<data_dir>/variants`). Cost is
  small — two image-dir derivation sites + a one-time idempotent `files/images` →
  `variants/images` startup relocation (à la `RelocateLegacyBlobs`); the `/images/*`
  URL is unchanged. Landing it before P4 lets the read-once-derive cover step write
  straight to `variants/images`. Design: `docs/architecture/variants.md`.
- **`CacheDir` → `SpoolDir` rename.** The upload "cache" (`os.TempDir` staging for
  hashing large uploads) is really a spool; rename it so "cache" unambiguously
  means the variant cache. Internal-only (~6 sites; no TOML/DB/user surface).
- **Audio variants (engine).** On-the-fly transcodes streamed to `variants/cache/`
  (LRU-evicted) with an opt-in permanent tier under `variants/audio/`. Recipe/key
  scheme, ffmpeg worker pool, eviction policy, and accounting are all undesigned.
  Sketch only in `docs/architecture/variants.md`.

## Artist / album entities

- **Track-level performer entity (`track_artist_id`).** `media_metadata.artist_id`
  is the *album* artist; the per-track performer survives only as raw tag text.
  Browsing by a featured/performing artist who is never an `album_artist` needs a
  track↔artist credits model and its own design.

(Browse-by-entity-id and the merge dry-run shipped — see
`docs/architecture/artist-album-model.md` §"Browse by entity id" / §"Merge
dry-run".)

## Listening shell & player

- **cmus on the shared player.** `/cmus` is a paused, standalone view with its own
  player and old header (outside the listening shell). Migrate it onto the shared
  `player-controller` + shell so playback continues across it. See
  `docs/ui/shells.md`, `docs/ui/player-and-queue.md`.
- **Theme FOUC.** `<html data-theme="dark">` is hardcoded and JS swaps to the saved
  theme after load, so a slow load flashes dark → chosen theme. Fix is a tiny
  inline `<head>` script that applies the saved theme before first paint —
  slated for when a user-settings page exists.
- **Prev play-history.** Prev steps back through the visible (shuffled) order, not
  the actual sequence that played across reshuffles. A history stack would make it
  exact.
- **Scroll-position memory.** The shell router does not restore scroll position on
  back navigation. Add if it feels bad in use.
- **PWA / mobile background playback.** Screen-off mobile listening needs a web-app
  manifest + service worker for installable, background-capable playback. The
  persistent shell is the prerequisite; the PWA layer itself is future direction.

## Playlists

- **Sharing / visibility.** Playlists are per-user and private; a visibility column
  + sharing is an additive migration later (ties into federation).
- **Smart / auto playlists.** Recently-played, most-played, and similar generated
  lists.

## Scale

- **Server-side batch endpoints.** Bulk delete / approve / trash currently loop the
  per-item endpoints client-side. A server-side batch endpoint is the shared
  follow-up across the file-management and moderation views.
- **Server-side pagination.** Browse listings and the admin file lists load in full;
  large libraries will want paginated/virtualised listing endpoints.

## Access (note, not planned)

- **Per-content restriction** (per artist/album/file, for authenticated users) was
  deliberately removed for the roles-only model. If a real use case appears it can
  return as an additive feature keyed off `album_id`/`artist_id` — recorded as a
  future idea, **not** planned. See `docs/architecture/auth.md`.

## Recordings — same-audio grouping & renditions (P0–P4 shipped)

Group files that are the *same audio in different encodings* under a
fingerprint-keyed **recording** overlay. **P0–P4 are implemented** (single-node):
the `ffprobe`/`fpcalc` ingest passes, the recording overlay + resolver +
auto-ranked quality ladder, the `/admin/duplicates` page (delete-with-confirm /
split-off), the "possible duplicate" flag in moderation (no auto-approve, with a
tag fallback when `fpcalc` is absent), and per-client rendition pick for
bandwidth (range requests; segmented ABR is a video-era non-goal). Still future:
**P5** (ffmpeg auto-transcode of derived renditions) and the cross-node
fingerprint index (a federation concern). Design: `docs/architecture/recordings.md`;
build log: `docs/plans/recordings-implementation.md`.

## Federation (Phase 4, deferred)

Server-to-server trust — peer keypairs, signed requests, file provenance, library
sharing scope (madnetwork / friends / none). Acknowledged throughout the auth and
entity models but not designed. See `docs/architecture/auth.md` §8.

## Android app — Capacitor remote-URL shell (designed, not built)

A thin Capacitor WebView that loads a running server's pages **same-origin**,
reusing the whole web UI. Works over plaintext on an encrypted overlay
(Yggdrasil / VPN) *and* over TLS, with a connection-safety gate that hard-warns
("you may be about to leak your auth data") before sending credentials over
plaintext across an untrusted/public network. Same-origin keeps cookie auth and
gated `<audio>` streaming working with no CORS/token plumbing. Full design, phases
P0–P4 (PWA stepping stone → shell+safety gate → background audio → multi-server):
`docs/architecture/android-app.md`.

## Native desktop/mobile client (designed, not built)

A **separate, native pure-Go GUI** (Gio/Fyne leaning) for desktop and mobile that
**embeds** the madshare backend *and* a Yggdrasil node as in-process libraries —
local-first music player by default, **federation peer** to other nodes as the
milestone. The web UI stays HTML/JS (best tool for the browser), so this is a
second, deliberately-accepted UI over the same HTTP API. Deferred until the backend
and the federation model are stable; the cross-node trust model
(`docs/architecture/federation.md`, see Federation above) is the prerequisite.
Full design: `docs/ui/native-client.md`.
