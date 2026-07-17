# Tag suggestions — multi-source tagsets

**Status:** P0 (local sources) implemented and live-verified 2026-07-17: the
`tagsource` package with the `id3v2`/`id3v1` providers, `media.ReadID3v1` +
charset detect/override (`media/charset.go`), `GET
/api/tagsets/{id}/suggestions`, and the `track-edit.js` "Suggest tags…" panel
(wired in My uploads, moderation, Recordings lens, All Appearances). P0.5
(bulk charset fix, owner follow-up — see §Bulk charset fix) implemented. P1
(MusicBrainz via AcoustID) implemented 2026-07-17:
`media.CompressFingerprint` (golden-tested against real fpcalc pairs),
`tagsource/acoustid.go` (serializing 1 req/s limiter → 429 "busy", 15-min TTL
cache, identifying User-Agent), the `tagsource.musicbrainz.enabled` /
`tagsource.acoustid.api_key` settings behind `GET/POST
/api/admin/settings/tagsource` (user.manage; the key is never echoed — only
set/unset + last 4), the "Tag services" card on `/admin/settings`, and the
on-demand MusicBrainz chip in the panel. Live-verified against the real
AcoustID service up to the invalid-key error path (an end-to-end match needs
a registered key). P2 (text-search fallback) implemented 2026-07-17:
`tagsource/musicbrainz.go` (recording search, shared limiter/cache plumbing
extracted to `tagsource/service.go`), `&query=` on the suggestions endpoint
(honoured only with `sources=musicbrainz`; empty query → seeded
`recording:"…" AND artist:"…"` + `dur:` window from the stored tags, with
ffprobe duration as the fallback), and the panel's "Search MusicBrainz" row
(revealed when the lookup yields nothing usable, prefilled from the modal
inputs; keyless servers work — text search needs no AcoustID key, though
*enabling* the source still requires one). Live-verified end-to-end against
the real musicbrainz.org with fpcalc off PATH. Owner
decisions (consult round): providers for v1 = **MusicBrainz via AcoustID**
(local ID3v1/ID3v2 always included); external lookups are **user-triggered
only** (never at ingest, never auto-applied); ID3v1 charset handling =
**auto-detect with a manual override + live preview**; provider configuration
lives in the **settings table / `/admin/settings`** (runtime, no restart).
Builds on the
fingerprint pass (`docs/architecture/recordings.md` P0) and the shared edit
modal (`track-edit.js`). Federation-relevant: a future "top tagsets from peers"
source is just another provider behind the same interface.

## Problem

Ingest extracts exactly **one** tag reading per file: `media.ExtractTags`
wraps `dhowden/tag`, which prefers ID3v2 and silently falls back to v1. That
single reading becomes the appearance's tagset; a file with no usable tags
lands under the pseudo entities ("Unknown artist" / filename title). Two
recurring failure modes have no recourse short of hand-typing every field:

- **Wrong charset.** ID3v1 has no charset (in the wild: Latin-1,
  Windows-1252/1251, KOI8-R, Shift-JIS…), and plenty of v2.3 files declare
  ISO-8859-1 while actually carrying a Windows codepage. The library shows
  mojibake and the only fix is manual retyping.
- **No/poor tags.** The uploader (or the admin tidying the library) knows the
  track; an authoritative database knows its tags. We already compute the exact
  key such databases use — a Chromaprint fingerprint (`audio_fingerprints`,
  fpcalc) — and never use it for anything beyond same-audio grouping.

## Model: suggestions, not writes

A **suggestion** is a read-only candidate tagset with provenance. Nothing is
ever applied automatically: suggestions populate the inputs of the existing
edit modal, the human edits freely, and apply goes through the **existing**
metadata PATCH paths. The server stores no suggestion state and no provenance
in v1 — this is a lookup surface, not a new lifecycle.

Default stays the default: ingest behaviour is untouched (v2-preferred
extraction). Suggestions are reached only when the default did not satisfy.

### Sources (v1)

