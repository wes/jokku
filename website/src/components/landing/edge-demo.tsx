'use client';

import { useEffect, useRef, useState } from 'react';

// An edge in front of a home lab: visitors' requests reach the edge over
// HTTPS and go on through the WireGuard tunnel the home servers dialed; a
// scanner's are refused at the edge and never use the home connection.

const VISITORS = [
  { name: 'visitor', where: 'phone · Denver' },
  { name: 'visitor', where: 'laptop · Berlin' },
  { name: 'scanner', where: '45.148.10.82', bad: true },
];

const SERVERS = [
  { name: 'server-1', apps: ['shop', 'blog'] },
  { name: 'server-2', apps: ['api', 'wiki'] },
];
const EXTERNAL = { name: 'ha', target: '192.168.1.50:8123' };
const TARGETS = [...SERVERS.flatMap((s) => s.apps), EXTERNAL.name];

// A lane is drawn across between the boxes, or down between them on a phone.
const across = (lane: HTMLElement) => lane.clientWidth >= lane.clientHeight;

export const EdgeDemo = () => {
  const [banned, setBanned] = useState(3);
  const [served, setServed] = useState(1284);
  const rootRef = useRef<HTMLDivElement>(null);
  const laneA = useRef<HTMLDivElement>(null);
  const laneB = useRef<HTMLDivElement>(null);
  const firewall = useRef<HTMLDivElement>(null);
  const chips = useRef<Record<string, HTMLElement | null>>({});

  useEffect(() => {
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    let visible = true;
    const io = new IntersectionObserver(([e]) => (visible = e?.isIntersecting ?? true));
    if (rootRef.current) io.observe(rootRef.current);

    const flash = (el: HTMLElement | null | undefined, cls: string[]) => {
      if (!el) return;
      el.classList.add(...cls);
      setTimeout(() => el.classList.remove(...cls), 520);
    };

    // A dot travels a lane, then calls next.
    const travel = (lane: HTMLElement | null, cls: string, ms: number, next: (dot: HTMLElement) => void) => {
      if (!lane) return;
      const dot = document.createElement('span');
      dot.textContent = '●';
      const flat = across(lane);
      dot.className = `absolute text-[10px] leading-none ${cls} ${flat ? 'left-0 top-1/2' : 'top-0 left-1/2'}`;
      lane.appendChild(dot);
      const far = (flat ? lane.clientWidth : lane.clientHeight) - 8;
      const at = (d: number) => (flat ? `translate(${d}px, -50%)` : `translate(-50%, ${d}px)`);
      dot.animate([{ transform: at(0), opacity: 0 }, { opacity: 1, offset: 0.1 }, { transform: at(far), opacity: 1 }], {
        duration: ms,
        easing: 'linear',
        fill: 'forwards',
      });
      setTimeout(() => next(dot), ms);
    };

    const request = () => {
      const scanner = Math.random() < 0.22;
      travel(laneA.current, scanner ? 'text-bad' : 'text-good', 750, (dot) => {
        if (scanner) {
          dot.textContent = '✕';
          dot.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 500, fill: 'forwards' });
          setTimeout(() => dot.remove(), 500);
          flash(firewall.current, ['bg-bad/15']);
          setBanned((n) => n + 1);
          return;
        }
        dot.remove();
        travel(laneB.current, 'text-accent', 900, (dot2) => {
          dot2.remove();
          const target = TARGETS[Math.floor(Math.random() * TARGETS.length)]!;
          flash(chips.current[target], ['ring-1', 'ring-accent', 'text-fg']);
          setServed((n) => n + 1);
        });
      });
    };

    const t = setInterval(() => {
      if (visible && !document.hidden) request();
    }, 700);
    return () => {
      clearInterval(t);
      io.disconnect();
    };
  }, []);

  return (
    <div
      ref={rootRef}
      className="grid items-stretch gap-0 font-mono text-[12px] md:grid-cols-[minmax(0,0.9fr)_minmax(2.5rem,0.5fr)_minmax(0,1fr)_minmax(4rem,0.8fr)_minmax(0,1.35fr)]"
    >
      {/* The internet */}
      <div className="rounded-2xl border border-line bg-elevated p-4">
        <div className="flex items-baseline justify-between gap-2">
          <span className="font-semibold text-fg">the internet</span>
          <span className="text-[10.5px] text-subtle">anyone</span>
        </div>
        <ul className="mt-3 space-y-1.5">
          {VISITORS.map((v) => (
            <li key={v.where} className="flex items-center gap-2 rounded-md bg-fg/[0.04] px-2 py-1">
              <span className={v.bad ? 'text-bad' : 'text-good'}>●</span>
              <span className={v.bad ? 'text-bad' : 'text-fg'}>{v.name}</span>
              <span className="ml-auto truncate text-[10.5px] text-subtle">{v.where}</span>
            </li>
          ))}
        </ul>
      </div>

      {/* https */}
      <div className="relative flex items-center justify-center py-2 md:py-0">
        <div ref={laneA} className="relative h-12 w-px bg-line-strong md:h-px md:w-full" />
        <span className="absolute left-[calc(50%+0.75rem)] text-[10.5px] text-subtle md:top-[calc(50%+0.5rem)] md:left-1/2 md:-translate-x-1/2">
          https
        </span>
      </div>

      {/* The edge */}
      <div className="rounded-2xl border border-accent/40 bg-accent-soft p-4">
        <div className="flex items-baseline justify-between gap-2">
          <span className="font-semibold text-fg">edge-1</span>
          <span className="text-[10.5px] text-subtle">$5 VPS · 203.0.113.7</span>
        </div>
        <div className="mt-3 space-y-1.5 text-[11.5px]">
          <div className="flex items-center gap-2 rounded-md bg-bg/50 px-2 py-1">
            <span className="text-good">✓</span> HTTPS certificates
          </div>
          <div className="flex items-center gap-2 rounded-md bg-bg/50 px-2 py-1">
            <span className="text-good">✓</span> routes and logins
          </div>
          <div ref={firewall} className="flex items-center gap-2 rounded-md bg-bg/50 px-2 py-1 transition-colors duration-300">
            <span className="text-bad">✕</span> firewall
            <span className="ml-auto text-[10.5px] text-subtle tabular-nums">{banned} banned</span>
          </div>
        </div>
      </div>

      {/* The tunnel home */}
      <div className="relative flex items-center justify-center py-2 md:py-0">
        <div ref={laneB} className="relative h-16 w-[5px] border-x border-accent/60 md:h-[5px] md:w-full md:border-x-0 md:border-y" />
        <span className="absolute left-[calc(50%+0.9rem)] w-[10rem] text-[10.5px] text-accent md:top-[calc(50%+0.6rem)] md:left-1/2 md:w-auto md:-translate-x-1/2 md:whitespace-nowrap">
          <span className="md:hidden">↑</span>
          <span className="max-md:hidden">←</span> WireGuard, dialed from home
        </span>
      </div>

      {/* Home */}
      <div className="rounded-2xl border border-line-strong bg-elevated p-4">
        <div className="flex items-baseline justify-between gap-2">
          <span className="font-semibold text-fg">home lab</span>
          <span className="text-[10.5px] text-subtle">no open ports</span>
        </div>
        <div className="mt-3 space-y-1.5">
          {SERVERS.map((s) => (
            <div key={s.name} className="flex items-center gap-2 rounded-md bg-fg/[0.04] px-2 py-1">
              <span className="text-good">●</span>
              <span className="w-[4.5rem] shrink-0 text-muted">{s.name}</span>
              <span className="flex flex-wrap gap-1">
                {s.apps.map((a) => (
                  <span
                    key={a}
                    ref={(el) => {
                      chips.current[a] = el;
                    }}
                    className="rounded border border-line px-1.5 text-[11px] text-muted transition"
                  >
                    {a}
                  </span>
                ))}
              </span>
            </div>
          ))}
          <div className="flex items-center gap-2 rounded-md bg-fg/[0.04] px-2 py-1">
            <span className="text-subtle">→</span>
            <span
              ref={(el) => {
                chips.current[EXTERNAL.name] = el;
              }}
              className="rounded border border-line px-1.5 text-[11px] text-muted transition"
            >
              {EXTERNAL.name}
            </span>
            <span className="truncate text-[10.5px] text-subtle">{EXTERNAL.target}</span>
          </div>
        </div>
        <div className="mt-3 text-[10.5px] text-subtle tabular-nums">{served.toLocaleString('en-US')} requests served from home</div>
      </div>
    </div>
  );
};
