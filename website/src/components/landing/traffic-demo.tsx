'use client';

import { useEffect, useRef, useState } from 'react';

// A recreation of `jokku top`'s Traffic view (press 6), fed by simulated
// requests: each one flies across its app's lane and lands on an instance.

type App = {
  name: string;
  rate: number; // requests per second
  ms: number; // typical latency
  e4: number; // share of 4xx
  e5: number; // share of 5xx
  targets: string[];
  paths: string[];
};

const APPS: App[] = [
  { name: 'api', rate: 19, ms: 4, e4: 0.03, e5: 0.002, targets: ['web.1@server-1', 'web.2@server-2'], paths: ['GET /v1/orders', 'POST /v1/orders', 'GET /v1/users/42', 'GET /v1/cart'] },
  { name: 'myapp', rate: 10, ms: 9, e4: 0.02, e5: 0.001, targets: ['web.1@server-1', 'web.2@server-2', 'web.3@server-3'], paths: ['GET /', 'GET /dashboard', 'POST /login', 'GET /assets/app.js'] },
  { name: 'shop', rate: 6, ms: 14, e4: 0.04, e5: 0.012, targets: ['web.1@server-3'], paths: ['GET /products', 'GET /cart', 'POST /checkout', 'GET /products/77'] },
  { name: 'docs', rate: 0.7, ms: 2, e4: 0.05, e5: 0, targets: ['web.1@server-2'], paths: ['GET /docs/install', 'GET /docs'] },
];
const NODES = ['server-1', 'server-2', 'server-3'];
const WINDOW_MS = 5000;
const TRAVEL_MS = 1500;

type Req = {
  at: number; // on the simulation's clock, which stops while nobody watches
  wall: number; // when it happened, for display
  app: string;
  method: string;
  path: string;
  status: number;
  ms: number;
  target: string;
  via: string;
};

type Stats = {
  rate: number;
  p50: number;
  p95: number;
  errors: number;
  total: number;
  byApp: Record<string, number>;
  byTarget: Record<string, Record<string, number>>;
  byNode: Record<string, number>;
  recent: Req[];
};

// What the server renders before the simulation starts.
const INITIAL: Stats = {
  rate: 35.7,
  p50: 5.1,
  p95: 24,
  errors: 0.4,
  total: 18204,
  byApp: { api: 19.2, myapp: 9.8, shop: 6.0, docs: 0.6 },
  byTarget: {
    api: { 'web.1@server-1': 9.8, 'web.2@server-2': 9.4 },
    myapp: { 'web.1@server-1': 3.4, 'web.2@server-2': 3.2, 'web.3@server-3': 3.2 },
    shop: { 'web.1@server-3': 6.0 },
    docs: { 'web.1@server-2': 0.6 },
  },
  byNode: { 'server-1': 12.1, 'server-2': 11.6, 'server-3': 12.0 },
  recent: [],
};

const pick = <T,>(xs: T[]) => xs[Math.floor(Math.random() * xs.length)]!;

function makeRequest(app: App, at: number, wall: number): Req {
  const r = Math.random();
  const status = r < app.e5 ? 502 : r < app.e5 + app.e4 ? pick([404, 401, 422]) : pick([200, 200, 200, 200, 201, 304]);
  const [method = 'GET', path = '/'] = pick(app.paths).split(' ');
  // A long tail, like real latencies.
  const ms = app.ms * (0.5 + Math.random()) * (Math.random() < 0.06 ? 4 + Math.random() * 6 : 1);
  return {
    at,
    wall,
    app: app.name,
    method,
    path,
    status,
    ms,
    target: pick(app.targets),
    via: pick(NODES),
  };
}

function summarize(window: Req[], total: number, recent: Req[]): Stats {
  const secs = WINDOW_MS / 1000;
  const lat = window.map((r) => r.ms).sort((a, b) => a - b);
  const pct = (p: number) => (lat.length ? lat[Math.min(lat.length - 1, Math.floor((p / 100) * lat.length))]! : 0);
  const byApp: Record<string, number> = {};
  const byTarget: Record<string, Record<string, number>> = {};
  const byNode: Record<string, number> = {};
  let errs = 0;
  for (const r of window) {
    if (r.status >= 500) errs++;
    byApp[r.app] = (byApp[r.app] ?? 0) + 1 / secs;
    (byTarget[r.app] ??= {})[r.target] = (byTarget[r.app]![r.target] ?? 0) + 1 / secs;
    byNode[r.via] = (byNode[r.via] ?? 0) + 1 / secs;
  }
  return {
    rate: window.length / secs,
    p50: pct(50),
    p95: pct(95),
    errors: window.length ? (errs / window.length) * 100 : 0,
    total,
    byApp,
    byTarget,
    byNode,
    recent,
  };
}

const fmtMS = (ms: number) => (ms === 0 ? '-' : ms < 10 ? `${ms.toFixed(1)}ms` : ms < 1000 ? `${ms.toFixed(0)}ms` : `${(ms / 1000).toFixed(1)}s`);
const clock = (t: number) => new Date(t).toTimeString().slice(0, 8);

