// Unit tests for the paged native-grouping stream (webui/static/js/section-stream.js).
// Run with: node --test tests/js/section-stream.test.mjs
// Kept outside webui/static so the embedded binary doesn't ship test files.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createSectionStream } from '../../webui/static/js/section-stream.js';

// row builds one server-ordered staging row. key is the group key (uploader id /
// state); the server returns rows already sorted by it, so equal keys are contiguous.
let hc = 0;
const row = (key, opts = {}) => ({ key, state: opts.state ?? 'submitted', hash: opts.hash ?? `h${++hc}` });

// A by-key config: header label is "g:<key>", every row selectable.
const byKey = () => createSectionStream({
  keyOf: f => f.key,
  makeHeader: f => ({ kind: 'ghead', streamed: true, key: String(f.key), label: `g:${f.key}` }),
  isSelectable: () => true,
});
// sig is the readable shape: "h:label" for a header, "r:hash" for a row.
const sig = items => items.map(it => it.kind === 'row' ? `r:${it.file.hash}` : `h:${it.label}`);

test('single page: a header per key change, rows under their header', () => {
  hc = 0;
  const ss = byKey();
  const delta = ss.ingest([
    row('amy', { hash: 'a1' }), row('amy', { hash: 'a2' }),
    row('ben', { hash: 'b1' }),
  ], true);
  assert.deepEqual(sig(delta), ['h:g:amy', 'r:a1', 'r:a2', 'h:g:ben', 'r:b1']);
  // The delta is also the accumulated items.
  assert.deepEqual(sig(ss.items), sig(delta));
});

test('running header hashes accumulate the group\'s selectable rows', () => {
  hc = 0;
  const ss = byKey();
  ss.ingest([row('amy', { hash: 'a1' }), row('amy', { hash: 'a2' }), row('ben', { hash: 'b1' })], true);
  const headers = ss.items.filter(it => it.kind === 'ghead');
  assert.deepEqual(headers[0].hashes, ['a1', 'a2']);
  assert.deepEqual(headers[1].hashes, ['b1']);
});

test('a group straddling a page boundary keeps one header, hashes bump in place', () => {
  hc = 0;
  const ss = byKey();
  const d1 = ss.ingest([row('amy', { hash: 'a1' }), row('amy', { hash: 'a2' })], false);
  // No header for amy in the second page (key unchanged) — just the new row.
  const d2 = ss.ingest([row('amy', { hash: 'a3' }), row('ben', { hash: 'b1' })], true);
  assert.deepEqual(sig(d1), ['h:g:amy', 'r:a1', 'r:a2']);
  assert.deepEqual(sig(d2), ['r:a3', 'h:g:ben', 'r:b1']);
  const amy = ss.items.find(it => it.kind === 'ghead' && it.label === 'g:amy');
  assert.deepEqual(amy.hashes, ['a1', 'a2', 'a3']);   // bumped in place across pages
});

test('isSelectable gates which rows feed the header hash set', () => {
  hc = 0;
  const ss = createSectionStream({
    keyOf: f => f.key,
    makeHeader: f => ({ kind: 'ghead', key: String(f.key), label: `g:${f.key}`, hashes: [] }),
    isSelectable: f => f.state === 'submitted',
  });
  ss.ingest([
    row('amy', { hash: 'a1', state: 'submitted' }),
    row('amy', { hash: 'a2', state: 'returned' }),  // not selectable
    row('amy', { hash: 'a3', state: 'submitted' }),
  ], true);
  const amy = ss.items[0];
  assert.deepEqual(amy.hashes, ['a1', 'a3']);
  // Every row still renders (selectability only affects the header's hash set).
  assert.equal(ss.items.filter(it => it.kind === 'row').length, 3);
});

test('section headers (no hashes supplied) still get an empty hashes array', () => {
  hc = 0;
  const ss = createSectionStream({
    keyOf: f => f.state,
    makeHeader: f => ({ kind: 'shead', label: `s:${f.state}` }),
    isSelectable: () => true,
  });
  const delta = ss.ingest([
    row('x', { state: 'returned', hash: 'r1' }),
    row('x', { state: 'draft', hash: 'd1' }),
    row('x', { state: 'draft', hash: 'd2' }),
  ], true);
  assert.deepEqual(sig(delta), ['h:s:returned', 'r:r1', 'h:s:draft', 'r:d1', 'r:d2']);
  // The stream fills a hashes array even on a header that didn't declare one.
  assert.deepEqual(ss.items[0].hashes, ['r1']);
});

test('reset clears items and the open group', () => {
  hc = 0;
  const ss = byKey();
  ss.ingest([row('amy', { hash: 'a1' })], false);
  ss.reset();
  assert.deepEqual(ss.items, []);
  // After reset the next row opens a fresh header even with the same key.
  const delta = ss.ingest([row('amy', { hash: 'a2' })], true);
  assert.deepEqual(sig(delta), ['h:g:amy', 'r:a2']);
});

test('empty final flush yields nothing (no buffering)', () => {
  hc = 0;
  const ss = byKey();
  ss.ingest([row('amy', { hash: 'a1' })], false);
  assert.deepEqual(ss.ingest([], true), []);
});
