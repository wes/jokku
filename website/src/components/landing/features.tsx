import type { ReactNode } from 'react';
import { Link } from 'waku';
import type { Href } from '../../lib/nav';

// The feature grid. Each card carries a small, true-to-life picture of the
// feature: real commands, real output shapes.

const Card = ({
  title,
  children,
  visual,
  href,
  className = '',
}: {
  title: string;
  children: ReactNode;
  visual: ReactNode;
  href: Href;
  className?: string;
}) => (
  <Link
    to={href}
    className={`group relative flex flex-col overflow-hidden rounded-2xl border border-line bg-elevated transition hover:border-accent/40 ${className}`}
  >
    <div className="relative flex-1 border-b border-line bg-surface/40 p-5">{visual}</div>
    <div className="p-6">
      <h3 className="flex items-center gap-2 text-[16.5px] font-semibold tracking-[-0.01em] text-fg">
        {title}
        <span className="text-subtle transition group-hover:translate-x-0.5 group-hover:text-accent">→</span>
      </h3>
      <p className="mt-2 text-[14.5px] leading-6 text-muted">{children}</p>
    </div>
  </Link>
);

const Mono = ({ children, className = '' }: { children: ReactNode; className?: string }) => (
  <div className={`font-mono text-[11.5px] leading-[1.9] ${className}`}>{children}</div>
);

const Cmd = ({ children }: { children: ReactNode }) => (
  <div className="truncate">
    <span className="text-subtle">$ </span>
    <span className="text-fg">{children}</span>
  </div>
);

const VM_LAYERS = [
  { name: 'your process', note: 'as the image’s USER', className: 'bg-fill text-on-fill' },
  { name: 'volumes', note: 'vdd · /app/data', className: 'bg-accent/25 text-fg' },
  { name: 'scratch layer', note: 'vdc · writable', className: 'bg-accent/15 text-fg' },
  { name: 'root disk', note: 'vda · read-only, shared', className: 'bg-accent/10 text-fg' },
  { name: 'kernel 6.1', note: 'init is PID 1', className: 'bg-fg/[0.06] text-muted' },
];

const MicroVMVisual = () => (
  <div className="grid h-full items-center gap-4 sm:grid-cols-2">
    {[1, 2].map((n) => (
      <div key={n} className={`rounded-xl border border-line-strong bg-elevated p-2.5 shadow-sm ${n === 2 ? 'max-sm:hidden' : ''}`}>
        <div className="mb-2 flex items-center justify-between px-1 font-mono text-[11px]">
          <span className="text-fg">web.{n}</span>
          <span className="text-subtle">firecracker microVM</span>
        </div>
        <div className="space-y-1">
          {VM_LAYERS.map((l) => (
            <div key={l.name} className={`flex items-center justify-between rounded-md px-2.5 py-1.5 font-mono text-[10.5px] ${l.className}`}>
              <span className="font-medium">{l.name}</span>
              <span className="opacity-75">{l.note}</span>
            </div>
          ))}
        </div>
      </div>
    ))}
  </div>
);

const ClusterVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku cluster:join-command</Cmd>
    <div className="mt-2 rounded-lg border border-line bg-elevated p-3">
      {[
        ['server-1', 'control', '4'],
        ['server-2', 'worker', '3'],
        ['server-3', 'worker', '3'],
      ].map(([name, role, n], i) => (
        <div key={name} className="flex items-center gap-2">
          <span className="size-1.5 rounded-full bg-good" />
          <span className="text-fg">{name}</span>
          <span className="text-subtle">{role}</span>
          <span className="ml-auto flex gap-0.5">
            {Array.from({ length: Number(n) }, (_, k) => (
              <span key={k} className={`size-2 rounded-[3px] ${k === 0 && i > 0 ? 'bg-accent/40' : 'bg-accent'}`} />
            ))}
          </span>
        </div>
      ))}
    </div>
    <div className="mt-2 text-[10.5px] text-subtle">WireGuard mesh · udp/51820</div>
  </Mono>
);

