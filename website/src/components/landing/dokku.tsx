const COMMANDS = [
  'apps:list',
  'apps:create myapp',
  'config:set myapp SECRET=s3cret',
  'domains:add myapp myapp.com',
  'letsencrypt:enable myapp',
  'ps:scale myapp web=3 worker=1',
  'logs myapp -t',
  'ps:report myapp',
];

const DIFFERENCES = [
  ['Runs apps in', 'Containers', 'Firecracker microVMs'],
  ['Servers', 'One', 'As many as you add'],
  ['Proxy', 'nginx', 'Caddy, built in'],
  ['storage:mount', 'A host directory', 'A disk that moves with its app'],
  ['Builds', 'Buildpacks, Dockerfile, …', 'Dockerfile, compose, images'],
];

export const DokkuCompare = () => (
  <div className="grid gap-4 lg:grid-cols-2">
    <div className="overflow-hidden rounded-2xl border border-term-line bg-term font-mono text-[12.5px] leading-[2] text-term-fg shadow-xl shadow-black/20">
      <div className="flex h-10 items-center border-b border-term-line bg-term-bar px-4 text-[12px] text-term-dim">
        muscle memory, intact
      </div>
      <div className="px-5 py-4">
        {COMMANDS.map((c, i) => (
          <div key={c} className="truncate">
            <span className="text-term-dim">$ </span>
            {/* "dokku" rolls over into "jokku", one line after another. */}
            <span className="inline-grid overflow-hidden align-bottom">
              <span
                className="dokku-word text-term-dim [grid-area:1/1]"
                style={{ animationDelay: `${i * 0.12}s` }}
              >
                dokku
              </span>
              <span
                className="jokku-word font-semibold text-term-accent [grid-area:1/1]"
                style={{ animationDelay: `${i * 0.12}s` }}
              >
                jokku
              </span>
            </span>{' '}
            <span className="text-term-bright">{c}</span>
          </div>
        ))}
      </div>
    </div>
    <div className="overflow-hidden rounded-2xl border border-line bg-elevated">
      <table className="w-full text-left text-[14px]">
        <thead>
          <tr className="border-b border-line bg-surface/60 text-[12px] text-subtle">
            <th className="px-5 py-3 font-medium" />
            <th className="px-5 py-3 font-mono font-medium">dokku</th>
            <th className="px-5 py-3 font-mono font-medium text-accent">jokku</th>
          </tr>
        </thead>
        <tbody>
          {DIFFERENCES.map(([area, dokku, jokku]) => (
            <tr key={area} className="border-b border-line last:border-0">
              <td className="px-5 py-3.5 text-muted">{area}</td>
              <td className="px-5 py-3.5 text-subtle">{dokku}</td>
              <td className="px-5 py-3.5 font-medium text-fg">{jokku}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  </div>
);