| Source | Needs | What it returns |
|---|---|---|
| `id3v2` (default) | blob on disk | The v2 frames re-read live; for v2.3 frames *declared* ISO-8859-1, an optional charset re-decode (same override UI as v1). |
| `id3v1` | blob on disk | The trailing 128-byte `TAG` block (v1.1 track number included), decoded with the **detected** charset, overridable from a fixed list with live preview. |
| `musicbrainz` | fingerprint + AcoustID API key (admin-configured), or text search | AcoustID `lookup` maps the Chromaprint fingerprint to MusicBrainz recording(s) + releases → candidate tagsets (title/artist/album/albumartist/year/track/disc). Text search (`recording` query by artist/title, duration-filtered) is the fallback when there is no fingerprint (fpcalc absent) or no fingerprint match. |

Local sources need **no schema change**: blobs stay on disk, so v1/v2 are
re-read on demand through the `storages.Registry` probe (same precedence as
`serveBlobs` — a links-storage blob works too). MP4/FLAC/OGG files simply have
no `id3v1` source and their `id3v2` source is the container's native tags.

### Charset handling

Auto-detect over a small fixed candidate set (UTF-8 strict → then a scoring
heuristic across Windows-1252, Windows-1251, KOI8-R, Shift-JIS, Latin-1);
decoding via `golang.org/x/text/encoding` (pure Go). The response carries the
detected charset **and** the decoded strings for every candidate charset is
*not* shipped — instead the endpoint accepts `?charset=` so the UI's override
dropdown re-queries and live-previews. Detection is best-effort; the override
is the contract.

## Architecture

### `tagsource` package (new)

```go
type Subject struct {        // what a provider may look at
    Blob     BlobOpener      // live re-read via storages.Registry (nil if blob gone)
    Raw      []uint32        // chromaprint sub-fingerprints (nil if none)
    Duration float64
    Current  media.Tags      // the tagset being edited (text-search seed)
}

type Suggestion struct {
    Source     string        // "id3v2" | "id3v1" | "musicbrainz"
    Label      string        // human label, e.g. "MusicBrainz — Album X (2004)"
    Tags       media.Tags    // candidate fields (zero values = "no opinion")
    Charset    string        // local sources: charset used for this decode
    Confidence float64       // external sources: match score; local: 1
}

type Provider interface {
    Name() string
    Enabled(ctx) bool        // settings-driven; local providers: always true
    Suggest(ctx, Subject) ([]Suggestion, error)
}
```

External providers (MusicBrainz/AcoustID, later Discogs/Last.fm/federation)
are HTTP clients living in `tagsource/`; local providers wrap new `media`
helpers (`media.ReadID3v1`, a raw-frames v2 read). One provider erroring
degrades to "that source shows an error chip" — never fails the endpoint.

### Fingerprint compression

AcoustID's API takes the **compressed base64** fingerprint; we store only the
raw uint32 stream (`fpcalc -raw`). Implement `media.CompressFingerprint(raw
[]uint32) string` — chromaprint's documented packing (3-bit deltas + 5-bit
exception codes + header) — unit-tested against known `fpcalc` raw/compressed
pairs. Pure Go, no schema change, works for every already-analyzed file.
Fallback if this proves flaky in practice: lazy second `fpcalc` run (no
`-raw`) with the result cached in a new nullable column.

### API

```
GET /api/tagsets/{id}/suggestions[?sources=id3v1,musicbrainz][&charset=cp1251]
```

- Tagset-addressed, so it works identically for drafts (My uploads), the
  moderation card, and the admin lenses. Authorization = "may edit this
  tagset": the draft's owner, or `metadata.edit` — same rule as the existing
  metadata PATCH paths.
- **No `sources` param → local sources only.** External providers run only
  when explicitly named — this is the enforcement point for "user-triggered
  only"; the UI's MusicBrainz chip issues the explicit request.
- Server-side proxy only; the browser never talks to MusicBrainz/AcoustID.
  Per-provider global rate limiter (MusicBrainz: 1 req/s, mandatory
  `User-Agent` from `internal/version`; AcoustID: keyed) + a small in-memory
  TTL cache keyed by fingerprint/query. No DB cache table in v1.

### Settings (runtime, `/admin/settings`)