const RolloutVisual = () => (
  <Mono className="text-muted">
    {[
      { label: 'v11', bars: [{ from: 0, to: 78, className: 'bg-fg/25' }] },
      { label: 'v12', bars: [{ from: 22, to: 44, className: 'bg-warn/50' }, { from: 44, to: 100, className: 'bg-accent' }] },
    ].map((row) => (
      <div key={row.label} className="flex items-center gap-3 py-1">
        <span className="w-7 text-fg">{row.label}</span>
        <span className="relative h-2.5 flex-1 rounded-full bg-fg/[0.05]">
          {row.bars.map((b) => (
            <span key={b.from} className={`absolute inset-y-0 rounded-full ${b.className}`} style={{ left: `${b.from}%`, right: `${100 - b.to}%` }} />
          ))}
        </span>
      </div>
    ))}
    <div className="relative mt-1 ml-10 h-8 text-[10px] text-subtle">
      <span className="absolute left-[22%] -translate-x-1/2">boot</span>
      <span className="absolute left-[44%] -translate-x-1/2 text-accent">switch</span>
      <span className="absolute left-[78%] -translate-x-1/2">retire</span>
    </div>
    <div className="text-[10.5px]">
      <span className="text-good">●</span> checks pass, then traffic moves
    </div>
  </Mono>
);

const HttpsVisual = () => (
  <Mono className="space-y-1.5 text-muted">
    <Cmd>jokku letsencrypt:enable myapp</Cmd>
    {['myapp.com', 'www.myapp.com', 'api.myapp.com'].map((d) => (
      <div key={d} className="flex items-center gap-2 rounded-lg border border-line bg-elevated px-2.5 py-1">
        <svg viewBox="0 0 24 24" className="size-3.5 text-good" fill="none" stroke="currentColor" strokeWidth={2} aria-hidden="true">
          <rect x="5" y="11" width="14" height="10" rx="2" />
          <path d="M8 11V8a4 4 0 0 1 8 0v3" />
        </svg>
        <span className="text-fg">https://{d}</span>
        <span className="ml-auto text-[10px] text-subtle">renews itself</span>
      </div>
    ))}
  </Mono>
);

const BuildersVisual = () => (
  <Mono className="space-y-1 text-muted">
    <Cmd>git push jokku main</Cmd>
    <Cmd>
      jokku builder:compose <span className="text-accent">shop</span>
    </Cmd>
    <Cmd>
      jokku builder:image cache <span className="text-accent">redis:7</span>
    </Cmd>
    <div className="pt-2 text-[10.5px] text-subtle">Dockerfile · compose.yaml · any registry image</div>
  </Mono>
);

const VolumeVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku nodes:drain server-1</Cmd>
    <div className="mt-2.5 flex items-center gap-2">
      <span className="rounded-md border border-line bg-elevated px-2 py-1 text-fg">server-1</span>
      <span className="relative h-px flex-1 bg-line-strong">
        <span className="absolute top-1/2 left-[68%] size-2 -translate-1/2 rounded-full bg-accent shadow-[0_0_10px] shadow-accent/60" />
      </span>
      <span className="rounded-md border border-accent/40 bg-accent-soft px-2 py-1 text-fg">server-2</span>
    </div>
    <div className="mt-2.5 flex items-center gap-2 text-[10.5px]">
      <span className="text-fg">data</span>
      <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-fg/[0.06]">
        <span className="block h-full w-[82%] rounded-full bg-accent" />
      </span>
      <span>82%</span>
    </div>
    <div className="mt-1 text-[10.5px] text-subtle">copied while the app keeps running</div>
  </Mono>
);

const NetworkVisual = () => (
  <Mono className="text-muted">
    <div className="rounded-lg border border-line bg-elevated p-3">
      <div className="text-subtle"># shop’s config</div>
      <div>
        <span className="text-accent">REDIS_URL</span>=redis://<span className="text-fg">cache.internal</span>:6379
      </div>
      <div>
        <span className="text-accent">JOBS</span>=http://<span className="text-fg">worker.shop.internal</span>
      </div>
    </div>
    <div className="mt-2 text-[10.5px] text-subtle">resolves on any server, follows deploys</div>
  </Mono>
);

const LogsVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku logs myapp -t</Cmd>
    <div className="truncate">
      <span className="text-subtle">app[web.1]:</span> Listening on :3000
    </div>
    <div className="truncate">
      <span className="text-subtle">app[router]:</span> GET / status=<span className="text-good">200</span> 4.2ms
    </div>
    <div className="mt-1.5">
      <Cmd>jokku enter myapp</Cmd>
    </div>
    <div>
      <span className="text-accent">/app</span> $ <span className="inline-block h-3 w-1.5 translate-y-0.5 animate-blink bg-accent" />
    </div>
  </Mono>
);

