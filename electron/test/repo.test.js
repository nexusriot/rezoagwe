'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const REPO = path.join(__dirname, '..', '..');
const read = (p) => fs.readFileSync(path.join(REPO, p), 'utf8');

/**
 * Facts about the repository itself: the version it claims and the files it
 * keeps.
 *
 * Both rot in the same quiet way. `build-deb.sh` sat at 0.0.3 for two releases
 * because nothing compared it to the Makefile, and an ignore rule that stops
 * matching shows up as a build artifact in a diff — or, worse, as a source file
 * nobody can see.
 */

const versions = () => ({
  'Makefile': read('Makefile').match(/^VERSION\s*\?=\s*(\S+)/m)[1],
  'electron/package.json': JSON.parse(read('electron/package.json')).version,
  'electron/package-lock.json': JSON.parse(read('electron/package-lock.json')).version,
  'android/app/build.gradle.kts': read('android/app/build.gradle.kts').match(/versionName\s*=\s*"([^"]+)"/)[1],
});

test('every part of the repository claims the same version', () => {
  const found = versions();
  const distinct = new Set(Object.values(found));
  assert.equal(distinct.size, 1,
    `the version has drifted: ${Object.entries(found).map(([k, v]) => `${k}=${v}`).join(', ')}`);
  assert.match([...distinct][0], /^\d+\.\d+\.\d+(-\S+)?$/, 'the version is not a version');
});

test('the version the documentation states is the version the repository is at', () => {
  // A number in prose is a number nothing updates, so it is checked rather than
  // trusted.
  const current = versions()['Makefile'];
  const roadmap = read('ROADMAP.md');
  const stated = roadmap.match(/repository is at \*\*([^*]+)\*\*/);
  assert.ok(stated, 'ROADMAP.md no longer states the version');
  assert.equal(stated[1], current, 'ROADMAP.md states a version the repository has moved past');
});

test('the lockfile agrees with the package it locks', () => {
  const pkg = JSON.parse(read('electron/package.json'));
  const lock = JSON.parse(read('electron/package-lock.json'));
  assert.equal(lock.name, pkg.name, 'the lockfile was left behind by a rename');
  assert.equal(lock.packages[''].version, pkg.version, 'the lockfile root still carries the old version');
  assert.deepEqual(lock.packages[''].devDependencies, pkg.devDependencies);
});

test('the Android versionCode moves with the version name', () => {
  // An unchanged versionCode makes the new build refuse to install over the old
  // one, which looks like a broken APK rather than a packaging slip.
  const gradle = read('android/app/build.gradle.kts');
  const code = Number(gradle.match(/versionCode\s*=\s*(\d+)/)[1]);
  const name = gradle.match(/versionName\s*=\s*"([^"]+)"/)[1];
  const [major, minor] = name.split('.').map(Number);
  assert.ok(code >= major * 10 + minor, `versionCode ${code} is behind versionName ${name}`);
});

/** git, if this is a checkout and git is installed; the ignore rules are unverifiable otherwise. */
function gitAvailable() {
  if (!fs.existsSync(path.join(REPO, '.git'))) return false;
  return spawnSync('git', ['--version'], { encoding: 'utf8' }).status === 0;
}

const ignored = (p) => spawnSync('git', ['check-ignore', '-q', p], { cwd: REPO }).status === 0;

test('everything a build generates is ignored', { skip: !gitAvailable() && 'not a git checkout' }, () => {
  const generated = [
    'dist/x', 'build/x', 'x.deb', 'x.test', 'coverage.out', 'debug.log',
    // `make clean` names these, so a build can leave them in the root.
    'rezoagwe-bootstrap', 'rezoagwe-discovery', 'rezoagwectl',
    // and this is what `go build ./cmd/...` drops without -o
    'bootstrap', 'discovery', 'ctl', 'cpu.prof', 'coverage.html',
    // what a node run from the checkout leaves behind: journal, retained
    // corruption evidence, profile-migration markers, rotated snapshots
    'node.json.wal', 'node.json.corrupt-1761400000000', 'node.json.corrupt-1-x.wal',
    'node.json.profile-owner', 'node.json.migrated', 'rezoagwe-20261025-120000.json',
    'android/captures/x', 'android/app/release/app-release.apk', 'release.jks', 'debug.keystore',
    'android/.gradle/x', 'android/app/build/x', 'android/local.properties', 'android/.kotlin/x',
    'electron/node_modules/x', 'electron/dist/x', 'electron/shots/keys.png',
    'electron/build/icon.png', 'electron/build/icons/16x16.png',
    '.idea/x', '.vscode/settings.json', '.DS_Store',
  ];
  for (const file of generated) {
    assert.ok(ignored(file), `.gitignore does not cover ${file}`);
  }
});

