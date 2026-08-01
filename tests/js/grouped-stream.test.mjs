// Unit tests for the paged grouped-view stream (webui/static/js/grouped-stream.js).
// Run with: node --test tests/js/grouped-stream.test.mjs
// Kept outside webui/static so the embedded binary doesn't ship test files.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createGroupedStream } from '../../webui/static/js/grouped-stream.js';

// row builds one server-list row; album_artist falls back to artist, both empty =
// the Unknown/Other bucket. hash defaults to a unique value; tagset_id counts
// alongside it, so a test can tell the two identities apart (the two scopes that
// use this stream key their rows on tagset_id, not on the blob hash).
let hc = 0;
function row(albumArtist, album, title, opts = {}) {
  const n = ++hc;
  return {
    album_artist: albumArtist, artist: opts.artist ?? '', album, title,
    year: opts.year ?? 0, disc_number: opts.disc ?? null, track_number: opts.track ?? null,
    hash: opts.hash ?? `h${n}`, tagset_id: opts.tagset_id ?? 1000 + n,
  };
}
// sig is the readable shape of the streamed items: "sep:label" or "t:title".
const sig = items => items.map(it => it.kind === 'grow' ? `t:${it.file.title}` : `${it.sep}:${it.label}`);
const selectAll = () => true;
// byHash is the hash-keyed scope's row identity; the streams take it from the
// scope rather than assuming it (see "select-all keys come from the scope" below).
const byHash = f => f.hash;

test('single page: artist → album → track separators, fallback buckets last', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, byHash);
  const rows = [
    row('Alpha', 'First', 'a1', { track: 1 }),
    row('Alpha', 'First', 'a2', { track: 2 }),
    row('Alpha', 'Second', 'b1', { track: 1 }),
    row('Beta', 'Solo', 'c1', { track: 1 }),
    row('', '', 'z1'),               // untagged → Unknown artist / Other
  ];
  const delta = gs.ingest(rows, true);
  assert.deepEqual(sig(delta), [
    'artist:Alpha', 'album:First', 't:a1', 't:a2',
    'album:Second', 't:b1',
    'artist:Beta', 'album:Solo', 't:c1',
    'artist:Unknown artist', 'album:Other', 't:z1',
  ]);
  // The delta is also the accumulated items.
  assert.deepEqual(sig(gs.items), sig(delta));
  // Artist running counts.
  const alpha = delta.find(it => it.sep === 'artist' && it.label === 'Alpha');
  assert.equal(alpha.meta, '2 albums · 3 tracks');
  assert.equal(delta.find(it => it.label === 'Beta').meta, '1 album · 1 track');
  // Fallback buckets are flagged (no cover affordance) and labelled.
  assert.equal(delta.find(it => it.label === 'Unknown artist').fallback, true);
  assert.equal(delta.find(it => it.label === 'Other').fallback, true);
});

test('album buffered across a page boundary: one separator each, counts accumulate', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, byHash);
  // Alpha/First straddles page 1 → page 2; a page wholly inside the open album
  // must flush nothing (the fetch loop relies on the empty delta to keep pulling).
  const d1 = gs.ingest([row('Alpha', 'First', 'a1', { track: 1 })], false);
  assert.deepEqual(d1, [], 'open album flushes nothing yet');
  const d2 = gs.ingest([
    row('Alpha', 'First', 'a2', { track: 2 }),
    row('Alpha', 'Second', 'b1', { track: 1 }),
  ], true);
  // The whole list, assembled once across the two ingests, has exactly one
  // separator per artist/album — no duplicate from the split.
  assert.deepEqual(sig(gs.items), [
    'artist:Alpha', 'album:First', 't:a1', 't:a2', 'album:Second', 't:b1',
  ]);
  assert.equal(sig(d2).filter(s => s === 'artist:Alpha').length, 1);
  assert.equal(gs.items.find(it => it.sep === 'artist').meta, '2 albums · 3 tracks');
});

test('multi-disc album straddling a page still gets a leading "Disc" header', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, byHash);
  gs.ingest([row('Q', 'Box', 'd1t1', { disc: 1, track: 1 })], false);
  gs.ingest([
    row('Q', 'Box', 'd1t2', { disc: 1, track: 2 }),
    row('Q', 'Box', 'd2t1', { disc: 2, track: 1 }),
  ], true);
  // Buffering the album to its boundary makes multi-disc detection exact, so the
  // FIRST disc gets its header too (not just disc 2).
  assert.deepEqual(sig(gs.items), [
    'artist:Q', 'album:Box',
    'disc:Disc 1', 't:d1t1', 't:d1t2',
    'disc:Disc 2', 't:d2t1',
  ]);
});

test('single-disc album shows no disc separators', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, byHash);
  gs.ingest([
    row('Q', 'EP', 't1', { disc: 1, track: 1 }),
    row('Q', 'EP', 't2', { disc: 1, track: 2 }),
  ], true);
  assert.equal(gs.items.some(it => it.sep === 'disc'), false);
});

