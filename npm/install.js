#!/usr/bin/env node
// Postinstall: download the correct Homeplate binary from the GitHub release,
// verify its SHA-256, and place it next to the bin shim.
const { createHash } = require('crypto');
const { execSync } = require('child_process');
const fs = require('fs');
const https = require('https');
const path = require('path');

const VERSION = '0.1.0';
const REPO = 'sikmabull/homeplate';
const CHECKSUMS = {
  'darwin-arm64': 'bfc888dfbc5b9efc328b0cd5e3a600b3f2cfb3e7ce182af9a25809af1ab6a1ee',
  'darwin-x64':   'ca9760827fe13b278547f8357d0bc1e88ae16b38619968cdfe83535967e9a95f',
  'linux-arm64':  'bc07754e1cc09b9be76941a397fb60f7fa4d5716368d119bfd3aa80d39e6abd9',
  'linux-x64':    '77351850c6be1b5dc2186c4b70564846ae730a5c5029db05cba5cc1959f28da6',
};

const key = `${process.platform}-${process.arch}`;
const want = CHECKSUMS[key];
if (!want) {
  console.error(`homeplate: unsupported platform ${key} (macOS/Linux, arm64/x64 only)`);
  process.exit(1);
}

const url = `https://github.com/${REPO}/releases/download/v${VERSION}/homeplate_${VERSION}_${key.replace('-x64', '-amd64')}.tar.gz`;
const dest = path.join(__dirname, 'bin');
const tarball = path.join(dest, 'homeplate.tar.gz');

function get(u, cb) {
  https.get(u, { headers: { 'User-Agent': 'homeplate-npm-installer' } }, (res) => {
    if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
      return get(res.headers.location, cb);
    }
    if (res.statusCode !== 200) return cb(new Error(`download failed: HTTP ${res.statusCode}`));
    cb(null, res);
  }).on('error', cb);
}

get(url, (err, res) => {
  if (err) { console.error('homeplate:', err.message); process.exit(1); }
  const hash = createHash('sha256');
  const out = fs.createWriteStream(tarball);
  res.on('data', (c) => hash.update(c));
  res.pipe(out);
  out.on('finish', () => {
    out.close(() => {
      const got = hash.digest('hex');
      if (got !== want) {
        fs.unlinkSync(tarball);
        console.error(`homeplate: checksum mismatch (got ${got})`);
        process.exit(1);
      }
      try {
        execSync(`tar xzf "${tarball}" -C "${dest}"`, { stdio: 'inherit' });
        fs.chmodSync(path.join(dest, 'homeplate'), 0o755);
        fs.unlinkSync(tarball);
      } catch (e) {
        console.error('homeplate: extraction failed:', e.message);
        process.exit(1);
      }
      console.log(`homeplate ${VERSION} installed. Run: homeplate auto`);
    });
  });
});
