'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const {
  DEFAULTS, SettingsStore, sanitize, seedList, toNodeConfig, toBootstrapConfig,
  nodeNeedsRestart, bootstrapNeedsRestart, refusals,
} = require('../src/core/settings');
const { Persister } = require('../src/core/persistence');
const { Runtime } = require('../src/core/runtime');

const tmpdir = () => fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-settings-'));

test('the cluster name and the key are trimmed, because a stray space changes the framing key', () => {
  // A pasted key brings a trailing newline along, and the node then stops
  // talking to every peer with nothing on screen to show why.
  const s = sanitize({ ...DEFAULTS, psk: '  s3cret\n', cluster: ' home ', nick: '  alice ' });
  assert.equal(s.psk, 's3cret');
  assert.equal(s.cluster, 'home');
  assert.equal(s.nick, 'alice');
});

test('a blank nickname or cluster falls back rather than becoming empty', () => {
  const s = sanitize({ ...DEFAULTS, nick: '   ', cluster: '' });
  assert.equal(s.nick, DEFAULTS.nick);
  assert.equal(s.cluster, DEFAULTS.cluster);
});

test('an impossible port is refused in favour of the previous one', () => {
  const previous = { ...DEFAULTS, port: 3137 };
  assert.equal(sanitize({ ...previous, port: 0 }, previous).port, 3137);
  assert.equal(sanitize({ ...previous, port: 70000 }, previous).port, 3137);
  assert.equal(sanitize({ ...previous, port: 'abc' }, previous).port, 3137);
  assert.equal(sanitize({ ...previous, port: 4000 }, previous).port, 4000);
});

test('seeds parse as a comma-separated list with the whitespace off', () => {
  assert.deepEqual(seedList(' a:1, b:2 ,, c:3 '), ['a:1', 'b:2', 'c:3']);
  assert.deepEqual(seedList(''), []);
  assert.deepEqual(seedList(undefined), []);
});

test('a negative tombstone age reads as "never", not as an instant GC', () => {
  assert.equal(sanitize({ ...DEFAULTS, tombstoneTtlSec: -5 }).tombstoneTtlSec, 0);
  assert.equal(sanitize({ ...DEFAULTS, tombstoneTtlSec: '90' }).tombstoneTtlSec, 90);
});

test('the settings map onto the two role configs', () => {
  const s = sanitize({ ...DEFAULTS, nick: 'me', port: 4000, seeds: 'a:1,b:2', psk: 'k', cluster: 'home', bootstrapPort: 9000 });
  assert.deepEqual(toNodeConfig(s), {
    advertiseHost: '', port: 4000, nick: 'me', seeds: ['a:1', 'b:2'], psk: 'k', cluster: 'home', tombstoneTtlSec: 0,
  });
  assert.deepEqual(toBootstrapConfig(s), { port: 9000, psk: 'k', cluster: 'home' });
});

test('only the settings baked into a socket or a codec force a restart', () => {
  const base = sanitize(DEFAULTS);
  const changed = (partial) => nodeNeedsRestart(base, sanitize({ ...base, ...partial }, base));
  assert.ok(changed({ port: 4000 }), 'the port is the bound socket');
  assert.ok(changed({ psk: 'new' }), 'the key is baked into the codec');
  assert.ok(changed({ cluster: 'other' }));
  assert.ok(changed({ advertiseHost: '10.0.0.9' }), 'what peers are told to dial is part of every packet');
  assert.ok(changed({ seeds: 'x:1' }));
  assert.ok(changed({ tombstoneTtlSec: 60 }));
  assert.ok(!changed({ nick: 'renamed' }), 'a rename is a protocol message, not a restart');
  assert.ok(!changed({ autoStartNode: false }));

  assert.ok(bootstrapNeedsRestart(base, sanitize({ ...base, bootstrapPort: 9001 }, base)));
  assert.ok(bootstrapNeedsRestart(base, sanitize({ ...base, psk: 'x' }, base)));
  assert.ok(!bootstrapNeedsRestart(base, sanitize({ ...base, nick: 'x' }, base)));
});

