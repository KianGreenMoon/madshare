// Unit tests for the windowed-list math (webui/static/js/virtual-list.js).
// Run with: node --test tests/js/virtual-list.test.mjs
// Kept outside webui/static so the embedded binary doesn't ship test files.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createHeightIndex, computeWindow } from '../../webui/static/js/virtual-list.js';

// Brute-force references the Fenwick tree is checked against.
const refPrefix = (h, i) => h.slice(0, i).reduce((a, b) => a + b, 0);
function refFindIndex(h, y) {
  if (!h.length) return -1;
  if (y <= 0) return 0;
  let acc = 0;
  for (let i = 0; i < h.length; i++) {
    if (acc <= y && y < acc + h[i]) return i;
    acc += h[i];
  }
  return h.length - 1;
}

test('createHeightIndex: uniform heights', () => {
  const idx = createHeightIndex(10, 40);
  assert.equal(idx.count, 10);
  assert.equal(idx.total(), 400);
  assert.equal(idx.prefix(0), 0);
  assert.equal(idx.prefix(5), 200);
  assert.equal(idx.prefix(10), 400);
  assert.equal(idx.findIndex(0), 0);
  assert.equal(idx.findIndex(39), 0);
  assert.equal(idx.findIndex(40), 1);
  assert.equal(idx.findIndex(200), 5);
  assert.equal(idx.findIndex(399), 9);
  assert.equal(idx.findIndex(400), 9, 'y at/over total clamps to last');
  assert.equal(idx.findIndex(99999), 9);
  assert.equal(idx.findIndex(-10), 0, 'negative y clamps to first');
});

test('createHeightIndex: setHeight updates prefix/find/total', () => {
  const idx = createHeightIndex(5, 40);
  idx.setHeight(0, 100);          // [100,40,40,40,40]
  assert.equal(idx.total(), 260);
  assert.equal(idx.prefix(1), 100);
  assert.equal(idx.prefix(2), 140);
  assert.equal(idx.findIndex(99), 0);
  assert.equal(idx.findIndex(100), 1);
  assert.equal(idx.findIndex(139), 1);
  assert.equal(idx.findIndex(140), 2);
  idx.setHeight(0, 40);           // back to uniform
  assert.equal(idx.total(), 200);
});

test('createHeightIndex: estimate as a function', () => {
  const idx = createHeightIndex(4, (i) => (i + 1) * 10);  // 10,20,30,40
  assert.equal(idx.total(), 100);
  assert.equal(idx.height(2), 30);
  assert.equal(idx.findIndex(35), 2, 'item 2 spans [prefix(2)=30, prefix(3)=60)');
});

test('createHeightIndex: extend keeps measured heights', () => {
  const idx = createHeightIndex(3, 40);   // [40,40,40]
  idx.setHeight(1, 100);                  // [40,100,40]
  idx.extend(2);                          // [40,100,40,40,40]
  assert.equal(idx.count, 5);
  assert.equal(idx.height(1), 100, 'measured height survives extend');
  assert.equal(idx.total(), 260);
  assert.equal(idx.findIndex(140), 2);    // prefix(2)=140
});

test('createHeightIndex: reset back to estimate', () => {
  const idx = createHeightIndex(3, 40);
  idx.setHeight(0, 999);
  idx.reset(2);
  assert.equal(idx.count, 2);
  assert.equal(idx.total(), 80, 'reset drops measurements');
});

test('createHeightIndex: matches brute force on random heights', () => {
  let seed = 1234;
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
  for (let trial = 0; trial < 20; trial++) {
    const n = 1 + Math.floor(rnd() * 60);
    const h = Array.from({ length: n }, () => 8 + Math.floor(rnd() * 90));
    const idx = createHeightIndex(n, 1);
    h.forEach((v, i) => idx.setHeight(i, v));
    for (let i = 0; i <= n; i++) assert.equal(idx.prefix(i), refPrefix(h, i), `prefix(${i})`);
    const total = refPrefix(h, n);
    for (const y of [-5, 0, 1, (total / 3) | 0, (total / 2) | 0, total - 1, total, total + 50]) {
      assert.equal(idx.findIndex(y), refFindIndex(h, y), `findIndex(${y}) n=${n}`);
    }
  }
});

test('computeWindow: empty list', () => {
  const idx = createHeightIndex(0, 40);
  assert.deepEqual(computeWindow(idx, 0, 100, 6), { firstIndex: 0, lastIndex: -1, topPad: 0, bottomPad: 0 });
});

test('computeWindow: uniform at top', () => {
  const idx = createHeightIndex(10, 40);   // total 400
  const w = computeWindow(idx, 0, 100, 1);
  assert.equal(w.firstIndex, 0);
  assert.equal(w.lastIndex, 3, 'findIndex(100)=2, +1 buffer');
  assert.equal(w.topPad, 0);
  assert.equal(w.bottomPad, 400 - 160);
});

test('computeWindow: uniform scrolled into the middle', () => {
  const idx = createHeightIndex(10, 40);
  const w = computeWindow(idx, 200, 100, 1);
  assert.equal(w.firstIndex, 4, 'findIndex(200)=5, -1 buffer');
  assert.equal(w.lastIndex, 8, 'findIndex(300)=7, +1 buffer');
  assert.equal(w.topPad, 160);
  assert.equal(w.bottomPad, 400 - 360);
});

test('computeWindow: buffer clamps at both ends, pads non-negative', () => {
  const idx = createHeightIndex(5, 40);    // total 200
  const w = computeWindow(idx, 0, 1000, 6);
  assert.equal(w.firstIndex, 0);
  assert.equal(w.lastIndex, 4, 'viewport taller than content → whole list');
  assert.equal(w.topPad, 0);
  assert.equal(w.bottomPad, 0);
});
