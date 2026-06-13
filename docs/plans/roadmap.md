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

## Artist / album entities

- **Browse by entity id.** Library listings still resolve artist/album by name
  (`?artist=`/`{album}` path segments); covers and *merge* already address the
  stable surrogate id. Move the browse endpoints to `?artist_id=`/`?album_id=`
  (and migrate the UI) to drop the name-resolution step and address the
  empty-name buckets cleanly. See `docs/architecture/artist-album-model.md`.
- **Track-level performer entity (`track_artist_id`).** `media_metadata.artist_id`
  is the *album* artist; the per-track performer survives only as raw tag text.
  Browsing by a featured/performing artist who is never an `album_artist` needs a
  track↔artist credits model and its own design.
- **Merge dry-run.** Artist/album merge is immediate and destructive; a preview
  ("what will move / collapse") is not built.

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

## Federation (Phase 4, deferred)

Server-to-server trust — peer keypairs, signed requests, file provenance, library
sharing scope (madnetwork / friends / none). Acknowledged throughout the auth and
entity models but not designed. See `docs/architecture/auth.md` §8.
