// Thin checked HTTP wrappers. Every request is tagged with its user `case` so
// the summary and thresholds can break metrics down per case. Bearer auth only
// (tokens come from setup()).

import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL } from '../config/env.js';

const bearer = (token) => (token ? { Authorization: `Bearer ${token}` } : {});

// GET with a 2xx check. opts.responseType:'none' streams+discards the body
// (used by `listen` so large audio blobs don't pile up in memory).
export function get(path, token, caseTag, opts = {}) {
  const params = { headers: bearer(token), tags: { case: caseTag } };
  if (opts.responseType) params.responseType = opts.responseType;
  const res = http.get(`${BASE_URL}${path}`, params);
  check(res, { [`${caseTag}: 2xx`]: (r) => r.status >= 200 && r.status < 300 }, { case: caseTag });
  return res;
}

// DELETE that treats `expected` statuses as success (so a delete racing another
// VU to 404 is not counted as an error by http_req_failed).
export function del(path, token, caseTag, expected = [200, 404]) {
  const params = {
    headers: bearer(token),
    tags: { case: caseTag },
    responseCallback: http.expectedStatuses(...expected),
  };
  const res = http.del(`${BASE_URL}${path}`, null, params);
  check(res, { [`${caseTag}: ${expected.join('/')}`]: (r) => expected.includes(r.status) }, { case: caseTag });
  return res;
}

// POST a multipart file under the `file` field (the upload endpoint's field
// name). `file` is an http.file() object. 200 = existed/dedup, 201 = created.
export function postFile(path, token, caseTag, file) {
  const params = {
    headers: bearer(token),
    tags: { case: caseTag },
    responseCallback: http.expectedStatuses(200, 201),
  };
  const res = http.post(`${BASE_URL}${path}`, { file }, params);
  check(res, { [`${caseTag}: 200/201`]: (r) => r.status === 200 || r.status === 201 }, { case: caseTag });
  return res;
}
