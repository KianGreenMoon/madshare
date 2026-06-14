// player.js — the shared audio player, used by the library page and the admin
// Files page (and, later, cmus). It owns the <audio> element and the player-bar
// DOM (the {{define "player-bar"}} partial) and handles everything intrinsic to
// playback: play/pause, the play/pause icon, the progress bar (click + keyboard
// seek), the time readout, and the volume slider.
//
// It is deliberately decoupled from any notion of a playlist. Navigation and
// list-specific behaviour (next/prev/shuffle, row highlighting, duration
// write-back) live in the page that creates the player and are driven through
// the callbacks below. This keeps one player implementation reusable across
// pages with different surrounding UIs.

// fmtTime renders seconds as m:ss (or h:mm:ss for long tracks). Exported so
// callers reuse the same formatting for their own track lists.
export function fmtTime(s) {
  if (!isFinite(s)) return '0:00';
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = String(Math.floor(s % 60)).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
}

// createPlayer wires the player-bar markup and returns a small control surface.
// callbacks (all optional):
//   onPrev()            — Previous button pressed
//   onNext()            — Next button pressed
//   onShuffleToggle(on) — Shuffle button toggled (on = new boolean state)
//   onRepeatToggle(mode)— Repeat button cycled ('off' | 'all' | 'one')
//   onEnded()           — current track finished
//   onError()           — current track failed to load/play
//   onLoadedMetadata(d) — duration (seconds) became known for the current track
//   onPlay() / onPause()— the audio element started / paused (for Media Session)
export function createPlayer(callbacks = {}) {
  const {
    onPrev = () => {},
    onNext = () => {},
    onShuffleToggle = () => {},
    onRepeatToggle = () => {},
    onEnded = () => {},
    onError = () => {},
    onLoadedMetadata = () => {},
    onPlay = () => {},
    onPause = () => {},
  } = callbacks;

  const audio        = document.getElementById('audio');
  const bar          = document.getElementById('player-bar');
  const titleEl      = document.getElementById('playerTitle');
  const artistEl     = document.getElementById('playerArtist');
  const timeEl       = document.getElementById('playerTime');
  const progressBar  = document.getElementById('progressBar');
  const progressFill = document.getElementById('progressFill');
  const btnPlay      = document.getElementById('btnPlay');
  const btnShuffle   = document.getElementById('btnShuffle');
  const btnRepeat    = document.getElementById('btnRepeat');
  const repeatBadge  = document.getElementById('repeatOneBadge');
  const btnPrev      = document.getElementById('btnPrev');
  const btnNext      = document.getElementById('btnNext');
  const iconPlay     = document.getElementById('iconPlay');
  const iconPause    = document.getElementById('iconPause');
  const volumeSlider = document.getElementById('volume-slider');
  const qualitySelect = document.getElementById('qualitySelect'); // may be absent

  let shuffle = false;
  let repeat  = 'off';                 // 'off' | 'all' | 'one'
  const REPEAT_MODES = ['off', 'all', 'one'];

  // load points the player at a new track and (by default) starts it, revealing
  // the bar. autoplay:false is the queue-resume path: it sets preload=none so
  // the browser doesn't even fetch the file until the user presses play — a
  // stale session must not trigger an error (and a login prompt) on page load.
  function load({ url, title, artist }, { autoplay = true } = {}) {
    audio.preload = autoplay ? 'auto' : 'none';
    audio.src = url;
    if (autoplay) audio.play().catch(() => {});
    titleEl.textContent = title || '';
    if (artistEl) artistEl.textContent = artist || '';
    bar.classList.remove('hidden');
    // A new track resets the quality control until its renditions are fetched
    // (the controller calls setRenditions); single-rendition tracks stay hidden.
    setRenditions([], url);
  }

  // ── Audio quality / renditions (recordings P4) ──
  // setRenditions populates the quality dropdown from the current track's
  // recording. renditions is ranked best-first; each carries {url, format,
  // bitrate, sample_rate, bit_depth, best}. currentURL is what's playing now, so
  // the control reflects reality without forcing a swap. A list of 0 or 1 hides
  // the control. No-op when the player-bar has no quality select.
  function setRenditions(renditions, currentURL) {
    if (!qualitySelect) return;
    if (!renditions || renditions.length < 2) {
      qualitySelect.classList.add('hidden');
      qualitySelect.replaceChildren();
      return;
    }
    const best = renditions[0];
    const opts = [makeOption('Auto', best.url, 'auto')];
    for (const r of renditions) opts.push(makeOption(qualityLabel(r), r.url, 'fixed'));
    qualitySelect.replaceChildren(...opts);
    // Reflect the playing rendition: Auto when it is the best, else the match.
    qualitySelect.value = (currentURL && currentURL === best.url) ? best.url : (currentURL || best.url);
    // The Auto option shares the best URL; selecting by value lands on whichever
    // option has it — for "Auto" vs best they are equal, which is fine.
    qualitySelect.classList.remove('hidden');
  }

  function makeOption(label, value, kind) {
    const o = document.createElement('option');
    o.textContent = label;
    o.value = value;
    o.dataset.kind = kind;
    return o;
  }

  function qualityLabel(r) {
    const parts = [r.format || 'audio'];
    if (r.bitrate) parts.push(Math.round(r.bitrate / 1000) + 'k');
    else if (r.sample_rate) {
      let s = (r.sample_rate / 1000).toFixed(1) + 'kHz';
      if (r.bit_depth) s += '/' + r.bit_depth + 'bit';
      parts.push(s);
    }
    return parts.join(' ');
  }

  // switchSource swaps the audio to a different rendition URL, preserving the
  // playback position and play/pause state across the reload.
  function switchSource(url) {
    if (!url || url === audio.src) return;
    const at = audio.currentTime;
    const wasPlaying = !audio.paused;
    audio.src = url;
    audio.addEventListener('loadedmetadata', () => {
      try { audio.currentTime = at; } catch { /* ignore if not seekable */ }
      if (wasPlaying) audio.play().catch(() => {});
    }, { once: true });
  }

  if (qualitySelect) {
    qualitySelect.addEventListener('change', () => switchSource(qualitySelect.value));
  }

  // Derive the play/pause icon purely from the audio element's state.
  function syncPlayIcon() {
    const playing = !audio.paused;
    iconPlay.style.display  = playing ? 'none' : '';
    iconPause.style.display = playing ? ''     : 'none';
    btnPlay.setAttribute('aria-label', playing ? 'Pause' : 'Play');
    btnPlay.title = playing ? 'Pause' : 'Play';
  }

  function toggle() {
    if (audio.paused) audio.play().catch(() => {});
    else              audio.pause();
  }

  function seekTo(sec) {
    if (audio.duration) audio.currentTime = Math.max(0, Math.min(audio.duration, sec));
  }
  function nudge(delta) {
    if (audio.duration) seekTo(audio.currentTime + delta);
  }

  // ── Wiring ──
  btnPlay.addEventListener('click', toggle);
  btnPrev?.addEventListener('click', () => onPrev());
  btnNext?.addEventListener('click', () => onNext());
  function applyShuffleUI() {
    if (!btnShuffle) return;
    btnShuffle.classList.toggle('active', shuffle);
    const label = shuffle ? 'Shuffle on' : 'Shuffle off';
    btnShuffle.setAttribute('aria-label', label);
    btnShuffle.title = label;
  }
  btnShuffle?.addEventListener('click', () => {
    shuffle = !shuffle;
    applyShuffleUI();
    onShuffleToggle(shuffle);
  });

  function applyRepeatUI() {
    if (!btnRepeat) return;
    btnRepeat.classList.toggle('active', repeat !== 'off');
    if (repeatBadge) repeatBadge.style.display = repeat === 'one' ? '' : 'none';
    const label = repeat === 'off' ? 'Repeat off' : repeat === 'all' ? 'Repeat all' : 'Repeat one';
    btnRepeat.setAttribute('aria-label', label);
    btnRepeat.title = label;
  }
  btnRepeat?.addEventListener('click', () => {
    repeat = REPEAT_MODES[(REPEAT_MODES.indexOf(repeat) + 1) % REPEAT_MODES.length];
    applyRepeatUI();
    onRepeatToggle(repeat);
  });

  audio.addEventListener('play',  () => { syncPlayIcon(); onPlay(); });
  audio.addEventListener('pause', () => { syncPlayIcon(); onPause(); });
  // On end/error the native element doesn't fire 'pause', so sync the icon here
  // before the caller decides whether to advance. If it loads a new track, the
  // subsequent 'play' event flips the icon back.
  audio.addEventListener('ended', () => { syncPlayIcon(); onEnded(); });
  audio.addEventListener('error', () => { syncPlayIcon(); onError(); });
  audio.addEventListener('loadedmetadata', () => {
    if (isFinite(audio.duration) && audio.duration > 0) onLoadedMetadata(audio.duration);
  });
  audio.addEventListener('timeupdate', () => {
    if (!audio.duration) return;
    const pct = (audio.currentTime / audio.duration) * 100;
    progressFill.style.width = pct + '%';
    progressBar.setAttribute('aria-valuenow', Math.round(pct));
    timeEl.textContent = fmtTime(audio.currentTime) + ' / ' + fmtTime(audio.duration);
  });

  progressBar.addEventListener('click', e => {
    if (!audio.duration) return;
    const r = progressBar.getBoundingClientRect();
    seekTo(((e.clientX - r.left) / r.width) * audio.duration);
  });
  progressBar.addEventListener('keydown', e => {
    if (!audio.duration) return;
    if (e.key === 'ArrowRight') nudge(5);
    if (e.key === 'ArrowLeft')  nudge(-5);
  });

  if (volumeSlider) {
    volumeSlider.addEventListener('input', () => { audio.volume = volumeSlider.value; });
  }

  return {
    load,
    setRenditions,
    toggle,
    play:  () => audio.play().catch(() => {}),
    pause: () => audio.pause(),
    seekTo,
    nudge,
    isShuffle: () => shuffle,
    // setShuffle restores shuffle state programmatically (e.g. after a reload).
    // It updates the button UI but does NOT fire onShuffleToggle — the caller
    // is re-establishing state, not requesting a re-shuffle.
    setShuffle: on => { shuffle = !!on; applyShuffleUI(); },
    getRepeat: () => repeat,
    get paused()      { return audio.paused; },
    get duration()    { return audio.duration; },
    get currentTime() { return audio.currentTime; },
  };
}
