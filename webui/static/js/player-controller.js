// player-controller.js — owns the play QUEUE on top of player.js's core (which
// owns the <audio> element and the player-bar UI). This is the single owner of
// "what's queued and what's playing"; pages are thin callers that build queues
// (setQueue) and reflect state through the events below.
//
// Phase 5 of the UI roadmap (docs/plans/playlists.md) made this a true
// ES-module SINGLETON (getController()) shared by the shell and every listening
// page, and added the queue-mutation API (enqueue/insertAt/removeAt/move/clear),
// the dirty-queue replace-with-undo contract, and localStorage resume. The queue
// is deliberately STABLE — browsing never changes it; only an explicit
// setQueue() or a manual edit does.
//
// As the single playback owner, this is also where Media Session (OS / lock-
// screen / hardware media-key integration) and the audio-error → re-auth probe
// live. The queue logic (advance/next/prev/shuffle/repeat + the mutation index
// arithmetic) is kept DOM-free so it can be unit-tested without a browser.
//
// Events (controller.on(event, cb) → unsubscribe):
//   'trackchange'   (track, index) — a new track became current (load + highlight)
//   'duration'      (track, secs)  — the current track's duration became known
//   'error'         (track, index) — the current track failed (genuine media error)
//   'autherror'     ()             — playback failed with 401/403 → prompt re-auth
//   'queuechange'   ()             — the queue's contents/order/current changed
//   'queuereplaced' (restore)      — a manually edited queue was replaced by
//                                    setQueue; restore() puts it back (undo toast)
import { createPlayer } from './player.js';
import { insertAdjust, removeAdjust, moveAdjust, clampIndex } from './queue-ops.js';

// QUEUE_KEY persists { tracks, index, dirty } so a reload resumes the queue
// (paused — see restoreFromStorage). localStorage only, per Decision §4.
const QUEUE_KEY = 'madshare-queue';

let instance = null;

// getController returns the shared controller, creating it (and wiring the
// player-bar DOM) on first use. Call only on pages that include the player-bar
// partial — i.e. the shell-native listening pages.
export function getController() {
  if (!instance) instance = createController();
  return instance;
}

