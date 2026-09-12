'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ELECTRON = path.join(__dirname, '..');
const REPO = path.join(ELECTRON, '..');
const read = (p) => fs.readFileSync(path.join(REPO, p), 'utf8');

const DOCS = ['README.md', 'DESIGN.md', 'ROADMAP.md', 'android/README.md', 'electron/README.md'];

/**
 * Documentation, checked the way code is.
 *
 * Everything asserted here is a claim a reader will act on: a command they will
 * type, a file they will open, a table they will trust instead of reading the
 * source. Prose rots quietly — the Android README still said the app had never
 * run on a phone months after it had — so the claims that *can* be mechanically
 * tied to the code are tied to it here.
 */

/** The targets a Makefile actually defines. */
function targetsOf(makefile) {
  const src = read(makefile);
  return new Set([...src.matchAll(/^\.PHONY:\s*(.+)$/gm)].flatMap((m) => m[1].trim().split(/\s+/)));
}

/**
 * Every `make <target>` a document tells the reader to *run*.
 *
 * Only code — fenced blocks and inline spans — counts: prose says things like
 * "make replays fail closed", and a checker that reads those is a checker
 * nobody can leave switched on.
 */
function invocations(doc) {
  const lines = read(doc).split('\n');
  const found = [];
  let inFence = false;
  let paragraph = '';
  for (const line of lines) {
    if (line.startsWith('```')) {
      inFence = !inFence;
      continue;
    }
    paragraph = line.trim() === '' ? '' : `${paragraph} ${line}`;
    // Inside a fence the whole line is a command; outside it, only the parts
    // wrapped in backticks are.
    const code = inFence ? line : [...line.matchAll(/`([^`]+)`/g)].map((m) => m[1]).join(' ; ');
    for (const m of code.matchAll(/\bmake\s+(?:-C\s+(\S+)\s+)?([a-z][a-z0-9_.-]*)/g)) {
      found.push({ dir: m[1] || null, target: m[2], context: paragraph });
    }
  }
  return found;
}

test('every make target the docs tell you to run exists', () => {
  const rootTargets = targetsOf('Makefile');
  const electronTargets = targetsOf('electron/Makefile');
  let checked = 0;

  for (const doc of DOCS) {
    const home = doc.startsWith('electron/') ? electronTargets : rootTargets;
    for (const { dir, target, context } of invocations(doc)) {
      checked++;
      // A doc may point the reader at the other Makefile, as electron/README.md
      // does when it lists what the repository root offers.
      const fromRoot = /repository root|from the root/i.test(context) || target.startsWith('electron-');
      const targets = dir ? targetsOf(path.join(dir, 'Makefile')) : fromRoot ? rootTargets : home;
      assert.ok(targets.has(target), `${doc} says "make ${dir ? `-C ${dir} ` : ''}${target}", which no Makefile defines`);
    }
  }
  // A silent pass because the extraction stopped matching would be worse than a
  // failure, so the checker proves it still finds the commands.
  assert.ok(checked > 20, `only ${checked} make commands found in the docs`);
});

test('the help each Makefile prints describes targets and variables it has', () => {
  // `make help` is the documentation most people read, and it is the copy most
  // likely to be left behind: it once offered "make shots DIR=…" for a variable
  // called SHOTS_DIR.
  for (const makefile of ['Makefile', 'electron/Makefile']) {
    const src = read(makefile);
    const targets = targetsOf(makefile);
    const variables = new Set([...src.matchAll(/^([A-Z_]+)\s*[:?]?=/gm)].map((m) => m[1]));
    const help = [...src.matchAll(/@echo "([^"]*)"/g)].map((m) => m[1]).join('\n');
    assert.ok(help.length > 100, `${makefile} prints no help worth checking`);

    for (const m of help.matchAll(/\bmake\s+([a-z][a-z0-9_.-]*)/g)) {
      assert.ok(targets.has(m[1]), `${makefile}'s help offers "make ${m[1]}", which it does not define`);
    }
    // "make run | dev" and "make deb-arm64 | deb-armhf | debs" list alternatives.
    for (const m of help.matchAll(/\|\s*([a-z][a-z0-9_-]*)/g)) {
      assert.ok(targets.has(m[1]), `${makefile}'s help offers "${m[1]}", which it does not define`);
    }
    for (const m of help.matchAll(/\b([A-Z][A-Z_]+)=/g)) {
      assert.ok(variables.has(m[1]), `${makefile}'s help mentions ${m[1]}=, which it never defines`);
    }
  }
});

test('every npm script the docs mention is defined', () => {
  const scripts = new Set(Object.keys(JSON.parse(read('electron/package.json')).scripts));
  for (const doc of DOCS) {
    for (const m of read(doc).matchAll(/npm run ([a-z][a-z0-9:-]*)/g)) {
      assert.ok(scripts.has(m[1]), `${doc} says "npm run ${m[1]}", which package.json does not define`);
    }
  }
});

