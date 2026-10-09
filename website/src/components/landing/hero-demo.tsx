'use client';

import { useEffect, useRef, useState } from 'react';
import { TerminalLine } from '../terminal-text';

// A deploy and a scale-up, replayed with Jokku's real output, while the
// cluster panel boots the microVMs the log talks about.

type InstState = 'booting' | 'up' | 'retiring';
type Inst = { key: string; app: string; name: string; node: number; state: InstState };
type Frame = {
  wait: number;
  type?: string;
  line?: string;
  apply?: (insts: Inst[]) => Inst[];
};

const NODES = ['server-1', 'server-2', 'server-3'];

const BASE: Inst[] = [
  { key: 'api1', app: 'api', name: 'web.1', node: 0, state: 'up' },
  { key: 'api2', app: 'api', name: 'web.2', node: 1, state: 'up' },
  { key: 'cache', app: 'cache', name: 'web.1', node: 1, state: 'up' },
  { key: 'shop', app: 'shop', name: 'web.1', node: 2, state: 'up' },
];

const add = (...items: Array<[string, string, number]>) => (s: Inst[]) => [
  ...s,
  ...items.map(([key, name, node]) => ({ key, app: 'myapp', name, node, state: 'booting' as const })),
];
const set = (key: string, state: InstState) => (s: Inst[]) =>
  s.map((i) => (i.key === key ? { ...i, state } : i));
const drop = (key: string) => (s: Inst[]) => s.filter((i) => i.key !== key);

const R = 'remote: ';
const URL = 'http://myapp.203.0.113.10.sslip.io';

const FRAMES: Frame[] = [
  { wait: 700, type: 'git push jokku main' },
  { wait: 260, line: 'Enumerating objects: 42, done.' },
  { wait: 160, line: 'Writing objects: 100% (42/42), 18.40 KiB | 9.20 MiB/s, done.' },
  { wait: 380, line: `${R}-----> Received git source for myapp (18.4 KiB)` },
  { wait: 420, line: `${R}-----> Building myapp from Dockerfile` },
  { wait: 300, line: `${R}       #5 [2/4] COPY package*.json ./` },
  { wait: 750, line: `${R}       #6 [3/4] RUN npm ci --omit=dev` },
  { wait: 420, line: `${R}       #7 [4/4] COPY . .` },
  { wait: 560, line: `${R}-----> Creating the microVM root filesystem` },
  { wait: 850, line: `${R}-----> $PORT is 3000, from EXPOSE 3000/tcp` },
  { wait: 360, line: `${R}-----> Starting web.1 on server-1 (1 vCPU, 256 MiB)`, apply: add(['a1', 'web.1', 0]) },
  { wait: 1250, line: `${R}       web.1 is up`, apply: set('a1', 'up') },
  { wait: 280, line: `${R}=====> Application deployed:` },
  { wait: 110, line: `${R}       ${URL}` },
  { wait: 220, line: 'To your-server:myapp' },
  { wait: 90, line: ' * [new branch]      main -> main' },
  { wait: 1900, type: 'jokku ps:scale myapp web=3' },
  { wait: 420, line: '-----> Applying changes to myapp (v1)' },
  {
    wait: 340,
    line: '-----> Starting web.1 on server-1 (1 vCPU, 256 MiB), web.2 on server-2 (1 vCPU, 256 MiB), web.3 on server-3 (1 vCPU, 256 MiB)',
    apply: add(['b1', 'web.1', 0], ['b2', 'web.2', 1], ['b3', 'web.3', 2]),
  },
  { wait: 1000, line: '       web.2 is up', apply: set('b2', 'up') },
  { wait: 260, line: '       web.1 is up', apply: set('b1', 'up') },
  { wait: 300, line: '       web.3 is up', apply: set('b3', 'up') },
  { wait: 340, line: '-----> Old instances will shut down in 60 seconds', apply: set('a1', 'retiring') },
  { wait: 260, line: '=====> Application deployed:' },
  { wait: 110, line: `       ${URL}` },
  { wait: 1800, apply: drop('a1') },
];

