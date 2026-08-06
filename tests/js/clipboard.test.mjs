// Unit tests for webui/static/js/clipboard.js.
//
// The bug these pin: every copy button in the UI called navigator.clipboard
// directly, and that object does not exist outside a secure context. On the
// origins madshare is actually reached on — http:// to a yggdrasil address, or
// to a plain-http reverse proxy — copying a node card, a mesh address or a
// public key did nothing (and on a node card the button was not even drawn).
// So the interesting case here is not "the modern API works", it is "the modern
// API is absent or refuses, and the copy still happens".
//
// clipboard.js touches no DOM at module scope, so it imports cleanly and runs
// against the stub environment below.
//
// Run with: node --test tests/js/clipboard.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { copyText, selectElementText } from '../../webui/static/js/clipboard.js';

// installEnv builds a DOM stub just deep enough for the two copy paths, and
// records what happened to it. Returns the record plus a restore function.
function installEnv({ clipboard = null, secure = true, execOK = true, noExec = false } = {}) {
  const log = { created: [], attached: [], detached: [], execArgs: [], copied: null, writes: [], focused: [] };

  const mkEl = tag => {
    const el = {
      tag, style: {}, attrs: {}, value: '', tabIndex: 0, attached: false,
      setAttribute(k, v) { this.attrs[k] = v; },
      focus() { log.focused.push(this); doc.activeElement = this; },
      select() { this.selected = true; },
      setSelectionRange(a, b) { this.selectionRange = [a, b]; },
      remove() { this.attached = false; log.detached.push(this); },
    };
    log.created.push(el);
    return el;
  };

  const doc = {
    activeElement: null,
    body: {
      appendChild(n) { n.attached = true; log.attached.push(n); },
    },
    createElement: mkEl,
    createRange: () => ({ selectNodeContents(n) { this.node = n; } }),
    execCommand(cmd) {
      log.execArgs.push(cmd);
      // The textarea must still be in the document and selected when the copy
      // is issued — a detached one copies nothing in a real browser.
      const ta = log.created[log.created.length - 1];
      assert.ok(ta.attached, 'textarea must be attached when execCommand runs');
      assert.ok(ta.selected, 'textarea must be selected when execCommand runs');
      log.copied = ta.value;
      return execOK;
    },
    getSelection: () => sel,
  };
  if (noExec) delete doc.execCommand;

  const ranges = [];
  const sel = {
    get rangeCount() { return ranges.length; },
    getRangeAt: i => ranges[i],
    removeAllRanges() { ranges.length = 0; },
    addRange(r) { ranges.push(r); },
  };

  const win = { isSecureContext: secure, getSelection: () => sel };
  const nav = clipboard ? { clipboard } : {};
  if (clipboard) clipboard._writes = log.writes;

  const prev = {
    document: globalThis.document, window: globalThis.window,
    navigator: Object.getOwnPropertyDescriptor(globalThis, 'navigator'),
  };
  globalThis.document = doc;
  globalThis.window = win;
  Object.defineProperty(globalThis, 'navigator', { value: nav, configurable: true, writable: true });

  return {
    log, doc, sel,
    restore() {
      globalThis.document = prev.document;
      globalThis.window = prev.window;
      Object.defineProperty(globalThis, 'navigator', prev.navigator);
    },
  };
}

const okClipboard = () => ({
  writeText(t) { this._writes.push(t); return Promise.resolve(); },
});
const failingClipboard = () => ({
  writeText(t) { this._writes.push(t); return Promise.reject(new Error('NotAllowedError')); },
});

test('secure context uses the async clipboard API', async () => {
  const env = installEnv({ clipboard: okClipboard(), secure: true });
  try {
    assert.equal(await copyText('abc'), true);
    assert.deepEqual(env.log.writes, ['abc']);
    assert.deepEqual(env.log.execArgs, [], 'no legacy fallback when the API worked');
    assert.equal(env.log.created.length, 0, 'no scratch textarea when the API worked');
  } finally { env.restore(); }
});

