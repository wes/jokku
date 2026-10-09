# Jokku website

The marketing site and docs for Jokku, built with [Waku](https://waku.gg) and
Tailwind CSS. Every page is prerendered to static HTML. Run these from `website/`:

```sh
npm install
npm run dev      # http://localhost:3000
npm run build    # static site in dist/public
npm start        # serve the build
```

## Where things live

| Path | What |
| --- | --- |
| `src/pages/index.tsx` | The landing page |
| `src/components/landing/` | Its sections, including the animated deploy (`hero-demo.tsx`) and `jokku top` traffic view (`traffic-demo.tsx`) |
| `src/content/docs/*.md` | The docs, one Markdown file per page |
| `src/lib/nav.ts` | The docs sidebar, in reading order |
| `src/components/markdown.tsx` | Renders the Markdown, with the extras below |
| `src/lib/highlight.ts` | Build-time syntax highlighting (Shiki) and its color theme |
| `src/lib/site.ts` | Version, GitHub URL and install command |
| `src/styles.css` | Color tokens for light and dark |

## Writing docs

Add `src/content/docs/<slug>.md` with a title and description, then list the slug in
`src/lib/nav.ts`. It's served at `/docs/<slug>` and shows up in search.

```md
---
title: Volumes
description: One sentence shown under the title and in search.
---
```

Beyond standard Markdown:

- **Steps.** `### 1. Push it` renders as a numbered step.
- **Callouts.** `> [!NOTE]`, `> [!TIP]` or `> [!WARNING]` as a blockquote's first line.
- **Terminal output.** A ` ```console ` block colors Jokku's `----->` and `=====>` lines; lines starting with `$ ` are commands, and the copy button copies only those.
- **Titled code.** ` ```yaml title="compose.yaml" `.
- **Link cards.** A ` ```cards ` block with one `Title | /docs/page | Description` per line.
- **Status badges.** In tables, ✅ 🔜 ➕ ✏️ ✖ render as Works, Planned, Jokku only, Replaces Dokku's and Not planned.

The docs here are written for readers and follow the repo's own `README.md` and
`docs/`. When a change to Jokku updates those, update the matching page here too.
When Jokku ships a release, update `VERSION` in `src/lib/site.ts`.

## Deploy it on Jokku

The site deploys like any app, from the root of this repo, with the build dir set
to `website`. The `Dockerfile` builds the site with Node and serves it with Caddy
on `$PORT`.

```sh
jokku apps:create website
jokku builder:set website build-dir website

git remote add website jokku@your-server:website    # in the repo root
git push website main

jokku domains:add website jokku.example.com
jokku letsencrypt:enable website
```

After that, `git push website main` deploys the latest site with zero downtime.

To try the image locally:

```sh
docker build -t jokku-website .
docker run --rm -p 8080:8080 jokku-website    # http://localhost:8080
```
