# Storage backends & capacity reporting

Uploaded blobs are content-addressed and persisted through the `storage.Storage`
interface (`api/storage/storage.go`). Today there is one implementation,
`storage.Local` (`api/storage/local.go`), which lays files out as
`<files_dir>/audio/<hash>/<filename>` — under an `audio/` subdir
(`storage.AudioSubdir`) so the served blob tree is a sibling of, not a parent
of, the cover-images tree (`<files_dir>/images`); the `/files` server therefore
cannot reach images. The interface is the seam for a future object store (S3);
everything below is written so that backend drops in without changing callers.

On startup the server relocates any pre-split blobs (hash dirs sitting directly
under `<files_dir>`) into `audio/` via `storage.RelocateLegacyBlobs` — a
one-time, idempotent migration.

## The `Storage` interface

`Put` / `Exists` / `DeleteAll` / `BlobPresent` / `VerifyBlob` cover the
content lifecycle (see the interface doc comments). Capacity reporting is one
more method:

```go
Stats() (Stats, error)
```

`Stats` is deliberately backend-neutral:

```go
type Stats struct {
    Backend    string // "local" today, "s3" later
    Location   string // base dir (local) or bucket (object store)
    HasVolume  bool   // true when the byte fields reflect a real filesystem
    TotalBytes uint64 // filesystem capacity      (valid only when HasVolume)
    FreeBytes  uint64 // available to an unprivileged user, df "Avail"
    UsedBytes  uint64 // df-style used: capacity − all free blocks
}
```

- **Local disk** fills the byte fields from a `statfs(2)` of its base directory
  and sets `HasVolume=true`. The `statfs` call is OS-specific, so it lives in
  build-tagged files: `diskusage_unix.go` (`//go:build unix`, the real
  implementation) and `diskusage_other.go` (`//go:build !unix`, returns
  `errNoStatfs` → `Stats` reports `HasVolume=false` instead of failing). The
  deployment target is Linux, so the unix path is what runs.
- **Object stores (future S3)** have no fixed capacity. Such a backend sets
  `HasVolume=false` and leaves the byte fields zero. The app's *own* footprint
  is then the only meaningful figure, and it comes from the database, not the
  backend (next section).

## App footprint vs. volume

Two different numbers, sourced separately:

- **Volume** (whole disk): from `Storage.Stats()`. Local only.
- **Library footprint** (just Madshare's blobs): `Repository.LibraryByteSize`
  (`database/files.go`) — `SELECT COALESCE(SUM(byte_size),0) FROM files`. Files
  are content-addressed (one row per hash), so this is the deduplicated blob
  total. Trashed-but-not-yet-pruned rows are included because their blobs still
  occupy the disk until a hard delete. This figure is backend-agnostic: it is the
  meaningful capacity number for an object store too.

## Admin endpoint

`GET /api/admin/storage` (gated `file.delete`, matching the other
storage-management routes) merges the two:

```json
{
  "backend": "local",
  "location": "./data/files",
  "library_bytes": 12345678,
  "volume": { "total_bytes": N, "free_bytes": N, "used_bytes": N, "used_percent": 42.0 }
}
```

`volume` is `null` when the backend has no fixed capacity (the S3 shape); the
admin dashboard then drops the disk meter and shows only `library_bytes` with a
"no fixed capacity" note. The dashboard (`webui/static/js/admin/dashboard.js`,
panel in `webui/html/admin/dashboard.html`) renders a usage bar split into the
Madshare-library segment and the rest-of-disk-used segment, plus "X free of Y".
