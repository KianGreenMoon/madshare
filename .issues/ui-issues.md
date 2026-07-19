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
- [ ] **BUG-16** A row's state badge can squeeze its title to zero width. In `.cell-title-line` (`file-view.css`) the badge is `flex-shrink: 0` and the title ellipsises, so on a narrow title column a long badge — e.g. Trash's `pending review` (~120px) in a ~145px column — leaves the title no room and it disappears entirely, leaving only the badge and the hash. Predates the icon-button work (the column was 141px before it, 145px after). **Fix idea:** give `.cell-title` a `min-width` floor, or move the badge onto its own line / shrink it below a column width.
- [ ] **BUG-17** The Review queue's and My-uploads' preview buttons carry `class="icon-btn rev-play"` / `"icon-btn mu-play"` and a `▶` **text glyph**, where every other row uses `.play-btn` with the shared `PLAY_ICON` SVG (`icons.js`). Now that `.icon-btn` exists in `file-view.css` they at least render as proper round icon buttons, but they still hover to `--text` rather than the accent and draw a glyph instead of the icon. **Fix idea:** switch `moderation.js` / `mine-list.js` to `.play-btn` + `PLAY_ICON`; the `.rev-play` / `.mu-play` hooks (unstyled) stay as JS selectors.

## Shell UI — found 2026-06-10 (user review)

Reported after the shell-native UI shipped (library/playlists pages, shell-owned
player + queue). These are about the queue/toast chrome and the row-menu dialog,
not the old `app.js`.

**All three implemented 2026-06-10 (aidev)** — `go build ./...` clean, `node
--test tests/js/queue-ops.test.mjs` 6/6, embedded assets verified served. Browser
spot-check still recommended (especially the mobile keyboard, BUG-18).

- [x] **BUG-16** Toast bubbles overlap the player controls (and the queue panel).
  The status toast stack is pinned bottom-right (`app.css:772`, `.toast-stack`
  → `bottom: var(--space-5); right: var(--space-5)`) at `z-index: 200`, i.e. the
  same corner as the player-bar volume/queue buttons and where `#queue-panel`
  opens (`player.css:140`). So "Added to queue" / "Saved playlist" / "Queue
  replaced" toasts cover the controls and the open queue window.
  **Decision:** keep toasts on the **right**, just lift them clear (user pref).
  **Plan:**
  1. Lift the *status* stack (`#toastStatus`) above the player bar: `bottom:
     calc(var(--player-h) + var(--space-3))` (player bar is `--player-h` = 84px).
     `#toastAlert` is already top-anchored, so errors are unaffected.
  2. When the queue panel is open it occupies that same right-above-player slot,
     so raise the status stack to clear it: `queue-panel.js` `setOpen()` toggles
     a state class (e.g. `document.body.classList.toggle('queue-open', on)`); CSS
     `body.queue-open #toastStatus { bottom: calc(var(--player-h) +
     min(60vh, 480px) + var(--space-4)); }` (panel max-height), staying right.
  3. Synergy with BUG-17: the persistent *"Queue replaced — Undo"* toast goes
     away (becomes the panel Restore button), so the only toasts left near the
     player are short transient ones.

