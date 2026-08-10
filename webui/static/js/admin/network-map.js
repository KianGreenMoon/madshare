// The network map (federation F6; navigation at scale is F7 item 7): the
// gossiped friendship graph drawn as a node-link diagram, with a detail panel
// that can act on what it shows.
//
// Two things shape the layout. Nodes are pulled onto RINGS by hop distance —
// your friends on the inner ring, their friends outside it — because distance
// is the one property that decides how much a claim is worth here, and a free
// force layout hides it. And labels thin out as the graph grows: past a few
// dozen nodes every label drawn is a label overlapping another, so beyond that
// only the nodes you have a relationship with (plus whatever you touch) keep
// theirs. Both are concessions to the honest weakness of a drawn graph, which
// is that it stops being readable long before it stops being interesting.
//
// Since the community is unbounded, the map scales by SHOWING LESS AT A TIME:
// a **view radius** (default 3 hops, expandable), zoom that resolves detail
// rather than truncating it, a **search that still reaches the whole component**
// even when the view does not, **branch highlighting** — everything that arrived
// through one friend, which is the unit blocking operates on — and **the paths
// between two nodes**, which is the question an admin actually has when
// something looks wrong.
//
// The radius is a RENDERING setting. It never limits who is served and never
// appears in a scope; it is about what an admin looks at, and `share_depth` is
// about whom we answer (docs/architecture/federation-trust.md §The network map).
//
// Names beyond your own friends are hearsay: the key rides along everywhere.

import { API, el, toast } from './shared.js';

const SVGNS = 'http://www.w3.org/2000/svg';

// Layout constants. RING is the gap between hop rings; the forces are tuned so
// a node settles near its ring without the graph freezing into a rigid wheel.
const RING = 110;
const REPULSION = 14000;
const SPRING = 0.012;
const SPRING_LEN = 78;
const RADIAL = 0.05;
const DAMPING = 0.86;
const LABEL_LIMIT = 40; // above this many nodes, labels thin out

// DEFAULT_RADIUS mirrors federation.DefaultMapRadius: the neighbourhood, not
// the component. WHOLE is the "show everything" value the server also reads.
const DEFAULT_RADIUS = 3;
const WHOLE = 0;

let state = {
  nodes: [],
  edges: [],
  byKey: new Map(),
  neighbours: new Map(),
  selected: null,
  hovered: null,
  view: { x: 0, y: 0, scale: 1 },
  alpha: 0,
  raf: 0,
  filter: '',
  radius: DEFAULT_RADIUS, // hops drawn; 0 = the whole component
  fullRadius: 0,          // how far this node can see, whatever we are drawing
  hidden: 0,              // nodes the radius is holding back
  branch: null,           // { key, keys:Set } — everything through one friend
  paths: [],              // [[key, …], …] between two nodes
  pathTo: null,           // the far end those paths run to
  pathEdges: new Set(),   // "from|to" pairs lit as part of a path
};

let svg, gRoot, gEdges, gNodes, detail, statsEl, emptyEl;
let hitsEl, radiusEl, radiusNote;
let findTimer = 0;
let onBlock = null;  // injected by network.js so the map reuses its modal + toasts
let onFriend = null; // likewise for the pairing request a stranger node can be sent
let onPull = null;   // and for asking the frontier to fetch this node's catalog now

// ── Building ─────────────────────────────────────────────────────────────────

function svgEl(name, attrs = {}) {
  const n = document.createElementNS(SVGNS, name);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  return n;
}

const shortKey = k => `${k.slice(0, 8)}…${k.slice(-4)}`;
// Our own node carries no name on the graph — nobody gossips about us to us — so
// it would otherwise render as a bare key prefix, including at the head of every
// path chain, where the one thing the reader already knows is where it starts.
const labelOf = n => (n.state === 'self' ? 'This node' : n.name || shortKey(n.key));

function stateClass(n) {
  if (n.state === 'self') return 'is-self';
  if (n.state === 'friend') return 'is-friend';
  if (n.state === 'pending') return 'is-pending';
  if (n.state === 'blocked') return 'is-blocked';
  return 'is-stranger';
}

