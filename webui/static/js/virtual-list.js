// virtual-list.js — a dependency-free, measured-height windowed list. Keeps only
// the on-screen rows (+ a buffer) in the DOM, with two spacer elements standing
// in for everything above/below, so a list of any length renders a bounded number
// of nodes and never freezes. Optional infinite-scroll via fetchMore.
//
// Design: docs/architecture/infinite-scroll-virtualization.md ("This pass").
//
// The module is presentation-only and reuse-oriented: the consumer owns the
// scroll element, supplies a spacer factory (so it works inside a <table> via
// spacer <tr>s, or with <div>s elsewhere), a per-item renderer, and a height
// estimate. Heights are corrected to the real offsetHeight the first time a row
// renders — the admin file table has a responsive card mode, so a fixed row
// height can't be assumed.
//
// The pure math (createHeightIndex / computeWindow) carries no DOM references and
// is unit-tested in tests/js/virtual-list.test.mjs.

// ── Pure math ────────────────────────────────────────────────────────────────

// createHeightIndex maintains per-item heights with O(log n) prefix-sum and
// "which item spans pixel y" via a Fenwick (binary-indexed) tree, so scroll math
// stays cheap at 10^4–10^5 rows. `estimate` is a number or (i)=>height used as the
// starting height before a row has been measured.
export function createHeightIndex(count, estimate) {
  const est = typeof estimate === 'function' ? estimate : () => estimate;
  let n = 0;
  let heights = [];      // source-of-truth per-item heights
  let tree = [];         // Fenwick tree, 1-indexed (tree[0] unused)
  let pow2 = 0;          // largest power of two <= n (for findIndex descent)

  function rebuild() {
    tree = new Array(n + 1).fill(0);
    // Linear Fenwick build: add each height, propagate to the parent.
    for (let i = 1; i <= n; i++) {
      tree[i] += heights[i - 1];
      const parent = i + (i & -i);
      if (parent <= n) tree[parent] += tree[i];
    }
    pow2 = 1;
    while (pow2 * 2 <= n) pow2 *= 2;
  }

  // prefix(i) = sum of heights[0..i-1] (the top offset of item i); i in [0, n].
  function prefix(i) {
    let r = 0;
    for (let k = i; k > 0; k -= k & -k) r += tree[k];
    return r;
  }

  // findIndex(y) = the item index whose vertical span contains y, i.e. the
  // largest i with prefix(i) <= y. Clamped to [0, n-1]; n==0 → -1.
  function findIndex(y) {
    if (n === 0) return -1;
    if (y <= 0) return 0;
    let pos = 0;        // running item count whose cumulative height <= y
    let remaining = y;
    for (let step = pow2; step > 0; step >>= 1) {
      const next = pos + step;
      if (next <= n && tree[next] <= remaining) { pos = next; remaining -= tree[next]; }
    }
    return pos >= n ? n - 1 : pos;
  }

  function setHeight(i, h) {
    if (i < 0 || i >= n) return;
    const delta = h - heights[i];
    if (delta === 0) return;
    heights[i] = h;
    for (let k = i + 1; k <= n; k += k & -k) tree[k] += delta;
  }

  const api = {
    get count() { return n; },
    total() { return prefix(n); },
    prefix,
    findIndex,
    height(i) { return heights[i]; },
    setHeight,
    // reset replaces the whole set (filter/sort change) — heights back to estimate.
    reset(newCount) {
      n = Math.max(0, newCount | 0);
      heights = new Array(n);
      for (let i = 0; i < n; i++) heights[i] = est(i);
      rebuild();
    },
    // extend appends `add` items (infinite-scroll append); existing measured
    // heights are kept, the new ones start from the estimate.
    extend(add) {
      const start = n;
      n += Math.max(0, add | 0);
      for (let i = start; i < n; i++) heights[i] = est(i);
      rebuild();
    },
  };
  api.reset(count);
  return api;
}

