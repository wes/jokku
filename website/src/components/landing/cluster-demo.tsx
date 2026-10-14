'use client';

import { useEffect, useRef, useState } from 'react';

// `jokku top`'s overview while a server dies: it stops reporting, a minute
// later its instances start on the others, and when it comes back it stops
// the copies that moved. The events are Jokku's own wording, and their
// times are the cluster's, not the replay's.

type NodeState = 'up' | 'down';
type InstState = 'up' | 'booting' | 'lost';
type Inst = { key: string; app: string; name: string; node: number; state: InstState };
type Event = { at: number; kind: 'node' | 'instance'; who: string; msg: string };
type State = { nodes: NodeState[]; insts: Inst[]; events: Event[] };
type Frame = { wait: number; apply: (s: State) => State };

const NODES = ['server-1', 'server-2', 'server-3'];

const BASE: State = {
  nodes: ['up', 'up', 'up'],
  insts: [
    { key: 'a1', app: 'api', name: 'web.1', node: 0, state: 'up' },
    { key: 's1', app: 'shop', name: 'web.1', node: 0, state: 'up' },
    { key: 'a2', app: 'api', name: 'web.2', node: 1, state: 'up' },
    { key: 'b1', app: 'blog', name: 'web.1', node: 1, state: 'up' },
    { key: 'a3', app: 'api', name: 'web.3', node: 2, state: 'up' },
    { key: 's2', app: 'shop', name: 'web.2', node: 2, state: 'up' },
  ],
  events: [
    { at: -3600, kind: 'node', who: 'server-3', msg: 'server-3 is updated to v0.8.17' },
    { at: -86400 * 2, kind: 'node', who: 'server-3', msg: 'server-3 joined the cluster (8 CPUs, 16384 MiB)' },
  ],
};

const event = (at: number, kind: Event['kind'], who: string, msg: string) => (s: State): State => ({
  ...s,
  events: [{ at, kind, who, msg }, ...s.events].slice(0, 5),
});
const node = (i: number, state: NodeState) => (s: State): State => ({
  ...s,
  nodes: s.nodes.map((n, k) => (k === i ? state : n)),
  insts: s.insts.map((x) => (x.node === i && state === 'down' ? { ...x, state: 'lost' } : x)),
});
const move = (key: string, to: number) => (s: State): State => {
  const from = s.insts.find((x) => x.key === key)!;
  return { ...s, insts: [...s.insts, { ...from, key: `${key}-moved`, node: to, state: 'booting' }] };
};
const up = (s: State): State => ({ ...s, insts: s.insts.map((x) => (x.state === 'booting' ? { ...x, state: 'up' } : x)) });
const back = (i: number) => (s: State): State => ({
  ...s,
  nodes: s.nodes.map((n, k) => (k === i ? 'up' : n)),
  insts: s.insts.filter((x) => x.state !== 'lost'),
});
const all = (...fs: Array<(s: State) => State>) => (s: State) => fs.reduce((acc, f) => f(acc), s);

const FRAMES: Frame[] = [
  {
    wait: 2600,
    apply: all(
      node(2, 'down'),
      event(0, 'node', 'server-3', 'server-3 stopped reporting; its instances move elsewhere if it is not back within 1m0s'),
    ),
  },
  { wait: 2800, apply: all(move('a3', 0), event(60, 'instance', 'api', 'moving web.3 from server-3 to server-1: server-3 is down')) },
  { wait: 450, apply: all(move('s2', 1), event(60, 'instance', 'shop', 'moving web.2 from server-3 to server-2: server-3 is down')) },
  { wait: 1500, apply: up },
  { wait: 3400, apply: all(back(2), event(540, 'node', 'server-3', 'server-3 is back')) },
];

// For reduced motion: the moment the instances have moved.
const MOVED = FRAMES.slice(0, 4).reduce((s, f) => f.apply(s), BASE);

// Times are in the viewer's zone, so they appear once the page has loaded
// (the server's render can't know it). The last event is a few seconds ago.
const stamp = (t: number) =>
  new Date(t).toLocaleString('en-US', { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }).replace(',', '');

const Bar = ({ pct, off }: { pct: number; off?: boolean }) => {
  const full = Math.round(pct / 10);
  const color = off ? 'text-term-dim/50' : pct >= 85 ? 'text-term-bad' : pct >= 60 ? 'text-term-warn' : 'text-term-good';
  return (
    <span>
      <span className={color}>{'█'.repeat(off ? 0 : full)}</span>
      <span className="text-term-dim/50">{'░'.repeat(off ? 10 : 10 - full)}</span>
    </span>
  );
};

