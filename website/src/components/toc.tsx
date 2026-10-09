'use client';

import { useEffect, useState } from 'react';
import type { Heading } from '../lib/docs';

// "On this page", with the section you're reading highlighted.
export const Toc = ({ headings }: { headings: Heading[] }) => {
  const [active, setActive] = useState(headings[0]?.id);

  useEffect(() => {
    const elements = headings
      .map((h) => document.getElementById(h.id))
      .filter((el): el is HTMLElement => el !== null);
    if (!elements.length) return;

    const update = () => {
      // The last heading above a line a third of the way down the viewport.
      const line = window.innerHeight / 3;
      let current = elements[0]!.id;
      for (const el of elements) {
        if (el.getBoundingClientRect().top <= line) current = el.id;
      }
      if (window.innerHeight + window.scrollY >= document.body.scrollHeight - 4) {
        current = elements[elements.length - 1]!.id;
      }
      setActive(current);
    };
    update();
    window.addEventListener('scroll', update, { passive: true });
    window.addEventListener('resize', update);
    return () => {
      window.removeEventListener('scroll', update);
      window.removeEventListener('resize', update);
    };
  }, [headings]);

  if (!headings.length) return null;

  return (
    <nav aria-label="On this page" className="text-[13px]">
      <h4 className="mb-3 font-mono text-[11px] tracking-wider text-subtle uppercase">On this page</h4>
      <ul className="space-y-0.5">
        {headings.map((h) => (
          <li key={h.id}>
            <a
              href={`#${h.id}`}
              className={`block py-1 leading-5 transition ${h.depth === 3 ? 'pl-3' : ''} ${
                active === h.id ? 'font-medium text-accent' : 'text-muted hover:text-fg'
              }`}
            >
              {h.text}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
};
