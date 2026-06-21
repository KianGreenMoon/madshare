// media-session.js — OS Media Session integration for the shared player (lock
// screen, notification, hardware media keys), kept OUT of player-controller.js so
// the queue controller stays platform-agnostic.
//
// Two backends behind one surface:
//   • Web — navigator.mediaSession.
//   • Capacitor Android shell — the System WebView's navigator.mediaSession is a
//     NO-OP, so we use the native @jofr/capacitor-media-session plugin, reached on
//     the remote server origin through the injected Capacitor bridge
//     (window.Capacitor.Plugins.MediaSession). That plugin also runs the Android
//     foreground service that keeps <audio> playing while the app is backgrounded.
//   • Neither — every call is a no-op.
//
// This module is served by the server (the player runs same-origin with the API,
// including inside the app's WebView), so the native branch lives here, not in the
// mobile/ app — but it is dormant on the web (guarded on window.Capacitor) and
// never changes web behaviour.

// createMediaSession wires the platform media session to `player` (the audio core)
// and the queue's navigation callbacks, returning the small interface the
// controller drives:
//   setTrack(track)  — call on track change (updates metadata + position)
//   setState(state)  — 'playing' | 'paused', call on play/pause
// Position (the scrubber) is updated from here; the OS extrapolates between
// updates from the reported playbackRate, so no polling is needed.
export function createMediaSession(player, { onPlay, onPause, onPrev, onNext } = {}) {
  const backend = resolveBackend();

  function updatePosition() {
    const dur = player.duration;
    if (!backend || !isFinite(dur) || dur <= 0) return;
    const pos = player.currentTime;
    backend.setPositionState({ duration: dur, position: isFinite(pos) ? Math.min(pos, dur) : 0, playbackRate: 1 });
  }

  if (backend) {
    backend.setActionHandler('play',  () => onPlay && onPlay());
    backend.setActionHandler('pause', () => onPause && onPause());
    backend.setActionHandler('previoustrack', () => onPrev && onPrev());
    backend.setActionHandler('nexttrack',     () => onNext && onNext());
    // seekto drives the draggable scrubber (both backends pass details.seekTime).
    backend.setActionHandler('seekto', (d) => {
      if (d && typeof d.seekTime === 'number') player.seekTo(d.seekTime);
    });
  }

  return {
    setTrack(track) { backend?.setMetadata(track); updatePosition(); },
    setState(state) { backend?.setPlaybackState(state); updatePosition(); },
  };
}

// resolveBackend picks the active media-session backend (native plugin first, then
// the web API) and normalises each to one shape:
//   setMetadata(track) · setPlaybackState(state) · setActionHandler(action, fn) ·
//   setPositionState({duration,position,playbackRate})
// Returns null when neither exists. Exported for unit testing.
export function resolveBackend() {
  const cap = typeof window !== 'undefined' ? window.Capacitor : undefined;
  const native = cap && cap.isNativePlatform && cap.isNativePlatform() && cap.Plugins
    ? cap.Plugins.MediaSession : null;
  if (native) {
    const guard = (fn) => { try { fn(); } catch { /* native bridge hiccup — non-fatal */ } };
    return {
      setMetadata: (t) => guard(() => native.setMetadata({ title: t.title || '', artist: t.artist || '', album: t.album || '' })),
      setPlaybackState: (s) => guard(() => native.setPlaybackState({ playbackState: s })),
      setActionHandler: (a, fn) => guard(() => native.setActionHandler({ action: a }, fn)),
      setPositionState: (p) => guard(() => native.setPositionState(p)),
    };
  }
  if (typeof navigator !== 'undefined' && 'mediaSession' in navigator) {
    const nav = navigator.mediaSession;
    const guard = (fn) => { try { fn(); } catch { /* unsupported bit — ignore */ } };
    return {
      setMetadata: (t) => guard(() => { nav.metadata = new MediaMetadata({ title: t.title || '', artist: t.artist || '' }); }),
      setPlaybackState: (s) => guard(() => { nav.playbackState = s; }),
      setActionHandler: (a, fn) => guard(() => nav.setActionHandler(a, fn)),
      setPositionState: (p) => guard(() => nav.setPositionState(p)),
    };
  }
  return null;
}
