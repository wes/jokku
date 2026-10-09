'use client';

import { MoonIcon, SunIcon } from './icons';

export const ThemeToggle = () => {
  const toggle = () => {
    const root = document.documentElement;
    const next = root.dataset.theme === 'dark' ? 'light' : 'dark';
    root.dataset.theme = next;
    try {
      localStorage.setItem('theme', next);
    } catch {
      // Private windows can refuse storage; the toggle still works.
    }
  };

  // Both icons render; CSS shows the right one, so server and client agree.
  return (
    <button
      type="button"
      onClick={toggle}
      aria-label="Toggle dark mode"
      title="Toggle dark mode"
      className="grid size-9 place-items-center rounded-lg text-muted transition hover:bg-surface hover:text-fg"
    >
      <SunIcon className="hidden size-[18px] dark:block" />
      <MoonIcon className="size-[18px] dark:hidden" />
    </button>
  );
};
