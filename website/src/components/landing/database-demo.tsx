'use client';

import { useEffect, useState } from 'react';

// A recreation of `jokku top`'s Databases view (press 8): every database's
// connections, queries a second and how long they take, and the selected
// one's clients, windows and slowest queries. Click a row to look inside
// another, and w to switch between the last minute, hour and day.

type Span = {
  perSec: number;
  p?: [number, number, number]; // p50, p95, p99 in ms, where the engine keeps a histogram
  avg?: number; // the mean, where it doesn't (Postgres)
  hit?: number; // cache hits, 0-1
};

type DB = {
  name: string;
  engine: 'mysql' | 'postgres' | 'redis';
  node: string;
  conns: number;
  max: number;
  active: number;
  head: string; // what the detail's heading says after "looked"
  spans: [Span, Span, Span]; // the last minute, hour and day
  clients: Array<{ from: string; detail: string; user: string; n: number; active?: number }>;
  slowest: Array<{ ms: number; calls: number; query: string }>;
  users: Array<{ name: string; access: string }>;
};

const DBS: DB[] = [
  {
    name: 'blog-db',
    engine: 'mysql',
    node: 'server-2',
    conns: 14,
    max: 151,
    active: 1,
    head: 'at most 31 connections since it started',
    spans: [
      { perSec: 42, p: [0.31, 2.8, 41] },
      { perSec: 39, p: [0.3, 2.6, 37] },
      { perSec: 35, p: [0.29, 2.4, 33] },
    ],
    clients: [
      { from: 'blog', detail: 'web.1', user: 'blog', n: 7, active: 1 },
      { from: 'blog', detail: 'web.2', user: 'blog', n: 7 },
    ],
    slowest: [
      { ms: 4.1, calls: 18200, query: 'SELECT option_name, option_value FROM wp_options WHERE autoload IN (?, ?, ?, ?)' },
      { ms: 23, calls: 2400, query: 'SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = …' },
      { ms: 0.9, calls: 51000, query: 'SELECT meta_key, meta_value FROM wp_postmeta WHERE post_id IN (…)' },
    ],
    users: [{ name: 'blog', access: 'all' }],
  },
  {
    name: 'shop-db',
    engine: 'postgres',
    node: 'server-1',
    conns: 9,
    max: 97,
    active: 2,
    head: '',
    spans: [
      { perSec: 21, avg: 1.1, hit: 0.997 },
      { perSec: 18, avg: 1.3, hit: 0.996 },
      { perSec: 15, avg: 1.2, hit: 0.995 },
    ],
    clients: [
      { from: 'shop', detail: 'web.1', user: 'shop', n: 5, active: 2 },
      { from: 'shop', detail: 'web.2', user: 'shop', n: 3 },
      { from: 'outside', detail: 'through edge-1', user: 'reporting', n: 1 },
    ],
    slowest: [
      { ms: 6.2, calls: 9100, query: 'SELECT p.id, p.name, p.price FROM products p JOIN stock s ON s.product_id = p.id WHERE s.qty > $1 …' },
      { ms: 2.1, calls: 22000, query: 'UPDATE carts SET items = $1, updated_at = now() WHERE id = $2' },
      { ms: 3.4, calls: 1800, query: 'INSERT INTO orders (cart_id, total, created_at) VALUES ($1, $2, now()) RETURNING id' },
    ],
    users: [
      { name: 'shop', access: 'all' },
      { name: 'reporting', access: 'read-only' },
    ],
  },
  {
    name: 'cache',
    engine: 'redis',
    node: 'server-3',
    conns: 4,
    max: 10000,
    active: 0,
    head: 'counting commands · memory 38M of 256M',
    spans: [
      { perSec: 310, p: [0.02, 0.06, 0.18], hit: 0.93 },
      { perSec: 280, p: [0.02, 0.05, 0.16], hit: 0.92 },
      { perSec: 240, p: [0.02, 0.05, 0.15], hit: 0.91 },
    ],
    clients: [
      { from: 'shop', detail: 'web.1', user: 'default', n: 2 },
      { from: 'shop', detail: 'web.2', user: 'default', n: 2 },
    ],
    slowest: [],
    users: [],
  },
];

const WINDOWS = ['last minute', 'last hour', 'last day'];

// Column layouts: the list and the selected database's windows, narrower
// on a phone (the hidden cells take no column).
const LIST =
  'grid grid-cols-[minmax(0,1fr)_3.5rem_8rem] gap-3 sm:grid-cols-[8.5rem_7.5rem_3.5rem_minmax(0,9.5rem)_3.5rem] md:grid-cols-[9rem_5rem_14rem_3.5rem_4rem_13rem_3.5rem]';
const WINDOW = 'grid grid-cols-[7.5rem_3.5rem_minmax(0,1fr)] gap-3 pl-[2ch] sm:grid-cols-[9rem_4rem_minmax(0,19rem)_3.5rem]';