function createController() {
  let queue = [];
  let index = -1;
  // dirty marks a queue the user has manually edited (panel reorder/remove,
  // quick-add). A dirty queue is never silently replaced: setQueue stashes it
  // and emits 'queuereplaced' so the shell can offer Undo.
  let dirty = false;

  // ── Events ──────────────────────────────────────────────────────────────────
  const listeners = new Map(); // event → Set<cb>
  function on(event, cb) {
    if (!listeners.has(event)) listeners.set(event, new Set());
    listeners.get(event).add(cb);
    return () => listeners.get(event).delete(cb);
  }
  function emit(event, ...args) {
    listeners.get(event)?.forEach(cb => {
      try { cb(...args); } catch (e) { console.error(`${event} listener:`, e); }
    });
  }

  function persist() {
    try { localStorage.setItem(QUEUE_KEY, JSON.stringify({ tracks: queue, index, dirty })); }
    catch { /* quota exceeded — not fatal */ }
  }
  function queueChanged() {
    persist();
    emit('queuechange');
  }

  // go loads queue[i] and notifies listeners. The single place "current" moves.
  // autoplay=false is the resume path: point the player at the track without
  // starting (or even fetching) it.
  function go(i, { autoplay = true } = {}) {
    if (i < 0 || i >= queue.length) return;
    index = i;
    const track = queue[i];
    player.load({ url: track.url, title: track.title, artist: track.artist }, { autoplay });
    persist();
    emit('trackchange', track, i);
    updateMediaSession(track);
  }

  // shuffleIndex picks a random queue position other than the current one.
  function shuffleIndex() {
    const others = queue.map((_, i) => i).filter(i => i !== index);
    return others[Math.floor(Math.random() * others.length)];
  }

  // goNext / goPrev are the MANUAL navigation paths (Next/Prev buttons, media
  // keys, controller.next/prev). Next honours shuffle — that's what makes the
  // shuffle button observable — and wraps at the end; Prev is sequential (no
  // play history yet) and wraps at the front.
  function goNext() {
    if (index < 0 || !queue.length) return;
    if (player.isShuffle() && queue.length > 1) { go(shuffleIndex()); return; }
    go(index < queue.length - 1 ? index + 1 : 0);
  }
  function goPrev() {
    if (index < 0 || !queue.length) return;
    go(index > 0 ? index - 1 : queue.length - 1);
  }

  // advance picks what plays after a track ends or fails, honouring repeat/shuffle.
  // fromError suppresses repeat-one (don't loop a broken track forever). Unlike
  // goNext it does NOT wrap at the end of the queue unless repeat-all is on.
  function advance({ fromError = false } = {}) {
    const repeat = player.getRepeat();
    if (repeat === 'one' && !fromError) { go(index); return; }
    if (player.isShuffle() && queue.length > 1) { go(shuffleIndex()); return; }
    if (index < queue.length - 1) { go(index + 1); return; }
    if (repeat === 'all' && queue.length) { go(0); return; }
    // else: end of queue, stop (audio is paused, bar shows the play icon).
  }

  // handleAudioError distinguishes an expired session from a real media failure.
  // The <audio> 'error' event hides the HTTP status, so probe the URL: a 401/403
  // means re-auth (don't skip the track); anything else is a genuine media error.
  async function handleAudioError() {
    if (index < 0) return;
    const track = queue[index];
    const i = index;
    let authFailed = false;
    try {
      const res = await fetch(track.url, { headers: { Range: 'bytes=0-0' } });
      authFailed = res.status === 401 || res.status === 403;
    } catch { /* network error → treat as a media error below */ }
    if (authFailed) { emit('autherror'); return; }
    emit('error', track, i);
    advance({ fromError: true });
  }

  const player = createPlayer({
    onPrev: goPrev,
    onNext: goNext,
    onEnded: () => advance(),
    onError: handleAudioError,
    onLoadedMetadata: dur => { if (index >= 0) emit('duration', queue[index], dur); },
    onPlay:  () => setPlaybackState('playing'),
    onPause: () => setPlaybackState('paused'),
  });

  // ── Media Session ───────────────────────────────────────────────────────────
  const hasMediaSession = 'mediaSession' in navigator;
  function setPlaybackState(state) {
    if (hasMediaSession) navigator.mediaSession.playbackState = state;
  }
  function updateMediaSession(track) {
    if (!hasMediaSession) return;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: track.title || '',
        artist: track.artist || '',
      });
    } catch { /* MediaMetadata unsupported — ignore */ }
  }
  if (hasMediaSession) {
    const ms = navigator.mediaSession;
    const set = (action, fn) => { try { ms.setActionHandler(action, fn); } catch { /* unsupported */ } };
    set('play',  () => player.play());
    set('pause', () => player.pause());
    set('previoustrack', goPrev);
    set('nexttrack',     goNext);
  }

  // restoreFromStorage resumes the persisted queue PAUSED: the player is pointed
  // at the saved track without fetching it (autoplay:false sets preload=none),
  // so a stale session can't pop the login modal before any user gesture.
  function restoreFromStorage() {
    let saved;
    try { saved = JSON.parse(localStorage.getItem(QUEUE_KEY) || 'null'); }
    catch { return; }
    if (!saved || !Array.isArray(saved.tracks) || saved.tracks.length === 0) return;
    queue = saved.tracks;
    dirty = !!saved.dirty;
    go(clampIndex(saved.index, queue.length), { autoplay: false });
    emit('queuechange');
  }

  const api = {
    on,

    // setQueue replaces the queue and starts playing at startIndex — the
    // browse-and-click path. A manually edited (dirty) queue is stashed first
    // and offered back through 'queuereplaced' (the Undo toast).
    setQueue(tracks, startIndex = 0) {
      if (dirty && queue.length) {
        const stashedQueue = queue;
        const stashedIndex = index;
        emit('queuereplaced', () => {
          queue = stashedQueue;
          dirty = true;
          index = -1;
          go(Math.max(0, Math.min(stashedIndex, queue.length - 1)));
          queueChanged();
        });
      }
      queue = tracks.slice();
      dirty = false;
      index = -1;
      go(startIndex);
      queueChanged();
    },

    // ── Queue mutations (manual edits — they mark the queue dirty) ──────────
    // enqueue appends; into an empty queue it behaves like setQueue (starts
    // playback, not dirty — there was nothing to protect).
    enqueue(tracks) {
      if (!tracks.length) return;
      const wasEmpty = queue.length === 0;
      queue.push(...tracks);
      if (wasEmpty) go(0);
      else dirty = true;
      queueChanged();
    },

    insertAt(i, tracks) {
      if (!tracks.length) return;
      const at = Math.max(0, Math.min(i, queue.length));
      queue.splice(at, 0, ...tracks);
      index = insertAdjust(index, at, tracks.length);
      dirty = true;
      queueChanged();
    },

    // playNext inserts right after the current track (or at the front).
    playNext(tracks) {
      api.insertAt(index < 0 ? 0 : index + 1, tracks);
    },

    removeAt(i) {
      if (i < 0 || i >= queue.length) return;
      const removingCurrent = i === index;
      const wasPlaying = !player.paused;
      queue.splice(i, 1);
      dirty = true;
      if (queue.length === 0) {
        index = -1;
        player.pause();
      } else if (removingCurrent) {
        // The next track slides into the removed slot; keep the play state.
        go(Math.min(i, queue.length - 1), { autoplay: wasPlaying });
      } else {
        index = removeAdjust(index, i);
      }
      queueChanged();
    },

    move(from, to) {
      if (from === to || from < 0 || from >= queue.length || to < 0 || to >= queue.length) return;
      const [t] = queue.splice(from, 1);
      queue.splice(to, 0, t);
      index = moveAdjust(index, from, to);
      dirty = true;
      queueChanged();
    },

    clear() {
      queue = [];
      index = -1;
      dirty = false;
      player.pause();
      queueChanged();
    },

    playAt: i => go(i),
    next: goNext,
    prev: goPrev,
    // current returns the playing track + index (or null), so a freshly rendered
    // view can re-highlight whatever is already playing.
    current: () => (index < 0 ? null : { track: queue[index], index }),
    getQueue: () => ({ tracks: queue.slice(), index }),
    isDirty: () => dirty,
    isShuffle: () => player.isShuffle(),
    get paused() { return player.paused; },
  };

  restoreFromStorage();
  return api;
}
