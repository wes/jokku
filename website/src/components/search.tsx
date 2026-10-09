'use client';

import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from 'react';
import { useRouter } from 'waku';
import type { SearchEntry } from '../lib/docs';
import { DocIcon, HashIcon, SearchIcon } from './icons';

let indexPromise: Promise<SearchEntry[]> | undefined;
const loadIndex = () =>
  (indexPromise ??= fetch('/search.json')
    .then((r) => r.json() as Promise<SearchEntry[]>)
    .catch(() => {
      indexPromise = undefined;
      return [];
    }));

function score(entry: SearchEntry, terms: string[]) {
  const heading = entry.heading.toLowerCase();
  const page = entry.page.toLowerCase();
  const text = entry.text.toLowerCase();
  let total = 0;
  for (const term of terms) {
    if (heading.includes(term)) total += heading.startsWith(term) ? 12 : 8;
    else if (page.includes(term)) total += entry.heading ? 3 : 10;
    else if (text.includes(term)) total += 1;
    else return 0;
  }
  return total;
}

function snippet(text: string, terms: string[]) {
  const lower = text.toLowerCase();
  const at = Math.max(0, ...terms.map((t) => lower.indexOf(t)));
  const start = Math.max(0, at - 40);
  const out = (start > 0 ? '…' : '') + text.slice(start, start + 140);
  return out;
}

function highlightTerms(text: string, terms: string[]): ReactNode {
  if (!terms.length) return text;
  const escaped = terms.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
  const parts = text.split(new RegExp(`(${escaped.join('|')})`, 'gi'));
  return parts.map((p, i) =>
    i % 2 === 1 ? (
      <mark key={i} className="rounded-sm bg-accent/20 px-px text-fg">
        {p}
      </mark>
    ) : (
      p
    ),
  );
}

export const Search = () => {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [entries, setEntries] = useState<SearchEntry[]>([]);
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const router = useRouter();

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing =
        e.target instanceof HTMLElement &&
        (e.target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName));
      if ((e.key === 'k' && (e.metaKey || e.ctrlKey)) || (e.key === '/' && !typing)) {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  useEffect(() => {
    if (!open) return;
    loadIndex().then(setEntries);
    inputRef.current?.focus();
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prev;
    };
  }, [open]);

  const terms = useMemo(
    () => query.toLowerCase().split(/\s+/).filter(Boolean),
    [query],
  );

  const results = useMemo(() => {
    if (!terms.length) return entries.filter((e) => !e.heading).slice(0, 8);
    return entries
      .map((e) => ({ e, s: score(e, terms) }))
      .filter((r) => r.s > 0)
      .sort((a, b) => b.s - a.s)
      .slice(0, 10)
      .map((r) => r.e);
  }, [entries, terms]);

  useEffect(() => setActive(0), [query]);

  useEffect(() => {
    listRef.current
      ?.querySelector(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: 'nearest' });
  }, [active]);

  const close = () => {
    setOpen(false);
    setQuery('');
  };

  const go = (entry: SearchEntry | undefined) => {
    if (!entry) return;
    close();
    router.push(entry.href);
  };

  const onKeyDown = (e: ReactKeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, results.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      go(results[active]);
    } else if (e.key === 'Escape') {
      close();
    }
  };

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="group flex h-9 items-center gap-2 rounded-lg border border-line bg-surface/60 pr-1.5 pl-2.5 text-[13.5px] text-subtle transition hover:border-line-strong hover:text-muted max-lg:w-9 max-lg:justify-center max-lg:px-0 lg:w-44 xl:w-52"
        aria-label="Search docs"
      >
        <SearchIcon className="size-4 shrink-0" />
        <span className="max-lg:hidden">Search docs…</span>
        <kbd className="ml-auto rounded border border-line bg-bg px-1.5 py-px font-mono text-[11px] text-subtle max-lg:hidden">
          ⌘K
        </kbd>
      </button>

      {open ? (
        <div className="fixed inset-0 z-[100] flex items-start justify-center px-4 pt-[12vh]" role="dialog" aria-modal="true" aria-label="Search docs">
          <div className="absolute inset-0 bg-bg/70 backdrop-blur-sm" onClick={close} />
          <div className="relative w-full max-w-xl overflow-hidden rounded-2xl border border-line-strong bg-elevated shadow-2xl shadow-black/30 animate-rise [animation-duration:.25s]">
            <div className="flex items-center gap-3 border-b border-line px-4">
              <SearchIcon className="size-[18px] shrink-0 text-subtle" />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={onKeyDown}
                placeholder="Search the docs: ps:scale, volumes, HTTPS…"
                className="h-14 w-full bg-transparent text-[15px] text-fg outline-none placeholder:text-subtle"
                aria-controls="search-results"
                aria-activedescendant={results[active] ? `search-${active}` : undefined}
              />
              <kbd className="rounded border border-line px-1.5 py-px font-mono text-[11px] text-subtle">esc</kbd>
            </div>
            <ul id="search-results" ref={listRef} role="listbox" className="scrollbar-thin max-h-[55vh] overflow-y-auto p-2">
              {!terms.length ? (
                <li className="px-3 pt-2 pb-1 font-mono text-[11px] tracking-wider text-subtle uppercase">Pages</li>
              ) : null}
              {results.map((r, i) => (
                <li
                  key={r.href}
                  id={`search-${i}`}
                  data-index={i}
                  role="option"
                  aria-selected={i === active}
                  onMouseMove={() => setActive(i)}
                  onClick={() => go(r)}
                  className={`flex cursor-pointer gap-3 rounded-xl px-3 py-2.5 ${i === active ? 'bg-accent-soft' : ''}`}
                >
                  <span className={`mt-0.5 grid size-7 shrink-0 place-items-center rounded-lg border ${i === active ? 'border-accent/30 text-accent' : 'border-line text-subtle'}`}>
                    {r.heading ? <HashIcon className="size-3.5" /> : <DocIcon className="size-3.5" />}
                  </span>
                  <span className="min-w-0">
                    <span className="block truncate text-[14px] font-medium text-fg">
                      {highlightTerms(r.heading || r.page, terms)}
                    </span>
                    <span className="block truncate text-[12.5px] text-subtle">
                      {r.heading ? `${r.group} › ${r.page}` : r.group}
                      {terms.length && r.text ? (
                        <>
                          {' · '}
                          {highlightTerms(snippet(r.text, terms), terms)}
                        </>
                      ) : null}
                    </span>
                  </span>
                </li>
              ))}
              {terms.length && !results.length ? (
                <li className="px-3 py-10 text-center text-[14px] text-muted">
                  Nothing found for “{query}”.
                </li>
              ) : null}
            </ul>
            <div className="flex items-center gap-4 border-t border-line px-4 py-2.5 font-mono text-[11px] text-subtle">
              <span>↑↓ to move</span>
              <span>↵ to open</span>
              <span className="ml-auto">/ or ⌘K anywhere</span>
            </div>
          </div>
        </div>
      ) : null}
    </>
  );
};
