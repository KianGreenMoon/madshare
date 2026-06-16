// disc.js — the single source of truth for how a track's disc number groups,
// orders, and labels it within an album. Imported by every renderer that shows
// "Disc N" separators (app.js, admin/files.js, file-list.js) so they can't drift
// apart again. Full model: docs/architecture/disc-numbering.md.
//
// disc_number is a 1-based integer by convention, but three states are DISTINCT
// and never folded together:
//   null/undefined → untagged (no disc info)
//   0              → disc zero (a real, distinct disc; rare but intentional)
//   N (≥1)         → disc N
// A track whose album resolves to a single distinct disc key shows no separator
// at all (the everyday single-disc album).

// discKey is a track's grouping/identity key: the integer disc (including 0), or
// null when untagged (undefined is normalised to null). Two tracks share a disc
// iff their keys are equal.
export function discKey(n) { return n == null ? null : n; }

// discSort maps a disc number to a sort value: numbered discs ascend (0 before
// 1), and an untagged disc sorts AFTER every numbered one.
export function discSort(n) { return n == null ? Infinity : n; }

// discLabel is the "Disc N" heading text; an untagged disc renders "Disc —".
export function discLabel(n) { return n == null ? 'Disc —' : `Disc ${n}`; }

// isMultiDisc is true when an album spans more than one distinct disc key — the
// gate for showing the "Disc N" separators at all.
export function isMultiDisc(tracks) {
  return new Set(tracks.map(t => discKey(t.disc_number))).size > 1;
}
