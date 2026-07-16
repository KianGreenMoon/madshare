# Tag suggestions — multi-source tagsets

**Status:** designed 2026-07-17, not yet implemented. Owner decisions (consult
round): providers for v1 = **MusicBrainz via AcoustID** (local ID3v1/ID3v2
always included); external lookups are **user-triggered only** (never at
ingest, never auto-applied); ID3v1 charset handling = **auto-detect with a
manual override + live preview**; provider configuration lives in the
**settings table / `/admin/settings`** (runtime, no restart). Builds on the
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

## Explicitly out of scope (v1) / deferred

- **Ingest-time lookup and auto-apply** — rejected for v1 by owner decision;
  revisit only as an opt-in "suggestions available" badge, never auto-apply.
- **Cover art fetch** (Cover Art Archive via the matched MBID release) —
  natural follow-up once MusicBrainz IDs flow; needs its own ingest-into-
  `album_images` design.
- **Discogs / Last.fm providers** — interface-ready; each is "a client + a
  settings key + a chip".
- **Federation top-tagsets provider** — peers' tagsets ranked by popularity as
  a suggestion source; waits for federation (auth Phase 4 /
  `docs/architecture/federation.md`).
- **Storing provenance** (which source a tagset came from) — becomes
  interesting with federation ranking; not before.

## Phasing

- **P0 — local sources.** `media.ReadID3v1` + raw v2 re-read + charset
  detect/override (x/text), `tagsource` package with the two local providers,
  the suggestions endpoint, the modal panel with diff table + charset
  dropdown. Fully offline; no settings, no migration.
- **P1 — MusicBrainz.** `media.CompressFingerprint` (tested against fpcalc
  pairs), AcoustID + MusicBrainz clients, rate limiter + TTL cache, settings
  keys + admin settings card, the MusicBrainz chip (fingerprint path).
- **P2 — text-search fallback.** MusicBrainz search when no fingerprint or no
  AcoustID match (seeded from current tags, editable query field in the
  panel).

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
