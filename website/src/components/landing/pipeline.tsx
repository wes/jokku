// What happens between `git push` and a live URL, left to right.
const STAGES = [
  { label: 'git push', detail: 'Your source streams to the control server.', mono: 'source.tar' },
  { label: 'BuildKit', detail: 'Your Dockerfile builds to an OCI image.', mono: 'oci image' },
  { label: 'Root disk', detail: 'Layers flatten into one read-only disk.', mono: 'rootfs.ext4' },
  { label: 'Release', detail: 'Build, config and sizes, numbered and immutable.', mono: 'v12' },
  { label: 'microVMs', detail: 'Instances boot, pass checks, take traffic.', mono: 'web.1 web.2 web.3' },
];

export const Pipeline = () => (
  <div className="relative">
    {/* The wire, with a packet riding it. */}
    <div className="absolute top-7 right-[10%] left-[10%] hidden h-px bg-gradient-to-r from-line via-line-strong to-line lg:block">
      <span className="absolute top-1/2 left-0 h-[3px] w-24 -translate-y-1/2 animate-[packet_3.2s_linear_infinite] rounded-full bg-gradient-to-r from-transparent via-accent to-transparent" />
    </div>
    <ol className="relative grid gap-6 sm:grid-cols-2 lg:grid-cols-5 lg:gap-4">
      {STAGES.map((s, i) => (
        <li key={s.label} className="relative flex gap-4 lg:flex-col lg:items-center lg:text-center">
          <span className="relative z-10 grid size-14 shrink-0 place-items-center rounded-2xl border border-line-strong bg-elevated font-mono text-[13px] text-accent shadow-sm">
            {String(i + 1).padStart(2, '0')}
          </span>
          <div>
            <h3 className="font-semibold text-fg lg:mt-4">{s.label}</h3>
            <p className="mt-1 text-[14px] leading-6 text-muted">{s.detail}</p>
            <p className="mt-2 inline-block rounded-md border border-line bg-surface px-2 py-0.5 font-mono text-[11px] text-body">
              {s.mono}
            </p>
          </div>
        </li>
      ))}
    </ol>
  </div>
);
