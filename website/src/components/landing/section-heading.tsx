import type { ReactNode } from 'react';

// Section labels borrow the CLI's own "----->" step marker.
export const Eyebrow = ({ children }: { children: ReactNode }) => (
  <p className="font-mono text-[12.5px] text-accent">
    <span className="text-subtle">-----&gt;</span> {children}
  </p>
);

export const SectionHeading = ({
  eyebrow,
  title,
  children,
  center = false,
}: {
  eyebrow: string;
  title: ReactNode;
  children?: ReactNode;
  center?: boolean;
}) => (
  <div className={center ? 'mx-auto max-w-2xl text-center' : 'max-w-2xl'}>
    <Eyebrow>{eyebrow}</Eyebrow>
    <h2 className="mt-4 text-[32px] leading-[1.08] font-bold tracking-[-0.04em] text-balance text-fg sm:text-[44px]">
      {title}
    </h2>
    {children ? (
      <p className="mt-5 text-[16.5px] leading-7 text-pretty text-muted sm:text-[17.5px]">{children}</p>
    ) : null}
  </div>
);
