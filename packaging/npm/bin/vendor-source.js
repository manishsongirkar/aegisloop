#!/usr/bin/env node
// Copies the Go sources into ./src so the published package can build the binary.
'use strict';
const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..', '..', '..');
const dest = path.resolve(__dirname, '..', 'src');
fs.rmSync(dest, { recursive: true, force: true });
fs.mkdirSync(dest, { recursive: true });
for (const entry of ['go.mod', 'cmd', 'internal', 'LICENSE']) {
  fs.cpSync(path.join(root, entry), path.join(dest, entry), {
    recursive: true,
    filter: (p) => !p.endsWith('_test.go'),
  });
}
console.log('vendored Go sources into', dest);
