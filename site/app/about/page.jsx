import CopyButton from '../components/CopyButton';
import SiteHeader from '../components/SiteHeader';
import SiteFooter from '../components/SiteFooter';

export const metadata = {
  title: 'About — Homeplate',
  description:
    'Homeplate comes from a hardware background: recycling, reuse, and not depending on outside infrastructure you don’t control.',
};

const X = 'https://x.com/MerkleGhost';
const GITHUB = 'https://github.com/sikmabull/homeplate';

export default function About() {
  return (
    <div>
      <SiteHeader />

      <section className="wrap">
        <div className="hero" style={{ paddingBottom: 48 }}>
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img className="mark" src="/assets/logo/mark-primary.svg" alt="" width="46" height="46" />
          <h1 style={{ fontSize: 'clamp(40px,7vw,84px)' }}>Hardware you already own.</h1>
          <p className="sub">
            Why Homeplate exists: a founder from the hardware world, an allergy to waste,
            and a distrust of dependencies you don’t control.
          </p>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">01 / Origin</div>
          <div className="content">
            <h2>From scrap heaps, not slideware.</h2>
            <p className="lead">
              Homeplate’s founder — <a href={X}>@MerkleGhost</a> — comes from hardware. The kind of
              background where you pull machines out of the recycling stream, open them up, and put
              them back to work. Where a “retired” laptop isn’t e-waste; it’s a perfectly good
              computer someone stopped wanting.
            </p>
            <p className="lead" style={{ marginTop: 16 }}>
              Spend enough years watching working silicon get shredded and you develop two instincts.
              First: <strong>hardware deserves a second life.</strong> The most sustainable machine is
              the one that already exists. Second: <strong>don’t build on dependencies you don’t
              control.</strong> If a critical part of your work happens on someone else’s computer, at
              someone else’s price, at someone else’s uptime — you don’t own your process, you rent it.
            </p>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">02 / The leap</div>
          <div className="content">
            <h2>CI was the same story.</h2>
            <p className="lead">
              Look at how most teams run GitHub Actions: every build rents time on a datacenter
              machine at up to $0.062 a minute — while the developer’s own Mac, often faster than the
              runner they’re renting, idles on the desk. Then GitHub has an outage and everyone just…
              stops. Not because their machines broke. Because the dependency did.
            </p>
            <p className="lead" style={{ marginTop: 16 }}>
              Homeplate is the hardware-world answer: <strong>reuse what you already have.</strong> Your
              machine runs your CI. Your code compiles on your silicon. And when GitHub goes down,
              Homeplate keeps working offline and syncs when it comes back — because a tool that
              dies with its vendor’s status page is exactly the kind of dependency this project
              exists to remove.
            </p>
            <p className="lead" style={{ marginTop: 16 }}>
              The name is the mission: <strong>bring your runners home.</strong>
            </p>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="inner">
          <div className="kicker">03 / Principles</div>
          <div className="content">
            <h2>What that means for the tool.</h2>
            <dl className="caveats">
              <div className="row">
                <dt>Your hardware first</dt>
                <dd>Compute you already paid for gets used before a single rented minute. Caps keep it a good neighbor on your machine.</dd>
              </div>
              <div className="row">
                <dt>No fake claims</dt>
                <dd>Self-hosted isn’t $0 — the $0.002/min private-repo fee is in the UI, the counter, and the docs. A tool about honesty can’t lie on its own homepage.</dd>
              </div>
              <div className="row">
                <dt>Degrade, don’t die</dt>
                <dd>Offline mode runs your workflows with no network at all and replays results later. GitHub being down is an inconvenience, not a stop work order.</dd>
              </div>
              <div className="row">
                <dt>No lock-in</dt>
                <dd>One binary, one config file, standard GitHub features. Uninstall and GitHub never knew you left.</dd>
              </div>
            </dl>
          </div>
        </div>
      </section>

      <section className="wrap block">
        <div className="final">
          <div>
            <h2 style={{ marginBottom: 10 }}>Follow along.</h2>
            <p style={{ margin: 0, maxWidth: '46ch', color: 'rgba(17,17,16,.72)', fontSize: 14 }}>
              <a href={X}>Follow @MerkleGhost on X</a> for the build-in-public version, and
              <a href={GITHUB}> give the repo a star</a> — it’s how a reused laptop beats a datacenter.
            </p>
          </div>
          <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
            <a className="btn" href={X}>Follow on X</a>
            <a className="btn" href={GITHUB}>★ Star on GitHub</a>
          </div>
        </div>
      </section>

      <SiteFooter />
    </div>
  );
}
