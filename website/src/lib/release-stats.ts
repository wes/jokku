// The latest release and how many times Jokku has been downloaded, from
// GitHub's releases API. Every install and every update downloads the jokku
// binary from a release, so its download count is how many times Jokku has
// been installed or updated. The build reads it once (releases.ts) and the
// page refreshes it in the browser (release-stats.tsx), so it stays current
// between deploys. Kept free of server-only imports: both sides use it.

export type ReleaseStats = {
  version: string;
  downloads: number | null;
};

type Release = {
  tag_name: string;
  draft: boolean;
  prerelease: boolean;
  assets: Array<{ name: string; download_count: number }>;
};

export const RELEASES_API = 'https://api.github.com/repos/wes/jokku/releases';

// The binaries install.sh and update.sh fetch; checksums.txt is fetched
// alongside them and would count every install twice.
const isBinary = (name: string) => name.startsWith('jokku-linux-');

export const summarize = (releases: Release[]): ReleaseStats | null => {
  const latest = releases.find((r) => !r.draft && !r.prerelease);
  if (!latest) return null;
  let downloads = 0;
  for (const r of releases) {
    for (const a of r.assets) {
      if (isBinary(a.name)) downloads += a.download_count;
    }
  }
  return { version: latest.tag_name, downloads };
};

// fetchReleaseStats reads every release, a hundred per page, newest first.
// It returns null when GitHub can't be reached or refuses (it allows 60
// unauthenticated requests an hour per address).
export const fetchReleaseStats = async (init?: RequestInit): Promise<ReleaseStats | null> => {
  const all: Release[] = [];
  for (let page = 1; page <= 10; page++) {
    const res = await fetch(`${RELEASES_API}?per_page=100&page=${page}`, {
      ...init,
      headers: { Accept: 'application/vnd.github+json', ...init?.headers },
    });
    if (!res.ok) return null;
    const batch = (await res.json()) as Release[];
    all.push(...batch);
    if (batch.length < 100) break;
  }
  return summarize(all);
};

export const formatCount = (n: number) => new Intl.NumberFormat('en-US').format(n);