// The same units as the terminal: microseconds under a millisecond.
const ms = (v: number) =>
  v < 1 ? `${Math.max(Math.round(v * 1000), 1)}µs` : v < 10 ? `${v.toFixed(1)}ms` : v < 1000 ? `${v.toFixed(0)}ms` : `${(v / 1000).toFixed(1)}s`;
const rate = (v: number) => (v < 10 ? v.toFixed(1) : v.toFixed(0));
const hit = (s: Span) => (s.hit === undefined ? '-' : `${(s.hit * 100).toFixed(1)}%`);

const Bar = ({ pct }: { pct: number }) => {
  const full = Math.round(Math.min(pct, 100) / 10);
  const color = pct >= 85 ? 'text-term-bad' : pct >= 60 ? 'text-term-warn' : 'text-term-good';
  return (
    <span>
      <span className={color}>{'█'.repeat(full)}</span>
      <span className="text-term-dim/60">{'░'.repeat(10 - full)}</span>
    </span>
  );
};

const Latency = ({ s, long }: { s: Span; long?: boolean }) =>
  s.p ? (
    long ? (
      <>
        <span className="text-term-dim">p50</span> {ms(s.p[0])} <span className="text-term-dim">p95</span> {ms(s.p[1])}{' '}
        <span className="text-term-dim">p99</span> {ms(s.p[2])}
      </>
    ) : (
      <>
        {ms(s.p[0])} {ms(s.p[1])} <span className="text-term-dim">{ms(s.p[2])}</span>
      </>
    )
  ) : (
    <>
      {ms(s.avg ?? 0)}
      <span className="text-term-dim"> avg</span>
    </>
  );

// Live numbers wander a little around their figures, like a busy server's.
const wander = (s: Span, k: number): Span => ({
  ...s,
  perSec: s.perSec * k,
  ...(s.p ? { p: [s.p[0] * (1 + (k - 1) / 2), s.p[1] * k, s.p[2] * (2 - k)] } : {}),
  ...(s.avg ? { avg: s.avg * (2 - k) } : {}),
});

