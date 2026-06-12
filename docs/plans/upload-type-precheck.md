# Upload Type Precheck

Add a client-side "is this file an allowed audio type?" check on the upload page,
in the same advisory spirit as the hash precheck: the server stays authoritative,
the check is purely a convenience that flags files before they cost bandwidth.
Disallowed files are skipped with a reason; there is **no UI toggle** (always on).
Folding in a related correctness fix, allowed audio files whose browser MIME is
empty (FLAC/M4A/OPUS, and the documented `curl -F` case) stop being rejected.

## Background

**Server rule** (`api/handlers.go` + `api/upload_handlers.go`): an upload must
pass *both* `allowedMIMETypes` (6 audio MIME types, checked against the multipart
part's `Content-Type`) *and* `allowedExtensions` (8 extensions). The part's
Content-Type comes from the browser's `File.type`.

**Client today** (`webui/static/js/upload.js`):
- `AUDIO_EXTS` already equals the server's `allowedExtensions` exactly.
- `classify()` calls a file `audio` if `file.type` starts with `audio/` **or**
  the extension matches; `image` for JPEG/PNG; everything else `other`, rendered
  as a `skipped` row that is never uploaded.
- The hash precheck (`/api/files/check`) is advisory, has a persisted toggle, and
  falls through to a normal upload on any failure.

**The gaps this closes:**
1. `classify()` accepts anything with an `audio/*` MIME even when the extension
   is not on the server's list — so e.g. a `.weba`/`audio/webm` file is queued,
   uploaded, then 415'd. The client and server disagree.
2. Browsers leave `File.type` **empty** for several allowed formats
   (`.flac`, `.m4a`, `.opus`, often `.ogg`/`.wav`). The empty type becomes
   `application/octet-stream` on the wire, which the server's MIME allow-list
   rejects — so real, allowed-extension audio fails. `curl -F "file=@x.flac"`
   (the example in `CLAUDE.md`) hits the same wall: curl's default part type is
   `application/octet-stream`.
3. The server's two lists are inconsistent: `.aac` and `.opus` are accepted
   extensions but have **no** entry in `allowedMIMETypes`, so no Content-Type can
   satisfy the gate — those formats are effectively un-uploadable today.

## Decisions

1. **Detection = filename extension** (no content read). The client already
   agrees with the server on extensions; this closes the gap with minimal code.
   A file with a valid audio extension but bogus bytes is still caught server-
   side (tag extraction / future sniffing) — acceptable for an advisory check.
2. **Disallowed file → skip with a reason.** Excluded from the upload, shown as a
   row noting "Not an accepted audio format" — an extension of today's `skipped`
   handling. No wasted bandwidth.
3. **Fold in the Content-Type fix** so the client's "allowed" verdict is
   truthful: an approved file must actually be accepted by the server.
4. **No toggle** — unlike the hash precheck, the type check is always on (there is
   no reason to upload a file the server will reject).

Note there is also **no server round-trip**: the type allow-list is static
server config, so the client checks locally. There is no `/api/files/check`
equivalent — the only thing fetched is the allow-list itself (see below).

## Design

### One source of truth — a canonical `ext → MIME` map

Replace the server's two ad-hoc maps (`allowedMIMETypes`, `allowedExtensions`)
with a single canonical map in `api/handlers.go`:

```go
// acceptedAudioTypes maps an accepted file extension to its canonical audio
// MIME. The extension is the security-relevant guard (it determines what the
// file server later advertises); the MIME is what we persist and serve.
var acceptedAudioTypes = map[string]string{
    ".mp3":  "audio/mpeg",
    ".ogg":  "audio/ogg",
    ".oga":  "audio/ogg",
    ".flac": "audio/flac",
    ".wav":  "audio/wav",
    ".mp4":  "audio/mp4",
    ".m4a":  "audio/mp4",
    ".aac":  "audio/aac",
    ".opus": "audio/opus",
}
```

This reconciles the lists (every accepted extension now has a canonical MIME,
fixing `.aac`/`.opus`) and becomes the **single definition** the gate, the config
endpoint, and the client all derive from.

### Server — extension-authoritative gate + persist the canonical MIME

The current gate rejects on a non-listed declared Content-Type. Browser/curl
declared types are unreliable, and the existing code comment already calls the
**extension** the real guard ("the stored filename's extension determines what
the file server advertises"). So:

- Gate on the **extension** only: `canonicalMIME, ok := acceptedAudioTypes[ext]`;
  reject with 415 when absent. The declared part Content-Type is no longer a
  gate.
- **Persist the canonical MIME** (`media_metadata.mime_type` / file record), not
  the client-declared one, so an `octet-stream` upload still stores
  `audio/flac`. Pass it to `extractTagsOrEmpty` too.

Effects: FLAC/M4A/OPUS/AAC and the `curl -F` example now succeed; video stays
rejected (no video extension is accepted — `TestUploadFile_VideoRejected` uses
`clip.bin`); `evil.sh` stays rejected (extension). The serving side already sends
`X-Content-Type-Options: nosniff`, so accepting a loose upload Content-Type does
not weaken what browsers do with the stored file.

### Server — expose the allow-list at `GET /api/ui/config`

`getUIConfig` already augments `UIConfig` with a server-derived field
(`trash_restore_policy`). Add the map the same way:

```go
writeJSON(w, http.StatusOK, struct {
    *config.UIConfig
    TrashRestorePolicy string            `json:"trash_restore_policy"`
    AcceptedAudio      map[string]string `json:"accepted_audio"` // ext → MIME
}{cfg, policy, acceptedAudioTypes})
```

Public and static; the upload page already fetches this endpoint in `init()`.

### Client — precheck, skip-with-reason, correct outgoing MIME

In `webui/static/js/upload.js`:

- **Consume `accepted_audio`** from the `/api/ui/config` response in
  `loadUIConfig()`; keep a small built-in fallback map for older servers/tests so
  the page degrades gracefully (mirrors today's hardcoded `AUDIO_EXTS`).
- **Tighten `classify()`**: a file is `audio` iff its extension is a key in the
  allow-list. Drop the loose `file.type.startsWith('audio/')` acceptance (that is
  the source of gap #1). Images are unchanged; anything else is `other`.
- **Skip with a reason** (decision 2): `other` rows that look like audio-ish but
  aren't accepted read "Not an accepted audio format." Reuse the existing
  `skipped` state and per-row message.
- **Set the outgoing Content-Type** (decision 3) in `uploadXhr()`: when the
  file's own `type` isn't the canonical MIME, append a retyped blob so the part
  carries it:
  ```js
  const want = acceptedAudio[extOf(item.relPath)];
  const blob = (want && item.file.type !== want)
      ? new File([item.file], item.file.name, { type: want })
      : item.file;
  form.append('file', blob, item.relPath);
  ```
- **No toggle**: the type check has no DOM control; the hash precheck toggle is
  untouched.

### No client trust

The server gate stays authoritative — the client check never *lets a bad file
through* (a misnamed file still faces the server). Tightening the client only
skips files the server would reject anyway, which is the whole point: a
convenient, earlier "no." This matches the hash precheck's philosophy, minus the
toggle and minus a round-trip (the allow-list is static).

## Plan

**Phase 1 — Server.**
- Add `acceptedAudioTypes`; remove `allowedMIMETypes` / `allowedExtensions`.
- Rewrite the gate to be extension-authoritative; persist the canonical MIME.
- Add `accepted_audio` to `getUIConfig`.
- Extend `api/*_test.go`: positive cases for `.flac`/`.m4a`/`.opus` with empty /
  `application/octet-stream` Content-Type (previously 415, now accepted, stored
  with the canonical MIME); keep the video + `evil.sh` 415 cases.

**Phase 2 — Client.**
- Consume `accepted_audio` (with built-in fallback); tighten `classify()`;
  skip-with-reason; set the outgoing Content-Type. No toggle.

**Phase 3 — Docs.**
- Update the `CLAUDE.md` note on `/files/upload` (audio MIME gate → extension-
  authoritative) and confirm the documented `curl -F` example now works for non-
  MP3 audio.

**Phase 4 — Verify.**
- `go build ./...`, `go test ./...`, `node --test tests/js/queue-ops.test.mjs`.
- Manual browser: drop a `.flac` (uploads, no 415), a `.txt` (skipped with
  reason), a `.aac`/`.opus` (uploads). `curl -F "file=@x.flac" …/files/upload`
  succeeds with default `octet-stream`.

## Files touched

- `api/handlers.go` — `acceptedAudioTypes`; drop the two old maps.
- `api/upload_handlers.go` — extension-authoritative gate; persist canonical MIME.
- `api/library_handlers.go` — `getUIConfig` adds `accepted_audio`.
- `webui/static/js/upload.js` — consume the list; tighten `classify()`;
  skip-with-reason; outgoing Content-Type.
- `api/handlers_test.go` / `api/upload_handlers_test.go` — extend/adjust.
- `CLAUDE.md` — the `/files/upload` MIME note.

## Security note

No new surface, and arguably tighter: the client now matches the server's
accepted set exactly (fewer doomed uploads), and the server gate keeps the
extension as the guard while persisting a canonical MIME instead of a client-
declared one. All enforcement remains server-side; the client check is advisory.
