# homeplate-site

Marketing site for [Homeplate](https://github.com/sikmabull/homeplate) — Next.js (App Router, static export), deployed to Cloudflare Workers.

## Develop

```bash
npm install
npm run dev
```

## Deploy

```bash
npm run deploy   # next build (static export to out/) + wrangler deploy
```

Live at https://homeplate.matthew-a46.workers.dev

## Brand

Assets in `public/assets/` come from the Homeplate brand kit:
field `#FAFAF8` / ink `#111110` / check green `#1F883D`; IBM Plex Mono for
commands/numbers/labels, Helvetica Neue for headings/body.

One rule: the pricing copy stays honest. Self-hosted is **not** $0 —
$0.002/min on private repos since March 2026, free on public repos.
