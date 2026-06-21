import { test, expect } from '@playwright/test';
import { storageStateFor } from '../helpers/auth';
import { openFirstAlbumTracks, centerInView } from '../helpers/library';

test.use({ storageState: storageStateFor('user') });

test.describe('Favorites', () => {
  test('favoriting a track adds it to Favorites and persists', async ({ page }) => {
    const tracks = await openFirstAlbumTracks(page);
    const track = tracks.first();
    const title = (await track.locator('.track-title').innerText()).trim();

    const heart = track.locator('.row-heart');
    await centerInView(heart);

    // Normalize to a known un-favorited state (a prior run may have left it on).
    if ((await heart.getAttribute('aria-pressed')) === 'true') {
      await heart.click();
      await expect(heart).toHaveAttribute('aria-pressed', 'false');
    }

    // Favorite it — the control reflects the new state.
    await heart.click();
    await expect(heart).toHaveAttribute('aria-pressed', 'true');
    await expect(heart).toHaveAttribute('aria-label', 'Remove from Favorites');

    // It now shows under the Favorites pseudo-playlist.
    await page.goto('/playlists');
    await page.getByRole('button', { name: 'Open playlist Favorites' }).click();
    await expect(page.locator('#plPanel')).toContainText(title);

    // The favorite survives navigating back to the library...
    const tracksAgain = await openFirstAlbumTracks(page);
    const heartAgain = tracksAgain.first().locator('.row-heart');
    await centerInView(heartAgain);
    await expect(heartAgain).toHaveAttribute('aria-pressed', 'true');

    // ...then clean up by un-favoriting.
    await heartAgain.click();
    await expect(heartAgain).toHaveAttribute('aria-pressed', 'false');
  });
});
