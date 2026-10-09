import { lexer, type Token, type Tokens } from 'marked';
import { docHref, NAV, type Href, type NavItem } from './nav';

export { docHref, NAV, type Href, type NavItem };

export type Heading = { id: string; text: string; depth: 2 | 3 };

export type Doc = {
  slug: string;
  title: string;
  description: string;
  group: string;
  tokens: Token[];
  headings: Heading[];
  prev: NavItem | undefined;
  next: NavItem | undefined;
};

const files = import.meta.glob<string>('../content/docs/*.md', {
  query: '?raw',
  import: 'default',
  eager: true,
});

const ORDER = NAV.flatMap((g) => g.items.map((item) => ({ ...item, group: g.title })));

export const slugify = (text: string) =>
  text
    .toLowerCase()
    .replace(/[`*_~]/g, '')
    .replace(/&\w+;/g, '')
    .replace(/[^\w\s-]/g, '')
    .trim()
    .replace(/\s+/g, '-');

// Plain text of inline markdown, for heading ids, the TOC and search.
export const plainText = (md: string) =>
  md
    .replace(/!\[[^\]]*\]\([^)]*\)/g, '')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/[`*_]/g, '')
    .replace(/&amp;/g, '&')
    .trim();

// Markdown source as readable text: no quote markers, list bullets,
// callout tags or table pipes.
const searchText = (md: string) =>
  plainText(
    md
      .replace(/^>\s?/gm, '')
      .replace(/^\s*(?:[-*]|\d+\.)\s+/gm, '')
      .replace(/\[!\w+\]/g, '')
      .replace(/^\|?\s*-{3,}.*$/gm, '')
      .replace(/\|/g, ' '),
  );

function parseFrontmatter(raw: string) {
  const match = /^---\n([\s\S]*?)\n---\n/.exec(raw);
  const data: Record<string, string> = {};
  if (!match) return { data, body: raw };
  for (const line of match[1]!.split('\n')) {
    const i = line.indexOf(':');
    if (i > 0) data[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { data, body: raw.slice(match[0].length) };
}

// Gives every h2/h3 a stable, unique id, stored on the token so the
// renderer and the TOC agree.
function assignIds(tokens: Token[]) {
  const seen = new Map<string, number>();
  const headings: Heading[] = [];
  for (const t of tokens) {
    if (t.type !== 'heading') continue;
    const h = t as Tokens.Heading & { id?: string };
    const text = plainText(h.text);
    let id = slugify(text.replace(/^\d+\.\s*/, ''));
    const n = seen.get(id) ?? 0;
    seen.set(id, n + 1);
    if (n > 0) id = `${id}-${n}`;
    h.id = id;
    if (h.depth === 2 || h.depth === 3) headings.push({ id, text, depth: h.depth });
  }
  return headings;
}

const cache = new Map<string, Doc>();

export function getDoc(slug: string): Doc | undefined {
  const cached = cache.get(slug);
  if (cached) return cached;
  const raw = files[`../content/docs/${slug}.md`];
  const index = ORDER.findIndex((item) => item.slug === slug);
  if (raw === undefined || index < 0) return undefined;
  const { data, body } = parseFrontmatter(raw);
  const tokens = lexer(body);
  const doc: Doc = {
    slug,
    title: data.title ?? ORDER[index]!.title,
    description: data.description ?? '',
    group: ORDER[index]!.group,
    tokens,
    headings: assignIds(tokens),
    prev: ORDER[index - 1],
    next: ORDER[index + 1],
  };
  cache.set(slug, doc);
  return doc;
}

export const allSlugs = () => ORDER.map((item) => item.slug);

export type SearchEntry = {
  href: Href;
  page: string;
  group: string;
  heading: string;
  text: string;
};

// One entry per page and per section, with the section's text flattened.
export function buildSearchIndex(): SearchEntry[] {
  const entries: SearchEntry[] = [];
  for (const slug of allSlugs()) {
    const doc = getDoc(slug);
    if (!doc) continue;
    const base = docHref(slug);
    let current: SearchEntry = {
      href: base,
      page: doc.title,
      group: doc.group,
      heading: '',
      text: doc.description,
    };
    entries.push(current);
    for (const t of doc.tokens) {
      if (t.type === 'heading' && (t.depth === 2 || t.depth === 3)) {
        const id = (t as Tokens.Heading & { id?: string }).id;
        current = {
          href: `${base}#${id}` as Href,
          page: doc.title,
          group: doc.group,
          heading: plainText(t.text),
          text: '',
        };
        entries.push(current);
      } else if (t.type !== 'space' && t.type !== 'heading') {
        const text = t.type === 'code' ? t.text : searchText(t.raw);
        current.text = `${current.text} ${text.replace(/\s+/g, ' ')}`.trim();
      }
    }
  }
  for (const e of entries) e.text = e.text.slice(0, 700);
  return entries;
}
