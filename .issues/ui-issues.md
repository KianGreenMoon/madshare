# UI Issues — Madshare Web UI

Found by tester agent review of `webui/static/js/app.js` + `webui/html/upload.html`.

---

## High

- [x] **BUG-08** `null.remove()` crash on second `loadLibrary()` call — `#emptyState` is permanently removed from the DOM on first load; every subsequent reload calls `empty.remove()` on `null` and throws, leaving the list blank. **Fixed:** hide with `style.display` instead of removing.
- [x] **BUG-02** `.playing` highlight lost after library reload while audio is running — `trackList.innerHTML = ''` destroys all rows; `currentIndex` is not re-applied to the new rows. **Fixed:** re-apply `.playing` class after rows are rebuilt.
- [x] **BUG-03** Race condition: concurrent `loadLibrary()` calls (boot + post-upload) interleave `playlist[]` mutations and `appendChild` calls, producing duplicate rows and mismatched indices. **Fixed:** `libraryLoading` guard + `libraryReloadPending` queue flag.
- [x] **BUG-01** Related null crash: if a reload returns zero tracks after `#emptyState` was already removed, the empty-state branch cannot re-render. **Fixed:** same as BUG-08 — element is never removed.

## Medium

- [x] **BUG-04** Clicking Prev before any track has been played jumps to the last track (`currentIndex` starts at `-1`, condition `> 0` is false, wraps to `playlist.length - 1`). **Fixed:** early-return if `currentIndex < 0`.
- [x] **BUG-05** Shuffle `do-while` infinite loop when `playlist.length === 1` — loop condition `next === currentIndex` is always true, hanging the JS thread. **Fixed:** build an array of other indices and pick from it — no loop needed.
- [x] **BUG-09** `t.duration` is never present in the API response (v0 omits it) — duration column always shows `—`, dead code path in `fmtTime`. **Fixed (acknowledged):** code is correct; `—` is the intended placeholder until the API exposes duration.
- [x] **BUG-10** Seeking while stopped then pressing Play resets to 0:00 — `playIndex` reassigns `audio.src`, which discards the seeked position. **Fixed:** `btnPlay` now clears `stopped` and calls `audio.play()` directly instead of `playIndex`.
- [x] **BUG-14** API URL hardcoded to `http://localhost:3000` — breaks any non-local or production deployment. **Fixed:** read from `<meta name="api-url">` in the HTML template; falls back to `localhost:3000`.

## Low

- [x] **BUG-06** `file.name` concatenated into status message — safe with `textContent` now, but becomes an XSS vector if ever switched to `innerHTML`. **Fixed (hardened):** added comment marking the safety contract.
- [x] **BUG-07** `localStorage` theme value written to `data-theme` without validation against known theme names. **Fixed:** `VALID_THEMES` set; unknown values fall back to `'dark'`.
- [x] **BUG-11** `stopped` flag can desync `syncPlayIcon` from actual audio element state if audio plays externally (e.g. MediaSession). **Fixed:** `syncPlayIcon` now derives state from `audio.paused` only; `stopped` is used only by the play button's click handler.
- [x] **BUG-12** Multi-file drag-and-drop silently discards all files after the first — only `files[0]` is uploaded. **Fixed:** `uploadFiles(files)` iterates all dropped/selected files sequentially.
- [x] **BUG-13** No mechanism to re-show the empty state if the library becomes empty after first load. **Fixed:** same as BUG-08 — element is hidden/shown with `style.display`, never removed.

## Refactoring (deferred — for the planned big UI refactor)

- [ ] **BUG-15** Header/nav styling is duplicated and diverges across pages instead of being single-sourced. After the header *markup* was extracted into a shared partial (`webui/html/partials.html`, `{{template "header"}}`), the *CSS* was not consolidated:
  - `app.css` carries the canonical header rules (`header`, `.logo`, `.main-nav`, `.nav-link`, `.theme-dot`, `.btn`, `.btn-neutral`) used by the library and upload pages.
  - `admin.css` has its **own copy** of `header` and `.main-nav` (and related rules), so the admin page renders the shared header markup with a *separate, independently-maintained* stylesheet that can (and does) drift from `app.css`.
  - `upload.css` redefines `.btn` (loaded after `app.css`), so the header's auth buttons pick up upload-page button styling rather than the shared `.btn`/`.btn-neutral` look on that page.
  Net effect: the same header HTML looks subtly different per page, and a header change must be made in 2–3 places. **Plan:** during the big UI refactor, move all header/nav/theme-switcher/button styles into a single shared stylesheet (e.g. a `header.css` or a dedicated section of `app.css`) linked by every page, and delete the duplicate rules from `admin.css` and the `.btn` override from `upload.css`. Also consider giving `cmus.html` the shared header (it still has its own markup). Related: the nav-centering fix in commit `6317723` (`.main-nav { margin-right: auto }`) is a symptom of this scattered styling.
  **Update (admin rework, Phase 7):** `admin.css` is **deleted** — the reworked admin pages all link `app.css` for the header, so the admin copy/drift is resolved. Remaining: the `upload.css` `.btn` override, and `cmus.html` still uses its own header markup + `cmus.css` (cmus is currently a paused view). **Update (2026-06-09):** the `upload.css` `.btn` remnant is folded into the upload-page rewrite (`docs/plans/upload-rework.md`, Phase 3); cmus stays deferred (paused).

## Ideas / future enhancements (not bugs)

- [ ] **IDEA-01 — Continuous playback across page navigations.** Today the UI is a multi-page app: each link is a full document load, so the `<audio>` element (in the `player-bar` partial) is destroyed on every navigation and playback stops/reloads. Goal: play on the library page, navigate to an admin page, and the music keeps playing gaplessly. Requires the `<audio>` to live in a document that survives navigation. Options, cheapest → best:
  1. *Save & resume* across loads (sessionStorage: `src` + `currentTime` + playing-state). Easy, but there's an audible gap and browser autoplay-blocking means it often pauses until the user clicks — not true continuity.
  2. *Persistent app shell + client-side content swap* (intercept link clicks, `fetch()` the page, swap only `<main>` + active nav via History API; header + player + `<audio>` never torn down). **Truly gapless; recommended.** Cost: changes the navigation model — each per-page JS module must be init/torn-down on swap instead of "run once on load". Feasible now that the player is a standalone `player.js` core and pages are clean per-section modules. Watch: back/forward + deep links, and the `nowebui` build.
  3. *Iframe-hosted content* (player in outer frame, content in an iframe). Works but brings URL/back-button sync + styling seams; not preferred.
  Write a short design doc in `docs/plans/` before implementing (option 2 is an architectural change). Builds on the player extraction from the admin-panel-rework plan.
  **Update (2026-06-09):** decided — **option 2** with **full playlist continuity** (queue is shell-owned, next/prev/shuffle work on every listening page) and **admin kept OUT of the shell** (separate full-load pages + page-local preview player; Admin nav link `target="_blank"`). Design doc: `docs/plans/persistent-shell-playback.md` (Phase 1 / foundation of a 5-phase UI roadmap that also covers the upload rework and admin-files rework — see that doc's roadmap table).
