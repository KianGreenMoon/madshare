// section-stream.js — the pure state machine behind file-list.js's paged native
// groupings (moderation = by uploader; My-uploads = by review-state section). It
// folds pages of rows — already in the server's group order (sort=uploader /
// sort=state) — into a flat windowed-item array of header + row items, emitted
// incrementally as the list scrolls, so these grouped views scale like the flat
// infinite-scroll instead of loading every row first.
//
// It is the single-level sibling of grouped-stream.js: there is no buffering (a
// header is emitted as soon as its key first appears), so unlike the artist/album
// stream a page always yields its delta immediately. The running header's
// select-all `keys` are bumped in place as rows of the open group arrive. Kept
// DOM-free so it unit-tests in node: tests/js/section-stream.test.mjs.
//
// Item shapes match file-list.js renderWindowItem: a header is whatever
// makeHeader(file) returns (kind:'ghead' for collapsible/uploader, 'shead' for
// sections), and rows are { kind:'row', file }.

// createSectionStream builds one streaming single-level grouper.
//   keyOf(file)      → the GROUP key (compared as-is across rows; the server
//                      orders by it, so equal keys are contiguous)
//   rowKey(file)     → the ROW identity, the scope's own — the same value the row
//                      checkbox carries in `data-key`. Distinct from keyOf, and
//                      not the blob hash: a scope may key rows on tagset_id, and a
//                      header whose select-all set holds hashes ticks nothing.
//   makeHeader(file) → the header item for a new group; gets a fresh `keys: []`
//                      if it doesn't supply one, which the stream fills with the
//                      group's selectable row keys (for the group-select cascade)
//   isSelectable(f)  → gates which rows feed the header's select-all key set
export function createSectionStream({ keyOf, rowKey, makeHeader, isSelectable = () => false }) {
  let items, curKey, curHeader;
  function reset() { items = []; curKey = null; curHeader = null; }
  reset();

  // ingest folds a page of server-ordered rows in: a new header is pushed when the
  // group key changes, then each row. isFinal is accepted for signature parity with
  // grouped-stream.js but is a no-op here (nothing is buffered). Returns the items
  // appended this call (also pushed onto `items`).
  function ingest(pageRows, _isFinal) {
    const delta = [];
    for (const f of pageRows) {
      const k = keyOf(f);
      if (curHeader === null || k !== curKey) {
        curKey = k;
        curHeader = makeHeader(f);
        if (!curHeader.keys) curHeader.keys = [];
        delta.push(curHeader);
      }
      if (isSelectable(f)) curHeader.keys.push(String(rowKey(f)));
      delta.push({ kind: 'row', file: f });
    }
    for (const it of delta) items.push(it);
    return delta;
  }

  return { reset, ingest, get items() { return items; } };
}
