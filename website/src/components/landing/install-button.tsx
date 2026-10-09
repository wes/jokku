'use client';

import { useEffect, useState } from 'react';
import { INSTALL_COMMAND } from '../../lib/site';
import { CheckIcon, CopyIcon } from '../icons';

// The install one-liner as a button: click anywhere on it to copy.
export const InstallButton = () => {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1800);
    return () => clearTimeout(t);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(INSTALL_COMMAND);
      setCopied(true);
    } catch {
      // Clipboard access can be refused; the command is visible to copy by hand.
    }
  };

  return (
    <button
      type="button"
      onClick={copy}
      title="Copy the install command"
      className="group inline-flex h-11 max-w-full min-w-0 items-center gap-3 rounded-lg border border-line-strong bg-elevated/80 pr-3 pl-4 font-mono text-[13px] text-body backdrop-blur transition hover:border-accent/50"
    >
      <span className="text-accent">$</span>
      <span className="truncate">
        curl -fsSL …/install.sh <span className="text-subtle">|</span> sudo sh
      </span>
      {copied ? (
        <span className="flex shrink-0 items-center gap-1 font-sans text-[12.5px] text-good">
          <CheckIcon className="size-4" /> Copied
        </span>
      ) : (
        <CopyIcon className="size-4 shrink-0 text-subtle transition group-hover:text-fg" />
      )}
    </button>
  );
};
