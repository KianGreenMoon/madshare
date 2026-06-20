// Shared k6 thresholds. Starting SLOs — re-baseline after the first capacity
// run (see README). Requests are tagged by user case, so per-case thresholds
// use the {case:...} sub-metric selector.
//
// http_req_failed stays meaningful because upload/delete mark their expected
// non-2xx results (dedup 200s, delete-race 404s) via http.expectedStatuses(),
// so those never count as failures here.

// Global thresholds applied to every load run.
const GLOBAL = {
  http_req_failed: ['rate<0.01'], // <1% genuine errors
  http_req_duration: ['p(95)<500', 'p(99)<1500'],
  checks: ['rate>0.99'],
};

// Per-case latency budgets (looser where the work is heavier).
const PER_CASE = {
  browse: ['p(95)<400'],
  search: ['p(95)<600'],
  listen: ['p(95)<3000'], // streams whole audio blobs
  playlists: ['p(95)<400'],
  admin_read: ['p(95)<800'],
  upload: ['p(95)<5000'], // large multipart bodies
  delete: ['p(95)<800'],
};

// thresholdsFor returns GLOBAL plus per-case budgets only for the cases present,
// so a profile that omits some cases (e.g. uploading) doesn't carry thresholds
// for metrics that will never get samples.
export function thresholdsFor(caseNames) {
  const t = { ...GLOBAL };
  for (const name of caseNames) {
    if (PER_CASE[name]) t[`http_req_duration{case:${name}}`] = PER_CASE[name];
  }
  return t;
}

// Convenience: thresholds for the full case set.
export const thresholds = thresholdsFor(Object.keys(PER_CASE));

// capacityThresholds defines the "knee": looser SLOs that ABORT the ramp once
// crossed, so capacity.js stops at the breaking point instead of running to the
// ceiling. delayAbortEval rides out transient blips. Per-case budgets are kept
// as non-aborting observability.
export function capacityThresholds(caseNames) {
  const t = {
    http_req_failed: [{ threshold: 'rate<0.05', abortOnFail: true, delayAbortEval: '15s' }],
    http_req_duration: [{ threshold: 'p(95)<1500', abortOnFail: true, delayAbortEval: '15s' }],
    checks: ['rate>0.95'],
  };
  for (const name of caseNames) {
    if (PER_CASE[name]) t[`http_req_duration{case:${name}}`] = PER_CASE[name];
  }
  return t;
}
