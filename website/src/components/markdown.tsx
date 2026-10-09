import type { ReactNode } from 'react';
import { Lexer, lexer, type Token, type Tokens } from 'marked';
import { Link } from 'waku';
import type { Href } from '../lib/nav';
import { CodeBlock } from './code-block';
import { ArrowRightIcon, InfoIcon, SparkIcon, WarnIcon } from './icons';

// Renders marked's tokens as React, so docs get real components (highlighted
// code, callouts, link cards) instead of an HTML string.

export const Markdown = ({ tokens }: { tokens: Token[] }) => <>{blocks(tokens)}</>;

function blocks(tokens: Token[], inList = false): ReactNode[] {
  return tokens.map((t, i) => block(t, i, inList));
}

function block(t: Token, key: number, inList: boolean): ReactNode {
  switch (t.type) {
    case 'heading':
      return <Heading key={key} token={t as Tokens.Heading & { id?: string }} />;
    case 'paragraph':
      return (
        <p key={key} className={inList ? 'my-2' : 'my-5'}>
          {inline(t.tokens)}
        </p>
      );
    case 'text':
      return <span key={key}>{inline(t.tokens ?? [t])}</span>;
    case 'code':
      return <Code key={key} token={t as Tokens.Code} />;
    case 'blockquote':
      return <Callout key={key} token={t as Tokens.Blockquote} />;
    case 'list':
      return <List key={key} token={t as Tokens.List} />;
    case 'table':
      return <Table key={key} token={t as Tokens.Table} />;
    case 'hr':
      return <hr key={key} className="my-12 border-line" />;
    default:
      return null;
  }
}

const Heading = ({ token }: { token: Tokens.Heading & { id?: string } }) => {
  const id = token.id;
  const anchor = (
    <a
      href={`#${id}`}
      aria-label="Link to this section"
      className="ml-2 font-normal text-subtle opacity-0 transition group-hover/h:opacity-100 hover:text-accent"
    >
      #
    </a>
  );
  if (token.depth === 2) {
    return (
      <h2
        id={id}
        className="group/h mt-14 mb-4 scroll-mt-24 text-[23px] leading-tight font-semibold tracking-[-0.02em] text-fg"
      >
        {inline(token.tokens)}
        {anchor}
      </h2>
    );
  }
  if (token.depth === 3) {
    // "### 1. Push it" renders as a numbered step.
    const step = /^(\d+)\.\s+/.exec(token.text);
    if (step) {
      const rest = Lexer.lexInline(token.text.slice(step[0].length));
      return (
        <h3
          id={id}
          className="group/h mt-12 mb-3 flex scroll-mt-24 items-center gap-3 text-[17px] font-semibold tracking-[-0.01em] text-fg"
        >
          <span className="grid size-7 shrink-0 place-items-center rounded-full border border-accent/30 bg-accent-soft font-mono text-[13px] text-accent">
            {step[1]}
          </span>
          <span>
            {inline(rest)}
            {anchor}
          </span>
        </h3>
      );
    }
    return (
      <h3
        id={id}
        className="group/h mt-10 mb-3 scroll-mt-24 text-[17px] font-semibold tracking-[-0.01em] text-fg"
      >
        {inline(token.tokens)}
        {anchor}
      </h3>
    );
  }
  return (
    <h4 id={id} className="mt-8 mb-2 text-[15px] font-semibold text-fg">
      {inline(token.tokens)}
    </h4>
  );
};

function Code({ token }: { token: Tokens.Code }) {
  const [lang = '', ...meta] = (token.lang ?? '').split(/\s+/);
  if (lang === 'cards') return <Cards source={token.text} />;
  const title = /title="([^"]+)"/.exec(meta.join(' '))?.[1];
  return <CodeBlock code={token.text} lang={lang} title={title} />;
}

const CALLOUTS = {
  NOTE: {
    icon: InfoIcon,
    label: 'Note',
    className: 'border-line bg-surface/60',
    iconClass: 'text-muted',
  },
  TIP: {
    icon: SparkIcon,
    label: 'Tip',
    className: 'border-accent/25 bg-accent-soft/60',
    iconClass: 'text-accent',
  },
  WARNING: {
    icon: WarnIcon,
    label: 'Heads up',
    className: 'border-warn/30 bg-warn/[0.06]',
    iconClass: 'text-warn',
  },
} as const;

function Callout({ token }: { token: Tokens.Blockquote }) {
  const match = /^\s*\[!(NOTE|TIP|WARNING)\]\s*/.exec(token.text);
  if (!match) {
    return (
      <blockquote className="my-6 border-l-2 border-line-strong pl-5 text-muted">
        {blocks(token.tokens)}
      </blockquote>
    );
  }
  const kind = CALLOUTS[match[1] as keyof typeof CALLOUTS];
  const Icon = kind.icon;
  return (
    <aside
      className={`my-7 flex gap-3.5 rounded-xl border px-4.5 py-4 text-[14.5px] leading-6.5 ${kind.className}`}
    >
      <Icon className={`mt-[3px] size-[18px] shrink-0 ${kind.iconClass}`} />
      <div className="min-w-0 [&>p:first-child]:mt-0 [&>p:last-child]:mb-0 [&>p]:my-2">
        {blocks(lexer(token.text.slice(match[0].length)))}
      </div>
    </aside>
  );
}

