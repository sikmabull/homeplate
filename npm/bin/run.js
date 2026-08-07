#!/usr/bin/env node
// Shim: exec the downloaded Homeplate binary, passing argv through.
const { spawnSync } = require('child_process');
const path = require('path');

const bin = path.join(__dirname, 'homeplate');
if (!require('fs').existsSync(bin)) {
  console.error('homeplate: binary not found — reinstall with `npm rebuild homeplate`');
  process.exit(1);
}
const r = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
process.exit(r.status === null ? 1 : r.status);