// What the end of the script looks like, for reduced motion.
const FINAL_LINES = FRAMES.flatMap((f) => (f.type ? [`$ ${f.type}`] : f.line !== undefined ? [f.line] : []));
const FINAL_INSTS = FRAMES.reduce((s, f) => (f.apply ? f.apply(s) : s), BASE);

const MAX_LINES = 40;

export const HeroDemo = () => {
  const [lines, setLines] = useState<string[]>([]);
  const [typing, setTyping] = useState<string | null>(null);
  const [insts, setInsts] = useState<Inst[]>(BASE);
  const [load, setLoad] = useState([0.18, 0.11, 0.14]);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setLines(FINAL_LINES);
      setInsts(FINAL_INSTS);
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    let visible = true;
    let resume: (() => void) | null = null;

    // Holds the script while the demo is scrolled out of view.
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
        setLines([]);
        setInsts(BASE);
        for (const f of FRAMES) {
          await sleep(f.wait);
          if (cancelled) return;
          if (f.type) {
            for (let i = 1; i <= f.type.length; i++) {
              setTyping(f.type.slice(0, i));
              await sleep(28 + Math.random() * 45);
              if (cancelled) return;
            }
            await sleep(260);
            setTyping(null);
            setLines((l) => [...l, `$ ${f.type}`].slice(-MAX_LINES));
          }
          if (f.line !== undefined) setLines((l) => [...l, f.line!].slice(-MAX_LINES));
          if (f.apply) setInsts(f.apply);
        }
        await sleep(4200);
      }
    };
    run();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      io.disconnect();
    };
  }, []);

  // Node CPU wanders a little, and rises with the instances it runs.
  useEffect(() => {
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    const t = setInterval(() => {
      setLoad((l) => l.map((v) => Math.min(0.9, Math.max(0.06, v + (Math.random() - 0.5) * 0.08))));
    }, 1400);
    return () => clearInterval(t);
  }, []);

  const healthy = insts.filter((i) => i.state === 'up').length;
  const total = insts.filter((i) => i.state !== 'retiring').length;
  const apps = new Set(insts.map((i) => i.app)).size;

  return (
    <div ref={rootRef} className="relative grid gap-3 lg:grid-cols-[minmax(0,1fr)_300px]">
      {/* Terminal */}
      <div className="overflow-hidden rounded-2xl border border-term-line bg-term shadow-2xl shadow-black/40">
        <div className="flex h-10 items-center gap-2 border-b border-term-line bg-term-bar px-4">
          <span className="size-3 rounded-full bg-[#ff5f57]/85" />
          <span className="size-3 rounded-full bg-[#febc2e]/85" />
          <span className="size-3 rounded-full bg-[#28c840]/85" />
          <span className="ml-3 font-mono text-[12px] text-term-dim">~/code/myapp</span>
        </div>
        <div className="relative h-[360px] overflow-hidden px-4 py-3 font-mono text-[11px] leading-[1.75] text-term-fg sm:h-[400px] sm:px-5 sm:text-[12.5px]">
          <div className="pointer-events-none absolute inset-x-0 top-0 z-10 h-10 bg-gradient-to-b from-term to-transparent" />
          <div className="flex h-full flex-col justify-end">
            {lines.map((line, i) => (
              <div key={`${i}-${line}`} className="-indent-[7ch] pl-[7ch] break-words whitespace-pre-wrap">
                <TerminalLine line={line} />
              </div>
            ))}
            <div className="-indent-[7ch] pl-[7ch] whitespace-pre-wrap">
              <span className="text-term-dim">$ </span>
              <span className="text-term-bright">{typing ?? ''}</span>
              <span className="ml-px inline-block h-[1.15em] w-[0.6em] translate-y-[0.2em] animate-blink bg-term-accent/90" />
            </div>
          </div>
        </div>
      </div>

      {/* Cluster */}
      <div className="hidden overflow-hidden rounded-2xl border border-term-line bg-term font-mono shadow-2xl shadow-black/40 sm:block">
        <div className="flex h-10 items-center justify-between border-b border-term-line bg-term-bar px-4 text-[12px]">
          <span className="font-semibold text-term-accent">jokku top</span>
          <span className="text-term-dim">
            <span className="text-term-good">3/3</span> nodes · {apps} apps
          </span>
        </div>
        <div className="grid gap-2.5 p-3 sm:grid-cols-3 lg:grid-cols-1">
          {NODES.map((node, n) => {
            const here = insts.filter((i) => i.node === n);
            const mem = here.filter((i) => i.state !== 'retiring').length * 0.11 + 0.1;
            return (
              <div key={node} className="rounded-xl border border-term-line bg-white/[0.02] p-3">
                <div className="flex items-center justify-between text-[11.5px]">
                  <span className="flex items-center gap-2 text-term-fg">
                    <span className="size-1.5 rounded-full bg-term-good shadow-[0_0_8px] shadow-term-good/60" />
                    {node}
                  </span>
                  <span className="text-term-dim">{n === 0 ? 'control' : 'worker'}</span>
                </div>
                <div className="mt-2.5 grid grid-cols-2 gap-3 text-[10px] text-term-dim">
                  <Meter label="cpu" value={load[n]! + here.filter((i) => i.state === 'booting').length * 0.18} />
                  <Meter label="mem" value={mem} />
                </div>
                <div className="mt-2.5 flex min-h-[26px] flex-wrap gap-1.5">
                  {here.map((i) => (
                    <Chip key={i.key} inst={i} />
                  ))}
                </div>
              </div>
            );
          })}
        </div>
        <div className="border-t border-term-line px-4 py-2.5 text-[11px] text-term-dim">
          <span className={healthy === total ? 'text-term-good' : 'text-term-warn'}>
            {healthy}/{total} instances healthy
          </span>
        </div>
      </div>
    </div>
  );
};

