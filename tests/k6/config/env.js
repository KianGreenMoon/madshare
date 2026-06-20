// Central configuration for the k6 suite. Every knob is an env var with a
// sensible default, so pointing at another server / tuning load is a one-line
// change (`-e KEY=val` or an exported OS env var — k6 reads both via __ENV).
//
// See ../README.md for the meaning of each.

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:3000';

// Per-role credentials. setup() logs in once per role and mints a bearer token.
export const creds = {
  user: {
    username: __ENV.USER_USER || 'user',
    password: __ENV.USER_PASS || 'password',
  },
  uploader: {
    username: __ENV.UPLOADER_USER || 'uploader',
    password: __ENV.UPLOADER_PASS || 'password',
  },
  admin: {
    username: __ENV.ADMIN_USER || 'admin',
    password: __ENV.ADMIN_PASS || 'password',
  },
};

// Which profile (config/profiles/<name>.json) and how hard to drive it.
export const PROFILE = __ENV.PROFILE || 'standard';
export const PROFILE_PROC = numEnv('PROFILE_PROC', 1.0);

// load.js run length: short = regression, long = soak.
export const DURATION = __ENV.DURATION || '5m';

// delete case: when true it also purges the file from trash (hard delete).
export const HARD_DELETE = (__ENV.HARD_DELETE || 'true').toLowerCase() !== 'false';

// Folder of audio files the upload case reads (never committed). Relative paths
// are resolved by k6 relative to the script file (tests/k6/<dir>/<file>.js), so
// the default reaches the repo-root test_data/. Override with an absolute path.
export const TEST_AUDIO_DIR = __ENV.TEST_AUDIO_DIR || '../../../test_data';

// capacity.js ramp shape — it ramps PROFILE_PROC (not raw RPS) to the knee.
export const ramp = {
  start: numEnv('RAMP_START', 0.5), // starting profile_proc
  step: numEnv('RAMP_STEP', 0.5), // profile_proc added each stage
  stepSecs: intEnv('RAMP_STEP_SECS', 60), // stage hold time
  max: numEnv('RAMP_MAX', 5.0), // ceiling profile_proc (safety cap)
};

// Per-case requests/hour override: PER_HOUR_<case> (e.g. PER_HOUR_upload=2000).
export function perHour(caseName, fallback) {
  return numEnv(`PER_HOUR_${caseName}`, fallback);
}

function numEnv(key, fallback) {
  const v = __ENV[key];
  if (v == null || v === '') return fallback;
  const n = parseFloat(v);
  return Number.isFinite(n) ? n : fallback;
}

function intEnv(key, fallback) {
  const v = __ENV[key];
  if (v == null || v === '') return fallback;
  const n = parseInt(v, 10);
  return Number.isFinite(n) ? n : fallback;
}