const statusClass = (s: number) => (s >= 500 ? 'text-term-bad' : s >= 400 ? 'text-term-warn' : s >= 300 ? 'text-term-cyan' : 'text-term-good');

export const TrafficDemo = () => {
  const [stats, setStats] = useState<Stats>(INITIAL);
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(false);
  const lanes = useRef<Record<string, HTMLDivElement | null>>({});
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  useEffect(() => {
    const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;
    let window_: Req[] = [];
    let recent: Req[] = [];
    let total = 18204;

    // Seed five seconds of traffic so the numbers are real from the start.
    const wall = Date.now() - WINDOW_MS;
    for (let t = 0; t < WINDOW_MS; t += 50) {
      for (const app of APPS) {
        if (Math.random() < app.rate * 0.05) {
          const r = makeRequest(app, t, wall + t);
          window_.push(r);
          recent = [r, ...recent].slice(0, 6);
          total++;
        }
      }
    }
    setStats(summarize(window_, total, recent));
    if (reduced) return;

    let visible = true;
    const io = new IntersectionObserver(([e]) => (visible = e?.isIntersecting ?? true));
    if (rootRef.current) io.observe(rootRef.current);

    // The clock only runs while the demo is on screen and not paused, so the
    // numbers pick up where they left off instead of draining to zero.
    let sim = WINDOW_MS;
    let last = performance.now();
    const running = () => visible && !pausedRef.current && !document.hidden;
    const advance = () => {
      const t = performance.now();
      const dt = running() ? t - last : 0;
      sim += dt;
      last = t;
      return dt;
    };

    const fly = (r: Req) => {
      const lane = lanes.current[r.app];
      if (!lane) return;
      const dot = document.createElement('span');
      dot.textContent = r.status >= 500 ? '✖' : '●';
      dot.className = `absolute left-0 top-1/2 text-[11px] leading-none ${statusClass(r.status === 304 ? 200 : r.status)}`;
      lane.appendChild(dot);
      const width = lane.clientWidth - 10;
      const anim = dot.animate(
        [
          { transform: 'translate(0, -50%)', opacity: 0 },
          { opacity: 1, offset: 0.06 },
          { opacity: 1, offset: 0.92 },
          { transform: `translate(${width}px, -50%)`, opacity: 0.2 },
        ],
        { duration: TRAVEL_MS * (0.9 + Math.random() * 0.2), easing: 'linear' },
      );
      anim.onfinish = () => dot.remove();
    };

    // Arrivals follow the time that actually passed, so a throttled timer
    // doesn't thin the traffic out.
    const spawn = setInterval(() => {
      const dt = advance();
      if (!dt) return;
      for (const app of APPS) {
        const expected = (app.rate * Math.min(dt, 1000)) / 1000;
        const n = Math.floor(expected) + (Math.random() < expected % 1 ? 1 : 0);
        for (let k = 0; k < n; k++) {
          const r = makeRequest(app, sim - Math.random() * dt, Date.now());
          window_.push(r);
          recent = [r, ...recent].slice(0, 6);
          total++;
          fly(r);
        }
      }
    }, 50);

    const tick = setInterval(() => {
      if (!running()) return;
      const cut = sim - WINDOW_MS;
      window_ = window_.filter((r) => r.at >= cut);
      setStats(summarize(window_, total, recent));
    }, 600);

    return () => {
      clearInterval(spawn);
      clearInterval(tick);
      io.disconnect();
    };
  }, []);

  const appCount = APPS.length;
  const instances = APPS.reduce((n, a) => n + a.targets.length, 0);

  return (
    <div
      ref={rootRef}
      className="overflow-hidden rounded-2xl border border-term-line bg-term font-mono text-[11.5px] leading-[1.8] text-term-fg shadow-2xl shadow-black/40 sm:text-[12.5px]"
    >
      <div className="flex h-10 items-center gap-2 border-b border-term-line bg-term-bar px-4">
        <span className="size-3 rounded-full bg-white/10" />
        <span className="size-3 rounded-full bg-white/10" />
        <span className="size-3 rounded-full bg-white/10" />
        <span className="ml-3 truncate text-[12px] text-term-dim">ssh -t jokku@your-server top</span>
        <button
          type="button"
          onClick={() => setPaused((p) => !p)}
          className="ml-auto shrink-0 rounded-md border border-term-line px-2 py-0.5 text-[11px] whitespace-nowrap text-term-dim transition hover:border-term-accent/50 hover:text-term-fg"
        >
          {paused ? '▶ resume' : '❚❚ pause'}
        </button>
      </div>

      <div className="px-4 pt-3 pb-4 sm:px-5">
        <div className="truncate">
          <span className="font-semibold text-term-accent">jokku top</span>
          <span className="text-term-dim">  ·  control server-1  ·  v0.5.0  ·  </span>
          <span className="text-term-good">3/3 nodes</span>
          <span className="text-term-dim">  ·  </span>
          {appCount} apps
          <span className="text-term-dim">  ·  </span>
          <span className="text-term-good">
            {instances}/{instances} instances healthy
          </span>
        </div>
        <div className="mt-1 flex flex-wrap">
          {['Overview', 'Nodes', 'Apps', 'Instances', 'Events', 'Traffic'].map((t, i) => (
            <span
              key={t}
              className={`px-2 ${t === 'Traffic' ? 'bg-term-accent font-semibold text-on-fill' : 'text-term-dim max-sm:hidden'}`}
            >
              {i + 1} {t}
            </span>
          ))}
        </div>

        <div className="mt-4 flex flex-wrap gap-x-3">
          <span className="font-semibold text-term-accent">{stats.rate.toFixed(1)} req/s</span>
          <span className="text-term-dim">·</span>
          <span>
            p50 {fmtMS(stats.p50)} <span className="text-term-dim">p95</span> {fmtMS(stats.p95)}
          </span>
          <span className="text-term-dim">·</span>
          <span className={stats.errors > 0 ? 'text-term-bad' : 'text-term-good'}>
            {stats.errors.toFixed(1)}% errors
          </span>
          <span className="text-term-dim max-sm:hidden">·</span>
          <span className="max-sm:hidden">{stats.total.toLocaleString('en-US')} total</span>
          {paused ? <span className="text-term-warn">· paused (p)</span> : null}
        </div>

        <div className="mt-4 space-y-1.5">
          {APPS.map((app) => {
            const targets = Object.entries(stats.byTarget[app.name] ?? {}).sort((a, b) => b[1] - a[1]);
            const most = targets[0]?.[1] ?? 1;
            return (
              <div key={app.name} className="grid grid-cols-[4.5rem_3.8rem_minmax(0,1fr)] items-center gap-2 md:grid-cols-[5rem_4rem_minmax(0,1fr)_minmax(0,17rem)] xl:grid-cols-[5rem_4rem_minmax(0,1fr)_minmax(0,31rem)]">
                <span className="truncate">{app.name}</span>
                <span className="text-right tabular-nums">{(stats.byApp[app.name] ?? 0).toFixed(1)}/s</span>
                <div
                  ref={(el) => {
                    lanes.current[app.name] = el;
                  }}
                  className="relative h-5 overflow-hidden"
                >
                  <span className="absolute inset-x-0 top-1/2 border-t border-dashed border-white/12" />
                </div>
                <span className="hidden truncate md:block">
                  <span className="text-term-dim">▶ </span>
                  {targets.length ? (
                    targets.slice(0, 2).map(([t, n], i) => (
                      <span key={t} className={`mr-3 ${i > 0 ? 'max-xl:hidden' : ''}`}>
                        {t} <span className="text-term-good">{'▮'.repeat(Math.max(1, Math.round((6 * n) / most)))}</span>{' '}
                        <span className="tabular-nums">{n.toFixed(1)}/s</span>
                      </span>
                    ))
                  ) : (
                    <span className="text-term-dim">idle</span>
                  )}
                  {targets.length > 1 ? (
                    <span className="text-term-dim xl:hidden">+{targets.length - 1} more</span>
                  ) : null}
                  {targets.length > 2 ? (
                    <span className="text-term-dim max-xl:hidden">+{targets.length - 2} more</span>
                  ) : null}
                </span>
              </div>
            );
          })}
        </div>

        <div className="mt-4 truncate">
          <span className="font-semibold text-term-dim">requests by node  </span>
          {NODES.map((n, i) => (
            <span key={n}>
              {i > 0 ? <span className="text-term-dim">  ·  </span> : null}
              {n} <span className="text-term-good tabular-nums">{(stats.byNode[n] ?? 0).toFixed(1)}/s</span>
            </span>
          ))}
        </div>

        <div className="mt-4 font-semibold text-term-accent">Recent requests</div>
        <div className="mt-1 min-h-[calc(6*1.8em)]">
          {stats.recent.map((r, i) => (
            <div key={`${r.at}-${i}`} className="grid grid-cols-[3.6rem_2.8rem_minmax(0,1fr)_2.2rem_3.6rem] gap-2 whitespace-nowrap sm:grid-cols-[4.6rem_3.6rem_2.8rem_minmax(0,1fr)_2.2rem_3.6rem] md:grid-cols-[4.6rem_4rem_3.4rem_minmax(0,14rem)_2.4rem_4rem_9.5rem_minmax(0,1fr)]">
              <span className="text-term-dim max-sm:hidden">{clock(r.wall)}</span>
              <span className="truncate">{r.app}</span>
              <span>{r.method}</span>
              <span className="truncate">{r.path}</span>
              <span className={statusClass(r.status)}>{r.status}</span>
              <span className="text-right tabular-nums">{fmtMS(r.ms)}</span>
              <span className="hidden truncate md:block">{r.target}</span>
              <span className="hidden truncate text-term-dim md:block">via {r.via}</span>
            </div>
          ))}
        </div>

        <div className="mt-4 text-term-dim max-sm:hidden">1-6/tab views  p pause  c clear  ? help  q quit</div>
      </div>
    </div>
  );
};
