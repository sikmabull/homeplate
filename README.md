# Homeplate

**Bring your runners home.** Your own machine (Mac-first, Linux second) as the
CI system for your GitHub Actions. GitHub keeps orchestration, PR checks, and
logs; your hardware does the compute — in resource-capped, one-job ephemeral
containers — and PR checks go green exactly as if GitHub ran them. Offline
mode keeps working on battery with no wifi and posts results when you're back.

```bash
brew install sikmabull/tap/homeplate
homeplate auto        # auth → scan disk → link repos → adopt PRs → daemon
```

Self-hosted is **not $0** — $0.002/min on private repos since March 2026,
free on public repos — but that is still **up to ~31x cheaper than hosted
macOS**, and you own the silicon. Homeplate never claims otherwise.

## Who

Homeplate is built by **[@MerkleGhost](https://x.com/MerkleGhost)**. The
project comes out of a hardware background — recycling, repair, watching
working machines get thrown away — and it shows: Homeplate exists to give
hardware you already own a second job, and to stop depending on outside
infrastructure you don't control for work your own silicon can do. That's the
foundation the whole tool is built on.

Like the philosophy? **[Follow @MerkleGhost on X](https://x.com/MerkleGhost)**
and **give this repo a star** — it helps more than you'd think.

## This repository

| Path | What it is |
|---|---|
| [`homeplate/`](homeplate/) | The CLI + daemon (Go, single binary). **[Start here — full README](homeplate/README.md)** with Known limits, FAQ, and security model |
| [`site/`](site/) | The marketing site (Next.js static export → Cloudflare Workers) — live at https://homeplate.matthew-a46.workers.dev |
| [`Formula/`](Formula/) | Homebrew formula |
| [`docs/RESEARCH.md`](docs/RESEARCH.md) | The verified-facts dossier every claim is built on (GitHub API behavior, macOS power management, pricing) |

## The honest shortlist

- Reserved labels (`ubuntu-latest`…) can't be intercepted — `homeplate adopt`
  opens one PR per repo instead. Matrix expressions are left alone.
- Offline results are **commit statuses** (Checks API is GitHub-App-only),
  always stamped *"ran locally via Homeplate offline mode at \<ts\>"*.
- macOS jobs get soft limits (no macOS container runtime exists); Linux jobs
  get hard container caps. `homeplate status` labels them SOFT vs HARD.
- Lid-closed operation needs an external display — or Homeplate's managed
  lid-close toggle (`homeplate power setup`), which auto-reverts.
- [nektos/act](https://github.com/nektos/act) (MIT) powers offline mode. ♥

MIT licensed.
