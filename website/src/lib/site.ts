// Where the site is served, with no trailing slash. Link previews need
// absolute URLs; until this is set they use relative ones, which some
// unfurlers ignore.
export const SITE_URL = '';

export const absoluteUrl = (path: string) => `${SITE_URL}${path}`;

// The link preview image. Its source is og/og-image.html; `npm run og`
// renders it.
export const OG_IMAGE = {
  path: '/og-image.jpg',
  type: 'image/jpeg',
  width: 1200,
  height: 630,
  alt: 'Jokku: Your apps, your servers. Every instance a microVM. Deploy with git push.',
};

export const VERSION = 'v0.5.0';
export const GITHUB_URL = 'https://github.com/wes/jokku';
export const RELEASES_URL = `${GITHUB_URL}/releases`;
export const INSTALL_COMMAND =
  'curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh';
export const DESCRIPTION =
  'Jokku is a self-hosted platform for deploying your apps. git push a Dockerfile and Jokku runs it in Firecracker microVMs across your own servers, with Dokku’s commands.';
