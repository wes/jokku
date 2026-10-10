'use client';

import { useEffect, useState } from 'react';
import { fetchReleaseStats, formatCount, type ReleaseStats } from '../lib/release-stats';
import { RELEASES_URL } from '../lib/site';
import { DownloadIcon } from './icons';

// The latest version and the download count, as the build saw them, then as
// GitHub says now. One request per page view at most, shared by every place
// that shows them, and none for an hour after (GitHub allows 60 an hour).

const CACHE_KEY = 'jokku-release-stats';
const CACHE_FOR = 60 * 60 * 1000;

let current: Promise<ReleaseStats | null> | undefined;

const latest = (): Promise<ReleaseStats | null> => {
  current ??= (async () => {
    try {
      const cached = JSON.parse(localStorage.getItem(CACHE_KEY) ?? 'null');
      if (cached && Date.now() - cached.at < CACHE_FOR) return cached.stats as ReleaseStats;
    } catch {
      // No storage (private mode, say): ask GitHub.
    }
    const stats = await fetchReleaseStats().catch(() => null);
    if (stats) {
      try {
        localStorage.setItem(CACHE_KEY, JSON.stringify({ at: Date.now(), stats }));
      } catch {
        // Storage full or refused: next time asks again.
      }
    }
    return stats;
  })();
  return current;
};

const useReleaseStats = (initial: ReleaseStats) => {
  const [stats, setStats] = useState(initial);
  useEffect(() => {
    let live = true;
    latest().then((s) => {
      if (live && s) setStats(s);
    });
    return () => {
      live = false;
    };
  }, []);
  return stats;
};

export const LatestVersion = ({ initial }: { initial: ReleaseStats }) => <>{useReleaseStats(initial).version}</>;

const DOWNLOADS_TITLE = 'Times the jokku binary was downloaded from GitHub releases: every install and every update';

// DownloadPill is the count, small, for beside the install button.
export const DownloadPill = ({ initial }: { initial: ReleaseStats }) => {
  const { downloads } = useReleaseStats(initial);
  if (downloads === null) return null;
  return (
    <a
      href={RELEASES_URL}
      target="_blank"
      rel="noreferrer"
      title={DOWNLOADS_TITLE}
      className="inline-flex h-11 shrink-0 items-center gap-2 rounded-lg px-2 text-[14px] text-muted transition hover:text-fg"
    >
      <DownloadIcon className="size-4 text-accent" />
      <span>
        <span className="font-semibold text-fg tabular-nums">{formatCount(downloads)}</span> downloads
      </span>
    </a>
  );
};

// InstallStats is a line for under the install command: how often Jokku
// has been installed or updated, and the release it installs.
export const InstallStats = ({ initial }: { initial: ReleaseStats }) => {
  const { downloads, version } = useReleaseStats(initial);
  return (
    <p className="text-[13.5px] text-subtle" title={downloads === null ? undefined : DOWNLOADS_TITLE}>
      {downloads !== null && (
        <>
          Installed and updated <span className="font-medium text-muted tabular-nums">{formatCount(downloads)}</span> times
          <span className="px-2">·</span>
        </>
      )}
      Installs{' '}
      <a href={`${RELEASES_URL}/tag/${version}`} target="_blank" rel="noreferrer" className="font-mono text-muted hover:text-fg">
        {version}
      </a>
    </p>
  );
};