test('every file a document links to or names in a layout tree is really there', () => {
  for (const doc of DOCS) {
    const dir = path.dirname(path.join(REPO, doc));
    const src = read(doc);
    for (const m of src.matchAll(/\]\(([^)]+)\)/g)) {
      const target = m[1];
      if (/^(https?:|#|mailto:)/.test(target)) continue;
      const [file] = target.split('#');
      assert.ok(fs.existsSync(path.resolve(dir, file)), `${doc} links to ${target}, which does not exist`);
    }
  }
});

test('the layout tree in each README names files that exist', () => {
  const trees = [
    ['electron/README.md', ELECTRON],
    ['android/README.md', path.join(REPO, 'android', 'app', 'src', 'main', 'java', 'com', 'nexusriot', 'rezoagwe')],
  ];
  for (const [doc, base] of trees) {
    const src = read(doc);
    // Lines of a box-drawing tree that name a file with an extension.
    for (const m of src.matchAll(/^[│├└─\s]+([A-Za-z0-9_.-]+\.(?:js|mjs|kt|sh|json))\s{2,}/gm)) {
      const name = m[1];
      const hits = [];
      const walk = (dir) => {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
          if (entry.name === 'node_modules' || entry.name === 'build' || entry.name.startsWith('.')) continue;
          const full = path.join(dir, entry.name);
          if (entry.isDirectory()) walk(full);
          else if (entry.name === name) hits.push(full);
        }
      };
      walk(base);
      assert.ok(hits.length > 0, `${doc} lists ${name} in its layout, which is not in the tree`);
    }
  }
});

test('the message kinds in DESIGN.md are the ones on the wire', () => {
  // The table is what a second implementation is written from; a kind that
  // drifted here would be read as protocol.
  const { Kind } = require('../src/proto/wire');
  const design = read('DESIGN.md');
  const documented = new Map();
  for (const m of design.matchAll(/^\|\s*(\d+)\s*\|\s*`(\w+)`\s*\|/gm)) documented.set(m[2], Number(m[1]));
  assert.ok(documented.size >= 14, `only ${documented.size} message kinds documented`);

  const actual = new Map();
  for (const [name, value] of Object.entries(Kind)) {
    // KV_BATCH in code is KVBatch in the table: compare on letters alone.
    actual.set(name.replace(/_/g, '').toLowerCase(), value);
  }
  for (const [name, value] of documented) {
    const key = name.toLowerCase();
    assert.ok(actual.has(key), `DESIGN.md documents a message kind "${name}" that the codec does not define`);
    assert.equal(actual.get(key), value, `DESIGN.md gives ${name} the wrong number`);
  }
  assert.equal(documented.size, actual.size, 'a message kind exists that DESIGN.md never documents');
});

test('the HTTP gateway table in DESIGN.md matches the routes the Go server registers', () => {
  const go = read('pkg/discovery/httpapi/httpapi.go');
  const routes = new Set();
  for (const m of go.matchAll(/mux\.HandleFunc\("(\w+) ([^"]+)"/g)) {
    routes.add(`${m[1]} ${m[2].replace(/\{[^}]+\}/, '{key}')}`);
  }
  const design = read('DESIGN.md');
  const table = design.slice(design.indexOf('## 8. HTTP gateway'), design.indexOf('## 9.'));
  const documented = new Set();
  for (const m of table.matchAll(/^\|\s*`(GET|PUT|POST|DELETE)`\s*\|\s*`([^`]+)`/gm)) {
    documented.add(`${m[1]} ${m[2]}`);
  }
  // The table folds "GET /chat; POST sends" into one row, so it is a subset.
  for (const route of documented) {
    assert.ok(routes.has(route), `DESIGN.md documents ${route}, which the server does not serve`);
  }
  for (const route of routes) {
    const [method, urlPath] = route.split(' ');
    const mentioned = [...documented].some((d) => d.endsWith(` ${urlPath}`));
    assert.ok(mentioned, `the server serves ${method} ${urlPath}, which DESIGN.md never documents`);
  }
});

test('the chat commands in the README are the ones the engines answer', () => {
  // Three implementations share this vocabulary; the README is where a user
  // reads it, so it has to be the same list.
  const engine = read('electron/src/core/node-engine.js');
  const help = engine.slice(engine.indexOf('commands: '), engine.indexOf('/del <key>') + 20);
  const fromEngine = new Set([...help.matchAll(/\/(\w+)/g)].map((m) => m[1]));
  assert.ok(fromEngine.size >= 9, `only ${fromEngine.size} commands read out of the help text`);
  const readme = read('README.md');
  const section = readme.slice(readme.indexOf('### Chat commands'), readme.indexOf('### HTTP gateway'));
  const documented = new Set([...section.matchAll(/`\/(\w+)/g)].map((m) => m[1]));

  for (const command of fromEngine) {
    assert.ok(documented.has(command), `/${command} is a command but the README never lists it`);
  }
  for (const command of documented) {
    if (command === 'help') continue; // the engine answers it without listing itself
    assert.ok(fromEngine.has(command), `the README documents /${command}, which no engine answers`);
  }
});

