import type { ReactNode } from 'react';
import { DocsMobileNav } from '../../components/docs-mobile-nav';
import { DocsSidebar } from '../../components/docs-sidebar';

export default async function DocsLayout({ children }: { children: ReactNode }) {
  return (
    <div className="mx-auto max-w-[88rem] px-4 sm:px-6 lg:px-8">
      <DocsMobileNav />
      <div className="lg:grid lg:grid-cols-[15rem_minmax(0,1fr)] lg:gap-12">
        <aside className="hidden lg:block">
          <div className="scrollbar-thin sticky top-16 max-h-[calc(100svh-4rem)] overflow-y-auto py-10 pr-2">
            <DocsSidebar />
          </div>
        </aside>
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
