// Authentication helpers used only in setup()/teardown().
//
// Strategy (PLAN.md §4): mint ONE bearer API token per role once, reuse it for
// all VUs. Per-iteration login is avoided — it runs argon2id behind a global
// 8-concurrent-verify cap and would measure the throttle, not the endpoint.

import http from 'k6/http';
import { BASE_URL, creds } from '../config/env.js';

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// mintToken logs in as `role` (storing the session cookie in the VU jar) and
// creates a short-lived API token via the now-authenticated session.
// Returns { id, token } — token is the raw secret, shown only once.
export function mintToken(role) {
  const c = creds[role];
  if (!c) throw new Error(`unknown role: ${role}`);

  const login = http.post(`${BASE_URL}/api/auth/login`, JSON.stringify(c), {
    headers: JSON_HEADERS,
  });
  if (login.status !== 200) {
    throw new Error(`login ${role} failed: ${login.status} ${login.body}`);
  }

  const res = http.post(
    `${BASE_URL}/api/auth/tokens`,
    JSON.stringify({ name: `k6-${role}-${Date.now()}`, expires_in_days: 1 }),
    { headers: JSON_HEADERS },
  );
  if (res.status !== 201) {
    throw new Error(`token mint ${role} failed: ${res.status} ${res.body}`);
  }
  const j = res.json();
  return { id: j.id, token: j.token };
}

// revokeToken deletes a token by id, authenticating with the token itself.
// Best-effort: a failed cleanup must not fail the run.
export function revokeToken(id, token) {
  if (id == null) return;
  http.del(`${BASE_URL}/api/auth/tokens/${id}`, null, {
    headers: { Authorization: `Bearer ${token}` },
  });
}
