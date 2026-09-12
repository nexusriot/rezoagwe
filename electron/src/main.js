'use strict';

const { app, BrowserWindow, ipcMain, Menu, shell, clipboard, dialog } = require('electron');
const path = require('node:path');
const fs = require('node:fs');

const { Runtime } = require('./core/runtime');
const { healthChecks, asReport } = require('./core/diagnostics');
const { version, versionZero } = require('./proto/wire');

let runtime;
let win;
let quitting = false;

function send(channel, payload) {
  if (win && !win.isDestroyed()) win.webContents.send(channel, payload);
}

function createWindow() {
  const ws = runtime.settings.windowState;
  win = new BrowserWindow({
    width: ws.width || 1280,
    height: ws.height || 860,
    ...(Number.isInteger(ws.x) && Number.isInteger(ws.y) ? { x: ws.x, y: ws.y } : {}),
    minWidth: 860,
    minHeight: 560,
    title: 'Rezoagwe',
    backgroundColor: '#0b1015',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      // The preload needs only contextBridge and ipcRenderer, both available to a
      // sandboxed preload, so there is nothing to gain from leaving the renderer
      // un-sandboxed.
      sandbox: true,
      webviewTag: false,
      spellcheck: false,
    },
  });
  if (ws.maximized) win.maximize();

  // The renderer is one local page whose only outward links go through
  // shell.openExternal. Anything else is a bug or an injection, so refuse it.
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//i.test(url)) shell.openExternal(url);
    return { action: 'deny' };
  });
  win.webContents.on('will-navigate', (event, url) => {
    if (url !== win.webContents.getURL()) event.preventDefault();
  });

  win.loadFile(path.join(__dirname, '..', 'renderer', 'index.html'));
  if (process.argv.includes('--dev')) win.webContents.openDevTools({ mode: 'detach' });
  if (process.argv.includes('--selftest')) runSelfTest();
  const shots = process.argv.indexOf('--screenshot');
  if (shots >= 0) captureScreens(process.argv[shots + 1] || '.');

  // getNormalBounds reports the pre-maximise size, so restoring an un-maximised
  // window does not snap to full screen.
  win.on('close', () => {
    const bounds = win.getNormalBounds ? win.getNormalBounds() : win.getBounds();
    runtime.settingsStore.save({
      windowState: {
        width: bounds.width, height: bounds.height, x: bounds.x, y: bounds.y, maximized: win.isMaximized(),
      },
    });
  });
}

function buildMenu() {
  const template = [
    {
      label: 'Node',
      submenu: [
        { label: 'Start node', click: () => runtime.startNode() },
        { label: 'Stop node', click: () => runtime.stopNode() },
        { type: 'separator' },
        { label: 'Start bootstrap', click: () => runtime.startBootstrap() },
        { label: 'Stop bootstrap', click: () => runtime.stopBootstrap() },
        { type: 'separator' },
        { role: 'quit' },
      ],
    },
    {
      label: 'Store',
      submenu: [
        { label: 'Export…', click: () => exportStore() },
        { label: 'Import (merge by version)…', click: () => importStore(false) },
        { label: 'Import (seed as local writes)…', click: () => importStore(true) },
        { type: 'separator' },
        { label: 'Copy diagnostics report', click: () => copyReport() },
      ],
    },
    {
      label: 'View',
      submenu: [
        { role: 'reload' },
        { role: 'toggleDevTools' },
        { type: 'separator' },
        { role: 'resetZoom' },
        { role: 'zoomIn' },
        { role: 'zoomOut' },
        { type: 'separator' },
        { role: 'togglefullscreen' },
      ],
    },
    {
      label: 'Help',
      submenu: [
        {
          label: 'Protocol notes (DESIGN.md)',
          click: () => shell.openExternal('https://github.com/nexusriot/rezoagwe/blob/main/DESIGN.md'),
        },
      ],
    },
  ];
  Menu.setApplicationMenu(Menu.buildFromTemplate(template));
}