// seedPositions places nodes on their ring before the simulation starts, so the
// layout converges from something already roughly right instead of untangling
// itself from a random cloud while the admin watches.
function seedPositions(nodes) {
  const perRing = new Map();
  for (const n of nodes) perRing.set(n.distance, (perRing.get(n.distance) ?? 0) + 1);
  const placed = new Map();
  for (const n of nodes) {
    if (n.distance === 0) { n.x = 0; n.y = 0; n.vx = 0; n.vy = 0; continue; }
    const i = placed.get(n.distance) ?? 0;
    placed.set(n.distance, i + 1);
    const count = perRing.get(n.distance);
    const angle = (i / count) * Math.PI * 2 + n.distance * 0.7;
    const r = n.distance * RING;
    n.x = Math.cos(angle) * r;
    n.y = Math.sin(angle) * r;
    n.vx = 0;
    n.vy = 0;
  }
}

// ── The simulation ───────────────────────────────────────────────────────────

function tick() {
  const { nodes, edges } = state;

  // Repulsion, every pair. O(n²) is honest at this scale: a friend network in
  // the hundreds costs well under a millisecond per frame, and a quadtree would
  // be a lot of code to save time nobody is waiting on.
  for (let i = 0; i < nodes.length; i++) {
    const a = nodes[i];
    for (let j = i + 1; j < nodes.length; j++) {
      const b = nodes[j];
      let dx = b.x - a.x, dy = b.y - a.y;
      let d2 = dx * dx + dy * dy;
      if (d2 < 1) { d2 = 1; dx = (Math.random() - 0.5); dy = (Math.random() - 0.5); }
      const f = REPULSION / d2;
      const d = Math.sqrt(d2);
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.vx -= fx; a.vy -= fy;
      b.vx += fx; b.vy += fy;
    }
  }

  // Springs along friendships.
  for (const e of edges) {
    const a = state.byKey.get(e.from), b = state.byKey.get(e.to);
    if (!a || !b) continue;
    const dx = b.x - a.x, dy = b.y - a.y;
    const d = Math.hypot(dx, dy) || 1;
    const f = (d - SPRING_LEN) * SPRING;
    const fx = (dx / d) * f, fy = (dy / d) * f;
    a.vx += fx; a.vy += fy;
    b.vx -= fx; b.vy -= fy;
  }

  // Radial pull toward the node's hop ring — what keeps distance readable.
  for (const n of nodes) {
    if (n.distance === 0) continue;
    const d = Math.hypot(n.x, n.y) || 1;
    const target = n.distance * RING;
    const f = (target - d) * RADIAL;
    n.vx += (n.x / d) * f;
    n.vy += (n.y / d) * f;
  }

  for (const n of nodes) {
    if (n.distance === 0 || n.pinned) { n.vx = 0; n.vy = 0; continue; }
    n.vx *= DAMPING; n.vy *= DAMPING;
    n.x += n.vx * state.alpha;
    n.y += n.vy * state.alpha;
  }
  state.alpha *= 0.985;
}

function animate() {
  state.raf = 0;
  if (state.alpha > 0.005) {
    tick();
    draw();
    state.raf = requestAnimationFrame(animate);
  } else {
    draw();
  }
}

function reheat(alpha = 1) {
  state.alpha = Math.max(state.alpha, alpha);
  if (!state.raf) state.raf = requestAnimationFrame(animate);
}

// ── Drawing ──────────────────────────────────────────────────────────────────

// markVisible flags which nodes are inside the current viewport and whether
// there is room to name them. This is what makes zoom a way of READING the graph
// rather than of cropping it: pull back and the frame holds more nodes than it
// can label, push in and the ones you are looking at resolve.
//
// Counting the whole graph instead — which this did first — means a large
// community can never resolve at all, because no reachable zoom level changes a
// total. The number that matters is how many nodes share the frame.
function markVisible() {
  const rect = svg.getBoundingClientRect();
  const { x, y, scale } = state.view;
  const pad = 48; // a node just off-screen still owns the space its label needs
  let inFrame = 0;
  for (const n of state.nodes) {
    const sx = n.x * scale + x, sy = n.y * scale + y;
    n.inView = sx >= -pad && sx <= rect.width + pad && sy >= -pad && sy <= rect.height + pad;
    if (n.inView) inFrame++;
  }
  state.labelRoom = inFrame <= LABEL_LIMIT;
}

function showLabel(n) {
  if (state.labelRoom && n.inView) return true;
  if (n.state && n.state !== '') return true; // anyone we have a relationship with
  if (state.selected === n.key || state.hovered === n.key) return true;
  if (state.paths.length && onAnyPath(n.key)) return true;
  const near = state.neighbours.get(state.selected ?? state.hovered);
  return !!near?.has(n.key);
}

