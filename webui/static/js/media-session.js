// media-session.js — OS Media Session integration for the shared player (lock
// screen, notification, hardware media keys), kept OUT of player-controller.js so
// the queue controller stays platform-agnostic.
//
// Backends behind one surface (picked in this order):
//   • Android app — window.MadshareMedia, a native bridge injected by the mobile
//     shell via WebView.addJavascriptInterface(). Unlike Capacitor's plugin
//     bridge, addJavascriptInterface IS present on the remote server origin (where
//     the player actually runs); the Capacitor bridge is NOT injected there — that
//     was the broken P2 assumption, see the design doc §6 correction. The native
//     side runs a foreground service (keeps <audio> playing while backgrounded) and
//     renders the media notification, and calls back into the page through
//     window.__madshareMediaAction(action[, posMs]).
//   • Capacitor Android shell — window.Capacitor.Plugins.MediaSession. Kept only as
//     a fallback for local app content (where Capacitor DOES inject its bridge); it
//     is dormant on the remote origin and the project no longer relies on it.
//   • Web — navigator.mediaSession.
//   • None — every call is a no-op (e.g. a System WebView with no native bridge,
//     which exposes no navigator.mediaSession either).
//
// This module is served by the server (the player runs same-origin with the API,
// including inside the app's WebView), so the native branch lives here, not in the
// mobile/ app — but it is dormant on the web (guarded on the relevant globals) and
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

// resolveBackend picks the active media-session backend (native app bridge first,
// then the Capacitor plugin, then the web API) and normalises each to one shape:
//   setMetadata(track) · setPlaybackState(state) · setActionHandler(action, fn) ·
//   setPositionState({duration,position,playbackRate})
// Returns null when none exists. Exported for unit testing.
export function resolveBackend() {
  // Android native bridge (mobile shell). Reachable on the remote origin because
  // addJavascriptInterface injects into every page, unlike the Capacitor bridge.
  const mad = typeof window !== 'undefined' ? window.MadshareMedia : undefined;
  if (mad && typeof mad.setMetadata === 'function') {
    const handlers = {};
    // The native side invokes this on transport events (notification / lock screen
    // / headset). seekto carries a position in MILLISECONDS; the web seekto handler
    // wants seconds, so normalise to the { seekTime } shape the controller expects.
    window.__madshareMediaAction = (action, posMs) => {
      const fn = handlers[action];
      if (!fn) return;
      if (action === 'seekto') fn({ seekTime: (Number(posMs) || 0) / 1000 });
      else fn();
    };
    const guard = (fn) => { try { fn(); } catch { /* native bridge hiccup — non-fatal */ } };
    return {
      setMetadata: (t) => guard(() => mad.setMetadata(JSON.stringify({ title: t.title || '', artist: t.artist || '', album: t.album || '', artUrl: t.artUrl || '' }))),
      setPlaybackState: (s) => guard(() => mad.setPlaybackState(s)),
      setActionHandler: (a, fn) => { handlers[a] = fn; },
      // The native side works in milliseconds; the player reports seconds.
      setPositionState: (p) => guard(() => mad.setPositionState(Math.round((p.duration || 0) * 1000), Math.round((p.position || 0) * 1000), p.playbackRate || 1)),
    };
  }
  const cap = typeof window !== 'undefined' ? window.Capacitor : undefined;
  const native = cap && cap.isNativePlatform && cap.isNativePlatform() && cap.Plugins
    ? cap.Plugins.MediaSession : null;
  if (native) {
    const guard = (fn) => { try { fn(); } catch { /* native bridge hiccup — non-fatal */ } };
    return {
      setMetadata: (t) => guard(() => native.setMetadata({ title: t.title || '', artist: t.artist || '', album: t.album || '', artUrl: t.artUrl || '' })),
      setPlaybackState: (s) => guard(() => native.setPlaybackState({ playbackState: s })),
      setActionHandler: (a, fn) => guard(() => native.setActionHandler({ action: a }, fn)),
      setPositionState: (p) => guard(() => native.setPositionState(p)),
    };
  }
  if (typeof navigator !== 'undefined' && 'mediaSession' in navigator) {
    const nav = navigator.mediaSession;
    const guard = (fn) => { try { fn(); } catch { /* unsupported bit — ignore */ } };
    return {
      // artwork is what puts the cover on the lock screen / notification; a
      // track without one gets text-only metadata, same as before.
      setMetadata: (t) => guard(() => {
        nav.metadata = new MediaMetadata({
          title: t.title || '', artist: t.artist || '', album: t.album || '',
          artwork: t.artUrl ? [{ src: t.artUrl, sizes: '300x300' }] : [],
        });
      }),
      setPlaybackState: (s) => guard(() => { nav.playbackState = s; }),
      setActionHandler: (a, fn) => guard(() => nav.setActionHandler(a, fn)),
      setPositionState: (p) => guard(() => nav.setPositionState(p)),
    };
  }
  return null;
}
