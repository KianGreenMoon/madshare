// player-controller.js — owns the play QUEUE on top of player.js's core (which
// owns the <audio> element and the player-bar UI). This is the single owner of
// "what's queued and what's playing"; the page that creates it is a thin caller
// that builds queues (setQueue) and reflects state through the callbacks below.
//
// Phase 1 of docs/plans/persistent-shell-playback.md: extracting the queue here
// is what lets playback (and the queue) survive page navigation later. The queue
// is deliberately STABLE — it changes only on an explicit setQueue(), never from
// browsing — so next/prev keep operating on what you actually pressed play on.
//
// As the single playback owner, this is also where Media Session (OS / lock-
// screen / hardware media-key integration) and the audio-error → re-auth probe
// live. The queue logic (advance/next/prev/shuffle/repeat) is kept DOM-free so it
// can be unit-tested without a browser later (considerations §6).
import { createPlayer } from './player.js';

// createController wires the player-bar and returns a small queue-aware surface.
// callbacks (all optional):
//   onTrackChange(track, index) — a new track became current (load + highlight)
//   onDuration(track, seconds)  — the current track's duration became known
//   onError(track, index)       — the current track failed (a genuine media error)
//   onAuthError()               — the current track failed with 401/403 (expired
//                                 session) — prompt re-auth instead of skipping
export function createController({
  onTrackChange = () => {},
  onDuration = () => {},
  onError = () => {},
  onAuthError = () => {},
} = {}) {
  let queue = [];
  let index = -1;

  // go loads queue[i] and notifies the caller. The single place "current" moves.
  function go(i) {
    if (i < 0 || i >= queue.length) return;
    index = i;
    const track = queue[i];
    player.load({ url: track.url, title: track.title, artist: track.artist });
    onTrackChange(track, i);
    updateMediaSession(track);
  }

  // advance picks what plays after a track ends or fails, honouring repeat/shuffle.
  // fromError suppresses repeat-one (don't loop a broken track forever).
  function advance({ fromError = false } = {}) {
    const repeat = player.getRepeat();
    if (repeat === 'one' && !fromError) { go(index); return; }
    if (player.isShuffle() && queue.length > 1) {
      const others = queue.map((_, i) => i).filter(i => i !== index);
      go(others[Math.floor(Math.random() * others.length)]);
      return;
    }
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
    if (authFailed) { onAuthError(); return; }
    onError(track, i);
    advance({ fromError: true });
  }

  const player = createPlayer({
    onPrev: () => { if (index >= 0) go(index > 0 ? index - 1 : queue.length - 1); },
    onNext: () => { if (index >= 0) go(index < queue.length - 1 ? index + 1 : 0); },
    onEnded: () => advance(),
    onError: handleAudioError,
    onLoadedMetadata: dur => { if (index >= 0) onDuration(queue[index], dur); },
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
    setPlaybackState('playing');
  }
  if (hasMediaSession) {
    const ms = navigator.mediaSession;
    const set = (action, fn) => { try { ms.setActionHandler(action, fn); } catch { /* unsupported */ } };
    set('play',  () => player.play());
    set('pause', () => player.pause());
    set('previoustrack', () => { if (index >= 0) go(index > 0 ? index - 1 : queue.length - 1); });
    set('nexttrack',     () => { if (index >= 0) go(index < queue.length - 1 ? index + 1 : 0); });
  }

  return {
    // setQueue replaces the queue and starts playing at startIndex. The only way
    // the queue changes — browsing never calls this.
    setQueue(tracks, startIndex = 0) {
      queue = tracks;
      index = -1;
      go(startIndex);
    },
    next: () => { if (index >= 0) go(index < queue.length - 1 ? index + 1 : 0); },
    prev: () => { if (index >= 0) go(index > 0 ? index - 1 : queue.length - 1); },
    // current returns the playing track + index (or null), so a freshly rendered
    // view can re-highlight whatever is already playing.
    current: () => (index < 0 ? null : { track: queue[index], index }),
    isShuffle: () => player.isShuffle(),
  };
}
