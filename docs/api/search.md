# Search API

## Endpoint

```
GET /api/search?q=<query>
```

Returns artists, albums, and tracks whose names contain the query string as a case-insensitive substring. All three result sets are returned in a single response.

An artist matches in **either role** — as an album-artist or as a track performer — so searching a performer who only appears on a compilation surfaces both the performer (in `artists`) and their tracks (in `tracks`). See `docs/architecture/artist-album-model.md`.

---

## Request

### Query parameters

| Parameter | Type   | Required | Description |
|-----------|--------|----------|-------------|
| `q`       | string | yes      | Search query. Must be ≤ 200 characters. Fewer than 2 characters produces empty results (the web UI enforces this client-side; the server applies no minimum). |

### Access control

The endpoint respects the caller's identity:

- **Users holding `content.access`** (admins and the built-in content roles) see the full library.
- **Anonymous callers (and authenticated users without `content.access`)** receive only guest-playable content (files with `guest_playable = 1` or auto-derived from a guest license). See `docs/architecture/auth.md` §5.

---

## Response

HTTP 200 with `Content-Type: application/json`.

```json
{
  "artists": [ /* ArtistResult, up to 50 */ ],
  "albums":  [ /* AlbumResult,  up to 50 */ ],
  "tracks":  [ /* TrackResult,  up to 50 */ ]
}
```

All three arrays are always present (never `null`). Each is capped at **50 results**.

### ArtistResult

```json
{
  "name":        "Jesper Kyd",
  "track_count": 12,
  "has_image":   false
}
```

| Field         | Type    | Description |
|---------------|---------|-------------|
| `name`        | string  | Artist name. Matched whether the entity is referenced as an album-artist or as a track performer. |
| `track_count` | integer | Number of accessible tracks crediting this artist in either role. |
| `has_image`   | boolean | Whether an artist image has been uploaded. |

### AlbumResult

```json
{
  "title":       "Assassin's Creed Valhalla (Original Game Soundtrack)",
  "artist_name": "Jesper Kyd",
  "year":        null,
  "track_count": 47,
  "has_image":   false
}
```

| Field         | Type        | Description |
|---------------|-------------|-------------|
| `title`       | string      | Album title. |
| `artist_name` | string      | Album artist (resolved as `album_artist` → `artist`). |
| `year`        | integer\|null | Release year, or `null` if not tagged. |
| `track_count` | integer     | Number of accessible tracks in the album. |
| `has_image`   | boolean     | Whether a cover image has been uploaded. |

### TrackResult

```json
{
  "id":               68,
  "title":            "Ezio's Family - Ascending to Valhalla",
  "track_number":     47,
  "duration_seconds": null,
  "url":              "/files/0f017b.../track.flac",
  "mime_type":        "audio/flac",
  "artist_name":      "Jesper Kyd",
  "album_title":      "Assassin's Creed Valhalla (Original Game Soundtrack)"
}
```

| Field              | Type          | Description |
|--------------------|---------------|-------------|
| `id`               | integer       | File ID. |
| `title`            | string        | Track title (falls back to filename if untagged). |
| `track_number`     | integer\|null | Track number from tags, or `null`. |
| `duration_seconds` | float\|null   | Duration in seconds, or `null` if not yet determined. |
| `url`              | string        | Relative URL to stream or download the file. |
| `mime_type`        | string        | Audio MIME type (e.g. `audio/flac`, `audio/mpeg`). |
| `artist_name`      | string        | The track's **performer** (its `artist_id` entity), which may differ from the album-artist on a compilation. |
| `album_title`      | string        | Album title, or empty string if untagged. |

---

## Error responses

| Status | Condition |
|--------|-----------|
| 400    | `q` exceeds 200 characters. Body: `query too long`. |
| 500    | Internal storage error. |

---

## Search behaviour

- **Substring match:** the query matches anywhere in the field, not only at the start (`q=hall` matches `Valhalla`).
- **Case-insensitive:** matching is performed with `LOWER(…) LIKE LOWER(?)`.
- **Literal wildcards:** `%` and `_` in the query are treated as ordinary characters, not SQL LIKE metacharacters.
- **Match fields:**
  - Artists — matched on the artist name, in either role (album-artist or performer).
  - Albums — matched on the album title.
  - Tracks — matched on the track title **or** the performer name, so an artist query returns their tracks even on a "Various Artists" compilation.
- **Soft-deleted files** are excluded from all result sets.
- **Result order:** alphabetical by name/title within each set.

---

## Examples

```bash
# Search for "valhalla" — returns the matching album and any tracks
curl "http://localhost:3000/api/search?q=valhalla"

# Search with a literal percent sign
curl "http://localhost:3000/api/search?q=100%25"

# Authenticated search (session cookie)
curl -b "session=<token>" "http://localhost:3000/api/search?q=beethoven"
```
