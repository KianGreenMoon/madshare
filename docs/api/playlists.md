# Playlists & Favorites API

Per-user, private playlists plus a per-user **favorites** system playlist.
Favorites is just a playlist with
`kind: "favorites"` — one per user, created lazily, not renamable or deletable;
the Like endpoints below toggle its membership.

## Access control

Every endpoint requires an **authenticated user** (session cookie or API token)
holding `content.access`; anonymous requests get **401**, users without the
permission **403**. The endpoints are only registered when auth is configured.
Playlists are scoped to the calling user: another user's playlist id returns
**404** (never 403, so ids don't leak existence).

## Endpoints

| Action | Endpoint | Body |
|--------|----------|------|
| List my playlists | `GET /api/playlists` | — |
| Create | `POST /api/playlists` | `{"name", "tagset_ids"?: []}` |
| Detail (with items) | `GET /api/playlists/{id}` | — |
| Rename | `PATCH /api/playlists/{id}` | `{"name"}` |
| Delete | `DELETE /api/playlists/{id}` | — |
| Append items | `POST /api/playlists/{id}/items` | `{"tagset_ids": []}` |
| Remove one item | `DELETE /api/playlists/{id}/items/{itemId}` | — |
| Reorder | `PUT /api/playlists/{id}/items` | `{"item_ids": []}` |
| Toggle Like | `POST /api/favorites/{tagsetId}` | — |
| Liked tagset ids | `GET /api/favorites` | — |

A track is addressed by its **tagset id** — the specific appearance the user
picked (docs/architecture/recording-tagsets.md, decision 4); the item's play
URL is resolved server-side to the recording's ladder-best surviving rendition.

Limits: playlist names ≤ 200 characters (trimmed, non-blank); at most 5000
ids per request.

---

## Playlist objects

`GET /api/playlists` returns the user's playlists, favorites first (the
favorites row is created on first call), then regular playlists by name:

```json
[
  { "id": 1, "name": "Favorites", "kind": "favorites", "track_count": 4, "updated_at": 1765400000 },
  { "id": 3, "name": "Road Trip", "kind": "regular",   "track_count": 12, "updated_at": 1765400100 }
]
```

`POST /api/playlists` creates a regular playlist, optionally seeded with items
(`tagset_ids`, in order — this is the web UI's "Save queue as playlist" path)
and returns **201** with the same summary shape.

`GET /api/playlists/{id}` adds the items in playlist order:

```json
{
  "id": 3, "name": "Road Trip", "kind": "regular", "track_count": 2, "updated_at": 1765400100,
  "items": [
    {
      "item_id": 17,
      "tagset_id": 42,
      "url": "/files/0f017b…/track.flac",
      "mime_type": "audio/flac",
      "title": "Ezio's Family",
      "artist": "Jesper Kyd",
      "album": "Assassin's Creed Valhalla",
      "duration_seconds": null,
      "status": "ok"
    },
    { "item_id": 18, "status": "trashed", "...": "…" }
  ]
}
```

| Item field | Notes |
|------------|-------|
| `item_id`  | Stable id for remove/reorder. Regular playlists may contain the same track twice, hence item-level ids. |
| `status`   | `"ok"` or `"trashed"`. Unavailable appearances (trashed/unapproved tagset, or a dormant recording with no surviving rendition) stay listed with their metadata but are not playable (the UI grays them out); restoring revives them in place. A **hard-deleted** tagset disappears from playlists entirely (FK cascade). |

## Item operations

- **Append** (`POST …/items {"tagset_ids": […]}`): atomic — every id must name
  a visible appearance (approved, non-trashed, playable), or the whole batch is
  rejected with 400. On the favorites playlist, already-present appearances are
  skipped. Response: `{"ok": true, "added": n}`.
- **Remove** (`DELETE …/items/{itemId}`): 404 when the item isn't in that
  playlist.
- **Reorder** (`PUT …/items {"item_ids": […]}`): the list must be a permutation
  of the playlist's current item ids (400 otherwise); positions are rewritten
  1..n in one transaction.

## Favorites / Like

- `POST /api/favorites/{tagsetId}` toggles the appearance's membership in the
  user's favorites playlist (creating it on first use) and returns the new
  state: `{"liked": true|false}`. Unknown or unavailable tagsets → 404.
- `GET /api/favorites` returns `{"tagset_ids": [ … ]}` — the user's liked
  appearances in playlist order, **excluding unavailable ones** (used by the
  web UI to paint the hearts).

## Error responses

| Status | Condition |
|--------|-----------|
| 400 | Blank/overlong name; empty or oversized batch; unknown/unavailable tagset in an add or create; reorder ids not a permutation. |
| 401 / 403 | Anonymous / missing `content.access`; **403** also for rename/delete of the favorites playlist. |
| 404 | Playlist id not owned by the caller; unknown item id; unknown/unavailable tagset on Like. |
| 500 | Internal storage error. |

## Examples

```bash
# Save a playlist with two tracks
curl -b "madshare_session=<token>" -X POST http://localhost:3000/api/playlists \
  -H 'Content-Type: application/json' \
  -d '{"name":"Road Trip","tagset_ids":[42, 57]}'

# Like a track / un-like it (same call)
curl -b "madshare_session=<token>" -X POST http://localhost:3000/api/favorites/42

# Reorder: full new ordering by item id
curl -b "madshare_session=<token>" -X PUT http://localhost:3000/api/playlists/3/items \
  -H 'Content-Type: application/json' -d '{"item_ids":[18,17]}'
```
