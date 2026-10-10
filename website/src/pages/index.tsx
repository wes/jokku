import { Link } from 'waku';
import { ArrowRightIcon } from '../components/icons';
import { DokkuCompare } from '../components/landing/dokku';
import { Features } from '../components/landing/features';
import { HeroArt } from '../components/landing/hero-art';
import { HeroDemo } from '../components/landing/hero-demo';
import { InstallButton } from '../components/landing/install-button';
import { InstallCommand } from '../components/landing/install-command';
import { Pipeline } from '../components/landing/pipeline';
import { Roadmap } from '../components/landing/roadmap';
import { SectionHeading } from '../components/landing/section-heading';
import { SocialMeta } from '../components/social-meta';
import { TrafficDemo } from '../components/landing/traffic-demo';
import { DownloadPill, InstallStats, LatestVersion } from '../components/release-stats';
import { releaseStats } from '../lib/releases';
import { DESCRIPTION } from '../lib/site';

const STEPS = [
  { n: '01', title: 'Install', cmd: 'curl -fsSL …/install.sh | sudo sh', text: 'On a fresh Ubuntu or Debian server with KVM. Your SSH keys can deploy right away.' },
  { n: '02', title: 'Push', cmd: 'git push jokku main', text: 'Jokku builds your Dockerfile, boots it and prints a working URL.' },
  { n: '03', title: 'Add a domain', cmd: 'jokku domains:add myapp myapp.com', text: 'Point your DNS at the server. Every server can take traffic.' },
  { n: '04', title: 'Turn on HTTPS', cmd: 'jokku letsencrypt:enable myapp', text: 'Certificates are issued, renewed, and HTTP redirects to HTTPS.' },
];

const BUILT_ON = ['Firecracker', 'WireGuard', 'Caddy', 'BuildKit', 'SQLite'];

