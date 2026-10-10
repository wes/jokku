import { fetchReleaseStats, type ReleaseStats } from './release-stats';
import { VERSION } from './site';

// releaseStats is the latest release and the download count, read from
// GitHub once per build. Without GitHub (an offline build, a rate limit) the
// page still builds, with VERSION and no count; the browser fills them in.
let stats: Promise<ReleaseStats> | undefined;

export const releaseStats = (): Promise<ReleaseStats> => {
  stats ??= (async () => {
    const token = process.env.GITHUB_TOKEN;
    try {
      const got = await fetchReleaseStats({
        signal: AbortSignal.timeout(8000),
        headers: token ? { Authorization: `Bearer ${token}` } : undefined,
      });
      if (got) return got;
    } catch {
      // Fall through to the fallback.
    }
    return { version: VERSION, downloads: null };
  })();
  return stats;
};
