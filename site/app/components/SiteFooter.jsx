export default function SiteFooter() {
  return (
    <footer className="site wrap">
      <a href="https://github.com/sikmabull/homeplate">GitHub</a>
      <a href="https://x.com/MerkleGhost">@MerkleGhost</a>
      <a href="/#how">How it works</a>
      <a href="/about/">About</a>
      <span>Made during a GitHub outage.</span>
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img src="/assets/logo/mark-primary.svg" alt="" width="11" height="11" />
    </footer>
  );
}