async function exportStore() {
  const { canceled, filePath } = await dialog.showSaveDialog(win, {
    title: 'Export the store',
    defaultPath: `rezoagwe-${runtime.settings.cluster}-${Date.now()}.json`,
    filters: [{ name: 'JSON', extensions: ['json'] }],
  });
  if (canceled || !filePath) return;
  try {
    fs.writeFileSync(filePath, runtime.engine.export(), 'utf8');
    send('toast', { kind: 'ok', text: `Exported to ${filePath}` });
  } catch (e) {
    send('toast', { kind: 'error', text: `Export failed: ${e.message}` });
  }
}

async function importStore(asLocalWrites) {
  const { canceled, filePaths } = await dialog.showOpenDialog(win, {
    title: asLocalWrites ? 'Seed the store from a file' : 'Merge a file into the store',
    properties: ['openFile'],
    filters: [{ name: 'JSON', extensions: ['json'] }],
  });
  if (canceled || !filePaths.length) return;
  try {
    const applied = await runtime.engine.import(fs.readFileSync(filePaths[0], 'utf8'), asLocalWrites);
    send('toast', { kind: 'ok', text: `Imported ${applied} entr${applied === 1 ? 'y' : 'ies'}` });
  } catch (e) {
    send('toast', { kind: 'error', text: `Import failed: ${e.message}` });
  }
}

function copyReport() {
  clipboard.writeText(asReport(runtime.engine.diagnostics(), Date.now()));
  send('toast', { kind: 'ok', text: 'Diagnostics copied to the clipboard' });
}

function registerIpc() {
  const handle = (channel, fn) => ipcMain.handle(channel, (_event, ...args) => fn(...args));

  handle('app:snapshot', () => ({ ...runtime.snapshot(), version: app.getVersion() }));
  handle('app:settings', () => runtime.settings);
  handle('app:applySettings', (partial) => runtime.applySettings(partial));

  handle('node:start', () => runtime.startNode());
  handle('node:stop', () => runtime.stopNode());
  handle('node:submit', (text) => runtime.engine.submit(text).then(() => ({ ok: true })));
  handle('node:history', (key) => runtime.engine.history(key).map((h) => ({
    version: h.version, value: h.value, deleted: h.deleted, local: h.local, at: h.at,
    writer: runtime.engine.writerName(h.version.node),
  })));

  handle('kv:set', async ({ key, value, ttlSeconds, guard, expect }) => {
    // A guarded write is a compare-and-swap against the version the form was
    // opened with; a guard on a new key means "only if absent", which is the
    // zero version.
    if (!guard) {
      await runtime.engine.set(key, value, ttlSeconds || 0);
      return { ok: true };
    }
    const expected = expect && !versionZero(expect) ? expect : version(0, '');
    const written = await runtime.engine.compareAndSet(key, value, ttlSeconds || 0, expected);
    return written ? { ok: true } : { ok: false, refused: true };
  });

  handle('kv:delete', async ({ key, guard, expect }) => {
    if (!guard) {
      await runtime.engine.delete(key);
      return { ok: true };
    }
    const removed = await runtime.engine.compareAndDelete(key, expect || version(0, ''));
    return removed ? { ok: true } : { ok: false, refused: true };
  });

  handle('peer:sync', (addr) => runtime.engine.requestStateFrom(addr).then(() => ({ ok: true })));
  handle('peer:add', (addr) => ({ ok: runtime.engine.addPeer(addr) }));
  handle('peer:forget', (addr) => ({ ok: runtime.engine.forgetPeer(addr) }));
  handle('peer:gossip', (addr) => runtime.engine.gossipTo(addr).then(() => ({ ok: true })));

  handle('bootstrap:start', () => runtime.startBootstrap());
  handle('bootstrap:stop', () => runtime.stopBootstrap());

  handle('diag:snapshot', () => {
    const d = runtime.engine.diagnostics();
    return { diagnostics: d, checks: healthChecks(d, Date.now()), bootstrap: runtime.bootstrap.status() };
  });
  handle('diag:copyReport', () => {
    copyReport();
    return { ok: true };
  });

  handle('store:export', () => exportStore().then(() => ({ ok: true })));
  handle('store:import', (asLocalWrites) => importStore(asLocalWrites).then(() => ({ ok: true })));
}

