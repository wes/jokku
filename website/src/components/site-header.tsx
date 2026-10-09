import { Link } from 'waku';
import { GITHUB_URL, VERSION } from '../lib/site';
import { HeaderNav } from './header-nav';
import { GitHubIcon } from './icons';
import { Logo } from './logo';
import { Search } from './search';
import { ThemeToggle } from './theme-toggle';

export const SiteHeader = () => (
  <header className="sticky top-0 z-50 border-b border-line/80 bg-bg/80 backdrop-blur-xl backdrop-saturate-150">
    <div className="mx-auto grid h-16 max-w-[88rem] grid-cols-[1fr_auto] items-center gap-3 px-4 sm:px-6 md:grid-cols-[1fr_auto_1fr] lg:px-8">
      <Link to="/" aria-label="Jokku home" className="flex items-center gap-2.5 justify-self-start">
        <Logo />
        <span className="hidden rounded-md border border-line px-1.5 py-0.5 font-mono text-[11px] text-subtle sm:inline">
          {VERSION}
        </span>
      </Link>
      <HeaderNav />
      <div className="flex items-center gap-1.5 justify-self-end">
        <Search />
        <ThemeToggle />
        <a
          href={GITHUB_URL}
          target="_blank"
          rel="noreferrer"
          aria-label="Jokku on GitHub"
          className="grid size-9 place-items-center rounded-lg text-muted transition hover:bg-surface hover:text-fg"
        >
          <GitHubIcon className="size-[18px]" />
        </a>
        <Link
          to="/docs/installation"
          className="ml-1.5 hidden h-9 items-center rounded-lg bg-fill px-3.5 text-[14px] font-medium text-on-fill shadow-[inset_0_1px_0_rgb(255_255_255/0.3),0_1px_2px_rgb(0_0_0/0.25)] transition hover:bg-fill-hover lg:inline-flex"
        >
          Get started
        </Link>
      </div>
    </div>
  </header>
);