// computeWindow turns a scroll position into the slice to render plus the two
// spacer pad heights. buffer = extra rows above/below the viewport.
export function computeWindow(idx, scrollTop, viewportH, buffer) {
  const n = idx.count;
  if (n === 0) return { firstIndex: 0, lastIndex: -1, topPad: 0, bottomPad: 0 };
  const top = Math.max(0, scrollTop);
  const first = Math.max(0, idx.findIndex(top) - buffer);
  const last = Math.min(n - 1, idx.findIndex(top + Math.max(0, viewportH)) + buffer);
  const topPad = idx.prefix(first);
  const bottomPad = Math.max(0, idx.total() - idx.prefix(last + 1));
  return { firstIndex: first, lastIndex: last, topPad, bottomPad };
}

// ── DOM-bound windowed list ──────────────────────────────────────────────────

/**
 * createVirtualList wires scroll + resize → window math → render + measure.
 *
 * opts:
 *   scrollEl        the overflow:auto scroll container (height is the viewport)
 *   sizerEl         element whose children become [topSpacer, …rows, bottomSpacer]
 *   makeSpacer(px)  → a spacer element of the given pixel height (e.g. a <tr>)
 *   renderRow(item, index) → the element for one item
 *   estimateHeight  number | (item,index)=>px — starting height before measured
 *   buffer          extra rows rendered above/below the viewport (default 6)
 *   fetchMore       optional async () => { items, done }; called near the tail
 *   prefetchPx      distance from the bottom that triggers fetchMore (default 800)
 *   onAfterRender(firstIndex, lastIndex)  hook after each window paint
 *   windowScroll    true = the page/window is the scroller (the sizer flows in the
 *                   document, e.g. the public library); the window slice is derived
 *                   from sizerEl's position in the viewport. Default false: scrollEl
 *                   is a fixed-height overflow container (e.g. the admin table).
 *
 * @returns {{ setItems, appendItems, refresh, scrollToTop, getItems, count, destroy }}
 */