function forwardEvents() {
  for (const event of ['entries', 'peers', 'chat', 'activity', 'status', 'metrics', 'topology']) {
    runtime.on(event, (payload) => send(`node:${event}`, payload));
  }
  runtime.on('bootstrap-roster', (payload) => send('bootstrap:roster', payload));
  runtime.on('bootstrap-status', (payload) => send('bootstrap:status', payload));
  runtime.on('error-message', (text) => send('toast', { kind: 'error', text }));
  runtime.on('warning', (text) => send('toast', { kind: 'warn', text }));
}

app.whenReady().then(async () => {
  runtime = new Runtime(app.getPath('userData'));
  registerIpc();
  forwardEvents();
  buildMenu();
  createWindow();

  const s = runtime.settings;
  if (s.autoStartBootstrap) await runtime.startBootstrap();
  if (s.autoStartNode) await runtime.startNode();
});

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit();
});

app.on('activate', () => {
  if (BrowserWindow.getAllWindows().length === 0) createWindow();
});

// Ctrl+C and a normal quit both have to reach stop(), or peers wait out the
// eviction timeout instead of being told this node left.
app.on('before-quit', (event) => {
  if (quitting || !runtime) return;
  quitting = true;
  event.preventDefault();
  runtime.shutdown().finally(() => app.exit(0));
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => app.quit());
}

/**
 * `electron . --selftest` boots the real window, drives the renderer through
 * every screen, and exits non-zero on anything it cannot prove.
 *
 * It catches what unit tests structurally cannot: a preload broken by a sandbox
 * change, a CSP that blocks the scripts, a renderer that throws before its first
 * paint, a graph that draws nothing.
 */
function runSelfTest() {
  const finish = (ok, report) => {
    console.log((ok ? 'SELFTEST PASS ' : 'SELFTEST FAIL ') + JSON.stringify(report, null, 2));
    app.exit(ok ? 0 : 1);
  };
  const timeout = setTimeout(() => finish(false, { error: 'renderer did not report within 40s' }), 40_000);

  win.webContents.once('did-finish-load', async () => {
    // The first paint is async (boot awaits the snapshot), so give it a moment.
    await new Promise((r) => setTimeout(r, 1500));
    try {
      // Prove the engine really replicates before asking the UI about it: a
      // green screen over a dead node is the failure this is here to catch.
      await runtime.engine.set('selftest-key', 'selftest-value', 0);
      await new Promise((r) => setTimeout(r, 300));

      const raw = await win.webContents.executeJavaScript(`(async () => {
        const r = { errors: window.__errors || [] };
        r.bridge = typeof window.rezoagwe;
        r.bridgeGroups = window.rezoagwe ? Object.keys(window.rezoagwe).length : 0;
        r.status = document.querySelector('#status-addr') ? document.querySelector('#status-addr').textContent : null;
        // Element.append renders a null child as the word "null"; a conditional
        // chip that is not showing must leave nothing behind, not print itself.
        const textOf = (sel) => (document.querySelector(sel) || {}).textContent || '';
        r.strayNullText = ['#status-chips', '#topbar'].filter((sel) => /\bnull\b/.test(textOf(sel)));
        const visit = async (name) => {
          window.__test.show(name);
          await new Promise((res) => setTimeout(res, 250));
          const view = document.querySelector('#pane-main .pane-body');
          return view ? view.childElementCount : 0;
        };
        r.panes = {};
        for (const name of ['keys', 'chat', 'peers', 'graph', 'activity', 'diag', 'bootstrap', 'settings']) {
          r.panes[name] = await visit(name);
        }
        window.__test.show('keys');
        await new Promise((res) => setTimeout(res, 250));
        r.keyRows = document.querySelectorAll('#pane-main .kv-row').length;
        r.selftestKeyOnScreen = [...document.querySelectorAll('#pane-main .kv-key')]
          .some((el) => el.textContent === 'selftest-key');

        window.__test.show('graph');
        await new Promise((res) => setTimeout(res, 350));
        const svg = document.querySelector('#pane-main svg.graph');
        r.graphNodes = svg ? svg.querySelectorAll('.graph-node').length : 0;
        r.graphDrawn = !!svg;

        window.__test.show('diag');
        await new Promise((res) => setTimeout(res, 600));
        r.checkRows = document.querySelectorAll('#pane-main .check').length;

        // The split view is the layout claim worth checking: two panes, two
        // different screens, and a working collapse back to one.
        window.__test.setSplit(true);
        await new Promise((res) => setTimeout(res, 300));
        r.splitPanes = document.querySelectorAll('.pane').length;
        r.splitDistinct = document.querySelector('#pane-main').dataset.screen
          !== document.querySelector('#pane-side') ?.dataset.screen;
        window.__test.setSplit(false);
        await new Promise((res) => setTimeout(res, 200));
        r.collapsedPanes = document.querySelectorAll('.pane').length;

        window.__test.show('chat');
        await new Promise((res) => setTimeout(res, 200));
        await window.__test.submit('/help');
        await new Promise((res) => setTimeout(res, 400));
        r.chatLines = document.querySelectorAll('#pane-main .chat-line').length;
        r.helpEchoed = [...document.querySelectorAll('#pane-main .chat-line')]
          .some((el) => el.textContent.includes('/nick'));
        return r;
      })()`);

      const problems = [];
      if (raw.bridge !== 'object') problems.push('preload bridge missing');
      if (!raw.status) problems.push('status header did not render');
      for (const [name, count] of Object.entries(raw.panes)) {
        if (count === 0) problems.push(`${name} pane rendered nothing`);
      }
      if (!raw.selftestKeyOnScreen) problems.push('a written key did not reach the keys pane');
      if (!raw.graphDrawn || raw.graphNodes < 1) problems.push('cluster graph drew no nodes');
      if (raw.checkRows < 1) problems.push('diagnostics produced no health checks');
      if (raw.splitPanes !== 2) problems.push(`split view showed ${raw.splitPanes} panes, expected 2`);
      if (!raw.splitDistinct) problems.push('both panes showed the same screen');
      if (raw.collapsedPanes !== 1) problems.push('collapsing the split did not return to one pane');
      if (!raw.helpEchoed) problems.push('/help produced no command list');
      if (raw.strayNullText && raw.strayNullText.length) {
        problems.push(`the word "null" is on screen in ${raw.strayNullText.join(', ')}`);
      }
      if (raw.errors.length) problems.push(`renderer errors: ${raw.errors.join(' | ')}`);

      clearTimeout(timeout);
      finish(problems.length === 0, { ...raw, problems });
    } catch (e) {
      clearTimeout(timeout);
      finish(false, { error: e.message });
    }
  });
}