test('streaming in any page split yields the same items as one shot', () => {
  // 9 rows across two artists / three albums, one multi-disc.
  const make = () => [
    row('Alpha', 'Zed', 'z1', { year: 1999, track: 1, hash: 'a1' }),
    row('Alpha', 'Zed', 'z2', { year: 0, track: 2, hash: 'a2' }),
    row('Alpha', 'Ack', 'k1', { year: 2005, track: 1, hash: 'a3' }),
    row('Beta', 'Box', 'd1', { disc: 1, track: 1, hash: 'b1' }),
    row('Beta', 'Box', 'd2', { disc: 1, track: 2, hash: 'b2' }),
    row('Beta', 'Box', 'd3', { disc: 2, track: 1, hash: 'b3' }),
    row('', '', 'u1', { hash: 'c1' }),
    row('', '', 'u2', { hash: 'c2' }),
    row('', '', 'u3', { hash: 'c3' }),
  ];
  const oneShot = createGroupedStream(selectAll, byHash);
  oneShot.ingest(make(), true);
  const want = sig(oneShot.items);

  // Drive the same rows through every page size 1..9 and compare.
  for (let size = 1; size <= 9; size++) {
    const rows = make();
    const gs = createGroupedStream(selectAll, byHash);
    for (let off = 0; off < rows.length; off += size) {
      const page = rows.slice(off, off + size);
      const isFinal = off + size >= rows.length;
      gs.ingest(page, isFinal);
    }
    assert.deepEqual(sig(gs.items), want, `page size ${size} diverged`);
  }
});

test('isSelectable gates the select-all key sets', () => {
  hc = 0;
  // Only "keep" hashes are selectable.
  const gs = createGroupedStream(f => f.hash !== 'skip', byHash);
  gs.ingest([
    row('Alpha', 'A', 't1', { hash: 'keep1', track: 1 }),
    row('Alpha', 'A', 't2', { hash: 'skip', track: 2 }),
    row('Alpha', 'A', 't3', { hash: 'keep2', track: 3 }),
  ], true);
  const album = gs.items.find(it => it.sep === 'album');
  const artist = gs.items.find(it => it.sep === 'artist');
  assert.deepEqual(album.keys, ['keep1', 'keep2']);
  assert.deepEqual(artist.keys, ['keep1', 'keep2']);
  // The track count still reflects ALL rows, not just selectable ones.
  assert.equal(artist.meta, '1 album · 3 tracks');
});

// The regression this stream was built wrong for: it derived the separators'
// select-all sets from f.hash, but BOTH scopes that turn the grouped view on
// (admin All Appearances, admin Trash) key their rows on tagset_id. Ticking an
// artist or an album therefore ticked only the separator itself — the track rows
// under it never moved, because their checkboxes carry the tagset id — and the
// bulk endpoint was then handed content hashes where it wanted appearance ids
// (Number('aa3f…') is NaN, which the server answers with "invalid tagset id").
test('select-all keys come from the scope, not from the blob hash', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, f => f.tagset_id);
  gs.ingest([
    row('Alpha', 'A', 't1', { hash: 'aaa', tagset_id: 11, track: 1 }),
    row('Alpha', 'A', 't2', { hash: 'bbb', tagset_id: 12, track: 2 }),
    row('Alpha', 'B', 't3', { hash: 'ccc', tagset_id: 13, track: 1 }),
  ], true);
  const artist = gs.items.find(it => it.sep === 'artist');
  const albums = gs.items.filter(it => it.sep === 'album');
  assert.deepEqual(artist.keys, ['11', '12', '13'], 'artist select-all governs every tagset under it');
  assert.deepEqual(albums[0].keys, ['11', '12']);
  assert.deepEqual(albums[1].keys, ['13']);
});

// Keys round-trip through `data-key` / `dataset.keys` attributes, so a numeric
// row id has to come back out of the DOM comparing equal to what the selection
// Set holds. file-list.js stringifies its own keyOf for exactly that reason; the
// stream must not be the one place that leaks a raw number.
test('numeric row keys are stringified, so DOM round-trips compare equal', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, f => f.tagset_id);
  gs.ingest([row('Alpha', 'A', 't1', { tagset_id: 7 })], true);
  const album = gs.items.find(it => it.sep === 'album');
  assert.deepEqual(album.keys, ['7']);
  assert.equal(typeof album.keys[0], 'string');
});

// A disc separator is decoration: it must carry no select-all set of its own, or
// it would offer a checkbox governing rows the album header already governs.
test('disc separators carry an empty key set', () => {
  hc = 0;
  const gs = createGroupedStream(selectAll, f => f.tagset_id);
  gs.ingest([
    row('Q', 'Box', 'd1t1', { disc: 1, track: 1 }),
    row('Q', 'Box', 'd2t1', { disc: 2, track: 1 }),
  ], true);
  for (const it of gs.items.filter(i => i.sep === 'disc')) assert.deepEqual(it.keys, []);
});

test('reset clears state for a fresh load', () => {
  const gs = createGroupedStream(selectAll, byHash);
  gs.ingest([row('Alpha', 'A', 't1')], true);
  assert.equal(gs.items.length > 0, true);
  gs.reset();
  assert.deepEqual(gs.items, []);
  // A new artist after reset starts its own separator + counts (no carry-over).
  gs.ingest([row('Beta', 'B', 'x1')], true);
  assert.equal(gs.items[0].label, 'Beta');
  assert.equal(gs.items.find(it => it.sep === 'artist').meta, '1 album · 1 track');
});
