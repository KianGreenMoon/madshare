// Shared k6 thresholds. Starting SLOs — re-baseline after the first capacity
// run (see ../PLAN.md §10). Requests are tagged by user case, so per-case
// thresholds are expressed with the {case:...} sub-metric selector.
//
// These are intentionally lenient on http_req_failed because upload/delete use
// http.expectedStatuses() to mark their non-2xx-but-expected results (e.g. a
// delete racing to 404) as NOT failed, so they never inflate this metric.

export const thresholds = {
  http_req_failed: ['rate<0.01'], // <1% genuine errors
  http_req_duration: ['p(95)<500', 'p(99)<1500'],
  checks: ['rate>0.99'],

  // Per-case latency budgets (looser where the work is heavier).
  'http_req_duration{case:browse}': ['p(95)<400'],
  'http_req_duration{case:search}': ['p(95)<600'],
  'http_req_duration{case:listen}': ['p(95)<3000'], // streams whole audio blobs
  'http_req_duration{case:playlists}': ['p(95)<400'],
  'http_req_duration{case:admin_read}': ['p(95)<800'],
  'http_req_duration{case:upload}': ['p(95)<5000'], // large multipart bodies
  'http_req_duration{case:delete}': ['p(95)<800'],
};
