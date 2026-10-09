import type { ReactNode } from 'react';

// Colors terminal output the way Jokku's CLI prints it: "----->" steps,
// "=====>" headlines, " !" errors, and "$ " for what you typed.

const URL = /(https?:\/\/[^\s]+)/g;

function linkify(text: string): ReactNode[] {
  return text.split(URL).map((part, i) =>
    i % 2 === 1 ? (
      <span key={i} className="text-term-cyan">
        {part}
      </span>
    ) : (
      part
    ),
  );
}

export function TerminalLine({ line }: { line: string }): ReactNode {
  let rest = line;
  let prefix: ReactNode = null;
  if (rest.startsWith('remote: ')) {
    prefix = <span className="text-term-dim">remote: </span>;
    rest = rest.slice(8);
  }
  if (rest.startsWith('$ ')) {
    return (
      <>
        <span className="text-term-dim select-none">$ </span>
        <span className="text-term-bright">{rest.slice(2)}</span>
      </>
    );
  }
  if (rest.startsWith('-----> ')) {
    return (
      <>
        {prefix}
        <span className="text-term-accent">-----&gt; </span>
        {linkify(rest.slice(7))}
      </>
    );
  }
  if (rest.startsWith('=====> ')) {
    return (
      <>
        {prefix}
        <span className="font-semibold text-term-good">=====&gt; </span>
        <span className="font-semibold text-term-bright">{rest.slice(7)}</span>
      </>
    );
  }
  if (rest.startsWith(' !')) {
    return (
      <>
        {prefix}
        <span className="text-term-bad">{rest}</span>
      </>
    );
  }
  if (/^\s*#\d+ /.test(rest)) {
    return (
      <>
        {prefix}
        <span className="text-term-dim">{rest}</span>
      </>
    );
  }
  // Column headers of list commands (NAME  ROLE  STATUS ...).
  if (/^[A-Z][A-Z ]{8,}$/.test(rest.trim())) {
    return <span className="text-term-dim">{rest}</span>;
  }
  return (
    <>
      {prefix}
      {linkify(rest)}
    </>
  );
}

export const TerminalText = ({ code }: { code: string }) => (
  <>
    {code.split('\n').map((line, i) => (
      <div key={i} className="min-h-[1.7em] whitespace-pre">
        <TerminalLine line={line} />
      </div>
    ))}
  </>
);

// What a reader wants on their clipboard: the commands, not the output.
export const commandsOf = (code: string) => {
  const cmds = code
    .split('\n')
    .filter((l) => l.startsWith('$ '))
    .map((l) => l.slice(2));
  return cmds.length ? cmds.join('\n') : code;
};
