// capacity: find the knee. Reuses the load engine (buildRamp) to ramp every
// case in lockstep as PROFILE_PROC climbs RAMP_START -> RAMP_MAX in RAMP_STEP
// steps. The abortOnFail thresholds stop the run at the breaking point, so the
// last sustained stage tells you the max multiple of the profile the system
// takes. Multiply the standard per_hour values by ~0.75 of that for the
// regression profile (see README "Deriving the profile").
//
//   k6 run -e BASE_URL=https://test.example.ygg tests/k6/scenarios/capacity.js
//
// HEAVY — run against the dedicated test server, never your dev box / localhost.
// Tune the ramp with RAMP_START / RAMP_STEP / RAMP_STEP_SECS / RAMP_MAX.

import { buildRamp, loadProfile, caseNames } from '../lib/engine.js';
import { capacityThresholds } from '../config/options.js';

export { setup, teardown } from '../lib/lifecycle.js';
export { runCase } from '../lib/runner.js';

const profile = loadProfile();

export const options = {
  scenarios: buildRamp(profile),
  thresholds: capacityThresholds(caseNames(profile)),
};
