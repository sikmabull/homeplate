export default function SiteHeader() {
  return (
    <header className="site">
      <div className="wrap bar">
        <div className="brand">
          <a href="/" style={{ display: 'flex', alignItems: 'center', gap: 9, border: 'none' }}>
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src="/assets/logo/mark-primary.svg" alt="Homeplate" width="17" height="17" />
            <span>homeplate</span>
          </a>
        </div>
        <nav className="site">
          <a href="/#how">How it works</a>
          <a href="/#honest">The honest part</a>
          <a href="/docs/">Docs</a>
          <a href="/about/">About</a>
          <a className="gh" href="https://github.com/sikmabull/homeplate">GitHub ↗</a>
        </nav>
      </div>
    </header>
  );
}
