# Homeplate

**Your Mac is your CI. GitHub keeps the checkmarks. You keep the change — up to 31x cheaper than hosted macOS, and the fee is on the label.**

```bash
brew install homeplate && homeplate init
```

GitHub Actions charges per minute. Your laptop has 8 idle cores and is
already paid for. Homeplate points the second thing at the first thing:
GitHub keeps doing orchestration, PR checks, and log storage while your
hardware does 100% of the compute, in clean, resource-capped, throwaway
containers.

Self-hosted runners are a **first-class, supported GitHub feature**. They are
not $0: since March 2026 GitHub bills **$0.002/min for self-hosted runtime on
private repos** (public repos are free). That is still **up to ~31x cheaper
than hosted macOS** ($0.062/min) — and you own the silicon. Homeplate's
savings counter subtracts the fee before claiming anything. This is not a
hack or a ToS grey area; Homeplate just makes self-hosted a one-command
experience instead of a weekend of YAML.

---

## Known limits (read this first)

Most tools bury this. Here it is at the top, because the fastest way to lose
your trust is to promise something the platform cannot do.

### 1. Homeplate cannot silently hijack `runs-on: ubuntu-latest`

**This is the big one.** The pitch "your existing workflows just move to your
machine, no edits" is *not achievable* on GitHub today.

