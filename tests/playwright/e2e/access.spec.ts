import { test, expect } from '@playwright/test';
import { ROLES, storageStateFor } from '../helpers/auth';

// Role-based access matrix: the header exposes Upload/Admin/Playlists via
// server-side template conditionals ({{if .CanUpload}} etc.), so what a role can
// see is deterministic and read-only to test. Each describe block adopts a
// different saved session via `test.use({ storageState })` — this is how you
// parametrize a whole block by identity.

test.describe('Header access — admin', () => {
  test.use({ storageState: storageStateFor('admin') });

  test('admin sees Upload and Admin', async ({ page }) => {
    await page.goto('/library');
    await expect(page.locator('#userName')).toHaveText(ROLES.admin.user);
    await expect(page.getByRole('link', { name: 'Upload', exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Admin', exact: true })).toBeVisible();
  });
});

test.describe('Header access — uploader', () => {
  test.use({ storageState: storageStateFor('uploader') });

  test('uploader sees Upload but not Admin', async ({ page }) => {
    await page.goto('/library');
    await expect(page.locator('#userName')).toHaveText(ROLES.uploader.user);
    await expect(page.getByRole('link', { name: 'Upload', exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Admin', exact: true })).toHaveCount(0);
  });
});

test.describe('Header access — plain user', () => {
  test.use({ storageState: storageStateFor('user') });

  test('user sees neither Upload nor Admin, but can reach Playlists', async ({ page }) => {
    await page.goto('/library');
    await expect(page.locator('#userName')).toHaveText(ROLES.user.user);
    await expect(page.getByRole('link', { name: 'Upload', exact: true })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'Admin', exact: true })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'Playlists', exact: true })).toBeVisible();
  });
});
