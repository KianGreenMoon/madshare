# Library page — the local browse surface

`/library` is the page a person listens from: a Jellyfin-style drill-down over
**this server's own** library. `/madnetwork` is its sibling over the merged
network catalog (`docs/ui/madnetwork-page.md`); the two share their row builders,
menus and search view, and where they deliberately differ is called out below.

This doc is the behavioural spec — written for whoever builds a second client
(`docs/ui/madplayer.md`) as much as for the web UI that implements it today.
The web-UI specifics are `webui/html/library.html` + `webui/static/js/app.js`,
over the shared core `browse-rows.js` / `quick-add.js` / `browse-search.js`.

Related: `docs/ui/artists-and-performers.md` (which names are artist rows — read
it before writing the artist list), `docs/architecture/artist-album-model.md`
(the entity model), `docs/ui/player-and-queue.md` (what happens after a click),
`docs/architecture/recording-tagsets.md` (why a track is a `tagset_id`).

## The page

```
Library                                    ← header section tab
[ Music | Playlists ]                      ← subtabs (docs/ui/shells.md)
[ 🔍 Search artists, albums, tracks…    ]  ← always visible, above both views
┌───────────────────────────────────────┐
│ Library › Air › Moon Safari            │  ← breadcrumb, hidden at the top level
├───────────────────────────────────────┤
│  1  La femme d'argent      ♥  ⋯  7:07 │  ← the drill panel
│  2  Sexy Boy               ♥  ⋯  4:58 │
└───────────────────────────────────────┘
```

Two view panels swap in place: the **browse view** (breadcrumb + panel) and the
**search-results view**. The search box sits above both, so typing switches views
without moving the control that did it, and clearing returns to exactly the drill
level you left.

It is a shell-native page — playback and the shared queue survive navigating to
it and away (`docs/ui/shells.md`). Re-entering the library resets the drill to the
artist list; the drill level is deliberately **not** in the URL (see §Open).

## The drill

Three levels, each addressed by a **stable entity id**, never by name:

| Level | Fetch | Notes |
|---|---|---|
| Artists | `GET /api/artists?limit=80[&cursor=]` | cursor-paged; the only unbounded level |
| Albums of an artist | `GET /api/albums?artist_id=<id>` | bounded — fetched and rendered whole |
| Tracks of an album | `GET /api/tracks?album_id=<id>` | bounded — fetched and rendered whole |

**Which names appear in the artist list is a server decision** — album artists,
carrying their guest appearances; a performer who never released anything of
their own is a search hit rather than a row. `docs/ui/artists-and-performers.md`
is the contract and §"The one way to get this wrong" there is the mistake to
avoid: never build an artist list by grouping track rows.

**An album always opens whole.** It is addressed by id, and an id names the
release, not a slice of it — so a compilation reached from a performer's album
list shows all 25 tracks even though the row that led there counted the 7 that
are theirs. The header names the album artist, which is the explanation. (The
network page differs here on purpose: there an album is addressed by an
*(artist, album)* text pair, so the artist is half of the address.)

### Paging and windowing

The artist list is the one surface that has no natural bound, so it is both
**cursor-paged** and **windowed**: `{items, next_cursor}` pages of 80 stream in as
the window scrolls, and only on-screen rows exist in the DOM
(`docs/architecture/infinite-scroll-virtualization.md`). The cursor is **opaque** —
pass back exactly what `next_cursor` gave you; never construct one.

`GET /api/artists` answers two shapes: with `limit` it pages as above, without it
it returns the whole list as a bare array (which is what the admin By-entity view
wants). A client picking the second shape on a large library has chosen to load
everything.

Albums and tracks per entity are bounded by the entity itself and are rendered
whole, with no paging and no windowing.

### Breadcrumb

`Library › <artist> › <album>`, where every step but the last is a link and
**the whole bar is hidden at the artist level** — there is nothing to step back
to, and an empty strip is worse than no strip. The section's subtab is the label
and the back-to-top affordance, so the breadcrumb never repeats "Library" as a
section name (`docs/ui/shells.md` §Library section + subtabs).

Missing names render as **`Unknown Artist`** / **`Other`**, the same buckets that
sort last in every list.

## Rows

All three row types come from `browse-rows.js`, so they are the same components
the network page uses.

**Artist row** — name, `N tracks`, a ⋯ menu, a chevron. No artwork: the artist
list is the long one, and it is text.

**Album row** — cover thumbnail (`GET /api/albums/{id}/image?size=small` when
`has_image`, else a note placeholder), title, `<year> · N tracks`, ⋯, chevron.

**Track row** — number, title, **performer** (the track's own credit, not the
album artist — that is what makes a compilation readable), heart, ⋯, duration.

- The **number** is `track_number` when tagged. Untagged, it falls back to the
  row's position — per disc on a multi-disc album, per list otherwise — so a
  column of numbers never has holes in it.
- **Multi-disc albums get `Disc N` subheadings.** "Multi-disc" and the disc's
  label follow one shared rule where untagged / `0` / `N` are three *distinct*
  discs (`docs/architecture/disc-numbering.md`). Headers are **visual only**: the
  queue stays one flat ordered list, so a header must never shift a track index.
