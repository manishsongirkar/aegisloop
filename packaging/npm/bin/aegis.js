#!/usr/bin/env node
// `npx aegisloop <args>` — runs an `aegis` binary found on PATH, otherwise
// builds one (once) from the bundled Go sources and caches it.
'use strict';
const { spawnSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const pkg = require('../package.json');
const isWin = process.platform === 'win32';
const exe = isWin ? '.exe' : '';

function onPath(name) {
  const dirs = (process.env.PATH || '').split(path.delimiter);
  for (const dir of dirs) {
    for (const candidate of [path.join(dir, name + exe)]) {
      try {
        fs.accessSync(candidate, fs.constants.X_OK);
        if (fs.realpathSync(candidate) !== fs.realpathSync(__filename)) return candidate;
      } catch (_) { /* keep looking */ }
    }
  }
  return null;
}

function cachedBinary() {
  const base = process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache');
  const dir = path.join(base, 'aegisloop', pkg.version);
  const bin = path.join(dir, 'aegis' + exe);
  if (fs.existsSync(bin)) return bin;

  const src = path.resolve(__dirname, '..', 'src');
  if (!fs.existsSync(path.join(src, 'go.mod'))) {
    fail('no `aegis` binary on PATH and no bundled sources to build from.\n' +
         'Install it with: go install ./cmd/aegis   (from a checkout of the repository)');
  }
  const probe = spawnSync('go', ['version'], { encoding: 'utf8' });
  if (probe.status !== 0) {
    fail('building aegis requires Go >= 1.22 (https://go.dev/dl/). Install Go or put a prebuilt `aegis` on PATH.');
  }
  fs.mkdirSync(dir, { recursive: true });
  console.error('aegisloop: building the aegis binary (first run only)…');
  const build = spawnSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${pkg.version}`, '-o', bin, './cmd/aegis'], {
    cwd: src,
    stdio: 'inherit',
  });
  if (build.status !== 0) fail('go build failed');
  return bin;
}

function fail(msg) {
  console.error('aegisloop: ' + msg);
  process.exit(3);
}

const bin = onPath('aegis') || cachedBinary();
const res = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
if (res.error) fail(res.error.message);
process.exit(res.status === null ? 1 : res.status);
