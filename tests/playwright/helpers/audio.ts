import { execFileSync } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';

export interface AudioFixture {
  path: string;
  title: string;
  artist: string;
  album: string;
}

// True when ffmpeg is on PATH — used to skip upload tests gracefully where it isn't.
export function hasFfmpeg(): boolean {
  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

// Generates a tiny, tagged MP3 in the OS temp dir. The title is unique per call so
// each upload produces a fresh content hash (no dedupe) and is findable by title.
export function makeAudioFixture(overrides: Partial<AudioFixture> = {}): AudioFixture {
  const stamp = `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  const title = overrides.title ?? `PW Track ${stamp}`;
  const artist = overrides.artist ?? 'Playwright QA';
  const album = overrides.album ?? 'E2E Fixtures';
  const filePath = overrides.path ?? path.join(os.tmpdir(), `pw-${stamp}.mp3`);

  execFileSync('ffmpeg', [
    '-f', 'lavfi', '-i', 'sine=frequency=440:duration=2',
    '-metadata', `title=${title}`,
    '-metadata', `artist=${artist}`,
    '-metadata', `album=${album}`,
    '-b:a', '128k', '-y', filePath,
  ], { stdio: 'ignore' });

  return { path: filePath, title, artist, album };
}
