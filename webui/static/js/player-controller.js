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
import { insertAdjust, removeAdjust, moveAdjust, clampIndex, shufflePerm, relinkTracks } from './queue-ops.js';

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
  // original holds the unshuffled order while shuffle is ON: toggling shuffle
  // reorders the queue itself (current track first) and toggling it off
  // restores this. Track objects are SHARED between the two arrays, so
  // identity-based ops (remove, un-shuffle) stay correct; after a reload
  // relinkTracks re-establishes the sharing. null = shuffle off.
  let original = null;

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
    try {
      localStorage.setItem(QUEUE_KEY, JSON.stringify({
        tracks: queue, index, dirty, original, shuffle: player.isShuffle(),
      }));
    } catch { /* quota exceeded — not fatal */ }
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

  // Shuffle reorders the QUEUE ITSELF (Spotify-style): toggling on snapshots
  // the original order, puts the current track first and shuffles the rest;
  // toggling off restores the original order with the current track still
  // current. Next/Prev/advance are then a plain walk of the visible queue, and
  // the queue panel shows the real play order.
  function shuffleOn() {
    original = queue.slice();
    if (queue.length > 1) {
      const perm = shufflePerm(queue.length, index);
      queue = perm.map(i => original[i]);
      if (index >= 0) index = 0;
    }
    queueChanged();
  }
  function shuffleOff() {
    if (!original) return;
    const current = index >= 0 ? queue[index] : null;
    queue = original;
    original = null;
    if (current) {
      const i = queue.indexOf(current);
      index = i >= 0 ? i : clampIndex(index, queue.length);
    } else {
      index = -1;
    }
    queueChanged();
  }

  // goNext / goPrev are the MANUAL navigation paths (Next/Prev buttons, media
  // keys, controller.next/prev): a sequential walk of the visible queue —
  // which IS the shuffled order while shuffle is on — wrapping at the ends.
  function goNext() {
    if (index < 0 || !queue.length) return;
    go(index < queue.length - 1 ? index + 1 : 0);
  }
  function goPrev() {
    if (index < 0 || !queue.length) return;
    go(index > 0 ? index - 1 : queue.length - 1);
  }

  // advance picks what plays after a track ends or fails, honouring repeat.
  // fromError suppresses repeat-one (don't loop a broken track forever). Unlike
  // goNext it does NOT wrap at the end of the queue unless repeat-all is on.
  function advance({ fromError = false } = {}) {
    const repeat = player.getRepeat();
    if (repeat === 'one' && !fromError) { go(index); return; }
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
    onShuffleToggle: on => (on ? shuffleOn() : shuffleOff()),
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
    // Re-establish the shuffle state across the reload: original order (with
    // object identity re-linked into the revived queue) + the button state, so
    // switching shuffle off later still restores the pre-shuffle order.
    original = Array.isArray(saved.original) ? relinkTracks(saved.original, queue) : null;
    if (saved.shuffle && original) player.setShuffle(true);
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
        const stashedOriginal = original;
        emit('queuereplaced', () => {
          queue = stashedQueue;
          original = stashedOriginal;
          dirty = true;
          index = -1;
          go(Math.max(0, Math.min(stashedIndex, queue.length - 1)));
          queueChanged();
        });
      }
      queue = tracks.slice();
      original = null;
      dirty = false;
      index = -1;
      go(startIndex);
      // With shuffle already on, a freshly set queue is shuffled too — the
      // clicked track plays first, the rest follow in shuffled order.
      if (player.isShuffle() && queue.length) shuffleOn();
      else queueChanged();
    },

    // ── Queue mutations (manual edits — they mark the queue dirty) ──────────
    // enqueue appends; into an empty queue it behaves like setQueue (starts
    // playback, not dirty — there was nothing to protect).
    enqueue(tracks) {
      if (!tracks.length) return;
      const wasEmpty = queue.length === 0;
      queue.push(...tracks);
      if (original) original.push(...tracks);
      if (wasEmpty) go(0);
      else dirty = true;
      queueChanged();
    },

    insertAt(i, tracks) {
      if (!tracks.length) return;
      const current = index >= 0 ? queue[index] : null; // before the splice shifts positions
      const at = Math.max(0, Math.min(i, queue.length));
      queue.splice(at, 0, ...tracks);
      // While shuffled, mirror the insert into the original order right after
      // the current track (a shuffled position has no exact original
      // counterpart; "near what's playing" preserves the intent best).
      if (original) {
        const oi = current ? original.indexOf(current) : -1;
        original.splice(oi >= 0 ? oi + 1 : original.length, 0, ...tracks);
      }
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
      const [removed] = queue.splice(i, 1);
      if (original) {
        const oi = original.indexOf(removed);
        if (oi >= 0) original.splice(oi, 1);
      }
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
      original = null; // nothing left to un-shuffle back to
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