/**
 * `electron . --screenshot <dir>` walks every screen and writes a PNG of each.
 *
 * capturePage renders through the window's own compositor, so it works on a
 * machine whose screen is locked or has no desktop at all — which is the only
 * way a change to this UI can be looked at from a terminal.
 */
function captureScreens(dir) {
  const { SCREENS } = require('../renderer/shared');
  win.webContents.once('did-finish-load', async () => {
    await new Promise((r) => setTimeout(r, 1500));
    fs.mkdirSync(dir, { recursive: true });
    const written = [];
    try {
      // Something to look at: an empty store makes for a screenshot that proves
      // only that the page painted.
      await runtime.engine.set('colour', 'blue');
      await runtime.engine.set('session/token', 'abc123', 3600);
      await runtime.engine.set('leader', 'node-a');
      await runtime.engine.submit('/help');
      await new Promise((r) => setTimeout(r, 400));

      for (const screen of SCREENS) {
        await win.webContents.executeJavaScript(`window.__test.show(${JSON.stringify(screen.id)})`);
        await new Promise((r) => setTimeout(r, screen.id === 'diag' ? 2400 : 700));
        const image = await win.webContents.capturePage();
        const file = path.join(dir, `${screen.id}.png`);
        fs.writeFileSync(file, image.toPNG());
        written.push(file);
      }
      await win.webContents.executeJavaScript('window.__test.setSplit(true)');
      await new Promise((r) => setTimeout(r, 900));
      const split = path.join(dir, 'split.png');
      fs.writeFileSync(split, (await win.webContents.capturePage()).toPNG());
      written.push(split);
      console.log(`SHOTS ${written.length}\n${written.join('\n')}`);
      app.exit(0);
    } catch (e) {
      console.error('screenshot failed:', e.message);
      app.exit(1);
    }
  });
}
