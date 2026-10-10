type Status = 'done' | 'building';

// The milestones from the design doc, and where each stands.
const MILESTONES: Array<{ id: string; title: string; status: Status; items: string }> = [
  { id: 'M0', title: 'Control plane', status: 'done', items: 'API, SQLite, CLI over SSH, git receive, the installer, safe updates' },
  { id: 'M1', title: 'Single-server deploys', status: 'done', items: 'BuildKit builds, Firecracker microVMs, embedded Caddy, rollouts, logs, ps:*' },
  { id: 'M2', title: 'Clusters', status: 'done', items: 'WireGuard mesh, one-command joins, scheduling, failover, jokku top' },
  { id: 'M3', title: 'Remote API', status: 'building', items: 'Registry images and logins are done; HTTPS API tokens, git:sync and deploy keys are next' },
  { id: 'M4', title: 'Depth', status: 'building', items: 'enter, volumes and S3 backups (scheduled, restored automatically when a server dies) are done; the jailer, run, rollbacks and services are next' },
];

export const Roadmap = () => (
  <ol className="grid gap-3 md:grid-cols-5">
    {MILESTONES.map((m) => (
      <li
        key={m.id}
        className={`relative rounded-2xl border p-5 ${m.status === 'done' ? 'border-line bg-elevated' : 'border-accent/30 bg-accent-soft/40'}`}
      >
        <div className="flex items-center justify-between">
          <span className="font-mono text-[12px] text-subtle">{m.id}</span>
          {m.status === 'done' ? (
            <span className="inline-flex items-center gap-1.5 rounded-full bg-good/10 px-2 py-0.5 text-[11px] font-medium text-good">
              <span className="size-1.5 rounded-full bg-good" /> Done
            </span>
          ) : (
            <span className="inline-flex items-center gap-1.5 rounded-full bg-accent/10 px-2 py-0.5 text-[11px] font-medium text-accent">
              <span className="size-1.5 animate-pulse rounded-full bg-accent" /> Building
            </span>
          )}
        </div>
        <h3 className="mt-4 font-semibold text-fg">{m.title}</h3>
        <p className="mt-1.5 text-[13.5px] leading-6 text-muted">{m.items}</p>
      </li>
    ))}
  </ol>
);