test('nothing a person wrote is ignored', { skip: !gitAvailable() && 'not a git checkout' }, () => {
  const source = [
    'README.md', 'DESIGN.md', 'ROADMAP.md', '.dockerignore',
    'Makefile', 'build-deb.sh', 'DEBIAN/control',
    'cmd/bootstrap/bootstrap.go', 'cmd/discovery/discovery.go', 'pkg/proto/wire.go',
    'e2e/go.mod', 'e2e/docker-compose.yml', 'scripts/e2e.sh',
    'android/app/build.gradle.kts', 'android/gradle/wrapper/gradle-wrapper.jar',
    'electron/package.json', 'electron/package-lock.json', 'electron/Makefile',
    'electron/build-deb.sh', 'electron/src/main.js', 'electron/renderer/index.html',
    // fixtures and sources sharing a name or suffix with a runtime rule above
    'testdata/journal.json', 'testdata/boundaries.json', 'pkg/discovery/model/journal.go',
    'electron/src/core/profiles.js',
    // The repository-wide build/ rule swallows electron/build, where a packaging
    // resource would live; the negation that rescues it is easy to lose.
    'electron/build/entitlements.mac.plist',
  ];
  for (const file of source) {
    assert.ok(!ignored(file), `.gitignore hides ${file}`);
  }
});

test('no build artifact is tracked', { skip: !gitAvailable() && 'not a git checkout' }, () => {
  const tracked = spawnSync('git', ['ls-files'], { cwd: REPO, encoding: 'utf8' }).stdout.split('\n');
  const artifacts = tracked.filter((f) => /\.(deb|apk|zip|exe|so|dylib|class)$|(^|\/)(dist|node_modules)\//.test(f));
  assert.deepEqual(artifacts, [], 'a build artifact is committed');
  // The Gradle wrapper jar is the one binary that belongs in a checkout.
  const jars = tracked.filter((f) => f.endsWith('.jar'));
  assert.deepEqual(jars, ['android/gradle/wrapper/gradle-wrapper.jar']);
});

/**
 * Nothing a person typed into the Android app may be held in state that a
 * configuration change throws away.
 *
 * Measured on a tablet: rotating the device dismissed the open key dialog
 * outright and took the key, the value and the TTL with it; emptied the chat
 * composer mid-sentence; and reverted every unsaved field on the settings
 * screen. The navigation state was already `rememberSaveable` — only the forms
 * were not — so the app came back on the right screen with every edit gone,
 * which reads as the app having discarded the work on purpose.
 *
 * A tablet is the device that gets rotated, and `remember` is the default
 * anyone reaches for, so this is checked rather than remembered. It looks only
 * at state that holds typed text or says which editor is open; ephemeral view
 * state (a scroll position, a pan, a cached layout) is deliberately exempt and
 * named here rather than guessed at.
 */
test('no Android screen holds typed text in state a rotation discards', () => {
  const screens = [
    'android/app/src/main/java/com/nexusriot/rezoagwe/ui/KeysScreen.kt',
    'android/app/src/main/java/com/nexusriot/rezoagwe/ui/ChatScreen.kt',
    'android/app/src/main/java/com/nexusriot/rezoagwe/ui/SettingsScreen.kt',
  ];
  // State that is recomputed or re-observed on every composition, and so has
  // nothing to preserve.
  const EPHEMERAL = /^(scale|pan|now|positions|components|selected)$/;

  const offenders = [];
  for (const file of screens) {
    const src = read(file);
    for (const m of src.matchAll(/\bvar\s+(\w+)\s+by\s+remember\s*(\([^)]*\))?\s*\{\s*mutable\w*StateOf/g)) {
      if (!EPHEMERAL.test(m[1])) offenders.push(`${file}: ${m[1]}`);
    }
  }
  assert.deepEqual(
    offenders, [],
    'these survive a recomposition and not a rotation; use rememberSaveable',
  );
});
