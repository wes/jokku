// Kept free of imports: client components (sidebar, mobile nav) use it.

// Every internal link on the site: the home page or a docs page.
export type Href = '/' | '/docs' | `/docs/${string}`;

export type NavItem = { slug: string; title: string };
export type NavGroup = { title: string; items: NavItem[] };

// The sidebar, in reading order. Prev/next links follow this order too.
export const NAV: NavGroup[] = [
  {
    title: 'Getting started',
    items: [
      { slug: 'introduction', title: 'Introduction' },
      { slug: 'installation', title: 'Installation' },
      { slug: 'quickstart', title: 'Deploy your first app' },
    ],
  },
  {
    title: 'Deploying',
    items: [
      { slug: 'dockerfile', title: 'Dockerfile apps' },
      { slug: 'compose', title: 'Compose apps' },
      { slug: 'images', title: 'Registry images' },
      { slug: 'config', title: 'Config vars' },
      { slug: 'domains', title: 'Domains & HTTPS' },
      { slug: 'logins', title: 'Logins' },
      { slug: 'github-actions', title: 'GitHub Actions' },
    ],
  },
  {
    title: 'Running apps',
    items: [
      { slug: 'processes', title: 'Processes & scaling' },
      { slug: 'logs', title: 'Logs & shell access' },
      { slug: 'storage', title: 'Volumes' },
      { slug: 'backups', title: 'Backups' },
      { slug: 'networking', title: 'Private networking' },
    ],
  },
  {
    title: 'Servers',
    items: [
      { slug: 'cluster', title: 'Adding servers' },
      { slug: 'edges', title: 'Edges & home labs' },
      { slug: 'top', title: 'jokku top' },
      { slug: 'updating', title: 'Updating' },
    ],
  },
  {
    title: 'Reference',
    items: [
      { slug: 'commands', title: 'Commands' },
      { slug: 'architecture', title: 'How it works' },
      { slug: 'dokku', title: 'Coming from Dokku' },
    ],
  },
];

export const docHref = (slug: string): Href =>
  slug === 'introduction' ? '/docs' : `/docs/${slug}`;
