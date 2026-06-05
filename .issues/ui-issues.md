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
