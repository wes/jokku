import { Link } from 'waku';
import type { Href } from '../lib/nav';
import { GITHUB_URL, RELEASES_URL } from '../lib/site';
import { Logo } from './logo';

const COLUMNS: Array<{ title: string; links: Array<{ label: string; to: Href }> }> = [
  {
    title: 'Start',
    links: [
      { label: 'Introduction', to: '/docs' },
      { label: 'Installation', to: '/docs/installation' },
      { label: 'Deploy your first app', to: '/docs/quickstart' },
      { label: 'Coming from Dokku', to: '/docs/dokku' },
    ],
  },
  {
    title: 'Guides',
    links: [
      { label: 'Compose apps', to: '/docs/compose' },
      { label: 'Domains & HTTPS', to: '/docs/domains' },
      { label: 'Volumes', to: '/docs/storage' },
      { label: 'Backups', to: '/docs/backups' },
      { label: 'Adding servers', to: '/docs/cluster' },
    ],
  },
  {
    title: 'Reference',
    links: [
      { label: 'Commands', to: '/docs/commands' },
      { label: 'How it works', to: '/docs/architecture' },
      { label: 'Updating', to: '/docs/updating' },
    ],
  },
];

export const SiteFooter = () => (
  <footer className="border-t border-line">
    <div className="mx-auto grid max-w-[88rem] gap-12 px-4 py-14 sm:px-6 md:grid-cols-[1.4fr_repeat(4,1fr)] lg:px-8">
      <div className="max-w-xs">
        <Logo />
        <p className="mt-4 text-[14px] leading-6 text-muted">
          Your own platform for deploying apps. Dokku’s commands, Firecracker microVMs, as many
          servers as you like.
        </p>
      </div>
      {COLUMNS.map((col) => (
        <div key={col.title}>
          <h3 className="font-mono text-[11.5px] tracking-wider text-subtle uppercase">{col.title}</h3>
          <ul className="mt-4 space-y-2.5 text-[14px]">
            {col.links.map((l) => (
              <li key={l.to}>
                <Link to={l.to} className="text-muted transition hover:text-fg">
                  {l.label}
                </Link>
              </li>
            ))}
          </ul>
        </div>
      ))}
      <div>
        <h3 className="font-mono text-[11.5px] tracking-wider text-subtle uppercase">Project</h3>
        <ul className="mt-4 space-y-2.5 text-[14px]">
          <li>
            <a href={GITHUB_URL} target="_blank" rel="noreferrer" className="text-muted transition hover:text-fg">
              GitHub
            </a>
          </li>
          <li>
            <a href={RELEASES_URL} target="_blank" rel="noreferrer" className="text-muted transition hover:text-fg">
              Releases
            </a>
          </li>
          <li>
            <a href={`${GITHUB_URL}/issues`} target="_blank" rel="noreferrer" className="text-muted transition hover:text-fg">
              Issues
            </a>
          </li>
        </ul>
      </div>
    </div>
    <div className="border-t border-line">
      <div className="mx-auto flex max-w-[88rem] flex-wrap items-center justify-between gap-3 px-4 py-6 text-[13px] text-subtle sm:px-6 lg:px-8">
        <p>
          Built on Firecracker, WireGuard, Caddy and BuildKit. Modeled on{' '}
          <a href="https://dokku.com" target="_blank" rel="noreferrer" className="underline decoration-line-strong underline-offset-4 hover:text-muted">
            Dokku
          </a>
          .
        </p>
        <p className="font-mono">git push jokku main</p>
      </div>
    </div>
  </footer>
);
