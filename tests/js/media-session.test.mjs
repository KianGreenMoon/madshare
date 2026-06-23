// Unit tests for media-session.js — the platform Media Session adapter extracted
// from player-controller.js. The module reads window/navigator globals lazily
// (guarded on typeof), so we can exercise both backends by mocking globals.
//
//   node --test tests/js/media-session.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createMediaSession, resolveBackend } from '../../webui/static/js/media-session.js';

// withGlobals temporarily installs globals (window/navigator/MediaMetadata) for
// the duration of fn, restoring them afterwards. Uses defineProperty because Node
// exposes `navigator` as a getter-only global that a plain assignment can't shadow.
function withGlobals(globals, fn) {
  const saved = {};
  for (const k of Object.keys(globals)) {
    saved[k] = Object.getOwnPropertyDescriptor(globalThis, k);
    Object.defineProperty(globalThis, k, { value: globals[k], configurable: true, writable: true });
  }
  try { return fn(); } finally {
    for (const k of Object.keys(globals)) {
      if (saved[k]) Object.defineProperty(globalThis, k, saved[k]);
      else delete globalThis[k];
    }
  }
}

const newPlayer = () => ({ duration: 100, currentTime: 5, played: 0, paused_: 0, seeked: null,
  play() { this.played++; }, pause() { this.paused_++; }, seekTo(t) { this.seeked = t; } });

test('no backend → resolveBackend null, all calls are safe no-ops', () => {
  withGlobals({ window: undefined, navigator: undefined }, () => {
    assert.equal(resolveBackend(), null);
    const ms = createMediaSession(newPlayer(), {});
    assert.doesNotThrow(() => { ms.setTrack({ title: 'a', artist: 'b' }); ms.setState('playing'); });
  });
});

test('native plugin: routes through Capacitor.Plugins.MediaSession with native shapes', () => {
  const calls = [];
  const handlers = {};
  const MediaSession = {
    setMetadata: (o) => calls.push(['meta', o]),
    setPlaybackState: (o) => calls.push(['state', o]),
    setPositionState: (o) => calls.push(['pos', o]),
    setActionHandler: ({ action }, fn) => { handlers[action] = fn; },
  };
  withGlobals({ window: { Capacitor: { isNativePlatform: () => true, Plugins: { MediaSession } } } }, () => {
    const player = newPlayer();
    const ms = createMediaSession(player, {
      onPlay: () => player.play(), onPause: () => player.pause(), onPrev: () => {}, onNext: () => {},
    });
    ms.setTrack({ title: 'T', artist: 'A', album: 'Al' });
    ms.setState('playing');

    // metadata + playbackState forwarded in the plugin's object shape
    assert.deepEqual(calls.find(c => c[0] === 'meta')[1], { title: 'T', artist: 'A', album: 'Al' });
    assert.deepEqual(calls.find(c => c[0] === 'state')[1], { playbackState: 'playing' });
    // position pushed with a known duration
    const pos = calls.find(c => c[0] === 'pos')[1];
    assert.equal(pos.duration, 100);
    assert.equal(pos.position, 5);

    // action handlers wired to the supplied callbacks / player
    handlers.play();  assert.equal(player.played, 1);
    handlers.pause(); assert.equal(player.paused_, 1);
    handlers.seekto({ seekTime: 42 }); assert.equal(player.seeked, 42);
  });
});

test('native app bridge: routes through window.MadshareMedia + __madshareMediaAction', () => {
  const calls = [];
  const MadshareMedia = {
    setMetadata: (json) => calls.push(['meta', json]),
    setPlaybackState: (s) => calls.push(['state', s]),
    setPositionState: (durMs, posMs, rate) => calls.push(['pos', durMs, posMs, rate]),
  };
  const win = { MadshareMedia };
  withGlobals({ window: win }, () => {
    const player = newPlayer(); // duration 100s, currentTime 5s
    const ms = createMediaSession(player, {
      onPlay: () => player.play(), onPause: () => player.pause(), onPrev: () => {}, onNext: () => {},
    });
    ms.setTrack({ title: 'T', artist: 'A', album: 'Al' });
    ms.setState('playing');

    // metadata forwarded as a JSON string
    assert.deepEqual(JSON.parse(calls.find(c => c[0] === 'meta')[1]), { title: 'T', artist: 'A', album: 'Al' });
    // playbackState forwarded as a bare string (the native interface signature)
    assert.deepEqual(calls.find(c => c[0] === 'state'), ['state', 'playing']);
    // position pushed in MILLISECONDS (seconds * 1000)
    const pos = calls.find(c => c[0] === 'pos');
    assert.equal(pos[1], 100000); // duration ms
    assert.equal(pos[2], 5000);   // position ms

    // the native side drives the page through the installed global dispatcher
    assert.equal(typeof win.__madshareMediaAction, 'function');
    win.__madshareMediaAction('play');  assert.equal(player.played, 1);
    win.__madshareMediaAction('pause'); assert.equal(player.paused_, 1);
    // seekto carries milliseconds; the handler receives seconds
    win.__madshareMediaAction('seekto', 42000); assert.equal(player.seeked, 42);
  });
});

test('web: routes through navigator.mediaSession (property assignment)', () => {
  const nav = { metadata: null, playbackState: null, positions: [],
    setActionHandler() {}, setPositionState(p) { this.positions.push(p); } };
  class MediaMetadata { constructor(o) { Object.assign(this, o); } }
  withGlobals({ window: {}, navigator: { mediaSession: nav }, MediaMetadata }, () => {
    const ms = createMediaSession(newPlayer(), {});
    ms.setTrack({ title: 'W', artist: 'B' });
    ms.setState('paused');
    assert.equal(nav.metadata.title, 'W');
    assert.equal(nav.metadata.artist, 'B');
    assert.equal(nav.playbackState, 'paused');
    assert.ok(nav.positions.length >= 1);
  });
});
