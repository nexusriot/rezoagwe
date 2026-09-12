'use strict';

const fs = require('node:fs');
const path = require('node:path');

const DEFAULTS = Object.freeze({
  nick: 'desktop',
  port: 3137,
  advertiseHost: '',
  seeds: ':9999',
  psk: '',
  cluster: 'rezoagwe',
  bootstrapPort: 9999,
  tombstoneTtlSec: 0,
  autoStartNode: true,
  autoStartBootstrap: false,
  windowState: { width: 1280, height: 860, x: null, y: null, maximized: false },
});

function seedList(seeds) {
  return String(seeds || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

/**
 * The values as they should be stored: surrounding whitespace off every text
 * field, and blanks replaced from the defaults.
 *
 * The cluster name and the key feed the framing key, so a stray space is not
 * cosmetic — it changes the key and the node stops talking to every peer, with
 * nothing on screen to show why. Paste is exactly the source of one: copying a
 * key out of a terminal picks up the trailing newline.
 */
function sanitize(s, fallback = DEFAULTS) {
  const port = Number(s.port);
  const bootstrapPort = Number(s.bootstrapPort);
  const ttl = Number(s.tombstoneTtlSec);
  return {
    nick: String(s.nick ?? '').trim() || fallback.nick,
    port: Number.isInteger(port) && port > 0 && port <= 65535 ? port : fallback.port,
    advertiseHost: String(s.advertiseHost ?? '').trim(),
    seeds: String(s.seeds ?? '').trim(),
    psk: String(s.psk ?? '').trim(),
    cluster: String(s.cluster ?? '').trim() || fallback.cluster,
    bootstrapPort: Number.isInteger(bootstrapPort) && bootstrapPort > 0 && bootstrapPort <= 65535
      ? bootstrapPort
      : fallback.bootstrapPort,
    tombstoneTtlSec: Number.isFinite(ttl) && ttl >= 0 ? Math.floor(ttl) : 0,
    autoStartNode: s.autoStartNode !== false,
    autoStartBootstrap: s.autoStartBootstrap === true,
    windowState: { ...DEFAULTS.windowState, ...(s.windowState || fallback.windowState) },
  };
}

function toNodeConfig(s) {
  return {
    advertiseHost: s.advertiseHost,
    port: s.port,
    nick: s.nick,
    seeds: seedList(s.seeds),
    psk: s.psk,
    cluster: s.cluster,
    tombstoneTtlSec: s.tombstoneTtlSec,
  };
}

function toBootstrapConfig(s) {
  return { port: s.bootstrapPort, psk: s.psk, cluster: s.cluster };
}

/** Which settings are baked into a running socket or codec, and so need a restart to take effect. */
function nodeNeedsRestart(previous, updated) {
  return previous.port !== updated.port
    || previous.psk !== updated.psk
    || previous.cluster !== updated.cluster
    || previous.advertiseHost !== updated.advertiseHost
    || previous.seeds !== updated.seeds
    || previous.tombstoneTtlSec !== updated.tombstoneTtlSec;
}

function bootstrapNeedsRestart(previous, updated) {
  return previous.bootstrapPort !== updated.bootstrapPort
    || previous.psk !== updated.psk
    || previous.cluster !== updated.cluster;
}

/** Settings on disk as one JSON file, written atomically so a crash cannot truncate it. */
class SettingsStore {
  constructor(filePath) {
    this.path = filePath;
    this.value = this.read();
  }

  read() {
    try {
      const raw = fs.readFileSync(this.path, 'utf8');
      return sanitize({ ...DEFAULTS, ...JSON.parse(raw) });
    } catch {
      return { ...DEFAULTS };
    }
  }

  get() {
    return this.value;
  }

  save(partial) {
    this.value = sanitize({ ...this.value, ...partial }, this.value);
    try {
      fs.mkdirSync(path.dirname(this.path), { recursive: true });
      const tmp = `${this.path}.tmp`;
      fs.writeFileSync(tmp, JSON.stringify(this.value, null, 2), 'utf8');
      fs.renameSync(tmp, this.path);
    } catch {
      // An unwritable settings file must not stop the app; the value still
      // applies for this session.
    }
    return this.value;
  }
}

module.exports = {
  DEFAULTS,
  SettingsStore,
  sanitize,
  seedList,
  toNodeConfig,
  toBootstrapConfig,
  nodeNeedsRestart,
  bootstrapNeedsRestart,
};
