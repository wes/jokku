import { INSTALL_COMMAND } from '../../lib/site';
import { CopyButton } from '../copy-button';

export const InstallCommand = ({ className = '' }: { className?: string }) => (
  <div
    className={`flex h-12 items-center gap-3 rounded-xl border border-line bg-elevated/80 pr-1.5 pl-4 font-mono text-[12.5px] shadow-sm backdrop-blur ${className}`}
  >
    <span className="text-accent select-none">$</span>
    <span className="min-w-0 truncate text-body">{INSTALL_COMMAND}</span>
    <CopyButton text={INSTALL_COMMAND} tone="page" label="Copy install command" className="ml-auto shrink-0" />
  </div>
);
