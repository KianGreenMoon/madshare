// Discover real library content via the authenticated API in setup(), so the
// read cases hit existing artists/albums/tracks instead of needing seeded data.
// Returns small, JSON-serialisable lists that k6 hands to every VU.

import http from 'k6/http';
import { BASE_URL } from '../config/env.js';

const MAX_ARTISTS = 8; // cap drill-down breadth so setup stays fast
const MAX_ALBUMS = 12;

export function discover(tokens) {
  const auth = (t) => ({ headers: { Authorization: `Bearer ${t}` } });
  const json = (res) => (res.status === 200 ? res.json() : []) || [];

  const artists = json(http.get(`${BASE_URL}/api/artists`, auth(tokens.user)));
  const artistIds = artists.map((a) => a.id);

  const albums = [];
  for (const a of artists.slice(0, MAX_ARTISTS)) {
    const al = json(http.get(`${BASE_URL}/api/albums?artist_id=${a.id}`, auth(tokens.user)));
    for (const x of al) albums.push({ id: x.id, artistId: a.id, hasImage: x.has_image });
  }

  const tracks = [];
  for (const al of albums.slice(0, MAX_ALBUMS)) {
    const tr = json(http.get(`${BASE_URL}/api/tracks?album_id=${al.id}`, auth(tokens.user)));
    for (const t of tr) if (t.url) tracks.push({ url: t.url });
  }

  return {
    artistIds,
    albums,
    tracks,
    searchTerms: buildSearchTerms(artists),
  };
}

// Build a handful of search terms from real artist names (prefixes + whole
// short names), with generic fallbacks so search always has something to send.
function buildSearchTerms(artists) {
  const terms = new Set(['a', 'e', 'the', 'o']);
  for (const a of artists.slice(0, 6)) {
    const name = (a.name || '').trim();
    if (name.length >= 2) terms.add(name.slice(0, 2).toLowerCase());
    if (name.length > 0 && name.length <= 12) terms.add(name);
  }
  return [...terms];
}