function List({ token }: { token: Tokens.List }) {
  const items = token.items.map((item, i) => (
    <li key={i} className="pl-1.5 [&>ul]:mt-2 [&>ol]:mt-2">
      {blocks(item.tokens, true)}
    </li>
  ));
  return token.ordered ? (
    <ol
      start={token.start || undefined}
      className="my-5 list-decimal space-y-2 pl-6 marker:font-mono marker:text-[13px] marker:text-subtle"
    >
      {items}
    </ol>
  ) : (
    <ul className="my-5 list-disc space-y-2 pl-6 marker:text-line-strong">{items}</ul>
  );
}

function Table({ token }: { token: Tokens.Table }) {
  return (
    <div className="scrollbar-thin my-7 overflow-x-auto rounded-xl border border-line">
      <table className="w-full border-collapse text-left text-[14px] leading-6">
        <thead className="bg-surface/70">
          <tr>
            {token.header.map((cell, i) => (
              <th key={i} className="px-4 py-2.5 font-medium whitespace-nowrap text-fg">
                {inline(cell.tokens)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {token.rows.map((row, r) => (
            <tr key={r} className="border-t border-line">
              {row.map((cell, i) => (
                <td key={i} className="px-4 py-3 align-top">
                  {inline(cell.tokens)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ```cards
// Title | /docs/page | One line about it.
// ```
function Cards({ source }: { source: string }) {
  const cards = source
    .split('\n')
    .map((line) => line.split('|').map((s) => s.trim()))
    .filter((parts) => parts.length >= 2);
  return (
    <div className="not-prose my-8 grid gap-3 sm:grid-cols-2">
      {cards.map(([title, href, description]) => (
        <Link
          key={href}
          to={href as Href}
          className="group/card relative rounded-xl border border-line bg-elevated p-5 transition hover:border-accent/50 hover:bg-accent-soft/40"
        >
          <div className="flex items-center justify-between gap-3 font-semibold text-fg">
            {title}
            <ArrowRightIcon className="size-4 text-subtle transition group-hover/card:translate-x-0.5 group-hover/card:text-accent" />
          </div>
          {description ? (
            <p className="mt-1.5 text-[14px] leading-6 text-muted">{description}</p>
          ) : null}
        </Link>
      ))}
    </div>
  );
}

// Status marks used in the command reference.
const BADGES: Record<string, { label: string; className: string }> = {
  '✅': { label: 'Works', className: 'bg-good/10 text-good ring-good/25' },
  '🔜': { label: 'Planned', className: 'bg-warn/10 text-warn ring-warn/25' },
  '➕': { label: 'Jokku only', className: 'bg-accent/10 text-accent ring-accent/25' },
  '✏️': { label: 'Replaces Dokku’s', className: 'bg-surface text-muted ring-line-strong' },
  '✖': { label: 'Not planned', className: 'bg-bad/10 text-bad ring-bad/20' },
};
const BADGE_RE = /(✅|🔜|➕|✏️|✖)/u;

function withBadges(text: string, key: number): ReactNode {
  if (!BADGE_RE.test(text)) return text;
  return text.split(BADGE_RE).map((part, i) => {
    const badge = BADGES[part];
    if (!badge) return part.trim() ? <span key={`${key}-${i}`}>{part}</span> : null;
    return (
      <span
        key={`${key}-${i}`}
        className={`mr-1.5 inline-flex items-center rounded-full px-2 py-px text-[11.5px] font-medium whitespace-nowrap ring-1 ring-inset ${badge.className}`}
      >
        {badge.label}
      </span>
    );
  });
}

const linkClass =
  'font-medium text-accent underline decoration-accent/30 underline-offset-[3px] transition hover:decoration-accent';

function inline(tokens: Token[] | undefined): ReactNode[] {
  if (!tokens) return [];
  return tokens.map((t, i) => {
    switch (t.type) {
      case 'text':
        return t.tokens ? <span key={i}>{inline(t.tokens)}</span> : withBadges(t.text, i);
      case 'escape':
        return t.text;
      case 'strong':
        return (
          <strong key={i} className="font-semibold text-fg">
            {inline(t.tokens)}
          </strong>
        );
      case 'em':
        return <em key={i}>{inline(t.tokens)}</em>;
      case 'del':
        return <del key={i}>{inline(t.tokens)}</del>;
      case 'codespan':
        return (
          <code
            key={i}
            className="rounded-md border border-line bg-surface px-[0.4em] py-[0.1em] font-mono text-[0.85em] text-fg [overflow-wrap:anywhere]"
          >
            {t.text}
          </code>
        );
      case 'br':
        return <br key={i} />;
      case 'link': {
        const href = (t as Tokens.Link).href;
        if (href.startsWith('/')) {
          return (
            <Link key={i} to={href as Href} className={linkClass}>
              {inline(t.tokens)}
            </Link>
          );
        }
        if (href.startsWith('#')) {
          return (
            <a key={i} href={href} className={linkClass}>
              {inline(t.tokens)}
            </a>
          );
        }
        return (
          <a key={i} href={href} target="_blank" rel="noreferrer" className={linkClass}>
            {inline(t.tokens)}
          </a>
        );
      }
      default:
        return 'text' in t ? t.text : null;
    }
  });
}
