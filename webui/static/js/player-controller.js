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
// The queue logic (advance/next/prev/shuffle) is kept DOM-free so it can be
// unit-tested without a browser later (considerations §6).
import { createPlayer } from './player.js';

// createController wires the player-bar and returns a small queue-aware surface.
// callbacks (all optional):
//   onTrackChange(track, index) — a new track became current (load + highlight)
//   onDuration(track, seconds)  — the current track's duration became known
//   onError(track, index)       — the current track failed to load/play
export function createController({ onTrackChange = () => {}, onDuration = () => {}, onError = () => {} } = {}) {
  let queue = [];
  let index = -1;

  // go loads queue[i] and notifies the caller. The single place "current" moves.
  function go(i) {
    if (i < 0 || i >= queue.length) return;
    index = i;
    const track = queue[i];
    player.load({ url: track.url, title: track.title, artist: track.artist });
    onTrackChange(track, i);
  }

  // advance picks what plays after a track ends or fails: a random other track in
  // shuffle mode, else the next in order; at the end of the queue it stops (the
  // audio is paused, so the bar already shows the play icon).
  function advance() {
    if (player.isShuffle() && queue.length > 1) {
      const others = queue.map((_, i) => i).filter(i => i !== index);
      go(others[Math.floor(Math.random() * others.length)]);
    } else if (index < queue.length - 1) {
      go(index + 1);
    }
  }

  const player = createPlayer({
    onPrev: () => { if (index >= 0) go(index > 0 ? index - 1 : queue.length - 1); },
    onNext: () => { if (index >= 0) go(index < queue.length - 1 ? index + 1 : 0); },
    onEnded: advance,
    onError: () => {
      if (index >= 0) onError(queue[index], index);
      advance();
    },
    onLoadedMetadata: dur => {
      if (index >= 0) onDuration(queue[index], dur);
    },
  });

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
