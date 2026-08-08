# Artists & performers — what a browse surface shows

Every Madshare browse surface answers one question the same way: **which names get
a row in an artist list, and what is under each one.** This doc is the reference
for that answer. It is written for whoever builds the *next* UI — a native
client, the Android shell, a third-party player against the API — as much as for
the two that exist.

The rule is enforced **server-side**, in both catalogs. A client that renders
what the endpoints return implements it for free; a client that re-derives artist
lists from track rows breaks it (see §"The one way to get this wrong").

Related: `docs/architecture/artist-album-model.md` (the entity model and the SQL
behind the local library), `docs/ui/library-page.md` and
`docs/ui/madnetwork-page.md` §"Artist identity" (the two browse surfaces that
implement this), `docs/ui/madplayer.md` (the client this was written for).

## The rule

A track carries two artist names: an **album artist** (the release it belongs to)
and a **performer** (who plays *this* track). On a normal release they are the
same name. They differ on compilations, soundtracks and features:

```
Album artist: The Witcher 3: Wild Hunt      ← the release
Performer:    Piotr Musiał                  ← this track
```

From that, three behaviours:

1. **An artist list shows ALBUM ARTISTS.** A name that only ever appears as a
   performer on other people's releases is **not** a row. Otherwise every
   soundtrack turns the grid into a credits roll — twenty session musicians with
   one track each, between the bands the listener came for.
2. **An artist who has a release of their own is a row — and brings their guest
   appearances with them.** Their album list, track counts and album counts
   include the compilations they only play on, counting just their own tracks
   there. Being on somebody else's record must not cost an artist the tracks that
   are theirs.
3. **Search matches BOTH roles.** A performer with no release of their own is a
   search hit, and their appearances are one drill away. Rule 1 keeps the grid
   readable; it does not make anyone unreachable.

Worked example, one library:

| Surface | Shows |
|---|---|
| Artist list | `The Witcher 3: Wild Hunt`, `Piotr Musiał` — **not** `Marcin Przybyłowicz` (performer only) |
| `Piotr Musiał` → albums | `Frostpunk (OST)`, `Frostpunk Expansions (OST)`, and `The Witcher 3 — Blood and Wine` **counting his 7 tracks**, not all 25 |
| Search "przyb" | `Marcin Przybyłowicz` as a clickable artist → the soundtracks he plays on |

## Where each surface gets it

Both catalogs implement all three behaviours. They differ only in how an artist
is addressed — by entity id locally, by name on the network, because a merged
catalog of other nodes' text has no ids to share.

| | Local library | Madnetwork (merged catalog) |
|---|---|---|
| Artist list | `GET /api/artists` (optionally `?limit=&cursor=`) | `GET /api/madnetwork/artists?q=&limit=&cursor=` (also per node, `?source=<key>`) |
| Albums of an artist | `GET /api/albums?artist_id=<id>` | `GET /api/madnetwork/albums?artist=<name>` |
| Tracks of an album | `GET /api/tracks?album_id=<id>` | `GET /api/madnetwork/tracks?artist=<name>&album=<title>` |
| Search (both roles) | `GET /api/search?q=` | `GET /api/madnetwork/search?q=` |

**One deliberate difference.** Opening a compilation *from a performer's album
list*:

- **Library** — shows the whole album. It is addressed by id, and an id names the
  release, not a slice of it. (The count you clicked said "7 tracks" and the album
  has 25; the header names the album artist, which is the explanation.)
- **Madnetwork** — shows only that performer's tracks on it. An album there is
  addressed by the *(artist, album)* text pair, so the artist is half of the
  address; the whole compilation is one click away under its own album artist.

Both are defensible; a new UI should follow whichever catalog it is reading
rather than trying to normalise the two.

## What a row displays

Inside a track list, the per-row artist is the **performer** — the track's own
credit — while the header/breadcrumb names the album artist. That is what makes a
compilation readable: `Various Artists` above, the actual singer on each line.
The local library (`/api/tracks` → `artist_name`) and the network browse
(`/api/madnetwork/tracks` → `artist` on each merged track) both send it.

Optional, and currently done by neither UI: marking a guest appearance in an
artist's album list (e.g. a small "appears on" tag on the albums that reached the
list through rule 2). Nothing in the API prevents it — the album's track count is
already the artist's own count, which is the signal.

## The one way to get this wrong

**Do not build an artist list by grouping track rows client-side.** It is the
obvious shortcut — you already have tracks with an `artist_name` on each — and it
silently reverses rule 1: every session musician becomes an artist row, which is
the exact clutter the rule exists to prevent. Worse, it disagrees with the server
about who exists, so search and browse start naming different sets.

Ask the artist endpoints for artist lists. They are cursor-paged for exactly this
reason.

Two more that follow from the same principle:

- **Don't filter or re-count the artist list.** The counts already answer "what
  will I see if I click this" — an artist row counts both credits, an album row
  under a performer counts only their tracks.
- **Don't drop the search path.** A UI that offers browse but no search makes
  every performer-only artist unreachable, which turns a display rule into
  missing data.

## Edge cases

- **Performer == album artist** (the normal release): one name, one row, counted
  once. The two-credit machinery is invisible.
- **No album artist tag**: the performer becomes the album artist for grouping —
  so an artist whose files are untagged that way is still a row, not a
  disappearance. (Local: `effectiveArtist`. Network: the `akey` fallback.)
- **Neither tag**: the track lands in the **"Unknown artist"** bucket, which sorts
  **last** in every list, after the named artists — as does the **"Other"** album
  bucket. A new UI should keep that placement; it is the one row nobody is looking
  for.
- **`"A feat. B"`** is one name, not two. Multi-credit parsing is deferred
  (`docs/architecture/artist-album-model.md`), so such a string is its own artist
  and follows the same rule as any other name.
- **A renamed or merged artist** keeps its identity locally (entity ids); on the
  network the bucket is the name, so two spellings of one artist are two buckets
  until the origin node fixes its tags. Case differences alone always fold.

## Where it lives in the code

| | Local library | Madnetwork |
|---|---|---|
| List (album artists only) | `database/library.go` — `listArtists`, `ListArtistsPage` | `database/madnetwork.go` — `MadnetworkArtists` (`HAVING MAX(album_artist_credit) = 1`) |
| Both credits | `listAlbumsByArtistID` (`al.artist_id = ? OR m.artist_id = ?`) | `fedcatCreditBase` (each row emitted under `akey` and, when different, `pkey`) |
| Search, both roles | `Search` (`m.album_artist_id = a.id OR m.artist_id = a.id`) | `MadnetworkSearchArtists` |

Pinned by `TestUnifiedArtistBrowse_PerformerOnCompilation` (library),
`TestMadnetworkPerformerCredits` and `TestMadnetworkOwnPerformerCredits`
(network). If a change makes one of those fail, it is changing this contract —
update this doc in the same commit.