- [x] **BUG-17** Restore (un-replace) is hidden in a disappearing toast — make it
  a permanent queue-panel button. Today there is **no history**: when a *dirty*
  (manually edited) queue is replaced by a browse-and-click, `setQueue`
  (`player-controller.js:252`) stashes the old queue in a **temporary closure**
  and fires `queuereplaced`; `shell.js:202` shows an 8 s *"Undo — restore my
  queue"* toast. Miss/dismiss it and the stash is gone.
  **Decision (user):** **one-level Restore** (no redo), and the stash is **cleared
  on the first manual edit of the new queue** ("if we already changed the new
  queue, there's nothing to restore").
  **Plan:**
  1. `player-controller.js`: promote the closure stash to controller state
     `stashed = { queue, original, index } | null`. In `setQueue`, when replacing
     a dirty queue, capture `stashed` **before** reassigning `queue`, and
     `emit('stashchange')`. Add `canRestore()` (`stashed !== null`) and
     `restoreQueue()` (re-applies the stash, then `stashed = null` +
     `emit('stashchange')`). **Clear `stashed = null` + emit on any manual edit**
     (`enqueue`/`insertAt`/`removeAt`/`move`/`clear`) and on a fresh `setQueue`.
  2. Drop the action-toast: `shell.js` no longer wires `queuereplaced` to an Undo
     toast (optionally keep a brief plain "Queue replaced." status toast, no
     action button). Remove the now-unused `queuereplaced` emit if nothing else
     uses it.
  3. `partials.html` (queue head, ~line 170): add `<button id="queueRestoreBtn"
     class="btn btn-neutral queue-btn-sm" hidden>⟲ Restore</button>` to
     `.queue-actions`.
  4. `queue-panel.js`: show/hide the Restore button from `controller.canRestore()`
     on `stashchange` (and on open/render); click → `controller.restoreQueue()`.
  5. `player.css`: minor styling for the restore button if needed.

- [x] **BUG-18** Mobile virtual keyboard dismisses the "New playlist…" dialog.
  Tapping the inline name input (`row-menu.js`, used by the library "⋯ → Add to…
  → New playlist…" flow, `app.js:244`) opens the on-screen keyboard, which
  **resizes the visual viewport** (and often scrolls to reveal the input). The
  row-menu closes itself on **any** scroll/resize (`row-menu.js:82-87`:
  `onScroll = () => closeRowMenu()` on `window` `scroll`(capture)+`resize`), so
  the menu — and the focused input — is destroyed and the keyboard drops.
  **Plan:** in the scroll/resize close handler, **skip closing while focus is
  inside the menu**: `if (menuEl && menuEl.contains(document.activeElement))
  return;`. This keeps deliberate page scroll/resize closing the menu on desktop
  (no focus inside the menu there) while ignoring keyboard-driven viewport
  changes. Optionally also switch resize detection to `visualViewport`'s
  `resize`. Verify desktop drill-down + the menu's existing scroll-to-close still
  work.

---

## Refactoring (deferred — for the planned big UI refactor)

- [ ] **BUG-15** Header/nav styling is duplicated and diverges across pages instead of being single-sourced. After the header *markup* was extracted into a shared partial (`webui/html/partials.html`, `{{template "header"}}`), the *CSS* was not consolidated:
  - `app.css` carries the canonical header rules (`header`, `.logo`, `.main-nav`, `.nav-link`, `.theme-dot`, `.btn`, `.btn-neutral`) used by the library and upload pages.
  - `admin.css` has its **own copy** of `header` and `.main-nav` (and related rules), so the admin page renders the shared header markup with a *separate, independently-maintained* stylesheet that can (and does) drift from `app.css`.
  - `upload.css` redefines `.btn` (loaded after `app.css`), so the header's auth buttons pick up upload-page button styling rather than the shared `.btn`/`.btn-neutral` look on that page.
  Net effect: the same header HTML looks subtly different per page, and a header change must be made in 2–3 places. **Plan:** during the big UI refactor, move all header/nav/theme-switcher/button styles into a single shared stylesheet (e.g. a `header.css` or a dedicated section of `app.css`) linked by every page, and delete the duplicate rules from `admin.css` and the `.btn` override from `upload.css`. Also consider giving `cmus.html` the shared header (it still has its own markup). Related: the nav-centering fix in commit `6317723` (`.main-nav { margin-right: auto }`) is a symptom of this scattered styling.
  **Update (admin rework, Phase 7):** `admin.css` is **deleted** — the reworked admin pages all link `app.css` for the header, so the admin copy/drift is resolved. Remaining: the `upload.css` `.btn` override, and `cmus.html` still uses its own header markup + `cmus.css` (cmus is currently a paused view). **Update (2026-06-09):** the `upload.css` `.btn` remnant is folded into the upload-page rewrite; cmus stays deferred (paused, see `docs/plans/roadmap.md`). **Update (2026-07-19):** the `/cmus` page (`cmus.html` + `cmus.css` + `cmus.js`) has been **removed** entirely, so its separate header markup is no longer a concern.

## Ideas / future enhancements (not bugs)

- [ ] **IDEA-01 — Continuous playback across page navigations.** Today the UI is a multi-page app: each link is a full document load, so the `<audio>` element (in the `player-bar` partial) is destroyed on every navigation and playback stops/reloads. Goal: play on the library page, navigate to an admin page, and the music keeps playing gaplessly. Requires the `<audio>` to live in a document that survives navigation. Options, cheapest → best:
  1. *Save & resume* across loads (sessionStorage: `src` + `currentTime` + playing-state). Easy, but there's an audible gap and browser autoplay-blocking means it often pauses until the user clicks — not true continuity.
  2. *Persistent app shell + client-side content swap* (intercept link clicks, `fetch()` the page, swap only `<main>` + active nav via History API; header + player + `<audio>` never torn down). **Truly gapless; recommended.** Cost: changes the navigation model — each per-page JS module must be init/torn-down on swap instead of "run once on load". Feasible now that the player is a standalone `player.js` core and pages are clean per-section modules. Watch: back/forward + deep links, and the `nowebui` build.
  3. *Iframe-hosted content* (player in outer frame, content in an iframe). Works but brings URL/back-button sync + styling seams; not preferred.
  Write a short design doc in `docs/plans/` before implementing (option 2 is an architectural change). Builds on the player extraction from the admin-panel-rework plan.
  **Update (2026-06-09):** decided — **option 2** with **full playlist continuity** (queue is shell-owned, next/prev/shuffle work on every listening page) and **admin kept OUT of the shell** (separate full-load pages + page-local preview player; Admin nav link `target="_blank"`). Shipped — behavior reference: `docs/ui/player-and-queue.md` + `docs/ui/shells.md`.