test('the TUI hotkey table matches the keys the controller binds', () => {
  const controller = read('pkg/discovery/controller/controller.go');
  const bound = new Set([...controller.matchAll(/case '(.)':/g)].map((m) => m[1]));
  const readme = read('README.md');
  const table = readme.slice(readme.indexOf('### Hotkeys'), readme.indexOf('### Chat commands'));
  const documented = new Set();
  for (const row of table.split('\n')) {
    if (!row.startsWith('|')) continue;
    const keys = row.slice(0, row.indexOf('|', 1) + 1);
    for (const m of keys.matchAll(/`([a-z?/])`/g)) documented.add(m[1]);
  }

  for (const key of bound) {
    assert.ok(documented.has(key), `the TUI binds "${key}", which the README's hotkey table omits`);
  }
  for (const key of documented) {
    assert.ok(bound.has(key), `the README documents the hotkey "${key}", which nothing binds`);
  }
});

test('the CLI flags the README shows are flags the binaries accept', () => {
  const flags = (file) => new Set(
    [...read(file).matchAll(/flag\.\w+\("([a-z-]+)"/g)].map((m) => m[1]),
  );
  const discovery = flags('cmd/discovery/discovery.go');
  const bootstrap = flags('cmd/bootstrap/bootstrap.go');
  const readme = read('README.md');
  const usage = readme.slice(readme.indexOf('### Usage'), readme.indexOf('### Desktop app'));

  for (const m of usage.matchAll(/rezoagwe-(bootstrap|discovery)([^\n`]*)/g)) {
    const known = m[1] === 'bootstrap' ? bootstrap : discovery;
    for (const flag of m[2].matchAll(/-([a-z-]+)/g)) {
      assert.ok(known.has(flag[1]), `the README passes -${flag[1]} to rezoagwe-${m[1]}, which has no such flag`);
    }
  }
});

test('DESIGN.md sections are numbered in order and its own links resolve', () => {
  const design = read('DESIGN.md');
  const headings = [...design.matchAll(/^## (\d+)\. (.+)$/gm)];
  headings.forEach((h, i) => {
    assert.equal(Number(h[1]), i + 1, `DESIGN.md jumps to section ${h[1]} ("${h[2]}") where ${i + 1} was expected`);
  });
  // GitHub lowercases, drops anything but letters, digits, spaces and hyphens,
  // then turns each remaining space into a hyphen — so "A & B" keeps the gap the
  // "&" left behind and anchors as "a--b".
  const anchors = new Set(headings.map((h) => `#${h[1]}-${h[2].toLowerCase()
    .replace(/[^a-z0-9 -]/g, '').replace(/ /g, '-')}`));
  for (const doc of DOCS) {
    for (const m of read(doc).matchAll(/\]\((?:DESIGN\.md)?(#[\w-]+)\)/g)) {
      if (!m[0].includes('DESIGN.md')) continue;
      assert.ok(anchors.has(m[1]), `${doc} links to DESIGN.md${m[1]}, which is not a heading there`);
    }
  }
});

test('the environment variables the docs offer are read by the scripts', () => {
  // A knob that was renamed in the script and kept in the README is worse than
  // an undocumented one: it looks supported and does nothing.
  const sources = ['scripts/e2e.sh', 'scripts/common.sh', 'e2e/docker-compose.yml']
    .filter((f) => fs.existsSync(path.join(REPO, f)))
    .map(read)
    .join('\n');
  assert.ok(sources.length > 100, 'the e2e scripts are missing');

  const readme = read('README.md');
  const section = readme.slice(readme.indexOf('### End-to-end tests'), readme.indexOf('### Debian packages'));
  const documented = new Set([...section.matchAll(/\b(E2E_[A-Z_]+|KEEP_STACK)=/g)].map((m) => m[1]));
  assert.ok(documented.size >= 2, `only ${documented.size} e2e variables documented`);
  for (const name of documented) {
    // A whole-word match: "E2E_LATE_JOIN" is a substring of "E2E_LATE_JOIN_SEC",
    // so `includes` would wave a truncated name through.
    assert.match(sources, new RegExp(`\\b${name}\\b`), `README.md offers ${name}=, which no e2e script reads`);
  }
});

test('no document still claims the Android app has never run on hardware', () => {
  // The claim outlived the run by months, which is exactly why it is pinned.
  for (const doc of DOCS) {
    const src = read(doc);
    assert.ok(!/not yet on a physical phone/i.test(src), `${doc} still says the app has not run on a phone`);
    assert.ok(!/not on a phone yet/i.test(src), `${doc} still says the app has not run on a phone`);
  }
});
