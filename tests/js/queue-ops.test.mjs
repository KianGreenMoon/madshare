// Unit tests for the queue index arithmetic (webui/static/js/queue-ops.js).
// Run with: node --test tests/js/queue-ops.test.mjs
// Kept outside webui/static so the embedded binary doesn't ship test files.
import test from 'node:test';
import assert from 'node:assert/strict';
import { insertAdjust, removeAdjust, moveAdjust, clampIndex } from '../../webui/static/js/queue-ops.js';

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
