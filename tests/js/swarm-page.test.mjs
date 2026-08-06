// The /admin/swarm page's judgement calls — the handful of places where the
// module decides what a row says rather than just drawing what the API sent
// (webui/static/js/admin/swarm.js, docs/architecture/swarm-admin.md).
//
// Three of these exist because a real browser against a real cache found them
// and no Go test could have: an untagged cached blob is named by its hash, and
// the second line repeated that same hash; an empty list under a filter claimed
// the library was empty while sixty files sat behind the filter. Both are text
// decisions, which is precisely the kind of bug that survives a green API suite.
//
// swarm.js cannot be imported here — it reads page DOM at module scope (see
// admin-network-actions.test.mjs for the same constraint) — so each function's
// real source text is lifted out of the shipped file and evaluated with its free
// names passed in. That asserts the bytes that ship, which a copy would not.
//
// Run with: node --test tests/js/swarm-page.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const SRC = readFileSync(
  path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../webui/static/js/admin/swarm.js'),
  'utf8',
);

// Top-level functions in this file close with a `}` in the first column, which is
// all the parsing needed to lift one out.
function extract(name) {
  const start = SRC.search(new RegExp(String.raw`^(?:async )?function ${name}\(`, 'm'));
  assert.notEqual(start, -1, `${name} not found in swarm.js — was it renamed?`);
  const end = SRC.indexOf('\n}\n', start);
  assert.notEqual(end, -1, `could not find the end of ${name}`);
  return SRC.slice(start, end + 3);
}

// Builds the real function with every free name it closes over passed in.
function load(name, deps = {}) {
  const keys = Object.keys(deps);
  return new Function(...keys, `${extract(name)}\nreturn ${name};`)(...keys.map(k => deps[k]));
}

// ── The second line ──────────────────────────────────────────────────────────

test('the sub-line never repeats what the title already says', () => {
  const dispSub = load('dispSub');

  // A cached blob with no tags at all is titled by its hash (dispName), and the
  // old code printed the same hash underneath it. On a real cache — where most
  // rows carry no tags — that was every second row.
  assert.equal(dispSub({ hash: 'abc123', title: '', filename: '', artist: '', album: '' }), '');

  // A file whose only tag is its own filename says it once, not twice.
  assert.equal(dispSub({ title: 'song.mp3', filename: 'song.mp3' }), '');

  // But a real tagged row gets the credit line it exists for.
  assert.equal(dispSub({ title: 'Galya', artist: 'Dakh Daughters', album: 'Make Up' }),
    'Dakh Daughters — Make Up');
  // Artist alone is enough; the em dash is a separator, not decoration.
  assert.equal(dispSub({ title: 'Galya', artist: 'Dakh Daughters' }), 'Dakh Daughters');
  // Untagged but named on disk: the filename is the only thing left to add.
  assert.equal(dispSub({ title: 'Galya', filename: '04 galya.flac' }), '04 galya.flac');
});

// ── The empty message ────────────────────────────────────────────────────────

function emptyTextWith(state) {
  return load('emptyText', { state, PILL_EMPTY: eval(`(${SRC.match(/const PILL_EMPTY = (\{[\s\S]*?\});/)[1]})`) });
}

test('an empty list says which of the two empties it is', () => {
  // "No files in the library yet" while a filter hides sixty of them is simply
  // false, and it was what the page said until someone ran it.
  assert.match(emptyTextWith({ scope: 'library', pill: '', q: 'nothing' })(), /matches that search/i);
  assert.match(emptyTextWith({ scope: 'all', pill: 'trashed', q: '' })(), /trash/i);
  assert.match(emptyTextWith({ scope: 'all', pill: 'private', q: '' })(), /Local/);

  // With nothing asked for, it is a fact about the node — and the two halves are
  // different facts, because they arrive by different routes.
  assert.match(emptyTextWith({ scope: 'library', pill: '', q: '' })(), /library/i);
  assert.match(emptyTextWith({ scope: 'cache', pill: '', q: '' })(), /fetches/i);
  assert.match(emptyTextWith({ scope: 'all', pill: '', q: '' })(), /nothing uploaded and nothing fetched/i);

  // A pill the page does not know still reads as a filter, never as "you have
  // none" — an unknown token means the answer is unknown, not empty.
  assert.match(emptyTextWith({ scope: 'all', pill: 'invented', q: '' })(), /matches that filter/i);
});

