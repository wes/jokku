// The logo mark drawn large, like an engraving: a server's nine slots, six
// of them hatched as running instances forming the J, one just booting.

const RUNNING: Array<[number, number]> = [
  [2, 1],
  [2, 2],
  [1, 2],
  [0, 2],
  [0, 1],
];
const EMPTY: Array<[number, number]> = [
  [0, 0],
  [1, 0],
  [1, 1],
];

const hatch = (angle: number, gap: number, alpha: number) =>
  `repeating-linear-gradient(${angle}deg, color-mix(in oklab, var(--accent) ${alpha}%, transparent) 0 1.2px, transparent 1.2px ${gap}px)`;

const cell = (x: number, y: number) => ({
  left: `${x * 34}%`,
  top: `${y * 34}%`,
  width: '32%',
  height: '32%',
});

export const HeroArt = () => (
  <div
    aria-hidden="true"
    className="pointer-events-none absolute top-24 -right-16 hidden size-[520px] xl:block 2xl:right-0"
    style={{ maskImage: 'radial-gradient(circle at 60% 45%, black 45%, transparent 78%)' }}
  >
    {/* Construction lines, as on an engraver's plate. */}
    <div className="absolute inset-[-12%] rounded-[3rem] border border-dashed border-accent/15" />
    <div className="absolute top-1/2 left-[-12%] h-px w-[124%] bg-accent/10" />
    <div className="absolute top-[-12%] left-1/2 h-[124%] w-px bg-accent/10" />

    {EMPTY.map(([x, y]) => (
      <div
        key={`${x}${y}`}
        className="absolute rounded-[22%] border border-dashed border-accent/20"
        style={cell(x, y)}
      />
    ))}
    {RUNNING.map(([x, y], i) => (
      <div
        key={`${x}${y}`}
        className="absolute rounded-[22%] border border-accent/45"
        style={{
          ...cell(x, y),
          backgroundImage: `${hatch(135, 7 + (i % 2), 55)}, ${hatch(45, 11, 22)}`,
        }}
      />
    ))}
    {/* The instance that's booting. */}
    <div
      className="absolute rounded-[22%] border border-accent shadow-[0_0_60px_-10px] shadow-accent/70"
      style={{ ...cell(2, 0), backgroundImage: `${hatch(135, 4, 85)}, ${hatch(45, 6, 45)}` }}
    >
      <span className="absolute inset-[30%] animate-pulse rounded-[22%] bg-accent/80 blur-[1px]" />
    </div>
  </div>
);
