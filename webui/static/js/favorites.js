// favorites.js — the shared liked-tracks cache (docs/api/playlists.md;
// remote entries: docs/ui/madnetwork-page.md §Remote tracks). One module-level
// Set of canonical like keys backs every heart in the UI — library rows,
// madnetwork rows, search rows, and the player-bar Like button — so they can
// never disagree.
//
// A like key is either a local appearance — `ts:<tagsetId>` — or a remote
// madnetwork track — `mn:<renditionHash>`. Favorites are just a system
// playlist server-side; this is only the client-side view of its membership.
import { openLoginModal } from './auth.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

let liked = null;      // Set<canonical key> | null (not loaded yet)
let loading = null;    // in-flight ensureLiked promise
const subs = new Set();

// likeKeyOf normalizes a heart identity: a bare tagset id (number or numeric
// string) becomes `ts:<id>`; canonical `ts:`/`mn:` keys pass through; anything
// empty is null (an inert heart).
export function likeKeyOf(v) {
  if (v == null || v === '') return null;
  const s = String(v);
  return (s.startsWith('ts:') || s.startsWith('mn:')) ? s : `ts:${s}`;
}

// trackKey returns the like identity for a queue track: the local tagset the
// page attached when building the queue, else the remote madnetwork ref.
// Tracks from a stale persisted queue (or a local preview blob) have neither —
// their hearts stay inert.
export function trackKey(track) {
  if (!track) return null;
  if (track.tagsetId) return `ts:${track.tagsetId}`;
  if (track.remoteLike?.hash) return `mn:${track.remoteLike.hash}`;
  return null;
}

// onLikedChange subscribes to any change of the liked set (load or toggle).
export function onLikedChange(cb) {
  subs.add(cb);
  return () => subs.delete(cb);
}
function notify() {
  subs.forEach(cb => { try { cb(); } catch (e) { console.error('liked listener:', e); } });
}

// ensureLiked loads the liked set once (anonymous / unauthorized requests
// resolve to an empty set — no login prompt for merely rendering hearts).
export function ensureLiked() {
  if (liked) return Promise.resolve(liked);
  if (!loading) {
    loading = (async () => {
      try {
        const res = await fetch(`${API}/api/favorites`);
        const data = res.ok ? await res.json() : {};
        liked = new Set([
          ...(data.tagset_ids || []).map(id => `ts:${id}`),
          ...(data.remote_hashes || []).map(h => `mn:${h}`),
        ]);
      } catch {
        liked = new Set();
      }
      notify();
      return liked;
    })();
  }
  return loading;
}

export function isLiked(v) {
  const key = likeKeyOf(v);
  return !!key && !!liked?.has(key);
}

// toggleLike flips membership server-side and mirrors the result locally.
// For a remote (`mn:`) key, remoteMeta carries the display text captured on
// first like ({title, artist, album}). 401/403 → login modal (an anonymous
// user clicked a heart). Returns the new state, or null when the toggle
// didn't happen.
export async function toggleLike(v, remoteMeta) {
  const key = likeKeyOf(v);
  if (!key) return null;
  try {
    let res;
    if (key.startsWith('mn:')) {
      res = await fetch(`${API}/api/favorites/remote/${encodeURIComponent(key.slice(3))}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(remoteMeta || {}),
      });
    } else {
      res = await fetch(`${API}/api/favorites/${encodeURIComponent(key.slice(3))}`, { method: 'POST' });
    }
    if (res.status === 401 || res.status === 403) { openLoginModal(); return null; }
    if (!res.ok) return null;
    const { liked: on } = await res.json();
    if (!liked) liked = new Set();
    if (on) liked.add(key); else liked.delete(key);
    notify();
    return on;
  } catch {
    return null;
  }
}
