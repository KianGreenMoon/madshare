// playlists (user): the per-user playlists + favorites views.
import { get } from '../lib/http.js';

export function playlists(data) {
  get('/api/playlists', data.tokens.user, 'playlists');
  get('/api/favorites', data.tokens.user, 'playlists');
}