test('settings survive a restart and an unreadable file falls back to defaults', () => {
  const dir = tmpdir();
  const file = path.join(dir, 'settings.json');
  const store = new SettingsStore(file);
  store.save({ nick: 'persisted', port: 4321 });

  const reopened = new SettingsStore(file);
  assert.equal(reopened.get().nick, 'persisted');
  assert.equal(reopened.get().port, 4321);

  fs.writeFileSync(file, '{ this is not json');
  assert.equal(new SettingsStore(file).get().nick, DEFAULTS.nick);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a saved state file is replaced atomically, never truncated', async () => {
  const dir = tmpdir();
  const file = path.join(dir, 'node.json');
  const persister = new Persister(file);
  persister.save(1, { node_id: 'a', clock: 1, entries: {} });
  await persister.flush();
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).node_id, 'a');
  assert.ok(!fs.existsSync(`${file}.tmp`), 'the temp file is renamed, not left behind');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('an out-of-order save cannot regress the file', async () => {
  const dir = tmpdir();
  const file = path.join(dir, 'node.json');
  const persister = new Persister(file);
  persister.save(5, { clock: 5, entries: {} });
  persister.save(3, { clock: 3, entries: {} }); // a slow writer arriving late
  await persister.flush();
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).clock, 5);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a burst of saves collapses to the newest snapshot', async () => {
  const dir = tmpdir();
  const file = path.join(dir, 'node.json');
  const persister = new Persister(file);
  for (let i = 1; i <= 20; i++) persister.save(i, { clock: i, entries: {} });
  await persister.flush();
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).clock, 20);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a missing file loads as null and a corrupt one throws for the caller to handle', () => {
  const dir = tmpdir();
  assert.equal(new Persister(path.join(dir, 'nothing.json')).load(), null);
  const bad = path.join(dir, 'bad.json');
  fs.writeFileSync(bad, 'nonsense');
  assert.throws(() => new Persister(bad).load());
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a write failure is reported rather than thrown at a mutation site', async () => {
  // A regular file standing where the directory should be: the write cannot
  // succeed, and a mutation that already happened in memory must not unwind.
  const dir = tmpdir();
  const blocker = path.join(dir, 'not-a-directory');
  fs.writeFileSync(blocker, 'in the way');
  const errors = [];
  const persister = new Persister(path.join(blocker, 'node.json'), { onError: (e) => errors.push(e) });
  persister.save(1, { clock: 1 });
  await persister.flush();
  assert.equal(errors.length, 1, 'the write failed and said so');
  assert.ok(['EEXIST', 'ENOTDIR'].includes(errors[0].code), `unexpected failure: ${errors[0].code}`);
  fs.rmSync(dir, { recursive: true, force: true });
});

// Autostart runs before the window has finished loading, so the toast `guard()`
// emits for a failed bind goes to a renderer that does not exist yet and is
// dropped. The snapshot the renderer pulls once it *is* ready has to carry the
// error across that gap, or the app opens on "STOPPED" with no reason anywhere
// — which is what a user hits the first time the configured port is already
// taken, and the one case they are least able to guess.
test('the snapshot carries a start-up failure the renderer was not there to hear', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-runtime-'));
  const runtime = new Runtime(dir, { listen: async () => { throw new Error('bind EADDRINUSE 0.0.0.0:3137'); } });

  assert.equal(runtime.snapshot().lastError, '', 'nothing has failed yet');

  const result = await runtime.startNode();
  assert.equal(result.ok, false);
  assert.match(runtime.snapshot().lastError, /EADDRINUSE/);

  // And it clears once something works, so a stale error cannot haunt the UI.
  runtime.opts.listen = async () => ({
    on() {}, send() {}, close() {}, address: () => ({ port: 3137 }),
  });
  await runtime.guard(async () => {});
  assert.equal(runtime.snapshot().lastError, '');

  fs.rmSync(dir, { recursive: true, force: true });
});

// Refusing a value is only half an answer. The form repaints from what comes
// back, so a refused port simply snaps back while the toast says "Settings
// applied" — which reads as the field not working rather than the value being
// wrong, and teaches people to stop trusting the screen.
test('a value that cannot be kept is named, and a good one is not', () => {
  const kept = { ...DEFAULTS, port: 3137, bootstrapPort: 9999, nick: 'desk', cluster: 'lab' };
  assert.deepEqual(refusals(kept, DEFAULTS), [], 'a settings screen full of valid values says nothing');

  // 0 binds an ephemeral port and the node then advertises an address no peer
  // can dial; 99999 cannot bind at all. Neither fails usefully on its own.
  assert.match(refusals({ ...kept, port: 0 }, kept)[0], /node port 0 .*kept 3137/);
  assert.match(refusals({ ...kept, port: 99999 }, kept)[0], /node port 99999 .*kept 3137/);
  assert.match(refusals({ ...kept, bootstrapPort: 0 }, kept)[0], /bootstrap port 0 .*kept 9999/);
  assert.match(refusals({ ...kept, nick: '   ' }, kept)[0], /nickname cannot be blank/);
  assert.match(refusals({ ...kept, cluster: '' }, kept)[0], /cluster name cannot be blank/);

  // Every message has to name a value the user can act on: "kept 3137" is the
  // half that says what is actually in effect now.
  assert.equal(refusals({ ...kept, port: 0, nick: '' }, kept).length, 2);
});

test('trimming is not reported, because it is what the user wanted', () => {
  // The screen documents it, and naming it on every paste would bury the two
  // messages that matter.
  const kept = { ...DEFAULTS, port: 3137, bootstrapPort: 9999, nick: 'desk', cluster: 'lab' };
  assert.deepEqual(refusals({ ...kept, psk: ' secret ', cluster: ' lab ' }, kept), []);
  assert.equal(sanitize({ ...kept, psk: ' secret ' }, kept).psk, 'secret', 'still trimmed, just not announced');
});

test('applying a refused value warns before it stores anything', async () => {
  // The warning is the only thing standing between a user and a port field that
  // appears to do nothing.
  const dir = tmpdir();
  const runtime = new Runtime(dir, { listen: async () => { throw new Error('no sockets in this test'); } });
  const warnings = [];
  runtime.on('warning', (w) => warnings.push(w));
  const before = runtime.settings.port;
  const after = await runtime.applySettings({ ...runtime.settings, port: 0 });
  assert.equal(after.port, before, 'the refused value is not stored');
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /Settings: node port 0 is not a usable port/);
  fs.rmSync(dir, { recursive: true, force: true });
});
