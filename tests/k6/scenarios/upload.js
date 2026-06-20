// upload: a period of active ingest. Same engine as load.js, but fixed to the
// upload-heavy `uploading` profile (upload + delete, self-balanced). Kept as its
// own scenario for obviousness; PROFILE_PROC and DURATION still apply.
//
//   k6 run tests/k6/scenarios/upload.js
//   k6 run -e DURATION=10m -e PROFILE_PROC=1.5 tests/k6/scenarios/upload.js
//
// Needs audio fixtures (TEST_AUDIO_DIR + config/audio-manifest.json); with none
// present the upload/delete cases no-op. Mutates the test library — test server only.

import { buildConstant, loadProfile, caseNames } from '../lib/engine.js';
import { thresholdsFor } from '../config/options.js';

export { setup, teardown } from '../lib/lifecycle.js';
export { runCase } from '../lib/runner.js';

const profile = loadProfile('uploading');

export const options = {
  scenarios: buildConstant(profile),
  thresholds: thresholdsFor(caseNames(profile)),
};
