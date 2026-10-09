import '../styles.css';

import type { ReactNode } from 'react';
import { SiteFooter } from '../components/site-footer';
import { SiteHeader } from '../components/site-header';

export default async function RootLayout({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-svh flex-col">
      <meta name="theme-color" content="#16120e" />
      <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
      <link rel="preconnect" href="https://fonts.googleapis.com" />
      <link rel="preconnect" href="https://fonts.gstatic.com" crossOrigin="" />
      <link
        rel="stylesheet"
        href="https://fonts.googleapis.com/css2?family=Geist:wght@400..700&family=Geist+Mono:wght@400..600&display=swap"
        precedence="font"
      />
      <SiteHeader />
      <main className="flex-1">{children}</main>
      <SiteFooter />
    </div>
  );
}

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
