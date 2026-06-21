// smoke: one pass over every user case, with all checks. The contract gate —
// run this first; it proves auth, discovery, and each endpoint group work
// before any load. 1 VU, 1 iteration. Reads run before the destructive delete.
//
//   k6 run tests/k6/scenarios/smoke.js
//
// Mutates the test library (upload + delete) — point it at a test server only.

import { group } from 'k6';
import { CASES, CASE_ORDER } from '../lib/runner.js';

export { setup, teardown } from '../lib/lifecycle.js';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: {
    checks: ['rate>0.99'],
  },
};

export default function (data) {
  for (const name of CASE_ORDER) {
    group(name, () => CASES[name](data));
  }
}
