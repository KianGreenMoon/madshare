import { test, expect } from '@playwright/test';
import fs from 'node:fs';
import { storageStateFor } from '../helpers/auth';
import { makeAudioFixture, hasFfmpeg } from '../helpers/audio';

// The full moderation journey spans THREE identities, so we drive three explicit
// browser contexts (each loaded from a saved session) instead of the single-page
// `page` fixture. This is the idiomatic way to test multi-actor workflows.
test.describe('Upload → moderation → library', () => {
  test.skip(!hasFfmpeg(), 'requires ffmpeg on PATH to generate a fixture');

  test('uploader stages a draft, admin approves, the track appears in the library', async ({ browser }) => {
    const track = makeAudioFixture();

    try {
      // ── 1. UPLOADER: upload the file and send it for review ────────────────
      const upCtx = await browser.newContext({ storageState: storageStateFor('uploader') });
      const up = await upCtx.newPage();
      up.on('dialog', (d) => d.accept()); // accept any confirm() on submit

      await up.goto('/upload');
      // The file input is hidden behind a drop-zone; setInputFiles works anyway.
      await up.setInputFiles('#fileInput', track.path);
      await up.locator('#startUpload').click();

      // With auth, an upload lands as a DRAFT, "staged in My uploads".
      await expect(up.locator('#uploadQueueList')).toContainText(/staged in My uploads/i);

      // Open the staging tab and locate my row by its unique title.
      await up.locator('#tabBtnMine').click();
      const myCheckbox = up.getByRole('checkbox', { name: `Select ${track.title}` });
      await expect(myCheckbox).toBeVisible();

      // Drafts come pre-selected, so deselect all, then select ONLY my file —
      // the selcount assertion proves the bulk action will touch just this one.
      const allChecks = up.locator('#mineFileList .fl-rowcheck:visible');
      for (let i = 0; i < (await allChecks.count()); i++) await allChecks.nth(i).uncheck();
      await myCheckbox.check();
      await expect(up.locator('#mineFileList .bulk-selcount')).toHaveText('1 selected');

      await up.getByRole('button', { name: 'Send to approval' }).click();
      await upCtx.close();

      // ── 2. ADMIN: approve it in the review queue ───────────────────────────
      const adCtx = await browser.newContext({ storageState: storageStateFor('admin') });
      const ad = await adCtx.newPage();
      ad.on('dialog', (d) => d.accept());

      await ad.goto('/admin/library#review');
      const reviewRow = ad.locator('tr', { hasText: track.title });
      await expect(reviewRow).toBeVisible();
      // Per-row "Approve" (exact, so it isn't the bulk "Approve selected").
      await reviewRow.getByRole('button', { name: 'Approve', exact: true }).click();
      // Once approved, the row leaves the review queue.
      await expect(reviewRow).toHaveCount(0);
      await adCtx.close();

      // ── 3. USER: the track is now browsable in the library ─────────────────
      const userCtx = await browser.newContext({ storageState: storageStateFor('user') });
      const u = await userCtx.newPage();

      await u.goto('/library');
      await u.getByRole('button', { name: `Browse ${track.artist}`, exact: true }).click();
      await u.getByRole('button', { name: `Browse album ${track.album}`, exact: true }).click();
      await expect(u.getByRole('button', { name: `Play ${track.title}`, exact: true })).toBeVisible();
      await userCtx.close();
    } finally {
      fs.rmSync(track.path, { force: true });
    }
  });
});
