// Takes full-page PNG screenshots of the static previews in design/preview/
// and writes them to design/screenshots/. Fonts come from the `geist` npm
// package so the screenshots look the same offline.
//
// Usage: (cd design/tools && npm install && npm run render && npm run screenshots)

import { readFileSync, mkdirSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { chromium } from 'playwright';
import { screens } from './screens.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const PREVIEW = join(here, '..', 'preview');
const OUT = join(here, '..', 'screenshots');
const geistDir = join(here, 'node_modules', 'geist', 'dist', 'fonts');

const face = (family, file, weight) => {
  const data = readFileSync(join(geistDir, file)).toString('base64');
  return `@font-face{font-family:'${family}';font-weight:${weight};font-style:normal;src:url(data:font/woff2;base64,${data}) format('woff2');}`;
};

const fontCss = [
  face('Geist', 'geist-sans/Geist-Regular.woff2', 400),
  face('Geist', 'geist-sans/Geist-Medium.woff2', 500),
  face('Geist', 'geist-sans/Geist-SemiBold.woff2', 600),
  face('Geist', 'geist-sans/Geist-Bold.woff2', 700),
  face('Geist Mono', 'geist-mono/GeistMono-Regular.woff2', 400),
  face('Geist Mono', 'geist-mono/GeistMono-Medium.woff2', 500)
].join('\n');

if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 });
await page.route(/fonts\.(googleapis|gstatic)\.com/, (route) =>
  route.fulfill({ status: 200, contentType: 'text/css', body: fontCss })
);

for (const screen of screens) {
  const url = pathToFileURL(join(PREVIEW, `${screen.source}.html`)).href;
  await page.goto(url, { waitUntil: 'load' });
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: join(OUT, `${screen.slug}.png`), fullPage: true });
  console.log(`screenshot ${screen.slug}.png`);
}

await browser.close();