test('plain http: no clipboard object at all still copies', async () => {
  // The reported bug, exactly: http://[201:...]:81 and http://[202:...]/ both
  // leave navigator.clipboard undefined.
  const env = installEnv({ clipboard: null, secure: false });
  try {
    assert.equal(await copyText('a4c7…the node key'), true);
    assert.deepEqual(env.log.execArgs, ['copy']);
    assert.equal(env.log.copied, 'a4c7…the node key');
  } finally { env.restore(); }
});

test('insecure context never calls a clipboard object that happens to exist', async () => {
  // Some browsers expose the object on an insecure origin and then reject or do
  // nothing. isSecureContext is asked first so the working path is taken.
  const env = installEnv({ clipboard: okClipboard(), secure: false });
  try {
    assert.equal(await copyText('key'), true);
    assert.deepEqual(env.log.writes, [], 'must not go through the secure-context API');
    assert.equal(env.log.copied, 'key');
  } finally { env.restore(); }
});

test('a rejected writeText falls back instead of failing', async () => {
  const env = installEnv({ clipboard: failingClipboard(), secure: true });
  try {
    assert.equal(await copyText('token'), true);
    assert.deepEqual(env.log.writes, ['token'], 'the API was tried');
    assert.equal(env.log.copied, 'token', 'and the legacy path finished the job');
  } finally { env.restore(); }
});

test('the scratch textarea is invisible, unfocusable by tab, and always removed', async () => {
  const env = installEnv({ clipboard: null, secure: false });
  try {
    await copyText('x');
    const ta = env.log.created[0];
    assert.equal(ta.tag, 'textarea');
    assert.equal(ta.style.position, 'fixed', 'fixed so copying never scrolls the page');
    assert.equal(ta.style.opacity, '0');
    assert.equal(ta.attrs.readonly, '', 'readonly keeps the mobile keyboard down');
    assert.equal(ta.tabIndex, -1);
    assert.deepEqual(ta.selectionRange, [0, 1], 'iOS ignores select() on a readonly field');
    assert.deepEqual(env.log.detached, [ta], 'removed even on the success path');
  } finally { env.restore(); }
});

test('focus goes back to the element that had it', async () => {
  const env = installEnv({ clipboard: null, secure: false });
  try {
    const btn = env.doc.createElement('button');
    env.doc.activeElement = btn;
    await copyText('x');
    assert.equal(env.doc.activeElement, btn, 'the copy button keeps focus');
  } finally { env.restore(); }
});

test('a refused execCommand reports failure rather than a false success', async () => {
  const env = installEnv({ clipboard: null, secure: false, execOK: false });
  try {
    assert.equal(await copyText('x'), false);
    assert.equal(env.log.detached.length, 1, 'and still cleans up');
  } finally { env.restore(); }
});

test('a browser without execCommand reports failure without throwing', async () => {
  const env = installEnv({ clipboard: null, secure: false, noExec: true });
  try {
    assert.equal(await copyText('x'), false);
    assert.equal(env.log.created.length, 0);
  } finally { env.restore(); }
});

test('empty text is not a copy', async () => {
  const env = installEnv({ clipboard: okClipboard(), secure: true });
  try {
    assert.equal(await copyText(''), false);
    assert.equal(await copyText(null), false);
    assert.equal(await copyText(undefined), false);
    assert.deepEqual(env.log.writes, []);
  } finally { env.restore(); }
});

test('selectElementText selects the node so Ctrl/Cmd+C finishes the job', () => {
  const env = installEnv({ clipboard: null, secure: false });
  try {
    const span = env.doc.createElement('span');
    assert.equal(selectElementText(span), true);
    assert.equal(env.sel.rangeCount, 1);
    assert.equal(env.sel.getRangeAt(0).node, span, 'the range covers the shown text');
    assert.equal(selectElementText(null), false);
  } finally { env.restore(); }
});
