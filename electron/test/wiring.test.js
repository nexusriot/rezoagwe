'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.join(__dirname, '..');
const read = (p) => fs.readFileSync(path.join(ROOT, p), 'utf8');

const mainSrc = read('src/main.js');
const preloadSrc = read('src/preload.js');
const rendererSrc = read('renderer/renderer.js');
const indexHtml = read('renderer/index.html');
const pkg = JSON.parse(read('package.json'));

/**
 * The seams a unit test usually cannot see: a channel the preload offers but the
 * main process never registered is a button that throws when clicked, and no
 * amount of testing either side alone would catch it.
 */

const channelsIn = (src, pattern) => new Set([...src.matchAll(pattern)].map((m) => m[1]));

test('every channel the preload exposes has a handler in the main process', () => {
  const exposed = channelsIn(preloadSrc, /invoke\('([^']+)'/g);
  const handled = new Set([
    ...channelsIn(mainSrc, /handle\('([^']+)'/g),
    ...channelsIn(mainSrc, /ipcMain\.handle\('([^']+)'/g),
  ]);
  assert.ok(exposed.size >= 18, `the bridge looks too small: ${exposed.size} channels`);
  for (const channel of exposed) {
    assert.ok(handled.has(channel), `the preload calls ${channel}, which nothing handles`);
  }
});

test('every handler the main process registers is reachable from the preload', () => {
  const exposed = channelsIn(preloadSrc, /invoke\('([^']+)'/g);
  const handled = channelsIn(mainSrc, /handle\('([^']+)'/g);
  for (const channel of handled) {
    assert.ok(exposed.has(channel), `${channel} is handled but nothing can call it`);
  }
});

test('every event the main process sends is subscribed to by the preload', () => {
  const sent = channelsIn(mainSrc, /send\('([^']+)'/g);
  const subscribed = channelsIn(preloadSrc, /on\('([^']+)'/g);
  assert.ok(sent.size >= 3, `main.js sends suspiciously few named events: ${sent.size}`);
  for (const channel of sent) {
    assert.ok(subscribed.has(channel), `main sends ${channel}, which the renderer never listens for`);
  }
  // The looped forwards, checked by name against the engine's own events.
  for (const event of ['entries', 'peers', 'chat', 'activity', 'status', 'metrics', 'topology']) {
    assert.ok(subscribed.has(`node:${event}`), `nothing listens for node:${event}`);
  }
});

test('the renderer only reaches the main process through the bridge', () => {
  assert.ok(!/require\(/.test(rendererSrc), 'the renderer is sandboxed: it has no require');
  assert.ok(!/ipcRenderer/.test(rendererSrc), 'the renderer must not touch ipcRenderer directly');
  assert.match(rendererSrc, /window\.rezoagwe/);
});

test('the window is built with the isolation settings that make the bridge safe', () => {
  for (const setting of ['contextIsolation: true', 'nodeIntegration: false', 'sandbox: true', 'webviewTag: false']) {
    assert.ok(mainSrc.includes(setting), `main.js should set ${setting}`);
  }
  assert.match(mainSrc, /setWindowOpenHandler/, 'a new-window request has to be refused or handed to the OS');
  assert.match(mainSrc, /will-navigate/, 'the one local page never navigates');
});

test('the page declares a content security policy with no inline escape hatch', () => {
  const csp = indexHtml.match(/Content-Security-Policy"\s*\n?\s*content="([^"]+)"/);
  assert.ok(csp, 'index.html must carry a CSP');
  assert.match(csp[1], /default-src 'none'/);
  assert.match(csp[1], /script-src 'self'/);
  assert.ok(!csp[1].includes('unsafe-inline'), "'unsafe-inline' would undo the point of the policy");
  assert.ok(!csp[1].includes('unsafe-eval'));
});

test('nothing in the page relies on an inline script or style the CSP would block', () => {
  // A style attribute set from HTML or from h() is dropped silently under this
  // policy, which shows up as a layout that is subtly wrong rather than an error.
  assert.ok(!/<script(?![^>]*src=)/.test(indexHtml), 'no inline <script> in index.html');
  assert.ok(!/\sstyle="/.test(indexHtml), 'no style attributes in index.html');
  assert.ok(!/\bstyle:\s*'/.test(rendererSrc), 'the renderer sets classes, not style attributes');
});

test('the renderer never builds DOM from a string', () => {
  // Values in this app come off the network: a key, a nickname or a chat line is
  // whatever a peer sent, so it is only ever assigned as text.
  assert.ok(!/innerHTML\s*=/.test(rendererSrc), 'innerHTML would make a peer’s chat line executable');
  assert.ok(!/insertAdjacentHTML|outerHTML\s*=/.test(rendererSrc));
});

test('the package ships the files the app needs and none of the ones it does not', () => {
  assert.equal(pkg.main, 'src/main.js');
  assert.ok(pkg.build.files.includes('src/**/*'));
  assert.ok(pkg.build.files.includes('renderer/**/*'));
  assert.ok(pkg.build.files.includes('!test/**'), 'the tests do not belong in a package');
  for (const script of ['start', 'test', 'selftest', 'dist:deb']) {
    assert.ok(pkg.scripts[script], `package.json should define the ${script} script`);
  }
});

test('every file the renderer loads exists', () => {
  for (const match of indexHtml.matchAll(/(?:src|href)="([^"]+)"/g)) {
    const file = path.join(ROOT, 'renderer', match[1]);
    assert.ok(fs.existsSync(file), `index.html loads ${match[1]}, which is not there`);
  }
});

test('the screens the rail offers are the screens the renderer can mount', () => {
  const S = require('../renderer/shared');
  for (const screen of S.SCREENS) {
    assert.ok(
      new RegExp(`screens\\.${screen.id}\\s*=`).test(rendererSrc),
      `the rail offers ${screen.id} with nothing to mount`,
    );
  }
});

test('no source file carries a raw control character', () => {
  // A literal NUL in a source file makes it binary to half the toolchain, and the
  // digest boundary is exactly the place a NUL is meant to be an escape sequence.
  const files = [];
  const walk = (dir) => {
    for (const entry of fs.readdirSync(path.join(ROOT, dir), { withFileTypes: true })) {
      if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
      const rel = path.join(dir, entry.name);
      if (entry.isDirectory()) walk(rel);
      else if (/\.(js|json|html|css|md)$/.test(entry.name)) files.push(rel);
    }
  };
  walk('src');
  walk('renderer');
  walk('test');
  assert.ok(files.length > 10);
  for (const file of files) {
    // eslint-disable-next-line no-control-regex
    assert.ok(!/[\x00-\x08\x0b\x0c\x0e-\x1f]/.test(read(file)), `${file} contains a raw control character`);
  }
});

test('the engine, the store and the codec are reachable without Electron', () => {
  // The protocol half of this app has to be testable and reusable off the main
  // process; a require of electron here would end that.
  for (const file of ['src/core/node-engine.js', 'src/core/kvstore.js', 'src/proto/codec.js', 'src/core/bootstrap-server.js']) {
    assert.ok(!/require\('electron'\)/.test(read(file)), `${file} must not depend on Electron`);
  }
});