// ── Why a blob is not seeding ────────────────────────────────────────────────

function whyWith(seeding, federationOn) {
  return load('whyNotSeeding', { seeding, federationOn });
}

test('the reason a blob is not seeding names the row before the node', () => {
  const on = whyWith({ enabled: true, cache: true }, true);

  // Facts about the row come first: a trashed blob would not be served even with
  // every switch on, so blaming a node-wide setting would send the admin to the
  // wrong page.
  assert.match(on({ trashed: true, review_state: 'approved', in_library: true }), /trash/i);
  assert.match(on({ review_state: 'draft', in_library: true }), /review/i);
  assert.match(on({ in_library: true, seedable: false, review_state: 'approved' }), /Local/);

  // A perfectly serveable row has nothing to explain.
  assert.equal(on({ in_library: true, seedable: true, review_state: 'approved' }), '');

  // Only then the node's own switches, and each says which one it is.
  const seedOff = whyWith({ enabled: false, cache: true }, true);
  assert.match(seedOff({ in_library: true, seedable: true, review_state: 'approved' }), /whole node/i);

  const cacheOff = whyWith({ enabled: true, cache: false }, true);
  assert.match(cacheOff({ in_cache: true, seedable: true }), /Cache seeding/i);
  // …and a library blob is unaffected by the cache switch.
  assert.equal(cacheOff({ in_library: true, seedable: true, review_state: 'approved' }), '');

  const fedOff = whyWith({ enabled: true, cache: true }, false);
  assert.match(fedOff({ in_library: true, seedable: true, review_state: 'approved' }), /Federation is off/i);
});

// ── The list request ─────────────────────────────────────────────────────────

test('the query carries only the filters actually set', () => {
  const build = state => load('params', { PAGE: 100, state })(0);

  // An empty q must be ABSENT, not q=. The forget endpoint's guardrail keys off
  // an empty filter meaning "everything", so the two must agree about what empty
  // looks like on the wire.
  const bare = build({ scope: 'all', pill: '', q: '', sort: 'newest' });
  assert.equal(bare.get('q'), null);
  assert.equal(bare.get('state'), null);
  assert.equal(bare.get('scope'), 'all');
  assert.equal(bare.get('limit'), '100');
  assert.equal(bare.get('offset'), '0');

  const full = load('params', { PAGE: 100, state: { scope: 'cache', pill: 'trashed', q: 'galya', sort: 'largest' } })(200);
  assert.equal(full.get('scope'), 'cache');
  assert.equal(full.get('state'), 'trashed');
  assert.equal(full.get('q'), 'galya');
  assert.equal(full.get('sort'), 'largest');
  assert.equal(full.get('offset'), '200');
});

// ── The limits line ──────────────────────────────────────────────────────────

test('a cap says where it came from, and 0 reads as unlimited', () => {
  const rateText = load('rateText');

  // The summary lying about which cap was in force is the defect that made
  // SwarmRates resolve before answering; the wording is the visible half of it.
  assert.match(rateText({ effective_kib: 1900, source: 'override' }), /1900 KiB\/s \(set here\)/);
  assert.match(rateText({ effective_kib: 512, source: 'config' }), /512 KiB\/s \(from config\)/);

  // 0 is a real override meaning unlimited — never "unset", and never "0 KiB/s",
  // which would read as a node that has been throttled to a standstill.
  assert.match(rateText({ effective_kib: 0, source: 'override' }), /^unlimited \(set here\)$/);
  assert.equal(rateText(null), '—');
});