function matchesFilter(n) {
  if (!state.filter) return false;
  const f = state.filter.toLowerCase();
  return n.key.toLowerCase().includes(f) || (n.name ?? '').toLowerCase().includes(f)
    || (n.address ?? '').toLowerCase().includes(f);
}

function onAnyPath(key) {
  return state.paths.some(p => p.includes(key));
}

// pathEdgeKey normalises an edge to the undirected pair the path highlighting
// looks up — a path names nodes, the map draws lines, and the two have to agree
// on which line a step is.
function pathEdgeKey(a, b) {
  return a < b ? `${a}|${b}` : `${b}|${a}`;
}

function recomputePathEdges() {
  state.pathEdges = new Set();
  for (const p of state.paths) {
    for (let i = 1; i < p.length; i++) state.pathEdges.add(pathEdgeKey(p[i - 1], p[i]));
  }
}

function draw() {
  const { view } = state;
  gRoot.setAttribute('transform', `translate(${view.x} ${view.y}) scale(${view.scale})`);
  markVisible();

  const focus = state.selected ?? state.hovered;
  const near = state.neighbours.get(focus);

  const pathing = state.paths.length > 0;

  for (const e of state.edges) {
    if (!e.line) continue;
    const a = state.byKey.get(e.from), b = state.byKey.get(e.to);
    if (!a || !b) continue;
    e.line.setAttribute('x1', a.x); e.line.setAttribute('y1', a.y);
    e.line.setAttribute('x2', b.x); e.line.setAttribute('y2', b.y);
    // A drawn path outranks the hover highlight: it is an answer the admin
    // asked for, while the highlight is wherever the pointer happens to be.
    const onPath = pathing && state.pathEdges.has(pathEdgeKey(e.from, e.to));
    const lit = onPath || (!pathing && focus && (e.from === focus || e.to === focus));
    e.line.classList.toggle('is-path', !!onPath);
    e.line.classList.toggle('is-lit', !!lit);
    e.line.classList.toggle('is-dim', pathing ? !onPath : (!!focus && !lit));
  }

  for (const n of state.nodes) {
    if (!n.g) continue;
    n.g.setAttribute('transform', `translate(${n.x} ${n.y})`);
    const inBranch = state.branch?.keys.has(n.key);
    const dim = pathing
      ? !onAnyPath(n.key)
      : (state.branch ? !inBranch : focus && n.key !== focus && !near?.has(n.key));
    n.g.classList.toggle('is-dim', !!dim);
    n.g.classList.toggle('is-branch', !!inBranch);
    n.g.classList.toggle('is-selected', state.selected === n.key);
    n.g.classList.toggle('is-hit', matchesFilter(n));
    const label = n.g.querySelector('text');
    if (label) label.style.display = showLabel(n) ? '' : 'none';
  }
}

function build() {
  gEdges.replaceChildren();
  gNodes.replaceChildren();

  for (const e of state.edges) {
    e.line = svgEl('line', { class: 'map-edge' + (e.mutual ? '' : ' is-onesided') });
    gEdges.append(e.line);
  }

  for (const n of state.nodes) {
    const g = svgEl('g', { class: `map-node ${stateClass(n)}`, tabindex: '0', role: 'button' });
    const r = n.distance === 0 ? 13 : n.state ? 10 : 7;
    if (n.mark_branches > 0) {
      g.append(svgEl('circle', { class: 'map-node-warn', r: r + 4 }));
    }
    g.append(svgEl('circle', { class: 'map-node-dot', r }));
    const label = svgEl('text', { class: 'map-node-label', y: -(r + 7) });
    label.textContent = labelOf(n);
    g.append(label);

    g.addEventListener('click', ev => { ev.stopPropagation(); select(n.key); });
    g.addEventListener('keydown', ev => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); select(n.key); }
    });
    g.addEventListener('pointerenter', () => { state.hovered = n.key; draw(); });
    g.addEventListener('pointerleave', () => { state.hovered = null; draw(); });
    g.addEventListener('pointerdown', ev => startDrag(ev, n));

    n.g = g;
    gNodes.append(g);
  }
  draw();
}

// ── Interaction ──────────────────────────────────────────────────────────────

