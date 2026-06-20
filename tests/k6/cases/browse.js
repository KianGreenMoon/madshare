// browse (user): the library drill-down — artists -> albums -> tracks.
import { get } from '../lib/http.js';
import { pick } from '../lib/data.js';

export function browse(data) {
  get('/api/artists', data.tokens.user, 'browse');
  const album = pick(data.albums);
  if (album) {
    get(`/api/albums?artist_id=${album.artistId}`, data.tokens.user, 'browse');
    get(`/api/tracks?album_id=${album.id}`, data.tokens.user, 'browse');
  }
}
