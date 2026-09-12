#!/usr/bin/env node
'use strict';

/**
 * A syntax and shape check over every source file, for a repository with no
 * linter installed.
 *
 * It parses each file the way Node would, and refuses the two mistakes that are
 * invisible in a diff and fatal at runtime here: a raw control character in a
 * source file (the digest boundary is meant to be an escape sequence, and a
 * literal NUL turns the file binary to half the toolchain), and a renderer that
 * reaches for Node or for innerHTML.
 */

const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const ROOT = path.join(__dirname, '..');
const problems = [];

function walk(dir, out = []) {
  for (const entry of fs.readdirSync(path.join(ROOT, dir), { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
    const rel = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(rel, out);
    else out.push(rel);
  }
  return out;
}

const files = [...walk('src'), ...walk('renderer'), ...walk('test'), ...walk('scripts')];

for (const file of files) {
  const source = fs.readFileSync(path.join(ROOT, file), 'utf8');

  // eslint-disable-next-line no-control-regex
  const control = source.match(/[\x00-\x08\x0b\x0c\x0e-\x1f]/);
  if (control) {
    const line = source.slice(0, control.index).split('\n').length;
    problems.push(`${file}:${line} contains a raw control character (use an escape sequence)`);
  }

  if (file.endsWith('.js')) {
    try {
      // eslint-disable-next-line no-new
      new vm.Script(source, { filename: file });
    } catch (e) {
      problems.push(`${file}: ${e.message}`);
    }
  }

  if (file.endsWith('.json')) {
    try {
      JSON.parse(source);
    } catch (e) {
      problems.push(`${file}: ${e.message}`);
    }
  }

  if (file.startsWith(`renderer${path.sep}`) && file.endsWith('.js')) {
    if (/\bipcRenderer\b/.test(source)) problems.push(`${file}: the renderer must go through the preload bridge`);
    if (/innerHTML\s*=/.test(source)) problems.push(`${file}: innerHTML would make a peer's chat line executable`);
    if (/\bstyle:\s*'/.test(source)) problems.push(`${file}: the CSP drops inline styles; use a class`);
  }
}

if (problems.length) {
  console.error('check failed:');
  for (const p of problems) console.error(`  ${p}`);
  process.exit(1);
}
console.log(`check ok: ${files.length} files`);