function startDrag(ev, n) {
  ev.stopPropagation();
  ev.preventDefault();
  const start = clientToWorld(ev.clientX, ev.clientY);
  const ox = n.x - start.x, oy = n.y - start.y;
  n.pinned = true;
  const move = e => {
    const p = clientToWorld(e.clientX, e.clientY);
    n.x = p.x + ox; n.y = p.y + oy;
    draw();
  };
  const up = () => {
    n.pinned = false;
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    reheat(0.5);
  };
  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}

function clientToWorld(cx, cy) {
  const rect = svg.getBoundingClientRect();
  return {
    x: (cx - rect.left - state.view.x) / state.view.scale,
    y: (cy - rect.top - state.view.y) / state.view.scale,
  };
}

function startPan(ev) {
  if (ev.target.closest('.map-node')) return;
  const sx = ev.clientX, sy = ev.clientY;
  const ox = state.view.x, oy = state.view.y;
  const move = e => {
    state.view.x = ox + (e.clientX - sx);
    state.view.y = oy + (e.clientY - sy);
    draw();
  };
  const up = () => {
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
  };
  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}

function onWheel(ev) {
  ev.preventDefault();
  const rect = svg.getBoundingClientRect();
  const mx = ev.clientX - rect.left, my = ev.clientY - rect.top;
  const before = state.view.scale;
  const next = Math.min(3, Math.max(0.2, before * (ev.deltaY < 0 ? 1.12 : 0.89)));
  // Zoom about the pointer, so the thing under the cursor stays put.
  state.view.x = mx - ((mx - state.view.x) / before) * next;
  state.view.y = my - ((my - state.view.y) / before) * next;
  state.view.scale = next;
  draw();
}

function resetView() {
  const rect = svg.getBoundingClientRect();
  state.view = { x: rect.width / 2, y: rect.height / 2, scale: 1 };
  draw();
}

// centreOn pans the view so a node sits in the middle of the stage — what makes
// "find" and the library's holder links land somewhere the eye can start from,
// rather than merely selecting something off-screen.
function centreOn(key) {
  const n = state.byKey.get(key);
  if (!n) return;
  const rect = svg.getBoundingClientRect();
  state.view.x = rect.width / 2 - n.x * state.view.scale;
  state.view.y = rect.height / 2 - n.y * state.view.scale;
  draw();
}

// ── Search over the whole community ──────────────────────────────────────────

// runFind asks the server, not the loaded subgraph: the view radius decides what
// is DRAWN, and a search that could only find the drawn part would make the
// radius a cost instead of a convenience.
async function runFind(q) {
  if (!q) { renderHits(null); return; }
  try {
    const res = await fetch(`${API}/api/admin/federation/graph/find?q=${encodeURIComponent(q)}`);
    if (!res.ok) return;
    const body = await res.json();
    renderHits(body.hits ?? []);
  } catch { /* a failed search leaves the last results alone */ }
}

function renderHits(hits) {
  if (!hitsEl) return;
  if (!hits) { hitsEl.hidden = true; hitsEl.replaceChildren(); return; }
  if (!hits.length) {
    hitsEl.replaceChildren(el('p', { class: 'map-hits-empty', text: 'No node matches that.' }));
    hitsEl.hidden = false;
    return;
  }
  hitsEl.replaceChildren(...hits.map(h => {
    const row = el('div', { class: 'map-hit', role: 'option', tabindex: '0' }, [
      el('span', { class: 'map-hit-name', text: labelOf(h) }),
      // Which field answered is part of the result: a key and an address are
      // facts, a name past our own friends is hearsay a friend passed on.
      el('span', { class: 'map-hit-why', text: h.matched === 'name' ? 'name (hearsay)' : `by ${h.matched}` }),
      el('code', { class: 'map-hit-key', text: shortKey(h.key) }),
      el('span', { class: 'map-hit-dist', text: distanceText(h) }),
    ]);
    const go = () => { renderHits(null); focusNode(h.key); };
    row.addEventListener('click', go);
    row.addEventListener('keydown', ev => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); go(); }
    });
    if (h.state === 'friend') {
      // A friend is the root of a branch, and the branch is what a block takes
      // with it — so the shortcut belongs next to the friend, here.
      row.append(el('button', {
        class: 'btn btn-neutral btn-mini map-hit-branch',
        text: 'Branch',
        title: 'Highlight everything that reached us through this friend',
        onclick: ev => { ev.stopPropagation(); renderHits(null); showBranch(h.key); },
      }));
    }
    return row;
  }));
  hitsEl.hidden = false;
}

