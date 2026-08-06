import CopyButton from './components/CopyButton';
import CopyLine from './components/CopyLine';
import SiteHeader from './components/SiteHeader';
import SiteFooter from './components/SiteFooter';

const INSTALL = 'brew install homeplate-ci/tap/homeplate';
const GITHUB = 'https://github.com/sikmabull/homeplate';

export default function Page() {
  return (
    <div>
      <SiteHeader />

      <section className="wrap">
        <div className="hero">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img className="mark" src="/assets/logo/mark-primary.svg" alt="" width="46" height="46" />
          <h1>Bring your runners home.</h1>
          <p className="sub">
            GitHub Actions, running on your own machine. Green checks on your PRs.
            Up to 31x cheaper than hosted macOS — and it keeps working when GitHub doesn’t.
          </p>
          <div className="cta-row">
            <CopyButton text={INSTALL} />
            <a className="star" href={GITHUB}>Star on GitHub</a>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">01 / Cost</div>
          <div className="content">
            <h2>The bill, itemized.</h2>
            <table className="bill">
              <thead>
                <tr>
                  <th>Runner</th>
                  <th className="r" style={{ width: 140 }}>Per minute</th>
                </tr>
              </thead>
              <tbody>
                <tr><td>GitHub-hosted · Linux, 2-core</td><td className="r">$0.006</td></tr>
                <tr><td>GitHub-hosted · Windows, 2-core</td><td className="r">$0.010</td></tr>
                <tr><td>GitHub-hosted · macOS (M1), 3-core</td><td className="r">$0.062</td></tr>
                <tr className="you"><td>Your machine · private repos</td><td className="r">$0.002</td></tr>
                <tr className="you"><td>Your machine · public repos</td><td className="r">$0.000</td></tr>
              </tbody>
            </table>
            <p>
              Self-hosted isn’t $0 — since March 2026 GitHub charges a $0.002/min control-plane fee on
              private repos, and Homeplate’s savings counter subtracts it before claiming anything.
              What’s left is still up to 31x cheaper. macOS runners cost what they do because Apple
              requires real Mac hardware. You own real Mac hardware. It’s asleep right now.
            </p>
          </div>
        </div>
      </section>

      <section className="wrap block" id="how">
        <div className="inner">
          <div className="kicker">02 / Setup</div>
          <div className="content">
            <h2>One command, then nothing.</h2>

            <div className="step">
              <div className="stepnum">1</div>
              <div className="stepbody">
                <div className="term"><CopyLine text={INSTALL} /></div>
                <p>One binary, no runtime. Works on macOS (Apple Silicon and Intel) and Linux.</p>
              </div>
            </div>

            <div className="step">
              <div className="stepnum">2</div>
              <div className="stepbody">
                <div className="term"><CopyLine text="homeplate auto" /></div>
                <p>
                  Auth, scan your disk for GitHub clones, link every repo you admin, open the
                  one-line <code>runs-on:</code> PR per repo, install the background daemon. Fully
                  non-interactive — an AI assistant can run it for you. Step by step instead:
                  <code> init → scan → adopt</code>.
                </p>
              </div>
            </div>

            <div className="step">
              <div className="stepnum">3</div>
              <div className="stepbody">
                <div className="term">
                  <div className="line"><span className="prompt">$</span><span>git push</span></div>
                  <div className="out">
                    <div><span className="ok">✓</span> build (macOS · local) — 41s</div>
                    <div><span className="ok">✓</span> test (linux · local container) — 1m 12s</div>
                  </div>
                </div>
                <p>
                  Jobs run in clean, resource-capped, one-job ephemeral containers on your machine.
                  Checks appear on the PR like nothing changed — because to GitHub, nothing did.
                </p>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">03 / Offline</div>
          <div className="content">
            <h2>Works when GitHub doesn’t.</h2>
            <p className="lead">
              GitHub Actions went down twice in the last week. Homeplate’s offline mode — built on the
              excellent <a href="https://github.com/nektos/act">nektos/act</a> — keeps running your
              workflows locally, on battery, with no wifi, and posts the results as commit statuses the
              moment you’re back online.
            </p>
            <div className="term" style={{ marginTop: 20 }}>
              <div className="line"><span className="prompt">$</span><span>homeplate status</span></div>
              <div className="out">
                <div>github.com — unreachable</div>
                <div>mode — offline (act)</div>
                <div><span className="ok">✓</span> 3 workflows run · 2 queued for sync</div>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section className="wrap block" id="honest">
        <div className="inner">
          <div className="kicker">04 / Caveats</div>
          <div className="content">
            <h2>The honest part.</h2>
            <dl className="caveats">
              <div className="row">
                <dt>Not actually $0</dt>
                <dd>Private repos bill $0.002/min for self-hosted runtime. Public repos are free. The savings counter shows the net, never the gross.</dd>
              </div>
              <div className="row">
                <dt>One PR per repo</dt>
                <dd>GitHub reserves the hosted labels (<code>ubuntu-latest</code>…), so <code>runs-on:</code> has to change. <code>homeplate adopt</code> opens that PR — or a repo-variable variant you can flip without new commits.</dd>
              </div>
              <div className="row">
                <dt>Fork PRs</dt>
                <dd>On a public repo, a fork PR is a stranger’s code on your machine. Off by default. Turn it on only if you know exactly what that means.</dd>
              </div>
              <div className="row">
                <dt>Lid closed</dt>
                <dd>macOS sleeps on lid close unless you attach an external display — or enable Homeplate’s managed toggle (<code>homeplate power setup</code>), which reverts itself when the work is done.</dd>
              </div>
              <div className="row">
                <dt>macOS sandboxing</dt>
                <dd>Linux jobs get hard container caps. macOS-native jobs (xcodebuild) get an ephemeral workspace and scheduling hints — soft limits, labelled SOFT in <code>homeplate status</code>. VM isolation is the v2 plan.</dd>
              </div>
              <div className="row">
                <dt>Your fans</dt>
                <dd>Ships with CPU, memory, and disk caps at half your machine. Raise them in <code>~/.homeplate/config.toml</code> if you want the whole machine.</dd>
              </div>
            </dl>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="final">
          <h2>Safe at home.</h2>
          <CopyButton text={INSTALL} />
        </div>
      </section>

      <SiteFooter />
    </div>
  );
}