New keys in the generic `settings` table (no migration — key/value):
`tagsource.musicbrainz.enabled` (default **off**), `tagsource.acoustid.api_key`
(free key, admin registers the app once). The settings page gets a "Tag
services" card with the toggle, the key field, and a privacy note: *enabling
this sends acoustic fingerprints / search text to the external service, only
when a user explicitly requests a suggestion*. Local sources have no settings —
always available.

### UI

`track-edit.js` (shared by My uploads, moderation, All Appearances, By entity,
duplicates) gains a **"Suggest tags…"** button beside "Extended edit", opening
a suggestions panel inside the modal:

- One **chip per source** — ID3v2 first (the default reading), ID3v1,
  MusicBrainz (rendered only when enabled in settings; clicking it triggers
  the external lookup with a spinner). Multiple MusicBrainz matches = multiple
  selectable entries under the chip, best confidence first.
- Selecting a candidate shows a **field-diff table** (current value vs.
  suggested, changed rows highlighted) with **Use all** + per-field copy.
  "Use" only fills the modal's inputs — Save remains the one write path.
- Local chips carry the **charset dropdown** (detected value preselected,
  live preview re-queries with `?charset=`).
- Create-mode ("Add appearance") and bulk-edit are out of scope: suggestions
  require a subject file.

## Bulk charset fix (P0.5, owner follow-up)

The per-file panel fixes one track; an uploaded album with a wrong charset is
dozens. The bulk fix reinterprets the **stored** text tags of a whole
selection instead of re-reading files: mojibake from a mis-decoded charset
round-trips losslessly through Latin-1 (`media.ReencodeLatin1`), so values
that don't fit Latin-1 — i.e. text that already reads correctly — are
**provably untouched**, making it safe to select generously. This is
deliberately *not* "pick a tagset source in bulk" (deferred, see below): it
fixes decoding, it never chooses between tag blocks.

- **DB:** `database.RecodeTagsetsText(ids, owner, recode)` — chunked like
  `BulkUpdateTagsetMetadata`; changed fields go through
  `applyMetadataPatchTagsetTx`, so artist/album identity changes re-resolve
  the entity FKs. A valid `owner` narrows the scope to that user's own
  non-trashed draft/returned appearances — the My-uploads path trusts its
  explicit id list no further than ownership.
- **API:** action `"recode"` (+ `charset`) on the two bulk endpoints —
  `POST /api/admin/appearances/bulk` (gated `metadata.edit`) and
  `POST /api/my/uploads/bulk` (owner-scoped). Both accept ids or a filter
  (`all:true` guard as usual); charset validated against `media.ValidCharset`.
- **UI:** shared `charset-edit.js` modal ("Fix charset…" in the My-uploads
  bulk bar and the All Appearances bulk toolbar via `scope.charsetApply` /
  `charsetApplyAll`): charset dropdown + live before-preview of the loaded
  selected rows (title/artist/album, changed cells highlighted). The preview
  is computed client-side with the same Latin-1→`TextDecoder` trick and
  auto-picks the charset that changes the most fields without introducing
  U+FFFD; the server-side apply is authoritative.

## Explicitly out of scope (v1) / deferred

- **Ingest-time lookup and auto-apply** — rejected for v1 by owner decision;
  revisit only as an opt-in "suggestions available" badge, never auto-apply.
- **Cover art fetch** (Cover Art Archive via the matched MBID release) —
  natural follow-up once MusicBrainz IDs flow; needs its own ingest-into-
  `album_images` design.
- **Bulk tagset-source choosing** — applying a chosen suggestion source (ID3v1
  / a service match) across a selection, the bulk sibling of the per-file
  chips. Owner-floated alongside the bulk charset fix; revisit once P1's
  service source exists (a bulk service lookup also needs the rate-limit story
  thought through).
- **Discogs / Last.fm providers** — interface-ready; each is "a client + a
  settings key + a chip".
- **Federation top-tagsets provider** — peers' tagsets ranked by popularity as
  a suggestion source; waits for federation (auth Phase 4 /
  `docs/architecture/federation.md`).
- **Storing provenance** (which source a tagset came from) — becomes
  interesting with federation ranking; not before.

## Phasing

### P0 — local sources (fully offline; no settings, no migration)

**`media` package** (new `media/id3v1.go` + additions to `extract.go`):