// focusNode selects and centres a node, EXPANDING THE VIEW if it sits outside
// the current radius. Searching for something and being shown nothing would make
// the radius feel like a limit on the network rather than on the drawing.
async function focusNode(key) {
  if (!state.byKey.has(key)) {
    await loadMap({ radius: WHOLE });
    if (!state.byKey.has(key)) {
      toast('That node is not on the graph any more.', 'error');
      return;
    }
    if (radiusEl) radiusEl.value = String(WHOLE);
  }
  clearOverlays();
  state.selected = key;
  renderDetail();
  centreOn(key);
  draw();
}

// ── Branches: what one block would take with it ──────────────────────────────

async function showBranch(friendKey) {
  try {
    const res = await fetch(`${API}/api/admin/federation/graph/find?branch=${encodeURIComponent(friendKey)}`);
    if (!res.ok) return;
    const body = await res.json();
    const keys = new Set((body.nodes ?? []).map(n => n.key));
    state.branch = { key: friendKey, keys, count: body.count ?? keys.size };
    state.paths = [];
    state.pathEdges = new Set();
    state.selected = friendKey;
    renderDetail();
    draw();
    renderRadiusNote();
  } catch { /* leave the view as it was */ }
}

// ── Paths: how is this node connected to me, and through whom ───────────────

async function showPaths(toKey) {
  try {
    const res = await fetch(`${API}/api/admin/federation/graph/paths?to=${encodeURIComponent(toKey)}`);
    if (!res.ok) return;
    const body = await res.json();
    state.paths = body.paths ?? [];
    state.pathTo = toKey;
    state.branch = null;
    recomputePathEdges();
    // A path can run through nodes the radius is not drawing, and a picture of
    // a connection with its middle missing is worse than no picture.
    const missing = state.paths.some(p => p.some(k => !state.byKey.has(k)));
    if (missing) {
      await loadMap({ radius: WHOLE, keepOverlays: true });
      if (radiusEl) radiusEl.value = String(WHOLE);
    }
    renderDetail();
    draw();
  } catch { /* leave the view as it was */ }
}

function clearOverlays() {
  state.branch = null;
  state.paths = [];
  state.pathTo = null;
  state.pathEdges = new Set();
}

// ── The detail panel ─────────────────────────────────────────────────────────

function select(key) {
  state.selected = state.selected === key ? null : key;
  // Selecting somewhere else abandons the question the overlay was answering,
  // rather than leaving a stale branch or path lit under a new subject.
  clearOverlays();
  renderDetail();
  draw();
}