export const DatabaseDemo = () => {
  const [selected, setSelected] = useState(0);
  const [win, setWin] = useState(0);
  const [wobble, setWobble] = useState([1, 1, 1]);
  const [looked, setLooked] = useState(2);

  useEffect(() => {
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    const t = setInterval(() => {
      setWobble((w) => w.map(() => 0.92 + Math.random() * 0.16));
      setLooked((n) => (n >= 4 ? 1 : n + 1));
    }, 1800);
    return () => clearInterval(t);
  }, []);

  const db = DBS[selected]!;
  const live = (d: DB, i: number, span: number) => (span === 0 ? wander(d.spans[0], wobble[i]!) : d.spans[span]!);

  return (
    <div className="overflow-hidden rounded-2xl border border-term-line bg-term font-mono text-[11.5px] leading-[1.8] text-term-fg shadow-2xl shadow-black/40 sm:text-[12.5px]">
      <div className="flex h-10 items-center gap-2 border-b border-term-line bg-term-bar px-4">
        <span className="size-3 rounded-full bg-white/10" />
        <span className="size-3 rounded-full bg-white/10" />
        <span className="size-3 rounded-full bg-white/10" />
        <span className="ml-3 truncate text-[12px] text-term-dim">ssh -t jokku@your-server top</span>
      </div>

      <div className="px-4 pt-3 pb-4 sm:px-5">
        <div className="truncate">
          <span className="font-semibold text-term-accent">jokku top</span>
          <span className="text-term-dim">  ·  control server-1  ·  v0.8.20  ·  </span>
          <span className="text-term-good">3/3 nodes</span>
          <span className="text-term-dim">  ·  </span>3 apps<span className="text-term-dim">  ·  </span>3 databases
        </div>
        <div className="mt-1 flex flex-wrap">
          {['Overview', 'Nodes', 'Apps', 'Instances', 'Events', 'Traffic', 'Backups', 'Databases'].map((t, i) => (
            <span
              key={t}
              className={`px-2 ${t === 'Databases' ? 'bg-term-accent font-semibold text-on-fill' : 'text-term-dim max-md:hidden'}`}
            >
              {i + 1} {t}
            </span>
          ))}
        </div>

        {/* Every database */}
        <div className="mt-4">
          <div>
            <div className={`${LIST} font-semibold text-term-dim`}>
              <span>DATABASE</span>
              <span className="max-md:hidden">ENGINE</span>
              <span className="max-sm:hidden">CONNECTIONS</span>
              <span className="text-right max-md:hidden">ACTIVE</span>
              <span className="text-right">QUERY/S</span>
              <span>
                LATENCY<span className="max-sm:hidden"> p50 p95 p99</span>
              </span>
              <span className="text-right max-sm:hidden">HIT</span>
            </div>
            {DBS.map((d, i) => {
              const s = live(d, i, win);
              return (
                <button
                  key={d.name}
                  type="button"
                  onClick={() => setSelected(i)}
                  aria-pressed={selected === i}
                  className={`${LIST} w-full text-left transition ${
                    selected === i ? 'bg-white/[0.07]' : 'hover:bg-white/[0.04]'
                  }`}
                >
                  <span className="truncate">
                    <span className="text-term-good">●</span> {d.name}
                  </span>
                  <span className="max-md:hidden">{d.engine}</span>
                  <span className="truncate max-sm:hidden">
                    <span className="max-md:hidden">
                      <Bar pct={(d.conns / d.max) * 100} />{' '}
                    </span>
                    {d.conns} / {d.max}
                  </span>
                  <span className="text-right max-md:hidden">{d.active}</span>
                  <span className="text-right tabular-nums">{rate(s.perSec)}</span>
                  <span className="truncate tabular-nums">
                    <Latency s={s} />
                  </span>
                  <span className="text-right tabular-nums max-sm:hidden">{hit(s)}</span>
                </button>
              );
            })}
          </div>
        </div>

        {/* The selected one, inside */}
        <div className="mt-5 truncate font-semibold text-term-accent">
          {db.engine} {db.name} on {db.node}
          <span className="font-normal text-term-dim">
            {' '}
            · looked {looked}s ago{db.head ? ` · ${db.head}` : ''}
          </span>
        </div>
        <div>
          <div>
            <div className={`${WINDOW} font-semibold text-term-dim`}>
              <span />
              <span className="text-right">PER SEC</span>
              <span>LATENCY</span>
              <span className="text-right max-sm:hidden">HIT</span>
            </div>
            {WINDOWS.map((w, i) => {
              const s = live(db, selected, i);
              return (
                <div key={w} className={WINDOW}>
                  <span className={i === win ? 'text-term-bright' : ''}>{w}</span>
                  <span className="text-right tabular-nums">{rate(s.perSec)}</span>
                  <span className="truncate tabular-nums">
                    <span className="sm:hidden">
                      <Latency s={s} />
                    </span>
                    <span className="max-sm:hidden">
                      <Latency s={s} long />
                    </span>
                  </span>
                  <span className="text-right tabular-nums max-sm:hidden">{hit(s)}</span>
                </div>
              );
            })}
          </div>
        </div>

        <div className="mt-1 pl-[2ch] font-semibold text-term-dim">CONNECTED</div>
        {db.clients.map((c) => (
          <div key={c.from + c.detail} className="grid grid-cols-[minmax(0,1fr)_auto] gap-3 pl-[4ch] sm:grid-cols-[14rem_6rem_minmax(0,1fr)]">
            <span className="truncate">
              {c.from} <span className="text-term-dim">{c.detail}</span>
            </span>
            <span className="truncate text-term-dim max-sm:hidden">{c.user}</span>
            <span className="whitespace-nowrap">
              {c.n}
              {c.active ? <span className="text-term-accent"> ({c.active} active)</span> : null}
            </span>
          </div>
        ))}

        {db.slowest.length ? (
          <>
            <div className="mt-1 pl-[2ch] font-semibold text-term-dim">SLOWEST QUERIES, BY TOTAL TIME</div>
            {db.slowest.map((q) => (
              <div key={q.query} className="grid grid-cols-[4rem_minmax(0,1fr)] gap-3 pl-[2ch] whitespace-nowrap sm:grid-cols-[4.5rem_4.5rem_minmax(0,1fr)] sm:pl-[4ch]">
                <span className="text-right tabular-nums">{ms(q.ms)}</span>
                <span className="text-term-dim tabular-nums max-sm:hidden">×{q.calls}</span>
                <span className="truncate">{q.query}</span>
              </div>
            ))}
          </>
        ) : null}

        {db.users.length ? (
          <div className="mt-1 truncate pl-[2ch]">
            <span className="font-semibold text-term-dim">USERS</span>{'  '}
            {db.users.map((u, i) => (
              <span key={u.name}>
                {i > 0 ? <span className="text-term-dim"> · </span> : null}
                {u.name} <span className="text-term-dim">{u.access}</span>
              </span>
            ))}
          </div>
        ) : null}

        <div className="mt-4 flex flex-wrap items-center gap-x-4 gap-y-1 text-term-dim">
          <button
            type="button"
            onClick={() => setWin((w) => (w + 1) % 3)}
            className="rounded border border-term-line px-1.5 text-term-fg transition hover:border-term-accent/50"
          >
            w
          </button>
          <span>{WINDOWS[win]} · minute/hour/day</span>
          <span className="max-sm:hidden">click a database to look inside</span>
          <span className="ml-auto whitespace-pre max-md:hidden">1-9 views  ? help  q quit</span>
        </div>
      </div>
    </div>
  );
};
