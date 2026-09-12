'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const ROOT = path.join(__dirname, '..');
const read = (p) => fs.readFileSync(path.join(ROOT, p), 'utf8');
const pkg = JSON.parse(read('package.json'));
const debScript = read('build-deb.sh');
const makefile = read('Makefile');
const rootMakefile = read('../Makefile');

/**
 * The app is packaged two ways — electron-builder, and dpkg-deb over an
 * unpacked build for a machine with no fpm and no network. Both write the same
 * package name and the same install prefix, so a deb from either can replace a
 * deb from the other; nothing but a test keeps those two descriptions in step,
 * and the failure mode is a half-removed installation rather than a build error.
 */

const linux = pkg.build.linux;

test('both packaging paths agree on the package name', () => {
  // electron-builder takes the deb's package name from package.json "name".
  assert.equal(pkg.name, 'rezoagwe-desktop');
  assert.match(debScript, /PKG_NAME="rezoagwe-desktop"/);
});

test('both packaging paths agree on the install prefix and the executable', () => {
  // electron-builder installs under /opt/<productName>.
  assert.equal(pkg.build.productName, 'Rezoagwe');
  assert.match(debScript, /INSTALL_DIR="\/opt\/Rezoagwe"/,
    'the fallback script must install where electron-builder does, or the two debs are different packages');
  // executableName may sit at the top of the build config or under linux; what
  // matters is the name the launcher, the symlink and the fallback script use.
  const executable = pkg.build.executableName || linux.executableName;
  assert.equal(executable, 'rezoagwe-desktop');
  assert.match(debScript, new RegExp(`EXE="${executable}"`));
  assert.match(debScript, /chrome-sandbox/, 'the sandbox helper needs its setuid bit restored on install');
});

test('the desktop entry says the same thing in both paths', () => {
  assert.equal(linux.desktop.Name, 'Rezoagwe');
  assert.equal(linux.desktop.StartupWMClass, 'Rezoagwe',
    'a wrong WM class detaches the running window from its launcher icon');
  assert.match(debScript, /^Name=Rezoagwe$/m);
  assert.match(debScript, /^StartupWMClass=Rezoagwe$/m);
  assert.match(debScript, /^Exec=\$EXE %U$/m);
  for (const category of linux.desktop.Categories.split(';').filter(Boolean)) {
    assert.match(debScript, new RegExp(`Categories=.*${category}`));
  }
});

test('the runtime dependencies are declared once and shared', () => {
  assert.ok(pkg.build.deb.depends.length >= 5);
  for (const dep of pkg.build.deb.depends) {
    assert.ok(debScript.includes(dep), `the fallback deb never declares ${dep}`);
  }
});

test('the package ships what the app loads and nothing it does not', () => {
  const files = pkg.build.files;
  for (const wanted of ['src/**/*', 'renderer/**/*', 'package.json']) {
    assert.ok(files.includes(wanted), `${wanted} is not packaged`);
  }
  for (const excluded of ['!test/**', '!scripts/**', '!**/*.md']) {
    assert.ok(files.includes(excluded), `${excluded} should not be shipped`);
  }
});

test('every file the page loads is inside a packaged path', () => {
  // The packaged selftest is what proves this at runtime; this catches it in a
  // second rather than in a five-minute build.
  const html = read('renderer/index.html');
  for (const match of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
    assert.ok(fs.existsSync(path.join(ROOT, 'renderer', match[1])), `renderer/${match[1]} is missing`);
  }
  // main.js reaches outside src/ for exactly one thing.
  assert.match(read('src/main.js'), /require\('\.\.\/renderer\/shared'\)/);
  assert.ok(pkg.build.files.includes('renderer/**/*'));
});

test('the icon is generated, at the sizes a desktop actually looks in', () => {
  assert.equal(linux.icon, 'build/icons');
  const script = read('scripts/make-icon.mjs');
  // A single PNG whose size electron-builder cannot read installs under
  // hicolor/0x0/apps, where no menu ever looks.
  assert.match(script, /const SIZES = \[16, 24, 32, 48, 64, 128, 256, 512\]/);
  assert.match(debScript, /build\/icons\/\*\.png/, 'the fallback deb installs the same set');
  assert.equal(pkg.scripts.icon, 'node scripts/make-icon.mjs');
});

test('generating the icon needs nothing but Node', () => {
  const script = read('scripts/make-icon.mjs');
  for (const forbidden of ['sharp', 'canvas', 'jimp', 'execSync', 'convert ']) {
    assert.ok(!script.includes(forbidden), `the icon build should not need ${forbidden}`);
  }
  const out = execFileSync(process.execPath, [path.join(ROOT, 'scripts', 'make-icon.mjs')], { encoding: 'utf8' });
  assert.match(out, /8 sizes/);
  for (const size of [16, 48, 512]) {
    const file = path.join(ROOT, 'build', 'icons', `${size}x${size}.png`);
    const png = fs.readFileSync(file);
    assert.deepEqual([...png.subarray(0, 8)], [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a], `${file} is not a PNG`);
    // The IHDR width and height are what electron-builder reads to decide where
    // the icon is installed.
    assert.equal(png.readUInt32BE(16), size, `${file} does not declare its width`);
    assert.equal(png.readUInt32BE(20), size, `${file} does not declare its height`);
  }
});

test('an overridden version reaches the artifact and the app together', () => {
  // Renaming the file alone would ship a build that still reports the old
  // version to --version and to the diagnostics report.
  assert.match(makefile, /--config\.extraMetadata\.version=\$\(VERSION\)/);
  assert.match(debScript, /VERSION="\$\{VERSION:-\$\(node -p "require\('\.\/package\.json'\)\.version"\)\}"/);
});

test('the Makefile offers a target for every artifact the client can produce', () => {
  for (const target of ['binary', 'tarball', 'deb', 'deb-arm64', 'deb-armhf', 'debs', 'appimage',
    'deb-manual', 'dist', 'verify', 'clean', 'icon']) {
    assert.match(makefile, new RegExp(`^\\.PHONY: ${target}$`, 'm'), `no ${target} target`);
  }
  // The fallback path has to be reachable without remembering the script name.
  assert.match(makefile, /deb-manual: binary/);
});

test('the repository root can build and verify the client without changing directory', () => {
  for (const target of ['electron', 'electron-test', 'electron-selftest', 'electron-interop',
    'electron-deb', 'electron-dist', 'electron-verify']) {
    assert.match(rootMakefile, new RegExp(`^\\.PHONY: ${target}$`, 'm'), `the root Makefile has no ${target}`);
  }
});

test('build output is ignored by git, and generated icons with it', () => {
  const ignore = read('../.gitignore');
  for (const pattern of ['electron/node_modules/', 'electron/dist/', 'electron/build/icon']) {
    assert.ok(ignore.includes(pattern), `.gitignore should cover ${pattern}`);
  }
});