function renderDetail() {
  const n = state.selected ? state.byKey.get(state.selected) : null;
  if (!n) { detail.hidden = true; detail.replaceChildren(); return; }

  const rows = [];
  const nameLine = el('div', { class: 'map-detail-name' }, [
    el('span', { text: labelOf(n) }),
  ]);
  if (n.named === 'heard') {
    // The single most important thing this panel says about a stranger.
    nameLine.append(el('span', { class: 'map-hearsay', text: 'name heard from a friend' }));
  } else if (n.named === 'local') {
    nameLine.append(el('span', { class: 'map-yours', text: 'your label' }));
  }
  rows.push(nameLine);

  rows.push(el('dl', { class: 'map-detail-grid' }, [
    el('div', {}, [el('dt', { text: 'Key' }), el('dd', {}, [el('code', { class: 'map-key', text: n.key })])]),
    el('div', {}, [el('dt', { text: 'Mesh address' }), el('dd', {}, [el('code', { class: 'map-key', text: n.address || '—' })])]),
    el('div', {}, [el('dt', { text: 'Distance' }), el('dd', { text: distanceText(n) })]),
    el('div', {}, [el('dt', { text: 'Reachable via' }), el('dd', { text: viaText(n) })]),
  ]));

  // How this node is joined to us, and through whom — the question a block is
  // the answer to, so it sits above the actions rather than in a tooltip.
  if (n.state !== 'self') {
    rows.push(buildPathsSection(n));
  }
  if (n.state === 'friend' || state.branch?.key === n.key) {
    rows.push(buildBranchSection(n));
  }

  if (n.marks?.length) {
    const branches = n.mark_branches ?? 0;
    rows.push(el('div', { class: 'map-marks' }, [
      el('p', { class: 'map-marks-head' }, [
        `Distrusted by ${branches} ${branches === 1 ? 'branch' : 'branches'}`,
        el('span', { class: 'map-marks-note', text: ` (${n.marks.length} mark${n.marks.length === 1 ? '' : 's'})` }),
      ]),
      // One branch is one voice: a farm behind a single friendship shouts once,
      // so the branch count leads and the raw mark count is the footnote.
      el('ul', { class: 'map-mark-list' }, n.marks.map(m => el('li', {}, [
        el('span', { class: 'map-mark-who', text: m.origin_name || shortKey(m.origin) }),
        el('span', { class: 'map-mark-why', text: m.reason || '(no reason given)' }),
      ]))),
    ]));
  }

  const actions = el('div', { class: 'map-detail-actions' });
  if (n.state === 'self') {
    actions.append(el('p', { class: 'map-note', text: 'This is your node.' }));
  } else if (n.state === 'blocked') {
    actions.append(el('p', { class: 'map-note', text: 'You have blocked this node; the block is published as a distrust mark.' }));
  } else {
    // Catalogs from beyond the friend ring are pulled a few nodes per cycle, so
    // a particular node can be hours away from its turn. This asks for it now —
    // interest beats rotation.
    if (onPull) {
      actions.append(el('button', {
        class: 'btn btn-neutral btn-mini',
        text: 'Fetch library now',
        title: 'Pull this node’s catalog on the next refresh round instead of waiting for its turn',
        onclick: () => onPull(n),
      }));
    }
    // A node the graph names but we have no relationship with — a friend of a
    // friend, or further out. The map already carries its key, which is all a
    // pairing needs, so the friendship graph can be grown from here instead of
    // only pruned: friending stays mutual, this sends the request.
    if (onFriend && !n.state) {
      actions.append(el('button', {
        class: 'btn btn-neutral btn-mini',
        text: 'Ask to be friends…',
        onclick: () => onFriend(n),
      }));
    }
    if (n.state === 'pending') {
      actions.append(el('p', { class: 'map-note', text: 'A pairing with this node is under way — its card in the node list has the details.' }));
    }
    if (onBlock) {
      actions.append(el('button', {
        class: 'btn btn-destructive-solid btn-mini',
        text: 'Block…',
        onclick: () => onBlock(n),
      }));
    }
  }
  rows.push(actions);

  detail.replaceChildren(
    el('button', { class: 'btn-close map-detail-close', text: '×', 'aria-label': 'Close', onclick: () => select(n.key) }),
    ...rows,
  );
  detail.hidden = false;
}

// buildPathsSection: on demand, every way this node is joined to us. Shown as
// chains of names because that is how the answer is used — "through whom" — and
// the map lights the same lines so the two readings agree.
function buildPathsSection(n) {
  const box = el('div', { class: 'map-paths' });
  const showing = state.pathTo === n.key && state.paths.length > 0;

  if (!showing) {
    box.append(el('button', {
      class: 'btn btn-neutral btn-mini',
      text: 'Show how we are connected',
      title: 'Every path from this node to yours, through the friendships that carry it',
      onclick: () => showPaths(n.key),
    }));
    if (state.pathTo === n.key) {
      box.append(el('p', { class: 'map-note', text: 'No path to this node on the graph we hold.' }));
    }
    return box;
  }

  box.append(el('p', { class: 'map-paths-head' },
    [`${state.paths.length} path${state.paths.length === 1 ? '' : 's'} from your node`]));
  box.append(el('ol', { class: 'map-path-list' }, state.paths.map(p =>
    el('li', {}, [
      el('span', {
        class: 'map-path-chain',
        text: p.map(k => {
          const node = state.byKey.get(k);
          return node ? labelOf(node) : shortKey(k);
        }).join(' → '),
      }),
      el('span', { class: 'map-path-len', text: `${p.length - 1} hop${p.length === 2 ? '' : 's'}` }),
    ]))));
  box.append(el('button', {
    class: 'btn btn-neutral btn-mini',
    text: 'Clear paths',
    onclick: () => { clearOverlays(); renderDetail(); draw(); },
  }));
  return box;
}

