# Recordings — implementation plan

Actionable build plan for the design in
`docs/architecture/recordings.md` (read that first — this doc assumes its
model, ladder, and degradation rules). Tracks **how** to land the feature,
phase by phase, with the migrations, packages, tests, and doc updates each
phase touches.

**Status:** not started. **Schema baseline:** latest migration is `018`, schema
version `18`; recordings migrations start at **`019`**.

## Cross-cutting principles (hold in every phase)

- **Binaries are optional, startup warns-never-fails.** `ffprobe` and `fpcalc`
  degrade independently (see Graceful degradation in the design doc). Every
  phase must keep working with neither, one, or both present.
- **Suggestions, never auto-action.** No auto-merge, no auto-delete. Grouping is
  a candidate; deletion is human-confirmed (reuses soft-delete); splits are
  human-pinned.
- **Derived flags, not stored state.** The duplicate flag (fingerprint or
  tag-collision fallback) is derived at submit time — no new column on the
  review state machine.
- **Mirror existing patterns.** The job queue copies `image_processing_jobs` +
  `imageproc.Pool` (migration `009`, `imageproc/worker.go`); the resolver copies
  the artist/album startup reconcile (`FoldUnknownBuckets` in `madshare.go`).

### Per-migration tripwires (apply to 019 and 020)

- `database/database_test.go` asserts the schema version (currently `18`, two
  places) and the sorted table list — bump both per migration.
- A new `database.Repository` method breaks `api`'s `fakeRepo`
  (`api/handlers_test.go`) — add a stub for each.

---

## P0 — Ingest media analysis (binaries + job queue) ✅ Done

The foundation: a job queue that runs `ffprobe` + `fpcalc` at ingest, fills the
tech columns, and stores the fingerprint. Pure data layer — nothing user-facing.

**Shipped:** migration `019`; `media/analyze.go` (`ProbeTech`,
`ComputeFingerprint`, `ToolStatus`, packed-fingerprint round-trip);
`database/analysis.go` (queue + `UpsertTechColumns` + `InsertAudioFingerprint` +
`FilesNeedingAnalysis`); `mediaproc.Pool`; `madshare.go` wiring (tool detection,
stale reset, backfill) + inline enqueue on upload; `EnqueueAnalysisJob` on
`Repository` (worker methods kept off it via `mediaproc.Repository`); tests in
`media/`, `database/`, `mediaproc/`; schema version `18`→`19`; `fakeRepo` stub.

**Migration `019_media_analysis.sql`**
- `media_analysis_jobs` table — copy `image_processing_jobs` shape: `id`,
  `file_id`, `status` (`pending|running|done|failed`), `error`, `retry_count`,
  timestamps; partial unique index on `file_id WHERE status IN ('pending','running')`
  for enqueue idempotency.
- `audio_fingerprints` table — exactly the design-doc schema (`file_id` PK,
  `algo`, `algo_version`, `duration`, `fingerprint` BLOB, `created_at`).
- `ALTER TABLE media_metadata ADD COLUMN bit_depth INTEGER;` (the one missing
  tech column; the rest already exist, NULL).

**`media/analyze.go` (new)**
- `type TechInfo struct{ DurationSeconds float64; Bitrate, SampleRate, Channels, BitDepth int; Codec string }`
- `type Fingerprint struct{ Algo, AlgoVersion string; Duration float64; Data []byte }`
- `func ProbeTech(path string) (*TechInfo, error)` — shells `ffprobe -v quiet
  -print_format json -show_streams`; parses the first audio stream. Mirrors the
  no-CGo `exec.Command` pattern.
- `func Fingerprint(path string) (*Fingerprint, error)` — shells `fpcalc -json`.
- `func ToolStatus() (ffprobe, fpcalc bool)` via `exec.LookPath`, called once at
  startup to log a warning per missing tool.

**`database/analysis.go` (new)** — `EnqueueAnalysisJob`, `ClaimAnalysisJob`,
`FinishAnalysisJob`, `ResetStaleAnalysisJobs` (boot reset of `running`→`pending`),
`UpsertTechColumns`, `InsertFingerprint`, `FilesNeedingAnalysis` (backfill query:
no fingerprint row / NULL tech). Add to the `Repository` interface.

**`mediaproc/` package (new)** — clone `imageproc/worker.go`: `Pool`, `NewPool`,
`Notify`, `Start`, claim→process→finish loop. `process` runs whichever tools are
present (skip ffprobe if absent → leave tech NULL; skip fpcalc if absent → no
fingerprint row), writes results, then (P1) calls the resolver.

**`madshare.go` wiring** — after the existing reconcile passes: log tool status,
`ResetStaleAnalysisJobs`, launch `mediaproc.Pool`, enqueue backfill for
`FilesNeedingAnalysis`. Enqueue inline on each successful upload
(`api/upload_handlers.go`, next to the existing image-job enqueue).

