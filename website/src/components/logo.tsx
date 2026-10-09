// The mark is a "J" drawn from six instance cells, the way `jokku top`
// draws instances as dots. The top cell is the one just booting.
const CELLS: Array<[number, number, boolean]> = [
  [2, 0, true],
  [2, 1, false],
  [2, 2, false],
  [1, 2, false],
  [0, 2, false],
  [0, 1, false],
];

export const LogoMark = ({ className = 'size-6' }: { className?: string }) => (
  <svg viewBox="0 0 24 24" className={className} aria-hidden="true">
    {CELLS.map(([x, y, lit]) => (
      <rect
        key={`${x}${y}`}
        x={1 + x * 7.6}
        y={1 + y * 7.6}
        width="6.4"
        height="6.4"
        rx="1.6"
        fill="currentColor"
        opacity={lit ? 1 : 0.55}
      />
    ))}
  </svg>
);

export const Logo = () => (
  <span className="flex items-center gap-2 text-fg">
    <LogoMark className="size-[22px] text-accent" />
    <span className="text-[17px] font-semibold tracking-tight">jokku</span>
  </span>
);