const EdgeVisual = () => (
  <Mono className="flex h-full flex-col text-muted">
    <Cmd>jokku edge:add root@203.0.113.7</Cmd>
    <div className="my-auto grid grid-cols-[auto_1fr_auto_1.6fr] items-center gap-2 py-4 max-sm:grid-cols-[auto_1fr_auto]">
      <span className="rounded-md border border-line bg-elevated px-2 py-1 text-fg">browser</span>
      <span className="relative h-px bg-line-strong">
        <span className="absolute top-2 left-1/2 -translate-x-1/2 text-[10px] text-subtle">https</span>
      </span>
      <div className="rounded-lg border border-accent/40 bg-accent-soft px-2.5 py-1.5">
        <div className="text-fg">edge1</div>
        <div className="text-[10px] text-subtle">public · certs · logins</div>
      </div>
      <div className="relative flex items-center gap-2 max-sm:col-span-3">
        <span className="relative h-px flex-1 bg-accent/50 max-sm:hidden">
          <span className="absolute top-1/2 left-[70%] size-2 -translate-1/2 rounded-full bg-accent shadow-[0_0_10px] shadow-accent/60" />
          <span className="absolute top-2 left-1/2 -translate-x-1/2 text-[10px] whitespace-nowrap text-accent">home dials out</span>
        </span>
        <div className="flex-[2] rounded-lg border border-line-strong bg-elevated p-2">
          <div className="mb-1 flex justify-between text-[10px]">
            <span className="text-fg">home lab</span>
            <span className="text-subtle">no open ports</span>
          </div>
          <div className="space-y-1 text-[10.5px]">
            <div className="flex justify-between rounded bg-fg/[0.05] px-1.5">
              <span className="text-fg">shop</span>
              <span>microVMs</span>
            </div>
            <div className="flex justify-between rounded bg-fg/[0.05] px-1.5">
              <span className="text-fg">ha</span>
              <span>192.168.1.50:8123</span>
            </div>
          </div>
        </div>
      </div>
    </div>
    <div className="text-[10.5px] text-subtle">WireGuard · the edge reaches only what it routes</div>
  </Mono>
);

const LoginVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku http-auth:enable photos wes sam</Cmd>
    <div className="mx-auto mt-2.5 max-w-[15rem] rounded-xl border border-line-strong bg-elevated p-3 font-sans shadow-sm">
      <div className="text-[12.5px] font-semibold text-fg">Log in to photos</div>
      <div className="mt-2 rounded-md border border-line bg-bg px-2 py-1 text-[11px] text-fg">wes</div>
      <div className="mt-1.5 rounded-md border border-line bg-bg px-2 py-1 font-mono text-[11px] tracking-[0.3em] text-fg">
        482 913
      </div>
      <div className="mt-2 rounded-md bg-fill py-1 text-center text-[11px] font-semibold text-on-fill">Verify</div>
    </div>
  </Mono>
);

const BackupsVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku backups:list shop data</Cmd>
    <div className="mt-1.5 grid grid-cols-[1fr_auto_auto] gap-x-3 text-[10.5px]">
      <span className="text-subtle">BACKUP</span>
      <span className="text-subtle">TAKEN</span>
      <span className="text-subtle">ADDED</span>
      {[
        ['2026-10-09T14-15-00Z', '2m ago', '1.8 MB'],
        ['2026-10-09T14-00-00Z', '17m ago', '640 KB'],
        ['2026-10-09T13-45-00Z', '32m ago', '2.3 MB'],
      ].map(([name, taken, added]) => (
        <div key={name} className="contents">
          <span className="text-fg">{name}</span>
          <span>{taken}</span>
          <span className="text-accent">+{added}</span>
        </div>
      ))}
    </div>
    <div className="mt-2 text-[10.5px]">
      <span className="text-good">●</span> server-2 down 5m: restored on server-3
    </div>
  </Mono>
);