**Tests**
- `media/analyze_test.go` — parse fixed `ffprobe`/`fpcalc` JSON (golden strings);
  `ToolStatus`. Integration variants `t.Skip` when the binary is absent so CI
  without the tools stays green.
- `mediaproc/worker_test.go` — mirror `imageproc/worker_test.go`: claim→finish,
  stale reset, both-tools-absent no-op path.
- `database` tests — job claim idempotency, fingerprint upsert, `FilesNeedingAnalysis`.
- `database_test.go` — bump version `18`→`19`, add 3 tables to the list.
- `api` `fakeRepo` — stubs for the new methods.

**Docs** — `recordings.md` P0 → in progress; `docs/building.md` gains an
"optional runtime tools (ffprobe, fpcalc)" note; `CLAUDE.md` migrations line
(latest `019`) and a one-line media-analysis mention.

---

## P1 — Recording overlay + resolver ✅ Done

**Shipped:** migration `020`; `database/recordings.go` (`ResolveRecording`,
`BackfillRecordings`, `FilesNeedingRecording`, duration-shortlisted matching via
`media.BitErrorRate` with a conservative `maxBitErrorRate`, `RankRenditions` +
codec classes); resolver wired inline into `mediaproc` (post-fingerprint) and at
startup in `madshare.go`; `ResolveRecording` added to `mediaproc.Repository`
(not the api `Repository`, so `fakeRepo` is untouched); tests in `database/` +
`media/`; schema version `19`→`20`.

**Migration `020_recordings.sql`** — `recordings` table (with
`preferred_file_id` nullable), `files.recording_id` + `files.recording_pinned`,
`idx_files_recording`. Per the design-doc schema.

**`database/recordings.go` (new)** — `CreateRecording`, `AssignRecording`,
`FingerprintShortlist` (coarse bucket / same tag-key cluster, per design
"Matching at scale"), `FilesNeedingRecording` (NULL `recording_id`,
`recording_pinned = 0`). Add to `Repository`.

**Resolver (`mediaproc` or a small `recordings` helper)**
- `matchFingerprint(fp, shortlist) (recordingID, ok)` — Hamming/bit-error
  threshold; documented constant.
- Inline: end of the analysis job, assign to matched recording or create one;
  skip pinned files.
