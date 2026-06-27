// grouped-stream.js — the pure state machine behind file-list.js's paged
// "By artist / album" view. It folds pages of rows — already in the server's
// grouped order (sort=grouped: album-artist, then album by earliest year, then
// disc, then track) — into a flat windowed-item array of artist/album/disc
// separators + track rows, emitted incrementally as the list scrolls, so the
// grouped view scales like the flat infinite-scroll instead of loading every
// row first. Kept DOM-free (only disc.js, which is pure) so it unit-tests in
// node: tests/js/grouped-stream.test.mjs. Design: docs/architecture/file-list-scaling.md.

import { discKey, discLabel, isMultiDisc } from './disc.js';

// createGroupedStream builds one streaming grouper. An album is buffered until its
// boundary (`pending`) so multi-disc detection and the album's select-all hashes are
// exact even when it straddles a page; an artist spans pages, so its header's "loaded
// so far" count and select-all hashes are bumped in place as each of its albums
// flushes. isSelectable(file) gates which rows feed the select-all hash sets.
//
// Item shapes match file-list.js renderWindowItem: separators are
//   { kind:'sep', sep:'artist'|'album'|'disc', label, meta, hashes, fallback, cover }
// and tracks are { kind:'grow', file }.
export function createGroupedStream(isSelectable = () => false) {
  const lc = s => (s || '').toLowerCase();
  const albumYear = files => { for (const f of files) if (f.year) return f.year; return 9999; };

  let items, pending, artKeyLc, artSep, artAlbums, artTracks;
  function reset() { items = []; pending = null; artKeyLc = null; artSep = null; artAlbums = 0; artTracks = 0; }
  reset();

  // emitAlbum turns one COMPLETE album into separator + track items, bumping the
  // running artist tally (a new album-artist emits its artist separator first).
  function emitAlbum(album) {
    const delta = [];
    if (album.aKeyLc !== artKeyLc) {
      artKeyLc = album.aKeyLc; artAlbums = 0; artTracks = 0;
      artSep = {
        kind: 'sep', sep: 'artist', label: album.aKey || 'Unknown artist', meta: '',
        hashes: [], fallback: !album.aKey,
        cover: { kind: 'artist', target: { artist: album.aKey }, hasImage: album.files[0]?.artist_has_image },
      };
      delta.push(artSep);
    }
    artAlbums += 1; artTracks += album.files.length;
    artSep.meta = `${artAlbums} album${artAlbums === 1 ? '' : 's'} · ${artTracks} track${artTracks === 1 ? '' : 's'}`;
    for (const f of album.files) if (isSelectable(f)) artSep.hashes.push(f.hash);

    const y = albumYear(album.files);
    delta.push({
      kind: 'sep', sep: 'album', label: album.alKey || 'Other', meta: y < 9999 ? String(y) : '',
      hashes: album.files.filter(isSelectable).map(f => f.hash), fallback: !album.alKey,
      cover: { kind: 'album', target: { artist: album.aKey, album: album.alKey }, hasImage: album.files[0]?.album_has_image },
    });
    // The album is complete here, so multi-disc detection (hence the leading "Disc N"
    // header) is exact even when the album straddled a page boundary.
    const multiDisc = isMultiDisc(album.files);
    let shownDisc;
    for (const f of album.files) {
      const disc = discKey(f.disc_number);
      if (multiDisc && disc !== shownDisc) {
        shownDisc = disc;
        delta.push({ kind: 'sep', sep: 'disc', label: discLabel(disc), meta: '', hashes: [], fallback: false, cover: null });
      }
      delta.push({ kind: 'grow', file: f });
    }
    return delta;
  }

  // ingest folds a page of server-sorted rows in: rows accumulate into the open album
  // and an album is emitted only once its boundary is crossed (or, with isFinal, the
  // listing ends). Returns the items appended this call (also pushed onto `items`).
  // Keys compare lower-cased to match the server's LOWER() ordering.
  function ingest(pageRows, isFinal) {
    const delta = [];
    for (const f of pageRows) {
      const aKey = (f.album_artist || f.artist || '').trim();
      const alKey = (f.album || '').trim();
      const aKeyLc = lc(aKey), alKeyLc = lc(alKey);
      if (pending && (aKeyLc !== pending.aKeyLc || alKeyLc !== pending.alKeyLc)) {
        delta.push(...emitAlbum(pending)); pending = null;
      }
      if (!pending) pending = { aKeyLc, alKeyLc, aKey, alKey, files: [] };
      pending.files.push(f);
    }
    if (isFinal && pending) { delta.push(...emitAlbum(pending)); pending = null; }
    for (const it of delta) items.push(it);
    return delta;
  }

  return { reset, ingest, get items() { return items; } };
}
