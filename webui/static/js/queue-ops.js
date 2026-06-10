// queue-ops.js — the pure index arithmetic behind the queue mutations
// (player-controller.js). Kept DOM-free and dependency-free so the trickiest,
// most regression-prone logic in the UI is unit-testable without a browser
// (persistent-shell-playback.md consideration §6); tests live in
// tests/js/queue-ops.test.mjs (outside the embedded static tree), run with
// `node --test tests/js/queue-ops.test.mjs`.

// insertAdjust returns the current-track index after inserting `count` tracks
// at position `at`. A non-playing index (-1) never moves.
export function insertAdjust(index, at, count) {
  if (index < 0) return index;
  return at <= index ? index + count : index;
}

// removeAdjust returns the current-track index after removing position `i`,
// for the cases where the removed track is NOT the current one. (Removing the
// current track is a playback decision — the caller picks what plays next.)
export function removeAdjust(index, i) {
  if (index < 0) return index;
  return i < index ? index - 1 : index;
}

// moveAdjust returns the current-track index after moving position `from` to
// position `to` (remove-then-insert semantics).
export function moveAdjust(index, from, to) {
  if (index < 0) return index;
  if (from === index) return to;
  if (from < index && to >= index) return index - 1;
  if (from > index && to <= index) return index + 1;
  return index;
}

// clampIndex confines a restored/requested index to the queue's bounds
// (or -1 for an empty queue).
export function clampIndex(i, length) {
  if (length <= 0) return -1;
  return Math.min(Math.max(0, i | 0), length - 1);
}
