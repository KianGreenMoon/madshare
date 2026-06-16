# Disc numbering

How Madshare models the disc a track belongs to within a multi-disc album: how
it is stored, ordered, grouped, and labelled — and what is deliberately deferred.

## Background: the tag is already a number before we see it

Audio formats carry a disc *number*, not free text, by convention: ID3v2 `TPOS`
("part of a set", e.g. `1` or `1/2`), Vorbis `DISCNUMBER`, MP4 `disk` (a binary
integer). The tag library (`github.com/dhowden/tag`) parses all of these to
`(int, int)` via `m.Disc()` — so by the time `media/extract.go` runs, the raw
string is already gone and we only ever receive an `int`. A non-numeric or absent
tag parses to `0` (the `strconv.Atoi` error is swallowed inside the library).

Storing `disc_number` as an `INTEGER` is therefore the correct model: it is what
the formats encode, what the library hands us, and what ordering needs (a string
column would break numeric sort — `"10" < "2"` lexically — and make disc-then-track
ordering ambiguous). A *named* disc ("Disc 2: Live in Tokyo") is a separate tag
(`DISCSUBTITLE` / `TSST`), handled by a future `disc_subtitle` field (below), not
by stringifying the number.

## The model

`media_metadata.disc_number` is a nullable `INTEGER`. Three states are distinct:

| Value | Meaning |
|---|---|
| `NULL` | **Untagged** — no disc information. |
| `0`    | **Disc zero** — a real, distinct disc (rare; some box sets number a prologue disc `0`). |
| `N` (≥1) | **Disc N** — 1-based by convention. |

`NULL ≠ 0 ≠ 1`: untagged, disc 0, and disc 1 are three *different* discs, never
folded together. (This replaces the old `disc_number || 1` / `COALESCE(…,1)`
folding that collapsed both `NULL` and `0` into disc 1 — the source of the
ordering-vs-grouping inconsistency logged in `.issues/open-issues.md`.)

Four operations, one rule each — the single source of truth is
`webui/static/js/disc.js`, mirrored by the server's `ORDER BY`:

- **Identity / grouping key** — `discKey(n)`: the integer (including `0`), or
  `null` when untagged. Two tracks are on the same disc iff their keys are equal.
- **Order** — `discSort(n)`: numbered discs ascend (`0` before `1`); untagged
  sorts **after** all numbered discs. Server SQL mirrors this with
  `ORDER BY (disc_number IS NULL) ASC, disc_number ASC, …`.
- **Label** — `discLabel(n)`: `"Disc N"` for a number (`"Disc 0"` included),
  `"Disc —"` for an untagged disc.
- **Multi-disc?** — `isMultiDisc(tracks)`: true iff there is **more than one
  distinct disc key**. This is the gate for showing any "Disc N" separators at
  all.

### Single disc → no separator (unchanged common case)

Because the gate is *distinct disc keys > 1*, an album that resolves to one disc
shows no "Disc N" heading regardless of which disc it is: the everyday all-`NULL`
single-disc album, an album that is entirely disc `1`, and an album that is
entirely disc `0` all render as a plain track list. Separators appear only when an
album genuinely spans more than one disc. This is already how it behaves; the
model just makes it consistent for `0` and partially-tagged albums.

### Consumers (kept in lock-step via `disc.js`)

The disc logic was previously duplicated — and had drifted (`|| 1` in one place,
`?? 1` in another) — across three renderers. They now all import `disc.js`:

- `app.js` — library drill-down track list ("Disc N" subheadings).
- `admin/files.js` `renderTracks` — the admin "By entity" track view.
- `file-list.js` — the grouped-by-album file table (`groupedTable` separators +
  `buildArtistGroups` disc-then-track sort), used by admin Files / moderation /
  My-uploads.

Server side, only `database/library.go` `listTracksByAlbumID` orders by disc; its
`ORDER BY` mirrors `discSort`. The flat file-list endpoints return `disc_number`
raw and let `file-list.js` re-sort.

### Behaviour change to note

A **partially-tagged** album (some tracks numbered, some `NULL`) previously folded
its untagged tracks into disc 1 (`COALESCE(disc_number,1)`). Under this model they
become their own `"Disc —"` group, ordered after the numbered discs. More honest,
and rare; called out here so it isn't a surprise.

## Deferred (designed here, built later)

### Preserving a file-tagged `0` through ingest

Today extraction maps `0 → NULL` (`api/handlers.go` `nullInt`, `Valid: i != 0`),
so a file never *extracts* as disc `0` — the only `0`s in the DB come from a
deliberate edit in the metadata editor. That is *why* rendering `0` as a distinct
disc is safe right now: every `0` is intentional.

To preserve an album genuinely **mastered** with a disc tagged `"0"`, extraction
would have to stop trusting `m.Disc() == 0` (which also means "absent" and
"unparseable") and instead detect tag *presence* by reading the raw field
(`m.Raw()` → `TPOS` / `discnumber` / `disk`) and parsing it itself. Low value
(such files are vanishingly rare) and easy to get wrong (a naïve "keep the 0"
would mislabel every untagged file as Disc 0), so it is deferred.

### `disc_subtitle` for named discs

A separate optional `media_metadata.disc_subtitle TEXT` column holds a disc's
human label (`DISCSUBTITLE` / ID3 `TSST`), independent of the number. Contract:

- **Ordering and grouping stay keyed on `disc_number`** — never on the subtitle.
- **Display prefers the subtitle when present**: a disc shows its subtitle instead
  of (or alongside) `"Disc N"`; absent → the `discLabel` fallback.
- Extraction reads `DISCSUBTITLE`/`TSST` from `m.Raw()` and degrades gracefully
  when absent (`dhowden/tag` has no typed accessor for it).
- Editable via the Extended-edit field in both the single (`track-edit.js`) and
  bulk (`bulk-edit.js`) editors.

This is a ~migration + extraction + one editor field + display tweak; written down
so it is a small, well-scoped job the day a named-disc album shows up.

## Scope

**Step 1 (now):** the accurate `disc_number` model above — shared `disc.js`, the
three renderers, and the `listTracksByAlbumID` `ORDER BY`. Makes `NULL`/`0`/`N`
distinct and consistent end-to-end; closes the logged inconsistency.

**Later:** `disc_subtitle`, and (if ever needed) file-tagged-`0` ingest.