- Startup backfill: idempotent pass over `FilesNeedingRecording` (fingerprints
  already computed in P0's backfill).

**Quality ladder** — `func RankRenditions([]Rendition) []Rendition` pure
function: codec class → (lossy: bitrate / lossless: sample-rate, bit-depth) →
size. **Degraded path:** when tech columns are NULL, fall back to
codec-class-from-MIME + size only.

**Tests** — same fingerprint → one recording; different → separate; pinned files
never re-merged; ladder order incl. the lossy/lossless branch and the
NULL-columns degraded ordering; backfill idempotency. Migration tripwires (`19`→`20`,
+1 table, fakeRepo).

**Docs** — `recordings.md` P1 → done; `CLAUDE.md` migrations line (`020`).

---

## P2 — Duplicates admin page ✅ Done

**Shipped:** `database.ListDuplicateRecordings` + `SplitRendition` (both on the
api `Repository`, with `fakeRepo` stubs); `api/duplicates_handlers.go`
(`GET /api/admin/duplicates` ranked via `RankRenditions` + keep/variant
suggestion, `POST /api/admin/duplicates/{file_id}/split`), gated
`content.moderate`; `/admin/duplicates` page (`webui/html/admin/duplicates.html`
+ `static/js/admin/duplicates.js` + `admin-duplicates.css`) with a page-local
preview player, registered in `adminSubPages`, nav link + dashboard card; delete
reuses the soft-delete endpoint. Tests in `database/` + `api/`.

**API (`api/`)** — `GET /api/admin/duplicates`: recordings with >1 non-trashed
rendition, each with renditions side-by-side (format, bitrate, sample-rate,
duration, size, ladder rank, best-marked) + a derived **suggestion** string.
`POST /api/admin/duplicates/split` — detach a rendition into a new recording and
set `recording_pinned = 1`. Delete reuses the existing soft-delete endpoint. All
gated under the `admin` group (already `RequirePermission`), plus
`content.moderate` semantics.

**webui** — `/admin/duplicates` sub-page (`webui.RegisterAdminPage`), JS reusing
the shared `track-edit.js` modal, a page-local preview player, and shared
`toast.js`. Tech compare shows format/size only when ffprobe was absent.

**Tests** — handler list shape, split-off pins + creates a recording, soft-delete
path, `content.moderate` authz (gate + 404 for others). JS: none required beyond
existing patterns unless new index math is added.

**Docs** — `recordings.md` P2 → done; add the page to the admin sub-pages list in
`CLAUDE.md`; cross-link from `docs/architecture/file-management-view.md`.

---

## P3 — Moderation integration (incl. tag-collision fallback) ✅ Done

**Shipped:** `database.IsDuplicateSubmission` (fingerprint→recording-sibling, or
non-default `title+artist+album` tag fallback; untagged files excluded), on the
api `Repository` with a `fakeRepo` stub. `submitMyUploads` now suppresses
self-approve per-hash for duplicates and returns `flagged` + a `warning`;
`moderationList` sets `duplicate` on each row; the upload page shows the warning
toast and the review-scope badge reads "possible duplicate". Tests in `database/`
(both paths, approved-sibling + untagged exclusion) and `api/` (self-approve
suppressed / moderator non-dup approves / non-moderator queues).

**Derived duplicate flag** — `func (h *handler) duplicateFlagged(ctx, hash) bool`:
- Fingerprint present → flagged iff the file's recording already has another
  **approved, non-trashed** rendition.
- Fingerprint absent (fpcalc missing) → **tag-collision fallback**: flagged iff
  another approved, non-trashed file shares the same **non-default**
  `artist + album + title` (exclude `Unknown artist` / `Other` / filename
  defaults — see design Graceful degradation).

**Self-approve suppression** — in `submitMyUploads` (`api/review_handlers.go:182`):
`selfApprove := id.Has(auth.PermContentModerate) && !duplicateFlagged(...)`.
Return a `warning` field so the upload page raises a popup (same channel as the
post-upload info notice).

**Moderation queue** — `moderationList` payload highlights "variant of an
existing recording" and carries the existing rendition(s) for side-by-side
compare (tech, or format/size when degraded).

**Tests** — self-approve suppressed for both fingerprint-dup and tag-collision-dup
even for a moderator; default-key exclusion (two untagged uploads do **not** flag
each other); non-duplicate submit still self-approves; queue highlight payload.

**Docs** — `recordings.md` P3 → done; cross-ref in
`docs/architecture/moderation.md` (the derived-flag + suppression rule).

---

## P4 — Adaptive playback ✅ Done

**Shipped:** `database.RecordingRenditionsByHash` (approved renditions of a
file's recording, or just itself) on the api `Repository` with a `fakeRepo` stub;
`GET /api/tracks/{hash}/renditions` (ranked, best-marked, via `buildDuplicateDTO`).
`player.js` gains `setRenditions` + an in-place `switchSource` (preserves
position/play state) behind the new `#qualitySelect` control;
`player-controller.js` fetches renditions on each track change (origin derived
from the track URL, guarded against stale responses). `player.css` styling.
Tests in `database/` + `api/`; JS syntax-checked. Player UI itself needs a manual
browser pass (no JS test harness for the player).

**Track API** — each track payload gains its recording's rendition list (`hash`,
format, bitrate, sample-rate, size, duration, ladder rank). No new delivery
machinery: `/files/*` already serves range requests via `http.ServeContent`.

**Player** — `player-controller.js` gains an **Auto / High / Low** control;
Auto = ladder best, Low/High walk the ladder. Survives shell navigation like the
rest of the controller.

**Tests** — API payload shape + rank ordering; a `tests/js` unit test if the
rendition-pick adds index math (run `node --test tests/js/...`).

**Docs** — `recordings.md` P4 → done; update
`docs/ui/player-and-queue.md` with the quality control.

---

## P5 (future) — ffmpeg auto-transcode

Out of scope for the initial build; kept as design only (canonical master +
derived streaming renditions, provenance `derived_from`, skip moderation +
fingerprint matching). Schedule after P4 ships and only if needed.

---

## Test matrix (summary)

| Layer            | New test files / additions                                  |
|------------------|-------------------------------------------------------------|
| Tool wrappers    | `media/analyze_test.go` (golden parse + `t.Skip` if absent) |
| Worker pool      | `mediaproc/worker_test.go` (mirror imageproc)               |
| DB               | analysis jobs, fingerprints, recordings, resolver; version bumps `18→19→20` |
| API              | duplicates list/split authz, submit self-approve suppression, tag-fallback default-key exclusion; `fakeRepo` stubs per method |
| JS               | rendition-pick index math only if introduced (`node --test`) |

## Doc update checklist

- `docs/architecture/recordings.md` — flip each phase's status as it lands;
  fix the stale "next is 018" gotcha (now `019`).
- `docs/building.md` — optional `ffprobe`/`fpcalc` runtime tools.
- `docs/architecture/moderation.md` — derived duplicate flag + self-approve
  suppression (P3).
- `docs/ui/player-and-queue.md` — Auto/High/Low quality control (P4).
- `docs/architecture/file-management-view.md` — link the duplicates page (P2).
- `CLAUDE.md` — migrations latest line, new admin sub-page, media-analysis mention.
- `docs/plans/roadmap.md` — drop the recordings entry once P4 ships.
