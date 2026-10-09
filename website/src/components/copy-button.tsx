'use client';

import { useEffect, useState } from 'react';
import { CheckIcon, CopyIcon } from './icons';

export const CopyButton = ({
  text,
  className = '',
  label = 'Copy',
  tone = 'term',
}: {
  text: string;
  className?: string;
  label?: string;
  // "term" sits on a dark code block; "page" on the page's own background.
  tone?: 'term' | 'page';
}) => {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1600);
    return () => clearTimeout(t);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      // Clipboard access can be refused (insecure context, permissions).
    }
  };

  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? 'Copied' : label}
      title={copied ? 'Copied' : label}
      className={`grid size-8 place-items-center rounded-md transition ${
        tone === 'term'
          ? 'text-term-dim hover:bg-white/8 hover:text-term-fg'
          : 'text-subtle hover:bg-surface hover:text-fg'
      } ${className}`}
    >
      {copied ? (
        <CheckIcon className={`size-4 ${tone === 'term' ? 'text-term-good' : 'text-good'}`} />
      ) : (
        <CopyIcon className="size-4" />
      )}
    </button>
  );
};
