import { expect, type Page } from '@playwright/test';

// ── The roles under test ─────────────────────────────────────────────────────
// Names/passwords default to the seeded test users; override per role via env.
// (All three share the password "password" on a typical disposable test server.)
export const ROLES = {
  admin:    { user: process.env.ADMIN_USER    ?? 'admin',    pass: process.env.ADMIN_PASS    ?? 'password' },
  uploader: { user: process.env.UPLOADER_USER ?? 'uploader', pass: process.env.UPLOADER_PASS ?? 'password' },
  user:     { user: process.env.USER_USER     ?? 'user',     pass: process.env.USER_PASS     ?? 'password' },
} as const;

export type Role = keyof typeof ROLES;

// Where each role's saved browser session (cookies) lands. `.auth/` is gitignored
// because these files contain live session secrets.
export function storageStateFor(role: Role): string {
  return `.auth/${role}.json`;
}

// Drives the real header → modal login flow (the same steps as auth.spec.ts,
// factored out so the setup project and any explicit-login test share one source
// of truth). Resolves once the header confirms we're signed in.
export async function login(page: Page, role: Role): Promise<void> {
  const { user, pass } = ROLES[role];

  await page.goto('/');
  await page.getByRole('button', { name: 'Sign in' }).click();

  const modal = page.locator('#loginModal');
  await modal.getByLabel('Username', { exact: true }).fill(user);
  await modal.getByLabel('Password', { exact: true }).fill(pass);
  await modal.getByRole('button', { name: 'Sign in' }).click();

  // Outcome gate: the server-rendered header now offers "Log out".
  await expect(page.getByRole('button', { name: 'Log out' })).toBeVisible();
}
