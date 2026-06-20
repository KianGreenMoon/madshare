// The shared engine: turn a profile (case -> requests/hour) plus a profile_proc
// multiplier into k6 `scenarios`. Every scenario is named after its case and
// runs the single `runCase` dispatcher (lib/runner.js). load.js uses the
// constant builder; capacity.js uses the ramp builder. Both read the same
// profiles, so capacity literally measures "how many x the profile we sustain".

import { DURATION, PROFILE, PROFILE_PROC, ramp, perHour } from '../config/env.js';

// Profiles are committed config. k6's runtime can't list a directory (no fs
// readdir/glob, and open() reads a single named file), so config/profiles/
// index.json is the manifest of available profile names — add a profile by
// dropping <name>.json and adding its name there. We preload every listed
// profile here so selection at use-time needs no I/O.
const PROFILE_NAMES = JSON.parse(open('../config/profiles/index.json'));
const PROFILES = {};
for (const name of PROFILE_NAMES) {
  PROFILES[name] = JSON.parse(open(`../config/profiles/${name}.json`));
}

export function loadProfile(name = PROFILE) {
  const p = PROFILES[name];
  if (!p) {
    throw new Error(`unknown profile "${name}" (have: ${Object.keys(PROFILES).join(', ')})`);
  }
  return p;
}

export function caseNames(profile) {
  return Object.keys(profile.cases);
}

// Scaled requests/hour for a case, honouring a PER_HOUR_<case> env override.
function ratePerHour(name, spec, proc) {
  const base = perHour(name, spec.per_hour);
  return Math.max(1, Math.round(base * proc));
}

// Rough VU sizing from a requests/hour rate. arrival-rate executors grow up to
// maxVUs as needed; preAllocatedVUs just avoids a cold start.
function vusFor(rph) {
  const pre = Math.min(50, Math.max(1, Math.ceil(rph / 600)));
  return { pre, max: Math.min(300, pre * 10) };
}

// One constant-arrival-rate scenario per case, each at its scaled requests/hour.
export function buildConstant(profile = loadProfile(), proc = PROFILE_PROC, duration = DURATION) {
  const scenarios = {};
  for (const [name, spec] of Object.entries(profile.cases)) {
    const rph = ratePerHour(name, spec, proc);
    const v = vusFor(rph);
    scenarios[name] = {
      executor: 'constant-arrival-rate',
      rate: rph,
      timeUnit: '1h',
      duration,
      preAllocatedVUs: v.pre,
      maxVUs: v.max,
      exec: 'runCase',
      tags: { case: name },
    };
  }
  return scenarios;
}

// One ramping-arrival-rate scenario per case: all cases ramp in lockstep as
// profile_proc climbs ramp.start -> ramp.max in ramp.step increments, so the
// traffic mix keeps its shape while total load rises to the breaking point.
export function buildRamp(profile = loadProfile()) {
  const procs = [];
  for (let p = ramp.start; p <= ramp.max + 1e-9; p += ramp.step) {
    procs.push(Number(p.toFixed(4)));
  }
  const scenarios = {};
  for (const [name, spec] of Object.entries(profile.cases)) {
    const stages = procs.map((p) => ({
      target: ratePerHour(name, spec, p),
      duration: `${ramp.stepSecs}s`,
    }));
    scenarios[name] = {
      executor: 'ramping-arrival-rate',
      startRate: ratePerHour(name, spec, ramp.start),
      timeUnit: '1h',
      stages,
      preAllocatedVUs: vusFor(ratePerHour(name, spec, ramp.start)).pre,
      maxVUs: vusFor(ratePerHour(name, spec, ramp.max)).max,
      exec: 'runCase',
      tags: { case: name },
    };
  }
  return scenarios;
}