const DatabaseVisual = () => (
  <Mono className="text-muted">
    <Cmd>jokku db:postgres:create shopdb</Cmd>
    <Cmd>jokku db:postgres:link shopdb shop</Cmd>
    <div className="mt-2 rounded-lg border border-line bg-elevated p-2.5">
      <div className="text-subtle"># set on shop</div>
      <div className="truncate">
        <span className="text-accent">DATABASE_URL</span>=postgres://…@<span className="text-fg">postgres-shopdb.internal</span>
      </div>
    </div>
    <div className="mt-2 text-[10.5px] text-subtle">Postgres · MySQL · Redis, each on its own volume</div>
  </Mono>
);

const UpdateVisual = () => (
  <Mono className="text-muted">
    <Cmd>sudo jokku update</Cmd>
    <div>
      <span className="text-accent">-----&gt;</span> Updating jokku from v0.8.1 to v0.8.2
    </div>
    <div>
      <span className="text-accent">-----&gt;</span> Backing up the database
    </div>
    <div>
      <span className="text-accent">-----&gt;</span> Restarting jokku
    </div>
    <div className="pl-[4.5ch]">jokku v0.8.2 is running</div>
    <div className="mt-1.5 text-[10.5px]">
      <span className="text-good">●</span> apps kept serving · rolls back if it fails
    </div>
  </Mono>
);

export const Features = () => (
  <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
    <Card title="A microVM for every instance" href="/docs/architecture" visual={<MicroVMVisual />} className="md:col-span-2">
      Each instance boots in its own Firecracker virtual machine, with its own kernel, from a
      read-only disk built from your image. Restarting or updating Jokku leaves them running.
    </Card>
    <Card title="Clusters in one command" href="/docs/cluster" visual={<ClusterVisual />}>
      One server is a complete Jokku. Join more over an encrypted mesh and apps spread across
      them. If one dies, its apps start on the others.
    </Card>
    <Card title="Zero-downtime deploys" href="/docs/processes" visual={<RolloutVisual />}>
      New instances boot beside the old ones and take traffic once they pass checks. A failed
      deploy is rejected, and the old version keeps serving.
    </Card>
    <Card title="HTTPS without thinking" href="/docs/domains" visual={<HttpsVisual />}>
      Let’s Encrypt certificates for every domain, renewed for you, by the Caddy built into
      Jokku and shared across your servers.
    </Card>
    <Card title="Dockerfiles, compose, images" href="/docs/compose" visual={<BuildersVisual />}>
      Build a Dockerfile, deploy a whole compose file with each service as a process, or run any
      image from a registry.
    </Card>
    <Card title="Serve from home, no port open" href="/docs/edges" visual={<EdgeVisual />} className="md:col-span-2">
      Run Jokku in a home lab and add an edge: a small public server your servers dial. It serves
      your domains, and the services on your network you route to it, like Home Assistant or a NAS.
    </Card>
    <Card title="A login in front of anything" href="/docs/logins" visual={<LoginVisual />}>
      A shared password, or your users with authenticator codes, before any app. One login for
      every app, share links that expire, and nothing to change in the app.
    </Card>
    <Card title="Volumes that move" href="/docs/storage" visual={<VolumeVisual />}>
      Give a database a disk that moves with it when its server is drained, copied while the app
      keeps running.
    </Card>
    <Card title="Backups that restore themselves" href="/docs/backups" visual={<BackupsVisual />}>
      Every 15 minutes, incremental and encrypted, to any S3-compatible bucket. If a server dies,
      its volumes come back on another one from their latest backup.
    </Card>
    <Card title="Databases in one command" href="/docs/databases" visual={<DatabaseVisual />}>
      Postgres, MySQL and Redis from their official images, linked to your apps with a connection
      URL, reachable only from them, and backed up from the start.
    </Card>
    <Card title="Apps find each other by name" href="/docs/networking" visual={<NetworkVisual />}>
      <code className="font-mono text-[13px]">cache.internal</code> reaches your cache app from
      any app, on any server, over WireGuard.
    </Card>
    <Card title="Logs and a shell, anywhere" href="/docs/logs" visual={<LogsVisual />}>
      Follow every instance and every request in one stream, or open a shell inside a running
      microVM with <code className="font-mono text-[13px]">jokku enter</code>.
    </Card>
    <Card title="Updates you don’t worry about" href="/docs/updating" visual={<UpdateVisual />}>
      One command updates a server. Your apps keep serving, the state is backed up first, and a
      release that doesn’t come up is rolled back.
    </Card>
  </div>
);
