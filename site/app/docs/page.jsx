import CopyLine from '../components/CopyLine';
import SiteHeader from '../components/SiteHeader';
import SiteFooter from '../components/SiteFooter';

export const metadata = {
  title: 'Docs — Homeplate',
  description:
    'Install, quickstart, command reference, and agent-mode docs for Homeplate — your machine as your GitHub Actions CI.',
};

const GITHUB = 'https://github.com/sikmabull/homeplate';
const INSTALL = 'brew install homeplate-ci/tap/homeplate';

const AGENT_PROMPT = `Install Homeplate and move all my GitHub Actions CI onto this computer.

1. Install:  brew install homeplate-ci/tap/homeplate
   (or: go install github.com/homeplate-ci/homeplate/cmd/homeplate@latest)
2. Run:      homeplate auto --json
   It authenticates (device flow, or I will paste a fine-grained PAT),
   scans my disk for GitHub clones, links every repo I admin, opens one
   small runs-on PR per repo, and installs the background daemon.
3. Report back the JSON summary: repos linked, PR URLs, daemon status.
4. Do NOT link public repos without asking me first.

Full machine-readable docs: https://homeplate.matthew-a46.workers.dev/llms.txt`;

function Cmd({ cmd, children }) {
  return (
    <div className="step">
      <div className="stepbody" style={{ flexBasis: '100%' }}>
        <div className="term" style={{ maxWidth: 640 }}><CopyLine text={cmd} /></div>
        <p>{children}</p>
      </div>
    </div>
  );
}

