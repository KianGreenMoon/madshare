// The /admin/network peer actions — accept, block, unblock — all go through one
// helper, peerOp (webui/static/js/admin/network.js), and it had a bug that no
// test could have caught the usual way:
//
//	async function peerOp(p, op, doneMsg, body = null) {
//	  const res = await fetch(url, { ...(body ? { body: JSON.stringify(body) } : {}) });
//	  const body = await res.json();   // ← puts the read above in a dead zone
//
// Legal to parse, and it throws on every call before the request is ever sent
// ("can't access lexical declaration 'body' before initialization"). The throw
// lands in peerOp's own try/catch and surfaces as a toast that reads like a
// server problem, so accepting a pairing request silently did nothing for five
// days — while importing a card, which does not go through peerOp, kept working.
// That is why the friendship it broke looked like a federation fault.
//
// network.js cannot be imported here: it reads page DOM at module scope. So the
// helper's real source text is lifted out of the file and evaluated with stubs.
// This asserts the actual shipped bytes, which a copy of the function would not.
//
// Run with: node --test tests/js/admin-network-actions.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const SRC = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../webui/static/js/admin/network.js',
);

// Top-level functions in this file close with a `}` in the first column, which
// is all the parsing needed to lift one out.
function extract(src, name) {
  const start = src.search(new RegExp(String.raw`^(?:async )?function ${name}\(`, 'm'));
  assert.notEqual(start, -1, `${name} not found in network.js — was it renamed?`);
  const end = src.indexOf('\n}\n', start);
  assert.notEqual(end, -1, `could not find the end of ${name}`);
  return src.slice(start, end + 3);
}

// Builds the real function with every free name it closes over passed in, so
// nothing about the module's DOM or imports is needed.
function load(source, name, deps) {
  const keys = Object.keys(deps);
  const make = new Function(...keys, `${source}\nreturn ${name};`);
  return make(...keys.map(k => deps[k]));
}

async function peerOpWith(response) {
  const src = await readFile(SRC, 'utf8');
  const calls = [];
  const toasts = [];
  const peerOp = load(extract(src, 'peerOp'), 'peerOp', {
    API: '',
    fetch: async (url, opts) => { calls.push({ url, opts }); return response; },
    toast: (msg, kind) => toasts.push({ msg, kind }),
    handleAuthError: () => false,
    refresh: () => {},
  });
  return { peerOp, calls, toasts };
}

const okResponse = { ok: true, status: 200, json: async () => ({ ok: true }) };

test('accept actually issues its request', async () => {
  const { peerOp, calls, toasts } = await peerOpWith(okResponse);
  await peerOp({ id: 7 }, 'accept', 'Friend added.');

  assert.equal(calls.length, 1, 'no request was sent — the pairing would never complete');
  assert.equal(calls[0].url, '/api/admin/federation/peers/7/accept');
  assert.equal(calls[0].opts.method, 'POST');
  assert.deepEqual(toasts, [{ msg: 'Friend added.', kind: 'info' }]);
});

test('block carries its reason, which is what the mark publishes', async () => {
  const { peerOp, calls } = await peerOpWith(okResponse);
  await peerOp({ id: 3 }, 'block', 'Node blocked.', { reason: 'bad fingerprint claim' });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, '/api/admin/federation/peers/3/block');
  assert.equal(calls[0].opts.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(calls[0].opts.body), { reason: 'bad fingerprint claim' });
});

test('a refusal is reported in the server’s own words', async () => {
  const { peerOp, toasts } = await peerOpWith({
    ok: false,
    status: 409,
    json: async () => ({ error: 'peer is friend, not awaiting acceptance' }),
  });
  await peerOp({ id: 5 }, 'accept', 'Friend added.');

  assert.equal(toasts.length, 1);
  assert.equal(toasts[0].kind, 'error');
  assert.match(toasts[0].msg, /not awaiting acceptance/);
});