// buildBranchSection: everything that reached us through this friend. The count
// is the honest size of what blocking them would forget — minus whatever a
// second friend also vouches for, which is why the highlight shows the set
// rather than just naming a number.
function buildBranchSection(n) {
  const box = el('div', { class: 'map-branch' });
  if (state.branch?.key === n.key) {
    box.append(
      el('p', { class: 'map-branch-head' },
        [`${state.branch.count} node${state.branch.count === 1 ? '' : 's'} reached us through this friend`]),
      el('p', { class: 'map-note', text: 'Highlighted on the map. Nodes another friend also vouches for would survive a block here.' }),
      el('button', {
        class: 'btn btn-neutral btn-mini',
        text: 'Clear highlight',
        onclick: () => { clearOverlays(); renderDetail(); draw(); },
      }),
    );
    return box;
  }
  box.append(el('button', {
    class: 'btn btn-neutral btn-mini',
    text: 'Highlight this branch',
    title: 'Everything that reached us through this friend — the unit a block operates on',
    onclick: () => showBranch(n.key),
  }));
  return box;
}

function distanceText(n) {
  if (n.distance === 0) return 'This node';
  if (n.distance === 1) return 'Your friend (1 hop)';
  return `${n.distance} hops`;
}

function viaText(n) {
  if (n.distance === 0) return '—';
  const names = (n.via ?? []).map(k => {
    const v = state.byKey.get(k);
    return v ? labelOf(v) : shortKey(k);
  });
  if (!names.length) return '—';
  return names.join(', ');
}

// ── Public entry ─────────────────────────────────────────────────────────────

export function initMap({ onBlockNode, onFriendNode, onPullNode }) {
  onBlock = onBlockNode;
  onFriend = onFriendNode;
  onPull = onPullNode;
  svg = document.getElementById('mapSvg');
  detail = document.getElementById('mapDetail');
  statsEl = document.getElementById('mapStats');
  emptyEl = document.getElementById('mapEmpty');
  hitsEl = document.getElementById('mapHits');
  radiusEl = document.getElementById('mapRadius');
  radiusNote = document.getElementById('mapRadiusNote');
  if (!svg) return;

  gRoot = svgEl('g');
  gEdges = svgEl('g');
  gNodes = svgEl('g');
  gRoot.append(gEdges, gNodes);
  svg.append(gRoot);

  svg.addEventListener('pointerdown', startPan);
  svg.addEventListener('wheel', onWheel, { passive: false });
  svg.addEventListener('click', ev => { if (!ev.target.closest('.map-node')) select(null); });
  document.getElementById('mapReset')?.addEventListener('click', () => {
    clearOverlays();
    renderDetail();
    resetView();
    reheat(0.6);
  });
  document.getElementById('mapRescan')?.addEventListener('click', rescan);

  // The search does two things at once, on purpose: it lights matches already on
  // screen (instant, no round trip) and it lists matches from the whole
  // community underneath (one round trip, reaches past the radius).
  document.getElementById('mapSearch')?.addEventListener('input', ev => {
    const q = ev.target.value.trim();
    state.filter = q;
    draw();
    clearTimeout(findTimer);
    findTimer = setTimeout(() => runFind(q), 200);
  });
  hitsEl?.addEventListener('pointerdown', ev => ev.stopPropagation());
  document.addEventListener('click', ev => {
    if (hitsEl && !hitsEl.hidden && !ev.target.closest('.map-find')) renderHits(null);
  });

  radiusEl?.addEventListener('change', () => {
    loadMap({ radius: Number(radiusEl.value) });
  });
  window.addEventListener('resize', () => draw());
}

// renderRadiusNote says what the current view is holding back — and says, once,
// that the number draws less rather than serving less. An admin who reads a
// radius as a sharing setting has misread the one thing this design most needs
// them not to (docs/architecture/federation-trust.md §The network map).
function renderRadiusNote() {
  if (!radiusNote) return;
  const bits = [];
  if (state.hidden > 0) {
    bits.push(`${state.hidden} node${state.hidden === 1 ? '' : 's'} further out than this view — `
      + `search still finds them, and “whole network” draws them.`);
  }
  if (state.branch) {
    const n = state.byKey.get(state.branch.key);
    bits.push(`Highlighting the branch behind ${n ? labelOf(n) : shortKey(state.branch.key)}.`);
  }
  if (state.radius !== WHOLE) {
    bits.push('This setting changes what is drawn, never who this node shares with.');
  }
  radiusNote.textContent = bits.join(' ');
  radiusNote.hidden = bits.length === 0;
}

