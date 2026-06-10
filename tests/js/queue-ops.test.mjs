// Unit tests for the queue index arithmetic (webui/static/js/queue-ops.js).
// Run with: node --test tests/js/queue-ops.test.mjs
// Kept outside webui/static so the embedded binary doesn't ship test files.
import test from 'node:test';
import assert from 'node:assert/strict';
import { insertAdjust, removeAdjust, moveAdjust, clampIndex, shufflePerm, relinkTracks } from '../../webui/static/js/queue-ops.js';

test('insertAdjust', () => {
  assert.equal(insertAdjust(-1, 0, 3), -1, 'no current track: unchanged');
  assert.equal(insertAdjust(2, 0, 2), 4, 'insert before current shifts it');
  assert.equal(insertAdjust(2, 2, 1), 3, 'insert at current shifts it (track keeps playing)');
  assert.equal(insertAdjust(2, 3, 5), 2, 'insert after current: unchanged');
});

test('removeAdjust', () => {
  assert.equal(removeAdjust(-1, 0), -1, 'no current track: unchanged');
  assert.equal(removeAdjust(3, 1), 2, 'remove before current shifts it down');
  assert.equal(removeAdjust(3, 4), 3, 'remove after current: unchanged');
});

test('moveAdjust', () => {
  assert.equal(moveAdjust(-1, 0, 2), -1, 'no current track: unchanged');
  assert.equal(moveAdjust(2, 2, 5), 5, 'moving the current track follows it');
  assert.equal(moveAdjust(2, 5, 0), 3, 'moving a later track before current shifts current up');
  assert.equal(moveAdjust(2, 0, 5), 1, 'moving an earlier track after current shifts current down');
  assert.equal(moveAdjust(2, 0, 1), 2, 'reorder entirely before current: unchanged');
  assert.equal(moveAdjust(2, 4, 5), 2, 'reorder entirely after current: unchanged');
  assert.equal(moveAdjust(2, 0, 2), 1, 'move earlier track TO the current slot shifts current down');
  assert.equal(moveAdjust(2, 4, 2), 3, 'move later track TO the current slot shifts current up');
});

test('clampIndex', () => {
  assert.equal(clampIndex(5, 0), -1, 'empty queue: -1');
  assert.equal(clampIndex(-3, 4), 0, 'negative clamps to 0');
  assert.equal(clampIndex(9, 4), 3, 'past the end clamps to last');
  assert.equal(clampIndex(2, 4), 2, 'in range: unchanged');
});

test('shufflePerm', () => {
  const perm = shufflePerm(5, 2);
  assert.equal(perm[0], 2, 'current position comes first');
  assert.deepEqual([...perm].sort(), [0, 1, 2, 3, 4], 'is a permutation of [0..n)');

  assert.deepEqual([...shufflePerm(4, -1)].sort(), [0, 1, 2, 3],
    'no current track: still a full permutation');

  // Deterministic with an injected rand: rand()=0 always swaps with slot 0.
  assert.deepEqual(shufflePerm(4, 1, () => 0), [1, 2, 3, 0],
    'injected rand makes the shuffle reproducible');

  assert.deepEqual(shufflePerm(1, 0), [0], 'single track: identity');
  assert.deepEqual(shufflePerm(0, -1), [], 'empty queue: empty permutation');
});

test('relinkTracks', () => {
  const a = { url: '/files/aa/x.mp3' };
  const b1 = { url: '/files/bb/y.mp3' };
  const b2 = { url: '/files/bb/y.mp3' }; // duplicate of the same track
  const queue = [b1, a, b2];

  // Revived-from-JSON original: equal data, different object identity.
  const original = [{ url: '/files/aa/x.mp3' }, { url: '/files/bb/y.mp3' },
    { url: '/files/bb/y.mp3' }, { url: '/files/zz/gone.mp3' }];
  const relinked = relinkTracks(original, queue);

  assert.equal(relinked[0], a, 'matched by url → shares the queue object');
  assert.equal(relinked[1], b1, 'first duplicate consumes the first occurrence');
  assert.equal(relinked[2], b2, 'second duplicate consumes the second occurrence');
  assert.equal(relinked[3], original[3], 'unmatched entry is kept as-is');
});
