import { test, expect } from '@playwright/test';
import { storageStateFor } from '../helpers/auth';

// Browse is content.access-gated, so we run it as a signed-in user (saved session).
test.use({ storageState: storageStateFor('user') });

test.describe('Library browse', () => {
  test('drills artist → album → tracks', async ({ page }) => {
    await page.goto('/library');

    // Artists view. Rows are rendered by app.js as .artist-row (role=button,
    // aria-label "Browse <name>"). Data-agnostic: assert there IS content, then
    // drive the first row — survives changes to the disposable server's library.
    const artistRows = page.locator('#libraryPanel .artist-row');
    await expect(artistRows.first()).toBeVisible();
    expect(await artistRows.count()).toBeGreaterThan(0);

    // Read the first artist's name, then drill in.
    const artistName = (await artistRows.first().locator('.row-name').innerText()).trim();
    await artistRows.first().click();

    // Breadcrumb reflects the location.
    await expect(page.locator('#breadcrumb')).toContainText(artistName);

    // Albums view → drill into the first album.
    const albumRows = page.locator('#libraryPanel .album-row');
    await expect(albumRows.first()).toBeVisible();
    await albumRows.first().click();

    // Tracks view: each row is role=button "Play <title>".
    const trackRows = page.locator('#libraryPanel .track-row');
    await expect(trackRows.first()).toBeVisible();
    await expect(trackRows.first()).toHaveAttribute('aria-label', /^Play /);
  });
});