export const ClusterDemo = () => {
  const [state, setState] = useState<State>(BASE);
  const [base, setBase] = useState<number | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setBase(Date.now() - 550_000);
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setState(MOVED);
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    let visible = true;
    let resume: (() => void) | null = null;
    const io = new IntersectionObserver(([entry]) => {
      visible = entry?.isIntersecting ?? true;
      if (visible && resume) {
        const r = resume;
        resume = null;
        r();
      }
    });
    if (rootRef.current) io.observe(rootRef.current);
    const sleep = (ms: number) =>
      new Promise<void>((done) => {
        timer = setTimeout(() => {
          if (visible) done();
          else resume = done;
        }, ms);
      });
    const run = async () => {
      while (!cancelled) {
        setState(BASE);
        for (const f of FRAMES) {
          await sleep(f.wait);
          if (cancelled) return;
          setState(f.apply);
        }
        await sleep(5000);
      }
    };
    run();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      io.disconnect();
    };
  }, []);

  const upNodes = state.nodes.filter((n) => n === 'up').length;
  const wanted = 6;
  const healthy = state.insts.filter((x) => x.state === 'up').length;

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
      </div>

      <div className="px-4 pt-3 pb-4 sm:px-5">
        <div className="truncate">
          <span className="font-semibold text-term-accent">jokku top</span>
          <span className="text-term-dim">  ·  control server-1  ·  </span>
          <span className={upNodes === 3 ? 'text-term-good' : 'text-term-warn'}>{upNodes}/3 nodes</span>
          <span className="text-term-dim">  ·  </span>3 apps<span className="text-term-dim">  ·  </span>
          <span className={healthy === wanted ? 'text-term-good' : 'text-term-warn'}>
            {healthy}/{wanted} instances healthy
          </span>
        </div>
        <div className="mt-1 flex flex-wrap">
          {['Overview', 'Nodes', 'Apps', 'Instances', 'Events'].map((t, i) => (
            <span key={t} className={`px-2 ${i === 0 ? 'bg-term-accent font-semibold text-on-fill' : 'text-term-dim max-sm:hidden'}`}>
              {i + 1} {t}
            </span>
          ))}
        </div>

        <div className="mt-4 font-semibold text-term-accent">Nodes</div>
        <div className="space-y-1">
          {NODES.map((name, i) => {
            const down = state.nodes[i] === 'down';
            const here = state.insts.filter((x) => x.node === i);
            const load = here.filter((x) => x.state !== 'lost').length;
            return (
              <div key={name} className="grid grid-cols-[6.5rem_minmax(0,1fr)] items-center gap-3 xl:grid-cols-[6.5rem_auto_minmax(0,1fr)]">
                <span className={down ? 'text-term-dim' : ''}>
                  <span className={`transition-colors ${down ? 'text-term-bad' : 'text-term-good'}`}>●</span> {name}
                </span>
                <span className="whitespace-nowrap max-xl:hidden">
                  <span className="text-term-dim">cpu </span>
                  <Bar pct={6 + load * 7} off={down} />
                  <span className="text-term-dim">   mem </span>
                  <Bar pct={12 + load * 12} off={down} />
                </span>
                <span className="flex min-w-0 flex-wrap gap-1.5">
                  {here.map((x) => (
                    <span
                      key={x.key}
                      className={`rounded border px-1.5 leading-[1.6] whitespace-nowrap transition-colors duration-500 ${
                        x.state === 'lost'
                          ? 'border-term-line text-term-dim line-through'
                          : x.state === 'booting'
                            ? 'animate-pulse border-term-warn/70 text-term-warn'
                            : x.key.endsWith('-moved')
                              ? 'border-term-accent/60 text-term-bright'
                              : 'border-term-line text-term-fg'
                      }`}
                    >
                      {x.app} <span className={x.state === 'lost' ? '' : 'text-term-dim'}>{x.name}</span>
                    </span>
                  ))}
                  {here.length === 0 ? <span className="text-term-dim">no instances</span> : null}
                </span>
              </div>
            );
          })}
        </div>

        <div className="mt-4 font-semibold text-term-accent">Recent events</div>
        <div className="min-h-[calc(5*1.8em)]">
          {state.events.map((e) => (
            <div key={`${e.at}-${e.msg}`} className="flex gap-3 whitespace-nowrap">
              <span className="w-[15ch] shrink-0 text-term-dim max-sm:hidden">{base === null ? '' : stamp(base + e.at * 1000)}</span>
              <span className="w-[4.5rem] shrink-0 text-term-dim max-sm:hidden">{e.kind}</span>
              <span className="w-[4.5rem] shrink-0">{e.who}</span>
              <span className={`truncate ${e.at >= 0 ? 'text-term-bright' : ''}`}>{e.msg}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
};
