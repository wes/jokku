'use client';

import { Link, useRouter } from 'waku';
import { docHref, NAV } from '../lib/nav';

export const DocsSidebar = ({ onNavigate }: { onNavigate?: () => void }) => {
  const { path } = useRouter();
  const current = path.replace(/\/$/, '') || '/';

  return (
    <nav aria-label="Documentation" className="space-y-8 text-[14px]">
      {NAV.map((group) => (
        <div key={group.title}>
          <h4 className="mb-2.5 px-3 font-mono text-[11px] tracking-wider text-subtle uppercase">
            {group.title}
          </h4>
          <ul className="space-y-px border-l border-line">
            {group.items.map((item) => {
              const href = docHref(item.slug);
              const active = current === href;
              return (
                <li key={item.slug}>
                  <Link
                    to={href}
                    onClick={onNavigate}
                    aria-current={active ? 'page' : undefined}
                    className={`-ml-px block border-l py-1.5 pr-2 pl-3 transition ${
                      active
                        ? 'border-accent font-medium text-accent'
                        : 'border-transparent text-muted hover:border-line-strong hover:text-fg'
                    }`}
                  >
                    {item.title}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
};