const Meter = ({ label, value }: { label: string; value: number }) => (
  <div className="flex items-center gap-1.5">
    <span className="w-6">{label}</span>
    <span className="h-1 flex-1 overflow-hidden rounded-full bg-white/8">
      <span
        className="block h-full rounded-full bg-term-accent/70 transition-[width] duration-700"
        style={{ width: `${Math.min(100, Math.round(value * 100))}%` }}
      />
    </span>
  </div>
);

const Chip = ({ inst }: { inst: Inst }) => {
  const mine = inst.app === 'myapp';
  const base = 'inline-flex items-center gap-1.5 rounded-md border px-1.5 py-[3px] text-[10.5px] leading-none transition-all duration-500';
  if (!mine) {
    return (
      <span className={`${base} border-white/8 bg-white/[0.03] text-term-dim`}>
        <span className="size-1 rounded-full bg-term-good/70" />
        {inst.app} {inst.name}
      </span>
    );
  }
  if (inst.state === 'booting') {
    return (
      <span className={`${base} animate-pulse border-dashed border-term-accent/70 text-term-accent`}>
        <span className="size-1 rounded-full bg-term-warn" />
        myapp {inst.name}
      </span>
    );
  }
  if (inst.state === 'retiring') {
    return (
      <span className={`${base} border-white/10 text-term-dim line-through opacity-50`}>
        <span className="size-1 rounded-full bg-term-dim" />
        myapp {inst.name}
      </span>
    );
  }
  return (
    <span className={`${base} border-term-accent/50 bg-term-accent/15 text-[#ffd9ad] shadow-[0_0_14px] shadow-term-accent/25`}>
      <span className="size-1 rounded-full bg-term-good" />
      myapp {inst.name}
    </span>
  );
};