export default function Docs() {
  return (
    <div>
      <SiteHeader />

      <section className="wrap">
        <div className="hero" style={{ paddingBottom: 44 }}>
          <h1 style={{ fontSize: 'clamp(40px,7vw,84px)' }}>Docs.</h1>
          <p className="sub">
            Everything in one page: install, the human path, the agent path, and the reference.
          </p>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">01 / Install</div>
          <div className="content">
            <h2>One binary.</h2>
            <Cmd cmd={INSTALL}>macOS (Apple Silicon and Intel) and Linux. Optional but recommended for offline mode: <code>brew install act</code>.</Cmd>
            <Cmd cmd="go install github.com/homeplate-ci/homeplate/cmd/homeplate@latest">No Homebrew? Go works too. Windows is not supported yet.</Cmd>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">02 / Humans</div>
          <div className="content">
            <h2>The four-command path.</h2>
            <Cmd cmd="homeplate init">Authenticate (device flow or <code>--pat</code>), create host-sized defaults, install the login daemon. For GitHub Enterprise: <code>homeplate auth add work --host git.example.com</code>.</Cmd>
            <Cmd cmd="homeplate scan">Discover GitHub clones on this machine. Add <code>--link</code> to register them and <code>--adopt</code> to open the routing PRs, all in one pass.</Cmd>
            <Cmd cmd="homeplate adopt --all">Open one PR per linked repo rewriting <code>runs-on:</code> to Homeplate labels. <code>--variable</code> uses a repo variable instead, so repos flip between hosted and local from settings, no commits. Merge the PRs once; done forever.</Cmd>
            <Cmd cmd="homeplate status">Queue, caps, power state, and net dollars saved. <code>--explain-savings</code> shows the full audit trail.</Cmd>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">03 / Agents</div>
          <div className="content">
            <h2>Hand it to an agent.</h2>
            <p>
              Homeplate is built to be operated by an AI assistant: <code>homeplate auto</code> is fully
              non-interactive, idempotent, and emits JSON. Paste this into your agent of choice:
            </p>
            <div className="term" style={{ maxWidth: 720, marginTop: 14 }}>
              <CopyLine text={AGENT_PROMPT}>
                <span style={{ whiteSpace: 'pre-wrap' }}>{AGENT_PROMPT}</span>
              </CopyLine>
            </div>
            <p style={{ marginTop: 14 }}>
              Agents should read{' '}
              <a href="/llms.txt">llms.txt</a> (machine-readable summary) or the repo’s{' '}
              <a href={GITHUB + '/blob/main/AGENTS.md'}>AGENTS.md</a> (operational runbook: env vars,
              exit behavior, safety flags).
            </p>
            <dl className="caveats" style={{ marginTop: 18 }}>
              <div className="row">
                <dt>Non-interactive</dt>
                <dd><code>auto</code> never prompts. Missing auth → it exits with the exact remediation (<code>HOMEPLATE_GITHUB_TOKEN</code> or <code>homeplate auth add</code>).</dd>
              </div>
              <div className="row">
                <dt>Idempotent</dt>
                <dd>Safe to re-run: link and adopt dedupe, service install is repeatable, mirrors refresh in place.</dd>
              </div>
              <div className="row">
                <dt>Machine-readable</dt>
                <dd><code>--json</code> on <code>auto</code>, <code>scan</code>, and <code>status</code>. Global <code>--profile</code> selects the identity.</dd>
              </div>
              <div className="row">
                <dt>Safe defaults</dt>
                <dd>Public repos require <code>--i-understand-public-repo-risk</code>. Private repos only, unless a human says otherwise.</dd>
              </div>
            </dl>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">04 / Reference</div>
          <div className="content">
            <h2>Commands.</h2>
            <table className="bill">
              <thead>
                <tr><th>Command</th><th>What it does</th></tr>
              </thead>
              <tbody>
                <tr><td><code>homeplate auto</code></td><td className="dim">One-shot setup: auth → scan → link → adopt → daemon. <code>--dry-run</code>, <code>--json</code></td></tr>
                <tr><td><code>homeplate scan</code></td><td className="dim">Find GitHub clones on disk. <code>--link</code>, <code>--adopt</code>, <code>--json</code></td></tr>
                <tr><td><code>homeplate init</code></td><td className="dim">Interactive setup: auth, defaults, daemon</td></tr>
                <tr><td><code>homeplate auth add/list/remove</code></td><td className="dim">Named identities, OS keychain storage, GHES via <code>--host</code></td></tr>
                <tr><td><code>homeplate link</code></td><td className="dim">Pick repos/orgs to serve. <code>--all</code>, <code>--orgs</code></td></tr>
                <tr><td><code>homeplate adopt</code></td><td className="dim">PR rewriting <code>runs-on:</code>. <code>--all</code>, <code>--variable</code>, <code>--dry-run</code></td></tr>
                <tr><td><code>homeplate status</code></td><td className="dim">Queue, caps, power, engine, net $ saved. <code>--explain-savings</code>, <code>--json</code></td></tr>
                <tr><td><code>homeplate limit</code></td><td className="dim">Hot-reloaded caps: <code>--cpus</code>, <code>--memory</code>, <code>--concurrency</code>, <code>--only-when-idle</code>, <code>--clamshell</code></td></tr>
                <tr><td><code>homeplate run</code></td><td className="dim">Run a workflow locally right now (offline engine)</td></tr>
                <tr><td><code>homeplate logs</code></td><td className="dim">Recent jobs, one job’s log, <code>--follow</code> the daemon</td></tr>
                <tr><td><code>homeplate pause / resume</code></td><td className="dim">Stop/start job pickup</td></tr>
                <tr><td><code>homeplate power setup</code></td><td className="dim">One-time sudo helper for the lid-close toggle. <code>--revert</code> removes it</td></tr>
                <tr><td><code>homeplate doctor</code></td><td className="dim">Diagnose Docker, power, routing, connectivity, drift</td></tr>
                <tr><td><code>homeplate service</code></td><td className="dim">Install/remove/status of the login daemon</td></tr>
              </tbody>
            </table>
            <p style={{ marginTop: 16 }}>
              Config lives at <code>~/.homeplate/config.toml</code> (hot-reloaded), rates at{' '}
              <code>~/.homeplate/rates.json</code>, tokens in the OS keychain. Full details in the{' '}
              <a href={GITHUB + '/blob/main/homeplate/README.md'}>repo README</a>.
            </p>
          </div>
        </div>
      </section>

      <SiteFooter />
    </div>
  );
}
