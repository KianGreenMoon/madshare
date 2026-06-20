import { test, expect } from '@playwright/test';

// Credentials come from the environment (see .env.example), with the k6 suite's
// seeded-user defaults. The test user must NOT be a first-run admin — see the
// README "Test environment" note about the forced password-change modal.
const USER = process.env.TEST_USER ?? 'user';
const PASS = process.env.TEST_PASS ?? 'password';

test.describe('Authentication', () => {
  test('a registered user can sign in from the header', async ({ page }) => {
    // ── Arrange ────────────────────────────────────────────────────────────
    // baseURL (config) + '/' → the library page. Auto-waits for load.
    await page.goto('/');

    // While logged out the header shows a single "Sign in" button. The modal's
    // own "Sign in" submit button exists in the DOM but is display:none, so it
    // is NOT in the accessibility tree yet — this getByRole matches exactly one.
    await page.getByRole('button', { name: 'Sign in' }).click();

    // ── Act ────────────────────────────────────────────────────────────────
    // Scope everything to the modal. Two wins: it documents intent, and it
    // sidesteps the strict-mode trap below.
    const modal = page.locator('#loginModal');
    await expect(modal).toBeVisible();

    // Fill by the <label> text — the user-facing way, robust to markup changes.
    // `exact` guards against the change-password modal's "...password" labels.
    await modal.getByLabel('Username', { exact: true }).fill(USER);
    await modal.getByLabel('Password', { exact: true }).fill(PASS);

    // ⚠️ STRICT-MODE TRAP: now the modal is open, the page has TWO visible
    // "Sign in" buttons (header + this submit). `page.getByRole(...)` would
    // match both and throw. Scoping to `modal` resolves to exactly one.
    await modal.getByRole('button', { name: 'Sign in' }).click();

    // ── Assert the OUTCOME, not the steps ────────────────────────────────────
    // On success the app calls location.reload(); the server-rendered header
    // then offers "Log out" and shows the username. These assertions auto-retry
    // until the reloaded page paints — no manual wait needed.
    await expect(page.getByRole('button', { name: 'Log out' })).toBeVisible();
    await expect(page.locator('#userName')).toHaveText(USER);
  });

  test('a wrong password shows an inline error and stays logged out', async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Sign in' }).click();

    const modal = page.locator('#loginModal');
    await modal.getByLabel('Username', { exact: true }).fill(USER);
    await modal.getByLabel('Password', { exact: true }).fill('definitely-wrong');
    await modal.getByRole('button', { name: 'Sign in' }).click();

    // The app shows an inline error and does NOT reload.
    await expect(page.getByText('Invalid username or password.')).toBeVisible();

    // Still logged out: there is no "Log out" control. `toHaveCount(0)` is the
    // clean way to assert absence (and avoids the two-"Sign in"-buttons trap).
    await expect(page.getByRole('button', { name: 'Log out' })).toHaveCount(0);
  });
});
