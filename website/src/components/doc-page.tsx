import { Link } from 'waku';
import { docHref, getDoc } from '../lib/docs';
import { GITHUB_URL } from '../lib/site';
import { ArrowLeftIcon, ArrowRightIcon, GitHubIcon } from './icons';
import { Markdown } from './markdown';
import { Toc } from './toc';

export const DocPage = ({ slug }: { slug: string }) => {
  const doc = getDoc(slug);
  if (!doc) return null;

  return (
    <div className="xl:grid xl:grid-cols-[minmax(0,1fr)_14rem] xl:gap-14">
      <title>{slug === 'introduction' ? 'Docs · Jokku' : `${doc.title} · Jokku docs`}</title>
      <meta name="description" content={doc.description} />

      <article className="mx-auto max-w-[46rem] min-w-0 pt-10 pb-20 text-[15.5px] leading-7 text-body lg:pt-12 xl:mx-0">
        <header className="mb-10 border-b border-line pb-8">
          <p className="mb-3 font-mono text-[12px] tracking-wide text-accent">{doc.group}</p>
          <h1 className="text-[34px] leading-[1.15] font-semibold tracking-[-0.03em] text-fg sm:text-[40px]">
            {doc.title}
          </h1>
          {doc.description ? (
            <p className="mt-4 text-[17px] leading-7 text-muted">{doc.description}</p>
          ) : null}
        </header>

        <Markdown tokens={doc.tokens} />

        <nav className="mt-16 grid gap-3 border-t border-line pt-8 sm:grid-cols-2" aria-label="Previous and next pages">
          {doc.prev ? (
            <Link
              to={docHref(doc.prev.slug)}
              className="group rounded-xl border border-line p-4 transition hover:border-accent/50"
            >
              <span className="flex items-center gap-1.5 text-[12.5px] text-subtle">
                <ArrowLeftIcon className="size-3.5 transition group-hover:-translate-x-0.5" /> Previous
              </span>
              <span className="mt-1 block font-medium text-fg">{doc.prev.title}</span>
            </Link>
          ) : (
            <span />
          )}
          {doc.next ? (
            <Link
              to={docHref(doc.next.slug)}
              className="group rounded-xl border border-line p-4 text-right transition hover:border-accent/50"
            >
              <span className="flex items-center justify-end gap-1.5 text-[12.5px] text-subtle">
                Next <ArrowRightIcon className="size-3.5 transition group-hover:translate-x-0.5" />
              </span>
              <span className="mt-1 block font-medium text-fg">{doc.next.title}</span>
            </Link>
          ) : null}
        </nav>
      </article>

      <aside className="hidden xl:block">
        <div className="scrollbar-thin sticky top-16 max-h-[calc(100svh-4rem)] overflow-y-auto pt-12 pb-10">
          <Toc headings={doc.headings} />
          <div className="mt-8 border-t border-line pt-6">
            <a
              href={`${GITHUB_URL}/issues`}
              target="_blank"
              rel="noreferrer"
              className="flex items-center gap-2 text-[13px] text-muted transition hover:text-fg"
            >
              <GitHubIcon className="size-3.5" /> Questions? Open an issue
            </a>
          </div>
        </div>
      </aside>
    </div>
  );
};