- `media.ReadID3v1(r io.ReadSeeker) (*RawID3v1, error)` — seek to the trailing
  128 bytes, check the `TAG` magic, parse title/artist/album/year/comment/
  genre-index; v1.1 track number when `comment[28] == 0 && comment[29] != 0`;
  genre index resolved against the standard ID3v1 genre table. Fields come
  back as **raw bytes**, undecoded — decoding is the charset layer's job.
  `tag.ErrNoTagsFound`-style sentinel when the magic is absent (→ no `id3v1`
  chip for this file).
- `media.DetectCharset(fields [][]byte) string` — UTF-8 strict validation
  first; otherwise a scoring pass over a fixed candidate set (Windows-1252,
  Windows-1251, KOI8-R, Shift-JIS, ISO-8859-1) using
  `golang.org/x/text/encoding/{charmap,japanese}` (x/text is already a direct
  dependency). Returns the canonical name; `DecodeWith(name, b)` is the
  decode primitive the endpoint's `?charset=` override calls into. The
  candidate list is the single source of truth for the API's `charset`
  allowlist and the UI dropdown.
- v2 chip = `media.ExtractTags` re-run on the blob (unchanged code path). The
  v2 re-decode override round-trips the already-decoded string back to
  Latin-1 bytes (lossless for frames dhowden/tag decoded as ISO-8859-1) and
  re-decodes with the chosen charmap — offered on the v2 chip only when the
  file is MP3.
- Unit tests: fixture byte strings (umlauts in cp1252, Cyrillic in cp1251 and
  KOI8-R, Shift-JIS), a hand-built 128-byte v1.1 block, detection
  right/wrong-but-overridable cases.

**`tagsource` package** (new): the `Provider`/`Subject`/`Suggestion` types
from §Architecture; `id3v2` + `id3v1` providers wrapping the `media` helpers;
a small ordered registry (`id3v2`, `id3v1`, externals appended in P1). A
provider error becomes an error entry for that source in the response — never
a non-200.

**`database`**: one new Repository method, e.g.
`SuggestionSubject(ctx, tagsetID)` → origin-file hash, duration, packed
fingerprint (nil if none), current tags, uploader id + review state (for
authz). Origin file (`tagsets.origin_file_id`) is the right blob: it is the
file whose bytes carried the tags. **Gotcha applies:** extend the `api`
package's `fakeRepo`.

**`api`**: `GET /api/tagsets/{id}/suggestions` handler — resolve subject,
authorize (draft owner or `metadata.edit`, mirroring the metadata-PATCH
split), validate `?charset=` against the allowlist, open the blob via the
`storages.Registry` probe, run the local providers. Response sketch:

```json
{
  "suggestions": [
    {"source": "id3v2", "label": "ID3v2.3", "charset": null,
     "tags": {"title": "…", "artist": "…", "album": "…", "…": "…"}},
    {"source": "id3v1", "label": "ID3v1.1", "charset": "windows-1251",
     "charsets": ["utf-8", "windows-1252", "…"], "tags": {"…": "…"}}
  ],
  "external_sources": []
}
```

`external_sources` lists the enabled-but-not-yet-queried external providers
(always empty in P0) so the UI knows which chips to render without a second
config endpoint.

**`webui`**: "Suggest tags…" button in `track-edit.js` beside "Extended edit"
(hidden in create mode — no subject file). The panel renders inside the wide
modal: source chips; per-candidate **diff table** (current vs. suggested,
changed rows highlighted); "Use all" + per-row copy writing into the normal
inputs; the charset `<select>` on local chips re-fetches with `?charset=` and
re-renders that chip only. Styling extends the shared admin/modal CSS —
**no page-local redefinition of shared classes** (toast lesson). Shell-module
rule respected for free (modal code runs at `init()` time).

**Done when:** a cp1251-tagged fixture MP3 uploaded to a dev server shows
mojibake by default, and the ID3v1 chip + charset override produce correct
Cyrillic in the inputs; `go vet`/`go test ./...` green; endpoint denies a
non-owner without `metadata.edit`.

### P1 — MusicBrainz via AcoustID (fingerprint path)