// rescan asks the node to pull the graph from every friend on its next refresh
// round, rather than when the sync cadence next comes due, then reloads the map
// once that round has had time to run.
//
// The endpoint answers 202 and returns immediately — the work happens on the
// background loop — so the delay here is only about when it is worth looking
// again. A press during a round already running folds into it (the server
// coalesces), which is why the button re-enables rather than queueing.
//
// What it buys is deliberately understated in the note beside it: gossip travels
// one ring per round, so this makes the map as fresh as our FRIENDS' stores, not
// as fresh as the network (docs/architecture/federation-trust.md §Refreshing the graph
// on demand).
async function rescan() {
  const btn = document.getElementById('mapRescan');
  const note = document.getElementById('mapRescanNote');
  if (!btn || btn.disabled) return;
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = 'Rescanning…';
  if (note) note.hidden = false;
  try {
    const res = await fetch(`${API}/api/admin/federation/graph/resync`, { method: 'POST' });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      toast(body.error || `Rescan failed (HTTP ${res.status}).`, 'error');
      return;
    }
    // One refresh round plus the round-trips it makes. Long enough that the
    // reload usually shows the result, short enough to still feel like an act.
    await new Promise(done => setTimeout(done, 3000));
    const before = state.nodes.length;
    await loadMap();
    const delta = state.nodes.length - before;
    // Say what happened rather than that it succeeded. A round can legitimately
    // learn nothing — the friends had nothing new — and reporting that as a
    // refreshed map would train an admin to distrust the button.
    toast(delta === 0
      ? 'Rescanned — your friends had nothing new for us.'
      : `Rescanned — ${delta > 0 ? '+' : ''}${delta} node${Math.abs(delta) === 1 ? '' : 's'} on the map.`, 'info');
  } catch (err) {
    toast(`Rescan failed: ${err.message}`, 'error');
  } finally {
    btn.disabled = false;
    btn.textContent = label;
    if (note) note.hidden = true;
  }
}

// loadMap fetches the graph at the current view radius and rebuilds. The radius
// is sent to the server rather than applied here, so a large community costs a
// small payload and a small simulation — the whole point of showing less at a
// time (F7 item 7).
export async function loadMap({ radius, keepOverlays } = {}) {
  const section = document.getElementById('mapSection');
  if (!section || !svg) return;
  if (radius != null) state.radius = radius;
  let graph;
  try {
    const res = await fetch(`${API}/api/admin/federation/graph?radius=${state.radius}`);
    if (!res.ok) return;
    const body = await res.json();
    graph = body.graph;
  } catch {
    return;
  }
  if (!graph) return;
  if (!keepOverlays) clearOverlays();

  section.hidden = false;
  state.nodes = (graph.nodes ?? []).map(n => ({ ...n }));
  state.edges = (graph.edges ?? []).map(e => ({ ...e }));
  state.byKey = new Map(state.nodes.map(n => [n.key, n]));
  state.neighbours = new Map(state.nodes.map(n => [n.key, new Set()]));
  for (const e of state.edges) {
    state.neighbours.get(e.from)?.add(e.to);
    state.neighbours.get(e.to)?.add(e.from);
  }
  if (state.selected && !state.byKey.has(state.selected)) state.selected = null;
  state.fullRadius = graph.radius ?? 0;
  state.hidden = graph.hidden ?? 0;
  if (state.paths.length) recomputePathEdges();

  const others = state.nodes.length - 1;
  const total = others + state.hidden;
  statsEl.textContent = others > 0
    ? `${others} node${others === 1 ? '' : 's'} drawn`
      + (state.hidden ? ` of ${total}` : '')
      + ` · ${state.edges.length} link${state.edges.length === 1 ? '' : 's'}`
      + ` · reach ${state.fullRadius} hop${state.fullRadius === 1 ? '' : 's'}`
    : '';
  emptyEl.hidden = others > 0;
  renderRadiusNote();

  seedPositions(state.nodes);
  build();
  resetView();
  renderDetail();
  reheat(1);
}

// focusKey is the map's entry point from elsewhere in the UI — the madnetwork
// library's ⓘ holder list links here, so discovery of a bad actor can start from
// the content that exposed it rather than from an admin remembering to come look
// at a diagram (docs/architecture/federation-trust.md §The network map).
export async function focusKey(key) {
  if (!svg || !key) return;
  await focusNode(key);
}
