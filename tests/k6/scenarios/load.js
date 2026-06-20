// load: run a profile (PROFILE, default standard) at PROFILE_PROC for DURATION.
// This one scenario IS both regression and soak — the only difference is run
// length:
//
//   regression (short):  k6 run -e DURATION=5m  tests/k6/scenarios/load.js
//   soak (long):         k6 run -e DURATION=2h  tests/k6/scenarios/load.js
//   lighter/heavier mix: ... -e PROFILE_PROC=0.75   (or 1.25)
//
// Includes the upload + delete cases, so it mutates the test library — run it
// against a test server only.

import { buildConstant, loadProfile, caseNames } from '../lib/engine.js';
import { thresholdsFor } from '../config/options.js';

export { setup, teardown } from '../lib/lifecycle.js';
export { runCase } from '../lib/runner.js';

const profile = loadProfile();

export const options = {
  scenarios: buildConstant(profile),
  thresholds: thresholdsFor(caseNames(profile)),
};