**`media.CompressFingerprint(raw []uint32) string`** — chromaprint's
documented compression (per-value XOR-delta bit encoding: 3-bit normal codes
with 5-bit exception escape, algorithm header, base64) so the stored raw
stream serves AcoustID directly. Tests: table of raw/compressed pairs
captured from real `fpcalc` runs (checked-in constants, not a runtime fpcalc
dependency). Documented fallback if real-world pairs disagree: lazy `fpcalc`
(no `-raw`) re-run, result cached in a new nullable `audio_fingerprints`
column — only then does a migration appear.

**`tagsource/acoustid.go`** — one POST to the AcoustID `lookup` endpoint
(client key + compressed fingerprint + duration,
`meta=recordings+releases+tracks+compress`), which already returns enough to
build full candidate tagsets (title/artist/album/album-artist/year/
track/disc) **without a second service round-trip**; direct MusicBrainz API
calls are deferred to P2 (text search). Score → `Confidence`; candidates
sorted best-first; below-threshold matches dropped.

**Plumbing** — per-provider serializing rate limiter (a token channel, not
just a cap — MusicBrainz's 1 req/s is per-IP and shared) returning a typed
"busy" error the handler maps to HTTP 429 + a friendly panel message; small
mutex-guarded TTL cache (keyed by fingerprint hash / normalized query,
~15 min, capped entries). `User-Agent: madshare/<version> (<git_repo URL>)`
from `internal/version` — MusicBrainz/AcoustID require identification.

**Settings** — keys `tagsource.musicbrainz.enabled` (default off) +
`tagsource.acoustid.api_key` in the generic `settings` table
(`database/settings.go` getters/setters, key/value → **no migration**); API
key stored plaintext (it must be replayable to the service — unlike our own
hashed tokens) and never echoed back in full (`GET` returns set/unset + last
4 chars). Endpoints `GET/POST /api/admin/settings/tagsource` following the
autoderive/trash-policy pattern and gating in `access_handlers.go`; a "Tag
services" card in `admin/settings.js` with toggle, key field, and the privacy
note (fingerprints leave the server only on explicit user request). Setting
changes go to `audit_log` like other admin settings writes.

**UI** — the MusicBrainz chip renders when `external_sources` contains it;
clicking issues the second, explicit request (`?sources=musicbrainz`) with a
spinner; multiple candidates listed best-first with confidence and
release/year in the label; 429 shows "service busy — try again in a moment"
on the chip, not a toast storm.

**Done when:** with a registered AcoustID key on a dev server, a known
commercial track uploaded with stripped tags yields correct
title/artist/album via the chip; with the provider disabled the chip is
absent and `?sources=musicbrainz` is refused; rapid repeat clicks hit the
cache (one outbound request, verified by log).

### P2 — text-search fallback

- Trigger: subject has no fingerprint row (fpcalc absent / analysis pending)
  **or** AcoustID returned nothing above threshold — the MusicBrainz chip
  then offers "Search MusicBrainz" with an **editable query field** prefilled
  from the current artist/title.
- `tagsource/musicbrainz.go`: `GET /ws/2/recording?query=` (Lucene syntax,
  `artist:"…" AND recording:"…"`, duration-windowed `dur:[a TO b]` when known,
  `fmt=json`), same limiter/cache/User-Agent plumbing as P1; top N mapped to
  suggestions with normalized confidence.
- API: the endpoint accepts `&query=` (only honoured with
  `sources=musicbrainz`); empty query falls back to the seeded one.
- **Done when:** a no-fingerprint dev server (fpcalc off PATH) still produces
  service suggestions through the search field.

## Gotchas

- `dhowden/tag` cannot expose v1 and v2 side by side — the v1 read is a
  bespoke trailing-128-byte parse; keep `ExtractTags` (ingest) untouched.
- Blob access must go through the `storages.Registry` probe, not a direct
  `files_dir` path (links storage; GC model: a reaped blob → local sources
  absent, external ones still fine via the stored fingerprint).
- `plain grep` skips nothing here, but any new `Repository` method breaks the
  `api` package's `fakeRepo`, and P0/P1 need **no migration** — keep it that
  way (a migration would also break `database_test.go` assertions).
- MusicBrainz rate limit is per-IP and shared with other apps on the host;
  the limiter must serialize, not just cap, and surface "busy, retry" to the
  panel instead of erroring.
