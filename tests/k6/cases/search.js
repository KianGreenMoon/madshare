// search (user): a library search with a term derived from real artist names.
import { get } from '../lib/http.js';
import { pick } from '../lib/util.js';

export function search(data) {
  const term = pick(data.searchTerms) || 'a';
  get(`/api/search?q=${encodeURIComponent(term)}`, data.tokens.user, 'search');
}