- **The heart is the one favourites control** on a row — there is no "Add to
  favourites" menu item, deliberately (it was dropped as redundant when the
  browse core was extracted).

### Row identity

A library track's listening identity is its **`tagset_id`** — the *appearance* —
and that is what favourites, playlists and the renditions endpoint key on. The
row also carries `hash`, which is the **origin blob** and exists for admin
surfaces; a track whose origin blob is gone still plays, because the play `url`
resolves server-side to a surviving rendition of the same recording.

The UI's row key is `ts:<tagset_id>`, falling back to `url:<url>` for a row with
no tagset. It decides two things, and the fallback is why it is a key rather than
just the id: **which row is highlighted as playing**, and **what a click on the
playing row does**. Clicking the currently-playing row toggles pause; clicking
any other row — *including a different appearance of the same audio* — starts it
fresh. That distinction is the whole reason the appearance, not the file, is the
identity.

### The ⋯ menu

`Play next · Add to queue · Add to playlist… · Download`

**Download is last, and it means save-to-device**: a plain anchor download of the
same-origin `/files/…` URL, no endpoint of its own. The word is reserved —
fetching *remote* content into this server's library is **Materialize**, and it
only exists on the network page (`docs/ui/madnetwork-page.md`). Track rows only;
there is no album or artist zip.

Artist and album ⋯ menus carry the three queue items, collecting their tracks
from the browse endpoints first (an artist's menu fetches its albums, then each
album's tracks, in browse order).

## Clicking a track

Clicking a track row calls `setQueue(<this view's tracks>, <index>)` — **the view
you clicked in becomes the queue**, in the order shown. Browsing never changes
the queue; only a click or an explicit queue edit does. If the queue had been
edited by hand, the replacement offers an undo. All of that is
`docs/ui/player-and-queue.md`, which a second client should implement from rather
than from this page.

## Durations

`duration_seconds` comes from `ffprobe` at ingest, so it is present for anything
analysed and absent for anything that arrived before the analysis pass or on a
server with no `ffprobe`. A row with no duration shows `—` and the page then
**probes the file itself in the background** (a metadata-only media load, four at
a time), writing what it learns into a `localStorage` cache shared with the queue
panel and the playlists page — so the second visit shows durations with no
fetch at all.

A native client with a real decoder can do better than the browser hack, but the
shape of the fix is the same and the cache is worth keeping: **the row renders
immediately with `—` rather than waiting**.

## Search

One input, **2+ characters, 300 ms debounce**, `GET /api/search?q=`. Results
render as three sections — **Artists / Albums / Tracks** — where an artist or
album hit **drills** (and clears the search) and a track hit **plays**. Escape or
the ✕ clears and returns to the browse view. The same machinery, the same shape
and the same endpoint contract as `/api/madnetwork/search`.

**Search matches both artist roles.** A performer with no release of their own is
a hit here even though they are not a row in the browse list — which is what
keeps rule 1 of `artists-and-performers.md` a display rule rather than missing
data. A client that ships browse without search has turned it into missing data.

Matching is a case-insensitive substring on artist name (either role), album
title and track title; each section is capped at 50. One thing deliberately *not*
done: a matching album does not also spill its tracks into the Tracks section —
that was tried and reverted, because those rows duplicate an album row already on
screen, whereas performer matches add rows reachable no other way. Server-side
details: `docs/api/search.md`.

## What an anonymous or restricted caller sees

The browse endpoints **narrow rather than refuse**. A caller without
`content.access` — anonymous, or an account that simply lacks it — gets the
**guest listing**: only guest-playable content, filtered in SQL, with the same
response shape. So a client must not treat "empty library" as "not signed in";
`GET /api/auth/me` is the question about identity.

Playback is enforced separately and for real: `/files/*` is default-deny and
checks the same access rules per blob (`docs/architecture/auth.md`,
`docs/architecture/license-access.md`).

## Empty states

| Case | What the page says |
|---|---|
| No artists at all | *No music yet.* + a link to `/upload` |
| An artist with no albums | *No albums found.* |
| An album with no tracks | *No tracks found.* |
| A failed fetch | *Failed to load library.* (`role="alert"`) — never an empty list, which would read as "you have no music" |

That last row is the one worth keeping in a second client: **a load failure and
an empty library must not look the same.**

## Hardware back

The page exposes `window.__madshareBack`, which the Capacitor Android shell calls
before doing its own history handling (`docs/architecture/android-app.md`): it
closes an open search, else drills one level up, else returns false so native
takes over. It exists because the drill has no browser-history entries — see
below.

## Open

- **The drill level is not addressable.** `/library` always opens on the artist
  list; an artist or album has no URL, and Back leaves the page rather than
  stepping up. The madnetwork section solved the equivalent problem for *nodes*
  by giving them real addresses (`docs/ui/madnetwork-nodes.md` §"Why a node needs
  an address"), and the same argument applies here — a shared link to an album is
  a reasonable thing to want. Not scheduled.
- **Artist artwork in the artist list.** The variants exist (`artist_images`); the
  list is deliberately text today.
