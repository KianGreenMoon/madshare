import { test as setup } from '@playwright/test';
import { ROLES, storageStateFor, login, type Role } from '../helpers/auth';

// A "setup" project (wired in playwright.config.ts) runs BEFORE the real specs.
// It logs in once per role through the UI and saves the resulting session to a
// file. Later specs do `test.use({ storageState })` and start already signed in —
// no per-test login clicks. This is THE pattern that keeps a real suite fast.
//
// Note: these files end in `.setup.ts`, not `.spec.ts`, so the default test
// matcher won't pick them up — only the dedicated setup project (which overrides
// `testMatch`) runs them.
for (const role of Object.keys(ROLES) as Role[]) {
  setup(`authenticate as ${role}`, async ({ page }) => {
    await login(page, role);
    await page.context().storageState({ path: storageStateFor(role) });
  });
}