GitHub resolves hosted labels (`ubuntu-latest`, `macos-latest`, ...) to its own
hosted pool. The registration API will happily *accept* the string
`ubuntu-latest` as a self-hosted runner label — there is no reserved-word list
in the API schema — and it demonstrably **did** route jobs that way in 2023.
But it stopped working around May–June 2025 ([community discussion
#162274](https://github.com/orgs/community/discussions/162274), still
unanswered), it is documented nowhere, and it is not a behaviour any serious
tool should depend on.

**What Homeplate does instead** — the honest, supported path:

```bash
homeplate adopt owner/repo
```

Opens **one pull request** rewriting `runs-on:` across your workflows:

```diff
-    runs-on: ubuntu-latest
+    runs-on: [self-hosted, homeplate, homeplate-linux]
```

You review it, merge it once, and every future job runs on your hardware.
`homeplate doctor` tells you per repo which jobs still bill GitHub, and Homeplate
*probes* what GitHub actually stored at registration — so if hosted-label
routing ever comes back, Homeplate notices without a code change.

Matrix expressions (`runs-on: ${{ matrix.os }}`) are **not** rewritten
automatically. Blindly replacing a computed value breaks the matrix, so they
are reported for you to decide.

### 2. Closing your MacBook's lid will stop your jobs (unless you have an external display)

Every "keep your Mac awake" tool implies otherwise. The kernel disagrees:

```c
// xnu, IOPMrootDomain.cpp
shouldSleepOnClamshellClosed() {
    return !clamshellDisabled && !(desktopMode && acAdaptorConnected)
           && !clamshellSleepDisableMask;
}
```

A `caffeinate -s` / `IOPMAssertionCreateWithName` assertion — what Homeplate
holds — prevents **idle** sleep. It does **not** survive the lid switch.
Apple's own header says so: *"The system may still sleep for lid close, Apple
menu, low battery, or other sleep reasons."* Overriding lid-close sleep needs
the Apple-private entitlement `com.apple.private.iokit.assertonlidclose`,
which powerd rejects for third-party apps in code.

| Situation | Lid closed, jobs keep running? |
|---|---|
| Mac desktop (no lid) | **YES** |
| MacBook, AC + external display (true clamshell mode) | **YES** |
| MacBook, AC, no external display | **NO** |
| MacBook on battery | **NO** |
| Linux + systemd | **YES** (logind allows a `handle-lid-switch` inhibitor) |

`homeplate status` prints the honest per-machine verdict, with the reason:

```
will keep running with lid closed: NO because on AC power, but with no external
    display. A sleep assertion (caffeinate -s) prevents IDLE sleep only; it does
    not survive the lid switch...
    fix: attach an external display (Apple's documented clamshell requirement)...
```

`sudo pmset -a disablesleep 1` genuinely does block lid-close sleep, but it is
undocumented, needs root, and is system-wide (the `pmset` power-source prefix is
ignored for it). Homeplate offers it as a **managed toggle**, off by default:

```bash
homeplate power setup     # one-time sudo: scoped NOPASSWD rule for pmset only
homeplate limit --clamshell true
```

Set while jobs run or the queue is non-empty (AC only, unless you opt into
battery with a 30% floor), reverted the moment work drains, on daemon exit,
after a crash (a marker file lets the next daemon start clean it up), and on
uninstall. The table's NO rows become **YES (managed toggle on)** — and
`homeplate status` says exactly which mode you're in.

**If the machine sleeps mid-job:** on wake, the job is marked
`interrupted` with a reason (never silently dropped). Connected jobs defer to
GitHub's normal re-run path; offline jobs are re-queued automatically.

### 3. Offline results post as commit statuses, not check runs

The Checks API is **GitHub App only**. GitHub's REST docs, verbatim: *"To
create a check run, you must use a GitHub App. OAuth apps and authenticated
users are not able to create a check suite."* A device-flow user token cannot
create check runs, full stop.

Homeplate uses the **Commit Status API** instead. Statuses appear in the same PR
checks box, and branch protection can require them. What you lose: no
per-step UI breakdown, and **no log upload** (neither API accepts logs without
a real `workflow_run`). Local logs stay on disk — `homeplate logs <id>`.

### 4. Device flow needs *your* OAuth App client_id

There is no anonymous device flow; `client_id` is required and device flow must
be enabled on the app. The GitHub CLI's client_id is public and would work, but
using it would make GitHub's consent screen say **"GitHub CLI"** for Homeplate's
access — that is impersonation, so Homeplate refuses to ship it as a default.

Either register a 30-second OAuth App (no client secret needed) and use
`--client-id`, or just skip OAuth:

```bash
homeplate auth add work --pat      # fine-grained PAT, works immediately
```

### 5. Engine B (offline mode) is BETA

[nektos/act](https://github.com/nektos/act) is excellent but is not
byte-identical to GitHub's runner. It ignores `concurrency`, `run-name`,
`GITHUB_STEP_SUMMARY`, annotations, `job.permissions`, `timeout-minutes`,
`continue-on-error`, `environment`, and OIDC. It has **no macOS or Windows
images**. Secrets are not available offline unless you supply them locally.

### 6. Native macOS jobs get soft resource limits

macOS has no cgroups and no container runtime. Linux jobs get **hard** caps
(`docker --cpus`, `--memory`, `--memory-swap`). Native macOS jobs get `nice` +
`taskpolicy -b` background QoS — a *scheduling hint*, not a cap — and memory is
not capped at all. `homeplate status` labels these `HARD` and `SOFT` explicitly.

### 7. The savings counter counts paid minutes you didn't spend

If your workload is **public repos**, hosted runners are already free and your
true saving is **$0**. Homeplate cannot see your remaining included-minutes
balance, so it prices every local minute at GitHub's paid rate. See
[Is the savings counter honest?](#is-the-savings-counter-honest).

### 8. Other sharp edges

- With `max_concurrent_jobs = 1` and several linked repos, listeners **rotate**
  between repos every 60s, so a job can wait up to one rotation.
  GitHub re-queues a job not picked up within 60s; queued > 24h fails.
- `--storage-opt` disk caps need overlay2+xfs or devicemapper; ignored elsewhere.
- Homeplate pins actions/runner **2.335.1** on Apple Silicon: v2.336.0 has an open
  regression where `Runner.Worker` deadlocks in `Process.Start` and every job
  wedges at 100% CPU ([actions/runner#4570](https://github.com/actions/runner/issues/4570)).
- Windows is not supported yet. Mac first, Linux second.

---

## How it works

Two engines, one brain.

```
                    ┌──────────────────────────────────────┐
                    │            SYNC BRAIN                │
                    │  polls githubstatus.com + API reach  │
                    │  replays queued results, idempotent  │
                    └───────────┬──────────────┬───────────┘
                                │              │
             GitHub healthy ────┘              └──── degraded / offline
                    │                                     │
        ┌───────────▼───────────┐             ┌───────────▼───────────┐
        │      ENGINE A         │             │       ENGINE B        │
        │  official GitHub      │             │     nektos/act        │
        │  actions/runner       │             │   (local mirrors)     │
        │  --ephemeral, 1 job   │             │  durable SQLite queue │
        │  fresh container      │             │  replay on reconnect  │
        └───────────────────────┘             └───────────────────────┘
```

**Engine A (default).** Homeplate wraps GitHub's *official* `actions/runner` — it
does not reimplement the protocol. For each job it mints a short-lived
registration token, starts a **fresh container**, registers an **ephemeral**
runner (`--ephemeral` = exactly one job per registration, so state cannot leak
between jobs), runs the job under hard resource caps, then destroys the
container and the workspace.

**Engine B (offline).** When `api.github.com` is unreachable, or the Actions
component on githubstatus.com is in `major_outage`, jobs run locally via `act`
against bare mirrors of your repos. Results land in a durable SQLite queue.

**The brain.** On reconnect it replays queued results in order as commit
statuses, and optionally approves/merges PRs whose local checks passed.
Replay is idempotent at the *storage* layer — a `UNIQUE(job, kind, target)`
constraint — not by hoping. Every replayed result says:

> `ran locally via Homeplate offline mode at 2026-08-06T22:41:07Z`

Homeplate **never** impersonates a GitHub-hosted run.

---

## Quick start

The one-shot version (also the version an AI assistant can run for you —
fully non-interactive, idempotent, JSON-capable):

```bash
brew install homeplate-ci/tap/homeplate
HOMEPLATE_GITHUB_TOKEN=<fine-grained PAT> homeplate auto
# auth → scan your disk for GitHub clones → link what you admin →
# open the adopt PR per repo → install the login daemon. Done.
```

Step by step:

```bash
brew install homeplate-ci/tap/homeplate
homeplate init                  # auth, sized defaults, daemon install
homeplate scan                  # find GitHub clones on this machine (--link --adopt to act)
homeplate link                  # checklist of repos/orgs you can admin
homeplate adopt owner/repo      # one PR to route workflows here (--all for everything)
homeplate status                # queue, caps, sleep state, net $ saved
```

### The 60-second demo

```bash
homeplate init                                  # device flow or --pat
homeplate link                                  # pick a PRIVATE repo
homeplate adopt me/my-app --yes                 # merge the PR it opens
git commit --allow-empty -m "ci: hello" && git push
homeplate status                                # green check ran on YOUR machine

# now the offline trick
sudo ifconfig en0 down                        # pull the plug
git commit --allow-empty -m "offline work"
homeplate run me/my-app                         # runs locally via act
sudo ifconfig en0 up                          # reconnect
homeplate status                                # check appears on GitHub, labelled
```

---

## Commands

| Command | What it does |
|---|---|
| `homeplate auto` | One-shot agent-mode setup: auth → scan → link → adopt → daemon; `--dry-run`, `--json` |
| `homeplate scan` | Find GitHub clones on this machine; `--link`, `--adopt`, `--json` |
| `homeplate init` | Auth, host-sized defaults, daemon install |
| `homeplate auth add <name>` | Add an identity (device flow, or `--pat`, `--host` for GHES) |
| `homeplate auth list` | Show identities and where tokens live |
| `homeplate link [repos...]` | Pick repos/orgs; `--all`, `--orgs` |
| `homeplate adopt <repo>` | PR that routes workflows here; `--all`, `--dry-run`, `--variable` |
| `homeplate status` | Queue, caps, power, engine, **net $ saved** |
| `homeplate limit --cpus 4 --memory 8g` | Change caps (hot-reloaded, no restart); also `--only-when-idle`, `--clamshell`, `--auto-relink` |
| `homeplate power setup` | One-time sudo helper for the managed lid-close toggle; `--revert` removes it |
| `homeplate logs [id]` | Recent jobs, one job's full log, `--follow` the daemon |
| `homeplate run [repo]` | Run a workflow locally now (Engine B) |
| `homeplate pause` / `resume` | Stop/start picking up jobs |
| `homeplate doctor` | Diagnose Docker, power, routing, connectivity, drift |
| `homeplate service install` | launchd agent / systemd user unit |

### Multiple identities

Personal, work, and client orgs coexist. One daemon multiplexes all their
queues; each linked repo remembers which identity owns it.

```bash
homeplate auth add personal
homeplate auth add work --pat
homeplate link --profile work --orgs
```

Tokens live in the **macOS Keychain** or **libsecret** — never plaintext.
Homeplate refuses to silently downgrade: if no credential store exists, it
errors rather than writing a token to disk.

---

## Configuration

`~/.homeplate/config.toml`. Everything is hot-reloadable — `homeplate limit` takes
effect within ~10s, no restart.

```toml
[limits]
max_cpus = 4                  # default: half your cores  -> docker --cpus
max_memory = "12g"            # default: half your RAM    -> docker --memory
max_disk_gb = 50
max_concurrent_jobs = 1
job_timeout = "6h0m0s"

[power]
pause_below_battery_pct = 20  # pause pickup under 20% when unplugged
run_on_battery = true
hold_sleep_assertion = true   # caffeinate / systemd-inhibit while work exists
allow_clamshell_pmset = false # opt in to `sudo pmset disablesleep 1`

[sync]
poll_interval = "30s"
status_poll_interval = "2m0s"
auto_approve = false          # opt-in: approve PRs whose local checks passed
auto_merge = false            # requires auto_approve
merge_method = "squash"
offline_fallback = true

[engine]
default_image = "catthehacker/ubuntu:act-latest"
host_network = false
```

Homeplate holds a sleep assertion **only** while work exists **and** you are on
AC. Holding it on battery would flatten your laptop, which is hostile.

---

## Security

These are non-negotiable, and the tests enforce them.

**Public repos are default-deny.** Linking one requires an explicit flag:

```bash
homeplate link owner/public-repo --i-understand-public-repo-risk
```

Because on a public repo, **anyone can open a pull request, and that PR's code
can execute on your machine, on your network.** That is a real risk, not a
formality. Set *Settings → Actions → General → Require approval for all
external contributors* first. Homeplate also refuses to auto-approve or
auto-merge any PR originating from a fork.

**Container posture** (asserted in `internal/runner/docker_test.go`):

- `--rm` — destroyed after every job; workspace deleted; token file shredded
- `--network bridge` — **never** host networking by default
- `--security-opt no-new-privileges`, non-root uid 1001
- `--cpus`, `--memory`, **`--memory-swap`** (without the swap cap the memory
  limit is a lie), `--pids-limit 4096` (fork-bomb protection)
- **the Docker socket is never mounted** — it is root-equivalent on the host

**Secrets.** Registration tokens are passed via a read-only bind mount, not env
(`docker inspect` exposes env). Output is masked line-buffered, so a secret
split across two writes still gets redacted. Native macOS jobs get a
**scrubbed environment** — an allowlist — so a workflow cannot read your
`AWS_*`, `NPM_TOKEN`, or `OPENAI_API_KEY`.

**Ephemeral by design.** One job per registration. No reused state, ever.

---

## FAQ

### What happens when my laptop sleeps?

The job is marked `interrupted` with a reason on wake — never silently
dropped. Engine A jobs use GitHub's normal re-run path; Engine B jobs are
automatically re-queued. Homeplate holds a sleep assertion while jobs run on AC,
which prevents *idle* sleep. See [Known limits #2](#2-closing-your-macbooks-lid-will-stop-your-jobs-unless-you-have-an-external-display)
for the honest lid-closed story.

### Is this allowed?

**Yes.** Self-hosted runners are a documented, first-class GitHub feature with
no per-minute charge. You are using GitHub exactly as intended; you are just
declining to rent their CPUs.

### Is the savings counter honest?

It tries very hard to be. Run `homeplate status --explain-savings` for the full
audit trail. Specifically:

- Rates are GitHub's **published paid per-minute prices**, verified 2026-08-06:
  Linux 2-core **$0.006**, Windows 2-core **$0.010**, macOS 3/4-core **$0.062**.
- The famous **1×/2×/10× multipliers apply only to *included free* minutes,
  never to paid rates.** Pricing off multipliers overstates Windows by ~33%.
  Homeplate does not do it. (Paid rates are ~1 : 1.67 : 10.3.)
- Every job rounds **up** to the whole minute, exactly as GitHub bills.
- Cancelled and interrupted jobs are **excluded** — GitHub wouldn't have billed
  a full run for work that didn't happen. Failed jobs **are** counted, because
  GitHub bills those.
- **Public repos are free on hosted runners.** If that's your workload, your
  real saving is $0 and no counter should tell you otherwise.
- If you still have unused included minutes, part of this would have been free.
  Homeplate can't see that balance and says so in the explain output.

### Do I have to edit my workflows?

Once, via `homeplate adopt`, which opens the PR for you. See
[Known limits #1](#1-homeplate-cannot-silently-hijack-runs-on-ubuntu-latest).

### What if nobody's machine is running Homeplate?

Those jobs queue, and GitHub fails them after 24h. Keep a hosted fallback for
anything release-critical. `homeplate doctor` warns when a linked repo has no
online runner.

### Does it work with private repos on the free plan?

Yes — that's the sweet spot. Free-plan private repos get 2,000 included
minutes/month; Homeplate makes that limit irrelevant.

---

## Credits

- **[nektos/act](https://github.com/nektos/act)** (MIT) — powers Engine B,
  offline mode. act is a genuinely impressive piece of work and Homeplate would
  have no offline story without it.
- **[actions/runner](https://github.com/actions/runner)** (MIT) — GitHub's
  official agent. Homeplate wraps it rather than reimplementing the job protocol,
  because fidelity matters more than cleverness.

## Development

```bash
go build ./...
go test ./...                 # auth multiplexing, registration idempotency,
                              # resource caps, offline replay idempotency
go test ./internal/syncbrain  # the replay-never-double-posts guarantee
```

MIT licensed.
