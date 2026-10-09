'use client';

import { useEffect, useState } from 'react';
import { useRouter } from 'waku';
import { docHref, NAV } from '../lib/nav';
import { DocsSidebar } from './docs-sidebar';
import { CloseIcon, MenuIcon } from './icons';

// Below lg the sidebar lives in a drawer opened from a bar under the header.
export const DocsMobileNav = () => {
  const [open, setOpen] = useState(false);
  const { path } = useRouter();
  const current = path.replace(/\/$/, '');
  const page = NAV.flatMap((g) => g.items.map((i) => ({ ...i, group: g.title }))).find(
    (i) => docHref(i.slug) === current,
  );

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false);
    window.addEventListener('keydown', onKey);
    document.body.style.overflow = 'hidden';
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = '';
    };
  }, [open]);

  return (
    <div className="sticky top-16 z-40 -mx-4 border-b border-line bg-bg/85 px-4 backdrop-blur-xl sm:-mx-6 sm:px-6 lg:hidden">
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="flex h-12 w-full items-center gap-3 text-[14px]"
        aria-expanded={open}
      >
        <MenuIcon className="size-[18px] text-muted" />
        {page ? (
          <span className="truncate">
            <span className="text-subtle">{page.group}</span>
            <span className="mx-2 text-line-strong">/</span>
            <span className="font-medium text-fg">{page.title}</span>
          </span>
        ) : (
          <span className="text-muted">Menu</span>
        )}
      </button>

      {open ? (
        <div className="fixed inset-0 z-[90]" role="dialog" aria-modal="true" aria-label="Documentation menu">
          <div className="absolute inset-0 bg-bg/70 backdrop-blur-sm" onClick={() => setOpen(false)} />
          <div className="absolute inset-y-0 left-0 w-[min(20rem,85vw)] overflow-y-auto border-r border-line bg-elevated px-4 pt-4 pb-10 shadow-2xl">
            <div className="mb-6 flex justify-end">
              <button
                type="button"
                onClick={() => setOpen(false)}
                className="grid size-9 place-items-center rounded-lg text-muted hover:bg-surface hover:text-fg"
                aria-label="Close menu"
              >
                <CloseIcon className="size-5" />
              </button>
            </div>
            <DocsSidebar onNavigate={() => setOpen(false)} />
          </div>
        </div>
      ) : null}
    </div>
  );
};
