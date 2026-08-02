import { type Page, type Locator } from '@playwright/test';

// Drills the library to the first artist's first album and returns the track rows.
export async function openFirstAlbumTracks(page: Page): Promise<Locator> {
  await page.goto('/library');
  await page.locator('#libraryPanel .artist-row').first().click();
  await page.locator('#libraryPanel .album-row').first().click();
  const tracks = page.locator('#libraryPanel .track-row');
  await tracks.first().waitFor();
  return tracks;
}

// The sticky header overlaps elements scrolled to the very top of the panel, which
// makes clicks fail with "intercepts pointer events". Centering the target in the
// viewport first avoids that whole class of flake.
export async function centerInView(loc: Locator): Promise<void> {
  await loc.evaluate((el) => el.scrollIntoView({ block: 'center' }));
}

// Opens a track row's "⋯" menu and returns the menu container.
export async function openTrackMenu(page: Page, track: Locator): Promise<Locator> {
  const kebab = track.locator('.row-more');
  await centerInView(kebab);
  await kebab.click();
  return page.locator('.row-menu');
}