export function createVirtualList(opts) {
  const {
    scrollEl, sizerEl, makeSpacer, renderRow,
    estimateHeight = 44, buffer = 6, fetchMore = null,
    prefetchPx = 800, onAfterRender = null, windowScroll = false,
  } = opts;

  const estOf = typeof estimateHeight === 'function'
    ? (i) => estimateHeight(items[i], i)
    : () => estimateHeight;

  let items = [];
  let idx = createHeightIndex(0, estOf);
  let hasMore = !!fetchMore;
  let fetching = false;
  let rafToken = 0;
  let destroyed = false;
  let suppressScroll = false;   // ignore the scroll event from a programmatic anchor

  // Scroll metrics, abstracted over the two modes. In window-scroll mode the
  // "scrollTop into the list" is how far the sizer's top has gone above the
  // viewport top (negative getBoundingClientRect top), and the viewport is the
  // window; the chrome above the list naturally falls out of the arithmetic.
  function viewportH() { return windowScroll ? window.innerHeight : scrollEl.clientHeight; }
  function scrollOffset() {
    return windowScroll ? Math.max(0, -sizerEl.getBoundingClientRect().top) : scrollEl.scrollTop;
  }
  function curWidth() { return windowScroll ? window.innerWidth : scrollEl.clientWidth; }
  let lastWidth = curWidth();

  // paint replaces the window in one shot: top spacer, the rendered rows, bottom
  // spacer. New row elements each paint — the window is small, so it's cheap.
  function paint(win) {
    const frag = document.createDocumentFragment();
    frag.appendChild(makeSpacer(win.topPad));
    for (let i = win.firstIndex; i <= win.lastIndex; i++) {
      const node = renderRow(items[i], i);
      if (node) { node.__vIndex = i; frag.appendChild(node); }
    }
    frag.appendChild(makeSpacer(win.bottomPad));
    sizerEl.replaceChildren(frag);
  }

  // measure reads the real height of each rendered row and corrects the index.
  // Only rows inside the window are measured, so rows above stay at their cached
  // height and the window's top offset (hence scroll position) doesn't drift.
  function measure(win) {
    let changed = false;
    for (let node = sizerEl.firstChild; node; node = node.nextSibling) {
      const i = node.__vIndex;
      if (i == null) continue;                 // a spacer
      const h = node.offsetHeight;
      if (h > 0 && h !== idx.height(i)) { idx.setHeight(i, h); changed = true; }
    }
    return changed;
  }

  function render() {
    if (destroyed || (!scrollEl && !windowScroll)) return;
    const vp = viewportH();
    // Up to 3 passes: a measurement can shrink/grow rows so the window no longer
    // covers the viewport; re-deriving it from corrected heights fills any gap.
    let win;
    for (let pass = 0; pass < 3; pass++) {
      win = computeWindow(idx, scrollOffset(), vp, buffer);
      paint(win);
      if (!measure(win)) break;
    }
    onAfterRender?.(win.firstIndex, win.lastIndex);
    maybeFetch();
  }

  function scheduleRender() {
    if (rafToken || destroyed) return;
    rafToken = requestAnimationFrame(() => { rafToken = 0; render(); });
  }

  async function maybeFetch() {
    if (!fetchMore || !hasMore || fetching || destroyed) return;
    const nearBottom = scrollOffset() + viewportH() >= idx.total() - prefetchPx;
    if (!nearBottom) return;
    fetching = true;
    try {
      const res = await fetchMore();
      if (destroyed) return;
      const more = (res && res.items) || [];
      if (more.length) appendItems(more);
      if (!res || res.done || !more.length) hasMore = false;
    } catch (err) {
      console.error('virtual-list fetchMore failed:', err);
      hasMore = false;        // stop hammering a failing endpoint
    } finally {
      fetching = false;
      if (!destroyed && hasMore) scheduleRender();   // keep filling if still short
    }
  }

  function onScroll() {
    if (suppressScroll) { suppressScroll = false; return; }
    scheduleRender();
  }

  // A width change usually means a layout-mode switch (the table's 640px card
  // mode), which changes every row's height — drop measurements back to estimates
  // so they re-measure. A height-only change just shows more/fewer rows.
  function onResize() {
    if (destroyed) return;
    const w = curWidth();
    if (w === 0) return;            // detached (consumer re-inserted the host) — ignore
    if (w !== lastWidth) { lastWidth = w; idx.reset(items.length); }
    scheduleRender();
  }

  const ro = (!windowScroll && typeof ResizeObserver !== 'undefined') ? new ResizeObserver(onResize) : null;

  // ── public surface ──────────────────────────────────────────────────────────
  function setItems(next, { keepScroll = false } = {}) {
    items = next || [];
    idx.reset(items.length);
    hasMore = !!fetchMore;
    // Reset the scroll position on a fresh data set (filter/sort/new load). In
    // window-scroll mode we leave the page where it is — the caller owns that.
    if (!keepScroll && !windowScroll && scrollEl) { suppressScroll = true; scrollEl.scrollTop = 0; }
    render();
  }
  function appendItems(more) {
    if (!more || !more.length) return;
    items = items.concat(more);
    idx.extend(more.length);
    render();
  }
  function refresh() { render(); }              // re-paint the current window (data unchanged)
  function scrollToTop() {
    if (windowScroll) window.scrollTo({ top: 0 });
    else if (scrollEl) scrollEl.scrollTop = 0;
    scheduleRender();
  }
  function getItems() { return items; }
  function count() { return items.length; }

  function destroy() {
    destroyed = true;
    if (rafToken) cancelAnimationFrame(rafToken);
    if (windowScroll) {
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onResize);
    } else {
      scrollEl?.removeEventListener('scroll', onScroll);
      ro?.disconnect();
    }
    if (sizerEl) sizerEl.replaceChildren();
  }

  if (windowScroll) {
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onResize);
  } else if (scrollEl) {
    scrollEl.addEventListener('scroll', onScroll, { passive: true });
    ro?.observe(scrollEl);
  }

  return { setItems, appendItems, refresh, scrollToTop, getItems, count, destroy };
}
