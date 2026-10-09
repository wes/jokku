import { highlight, isHighlighted } from '../lib/highlight';
import { CopyButton } from './copy-button';
import { commandsOf, TerminalText } from './terminal-text';

const LABELS: Record<string, string> = {
  sh: 'Terminal',
  bash: 'Terminal',
  shell: 'Terminal',
  console: 'Terminal',
  yaml: 'YAML',
  yml: 'YAML',
  dockerfile: 'Dockerfile',
  json: 'JSON',
  procfile: 'Procfile',
};

export async function CodeBlock({
  code,
  lang,
  title,
}: {
  code: string;
  lang: string;
  title?: string | undefined;
}) {
  const label = title ?? LABELS[lang];
  const isConsole = lang === 'console';
  const html = isHighlighted(lang) ? await highlight(code, lang) : null;

  return (
    <div className="group/code relative my-6 overflow-hidden rounded-xl border border-term-line bg-term shadow-[0_1px_0_0_rgb(255_255_255/0.04)_inset] dark:shadow-none">
      {label ? (
        <div className="flex h-10 items-center justify-between border-b border-term-line bg-term-bar pr-1.5 pl-4">
          <span className="font-mono text-[11.5px] tracking-wide text-term-dim">{label}</span>
          <CopyButton text={isConsole ? commandsOf(code) : code} />
        </div>
      ) : (
        <CopyButton
          text={code}
          className="absolute top-2 right-2 z-10 pointer-fine:opacity-0 pointer-fine:group-hover/code:opacity-100 focus-visible:opacity-100"
        />
      )}
      {html ? (
        <div className="code-scroll scrollbar-thin" dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        <div className="code-scroll scrollbar-thin">
          <pre className="text-term-fg">
            <code>{isConsole ? <TerminalText code={code} /> : code}</code>
          </pre>
        </div>
      )}
    </div>
  );
}