export default async function HomePage() {
  const stats = await releaseStats();
  return (
    <>
      <SocialMeta title="Jokku · git push to microVMs on your own servers" description={DESCRIPTION} path="/" />

      {/* Hero */}
      <section className="relative overflow-hidden">
        <div className="pointer-events-none absolute -top-40 right-[-10%] h-[640px] w-[900px] rounded-full bg-[var(--glow)] blur-[140px]" />
        <HeroArt />

        <div className="relative mx-auto max-w-[88rem] px-4 pt-14 pb-20 sm:px-6 sm:pt-20 lg:px-8">
          <Link
            to="/docs/changelog"
            className="animate-rise group inline-flex items-center gap-3 text-[14px]"
          >
            <span className="rounded-[4px] bg-fill px-1.5 py-0.5 font-mono text-[11px] font-semibold tracking-wide text-on-fill">
              <LatestVersion initial={stats} />
            </span>
            <span className="text-accent underline decoration-accent/40 underline-offset-4 transition group-hover:decoration-accent">
              New: serve from home with edges<span className="max-sm:hidden">, logins and databases</span>
            </span>
            <ArrowRightIcon className="size-4 text-accent transition group-hover:translate-x-0.5" />
          </Link>

          <h1 className="animate-rise mt-7 text-[40px] leading-[1.06] font-bold tracking-[-0.045em] text-fg [animation-delay:80ms] sm:text-[62px] lg:text-[78px]">
            {/* One line each from sm up; on phones the sentences flow together. */}
            <span className="sm:block">Your apps, your servers.</span>{' '}
            <span className="sm:block">Every instance a microVM.</span>{' '}
            <span className="text-accent sm:block">Deploy with git push.</span>
          </h1>

          <p className="animate-rise mt-7 max-w-xl text-[17px] leading-7 text-pretty text-body [animation-delay:160ms] sm:text-[18px] sm:leading-[1.7]">
            Jokku is a self-hosted platform modeled on Dokku. It builds your Dockerfile, boots each
            instance in its own Firecracker microVM, puts your domains on HTTPS, and spreads your
            apps across every server you add.
          </p>

          <div className="animate-rise mt-9 flex flex-wrap items-center gap-3 [animation-delay:240ms]">
            <Link
              to="/docs/installation"
              className="group inline-flex h-11 shrink-0 items-center gap-2 rounded-lg bg-fill px-5 text-[15px] font-medium text-on-fill shadow-[inset_0_1px_0_rgb(255_255_255/0.3),0_1px_2px_rgb(0_0_0/0.25)] transition hover:bg-fill-hover"
            >
              Get started
              <ArrowRightIcon className="size-4 transition group-hover:translate-x-0.5" />
            </Link>
            <InstallButton />
            <DownloadPill initial={stats} />
          </div>

          <div className="animate-rise mt-20 [animation-delay:380ms]">
            <HeroDemo />
          </div>
        </div>
      </section>

      {/* Built on */}
      <section className="border-y border-line bg-surface/40">
        <div className="mx-auto flex max-w-[88rem] flex-wrap items-center gap-x-10 gap-y-3 px-4 py-7 sm:px-6 lg:px-8">
          <span className="font-mono text-[11.5px] tracking-[0.18em] text-subtle uppercase">Built on</span>
          {BUILT_ON.map((name) => (
            <span key={name} className="text-[16px] font-semibold tracking-tight text-muted">
              {name}
            </span>
          ))}
        </div>
      </section>

      {/* Four commands */}
      <section className="mx-auto max-w-[88rem] px-4 py-24 sm:px-6 sm:py-32 lg:px-8">
        <SectionHeading eyebrow="getting started" title="From a bare server to HTTPS in four commands.">
          Nothing to configure, and nothing to install on your laptop. Just git and ssh.
        </SectionHeading>
        <ol className="mt-14 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {STEPS.map((s) => (
            <li key={s.n} className="relative rounded-2xl border border-line bg-elevated p-6">
              <span className="font-mono text-[12px] text-accent">{s.n}</span>
              <h3 className="mt-3 text-[17px] font-semibold text-fg">{s.title}</h3>
              <div className="mt-3 truncate rounded-lg bg-term px-3 py-2 font-mono text-[11.5px] text-term-fg">
                <span className="text-term-dim">$ </span>
                {s.cmd}
              </div>
              <p className="mt-3 text-[14px] leading-6 text-muted">{s.text}</p>
            </li>
          ))}
        </ol>
      </section>

      {/* Features */}
      <section className="mx-auto max-w-[88rem] px-4 pb-24 sm:px-6 sm:pb-32 lg:px-8">
        <SectionHeading eyebrow="what you get" title="Everything a platform does. On servers you own.">
          Deploys, scaling, certificates, databases, backups, logins and private networking, running
          on hardware you control, even at home, from one binary.
        </SectionHeading>
        <div className="mt-14">
          <Features />
        </div>
      </section>

      {/* Traffic */}
      <section className="relative overflow-hidden border-y border-line bg-surface/40">
        <div className="pointer-events-none absolute -top-40 right-0 h-[420px] w-[620px] rounded-full bg-[var(--glow)] blur-[120px]" />
        <div className="relative mx-auto max-w-[88rem] px-4 py-24 sm:px-6 sm:py-32 lg:px-8">
          <div className="flex flex-wrap items-end justify-between gap-8">
            <SectionHeading eyebrow="jokku top" title="Watch every request land.">
              A live view of your servers, apps and microVMs, right in your terminal. Press{' '}
              <kbd className="rounded border border-line-strong bg-elevated px-1.5 font-mono text-[14px] text-fg">6</kbd> and
              every request flies across its app’s lane, green, yellow or red, and lands on the
              instance and server that answered.
            </SectionHeading>
            <Link to="/docs/top" className="inline-flex items-center gap-2 text-[15px] font-medium text-accent hover:underline">
              Tour jokku top <ArrowRightIcon className="size-4" />
            </Link>
          </div>
          <div className="mt-14">
            <TrafficDemo />
          </div>
        </div>
      </section>

      {/* Pipeline */}
      <section className="mx-auto max-w-[88rem] px-4 py-24 sm:px-6 sm:py-32 lg:px-8">
        <SectionHeading eyebrow="under the hood" title="From git push to a running microVM." center>
          Every deploy takes the same path, whether it started as a push, a compose file or a
          registry image. Nothing changes until the new version is healthy.
        </SectionHeading>
        <div className="mt-16">
          <Pipeline />
        </div>
        <div className="mt-14 text-center">
          <Link to="/docs/architecture" className="inline-flex items-center gap-2 text-[15px] font-medium text-accent hover:underline">
            Read how it works <ArrowRightIcon className="size-4" />
          </Link>
        </div>
      </section>

      {/* Dokku */}
      <section className="mx-auto max-w-[88rem] px-4 pb-24 sm:px-6 sm:pb-32 lg:px-8">
        <SectionHeading eyebrow="coming from dokku" title="If you know Dokku, you already know Jokku.">
          Same command names, same argument order, same output, same <code className="font-mono text-[0.9em]">git push</code>.
          What changes is where your apps run, and how many servers they can use.
        </SectionHeading>
        <div className="mt-14">
          <DokkuCompare />
        </div>
      </section>

      {/* Roadmap */}
      <section className="mx-auto max-w-[88rem] px-4 pb-24 sm:px-6 sm:pb-32 lg:px-8">
        <SectionHeading eyebrow="status" title="Early, and moving fast.">
          One server or a cluster, git push deploys work today. Here’s what’s built and what’s next.
        </SectionHeading>
        <div className="mt-14">
          <Roadmap />
        </div>
      </section>

      {/* CTA */}
      <section className="relative overflow-hidden border-t border-line">
        <div className="pointer-events-none absolute bottom-[-200px] left-1/2 h-[400px] w-[900px] -translate-x-1/2 rounded-full bg-[var(--glow)] blur-[120px]" />
        <div className="relative mx-auto max-w-3xl px-4 py-28 text-center sm:px-6 sm:py-36">
          <h2 className="text-[38px] leading-[1.05] font-bold tracking-[-0.045em] text-balance text-fg sm:text-[56px]">
            Your servers. Your apps. <span className="text-accent">One push.</span>
          </h2>
          <p className="mx-auto mt-5 max-w-xl text-[17px] leading-7 text-muted">
            Bring a fresh Ubuntu or Debian server with KVM and ports 80 and 443 free. The installer
            does the rest.
          </p>
          <InstallCommand className="mx-auto mt-9 max-w-xl" />
          <div className="mt-4">
            <InstallStats initial={stats} />
          </div>
          <div className="mt-6 flex flex-wrap items-center justify-center gap-x-6 gap-y-2 text-[14.5px]">
            <Link to="/docs/quickstart" className="inline-flex items-center gap-1.5 font-medium text-accent hover:underline">
              Deploy your first app <ArrowRightIcon className="size-4" />
            </Link>
            <Link to="/docs/installation#requirements" className="text-muted hover:text-fg">
              Check the requirements
            </Link>
          </div>
        </div>
      </section>
    </>
  );
}

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
