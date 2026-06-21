import { test, expect } from '@playwright/test';
import { storageStateFor } from '../helpers/auth';
import { openFirstAlbumTracks, openTrackMenu } from '../helpers/library';

test.use({ storageState: storageStateFor('user') });

test.describe('Playlists', () => {
  test('create a playlist from a track, verify it, then delete it', async ({ page }) => {
    page.on('dialog', (d) => d.accept()); // the Delete confirm

    const tracks = await openFirstAlbumTracks(page);
    const track = tracks.first();
    const title = (await track.locator('.track-title').innerText()).trim();

    // Create a uniquely-named playlist via the track menu's "New playlist…" input.
    const name = `PW List ${Date.now()}`;
    const menu = await openTrackMenu(page, track);
    await menu.getByRole('menuitem', { name: 'Add to playlist…' }).click();
    await menu.getByPlaceholder('New playlist…').fill(name);
    await menu.getByRole('button', { name: 'OK' }).click();

    // It appears on the playlists page with the one track.
    await page.goto('/playlists');
    const plRow = page.getByRole('button', { name: `Open playlist ${name}` });
    await expect(plRow).toBeVisible();
    await expect(plRow).toContainText('1 track');

    // Open it: the track is inside, and the detail offers the management actions.
    await plRow.click();
    await expect(page.locator('#plPanel')).toContainText(title);
    await expect(page.locator('#plHeadActions')).toContainText('Delete');

    // Clean up: delete the playlist; it disappears from the list.
    await page.locator('#plHeadActions').getByRole('button', { name: 'Delete' }).click();
    await expect(page.getByRole('button', { name: `Open playlist ${name}` })).toHaveCount(0);
  });
});
