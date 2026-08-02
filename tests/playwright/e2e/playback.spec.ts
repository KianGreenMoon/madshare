import { test, expect } from '@playwright/test';
import { storageStateFor } from '../helpers/auth';

test.use({ storageState: storageStateFor('user') });

test.describe('Playback', () => {
  test('clicking a track starts the player', async ({ page }) => {
    await page.goto('/library');
    await page.locator('#libraryPanel .artist-row').first().click();
    await page.locator('#libraryPanel .album-row').first().click();

    const firstTrack = page.locator('#libraryPanel .track-row').first();
    await expect(firstTrack).toBeVisible();
    const title = (await firstTrack.locator('.track-title').innerText()).trim();
    await firstTrack.click();

    // The player bar reveals (it's display:none until something plays) and shows
    // the track we picked.
    const bar = page.locator('#player-bar');
    await expect(bar).toBeVisible();
    await expect(bar).toContainText(title);

    // The real proof: the <audio> element is actually playing. expect.poll
    // retries the page evaluation until it flips to not-paused (or times out).
    await expect
      .poll(
        () => page.evaluate(() => {
          const a = document.querySelector('audio');
          return a ? !a.paused : false;
        }),
        { timeout: 5000, message: 'audio never started playing' },
      )
      .toBe(true);
  });
});
