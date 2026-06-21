// listen (user): stream an audio blob. responseType:'none' makes k6 read and
// discard the body, so the server fully serves the file (real transfer load)
// without retaining large blobs in VU memory. The track url comes from the API
// with literal spaces/unicode, so encodeURI() makes it a valid request path.
import { get } from '../lib/http.js';
import { pick } from '../lib/util.js';

export function listen(data) {
  const track = pick(data.tracks);
  if (!track) return;
  get(encodeURI(track.url), data.tokens.user, 'listen', { responseType: 'none' });
}
