import { Link } from 'waku';
import { ArrowRightIcon } from '../components/icons';

export default async function NotFoundPage() {
  return (
    <div className="mx-auto flex max-w-2xl flex-col items-center px-4 py-28 text-center sm:py-36">
      <title>Not found · Jokku</title>
      <meta name="robots" content="noindex" />
      <div className="w-full overflow-hidden rounded-2xl border border-term-line bg-term text-left font-mono text-[13px] leading-[1.9] shadow-2xl shadow-black/30">
        <div className="flex h-10 items-center gap-2 border-b border-term-line bg-term-bar px-4">
          <span className="size-3 rounded-full bg-white/10" />
          <span className="size-3 rounded-full bg-white/10" />
          <span className="size-3 rounded-full bg-white/10" />
        </div>
        <div className="px-5 py-4 text-term-fg">
          <div>
            <span className="text-term-dim">$ </span>
            <span className="text-term-bright">jokku domains:report this-page</span>
          </div>
          <div className="text-term-bad"> !     No page lives at this address</div>
          <div>
            <span className="text-term-dim">$ </span>
            <span className="inline-block h-[1.1em] w-[0.6em] translate-y-[0.2em] animate-blink bg-term-accent" />
          </div>
        </div>
      </div>
      <h1 className="mt-12 text-[36px] font-semibold tracking-[-0.035em] text-fg">404, nothing deployed here.</h1>
      <p className="mt-3 text-[16px] text-muted">The page moved, or never existed. Try search, or start from the docs.</p>
      <div className="mt-8 flex flex-wrap justify-center gap-3">
        <Link
          to="/docs"
          className="inline-flex h-10 items-center gap-2 rounded-lg bg-fill px-4 text-[14.5px] font-medium text-on-fill shadow-[inset_0_1px_0_rgb(255_255_255/0.3)] transition hover:bg-fill-hover"
        >
          Read the docs <ArrowRightIcon className="size-4" />
        </Link>
        <Link
          to="/"
          className="inline-flex h-10 items-center rounded-xl border border-line-strong px-4 text-[14.5px] font-medium text-fg transition hover:border-fg/30"
        >
          Home
        </Link>
      </div>
    </div>
  );
}

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
