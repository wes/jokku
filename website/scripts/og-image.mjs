// Renders og/og-image.html to public/og-image.jpg with headless Chrome.
// Usage: npm run og (set CHROME to your browser's binary if it isn't found).
//
// It's a JPEG because the film grain makes a PNG about 900 KB, and some
// apps (WhatsApp among them) skip previews over roughly 300 KB.

import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const source = pathToFileURL(resolve(root, 'og/og-image.html')).href;
const output = resolve(root, 'public/og-image.jpg');
const QUALITY = 86;

const which = (...paths) => paths.filter(Boolean).find((path) => existsSync(path));
const onPath = (cmd) => {
  try {
    return execFileSync('which', [cmd], { encoding: 'utf8' }).trim() || undefined;
  } catch {
    return undefined;
  }
};

const chrome = which(
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
);
if (!chrome) {
  console.error('Chrome not found. Set CHROME to the path of a Chrome or Chromium binary.');
  process.exit(1);
}

const work = mkdtempSync(join(tmpdir(), 'og-'));
const png = join(work, 'og-image.png');
try {
  execFileSync(
    chrome,
    [
      '--headless=new',
      '--hide-scrollbars',
      '--force-device-scale-factor=1',
      '--window-size=1200,630',
      // Gives the web fonts time to load before the shot.
      '--virtual-time-budget=10000',
      `--screenshot=${png}`,
      source,
    ],
    { stdio: ['ignore', 'ignore', 'ignore'] },
  );

  const sips = onPath('sips');
  const magick = onPath('magick');
  if (sips) {
    execFileSync(sips, ['-s', 'format', 'jpeg', '-s', 'formatOptions', String(QUALITY), png, '--out', output], {
      stdio: 'ignore',
    });
  } else if (magick) {
    execFileSync(magick, [png, '-quality', String(QUALITY), output]);
  } else {
    console.error('Need sips (macOS) or ImageMagick (magick) to write the JPEG.');
    process.exit(1);
  }
} finally {
  rmSync(work, { recursive: true, force: true });
}

console.log(`Wrote ${output} (${Math.round(statSync(output).size / 1024)} KB)`);
