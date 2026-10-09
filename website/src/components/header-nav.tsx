'use client';

import { Link, useRouter } from 'waku';
import type { Href } from '../lib/nav';

const LINKS: Array<{ to: Href; label: string; match: (p: string) => boolean }> = [
  { to: '/docs', label: 'Docs', match: (p: string) => p.startsWith('/docs') && !/^\/docs\/(commands|architecture)/.test(p) },
  { to: '/docs/commands', label: 'Commands', match: (p: string) => p === '/docs/commands' },
  { to: '/docs/architecture', label: 'How it works', match: (p: string) => p === '/docs/architecture' },
];

export const HeaderNav = () => {
  const { path } = useRouter();
  return (
    <nav className="hidden items-center gap-0.5 text-[14px] md:flex">
      {LINKS.map((l) => {
        const active = l.match(path);
        return (
          <Link
            key={l.to}
            to={l.to}
            className={`rounded-lg px-3 py-1.5 transition ${active ? 'font-medium text-fg' : 'text-muted hover:text-fg'}`}
          >
            {l.label}
          </Link>
        );
      })}
    </nav>
  );
};
