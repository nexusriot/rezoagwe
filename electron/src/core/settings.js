'use strict';

const fs = require('node:fs');
const path = require('node:path');

const DEFAULTS = Object.freeze({
  nick: 'desktop',
  durability: 'snapshot',
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
const ipv6Literal = (h) => String(h ?? '').includes(':');

function sanitize(s, fallback = DEFAULTS) {
  const port = Number(s.port);
  const bootstrapPort = Number(s.bootstrapPort);
  const ttl = Number(s.tombstoneTtlSec);
  return {
    durability: ['snapshot','journal','journal-sync'].includes(s.durability) ? s.durability : fallback.durability,
    nick: String(s.nick ?? '').trim() || fallback.nick,
    port: Number.isInteger(port) && port > 0 && port <= 65535 ? port : fallback.port,
    // The sockets are udp4: an IPv6 host would be advertised to a cluster
    // that then sends to an address this node never listens on.
    advertiseHost: ipv6Literal(s.advertiseHost) ? fallback.advertiseHost : String(s.advertiseHost ?? '').trim(),
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

/**
 * What sanitize would not keep, phrased for the person who typed it.
 *
 * Refusing a value is only half an answer: the form repaints from the stored
 * settings, so a refused port simply snaps back while the toast says "Settings
 * applied". Saying nothing teaches people that the field does not work.
 *
 * Trimming is deliberately not reported. It is documented on the screen, it is
 * what the user wanted, and naming it on every paste would bury the messages
 * that matter.
 */
function refusals(s, fallback = DEFAULTS) {
  const out = [];
  const port = Number(s.port);
  const bootstrapPort = Number(s.bootstrapPort);
  if (!(Number.isInteger(port) && port > 0 && port <= 65535)) {
    out.push(`node port ${s.port} is not a usable port, kept ${fallback.port}`);
  }
  if (!(Number.isInteger(bootstrapPort) && bootstrapPort > 0 && bootstrapPort <= 65535)) {
    out.push(`bootstrap port ${s.bootstrapPort} is not a usable port, kept ${fallback.bootstrapPort}`);
  }
  if (!String(s.nick ?? '').trim()) out.push(`nickname cannot be blank, kept ${fallback.nick}`);
  if (ipv6Literal(s.advertiseHost)) {
    out.push(`advertise host ${String(s.advertiseHost).trim()} is IPv6; the desktop client speaks IPv4 only, kept ${fallback.advertiseHost || 'the detected LAN address'}`);
  }
  if (!String(s.cluster ?? '').trim()) out.push(`cluster name cannot be blank, kept ${fallback.cluster}`);
  return out;
}

function toNodeConfig(s) {
  return {
    ...(s.durability && s.durability!=='snapshot' ? {durability:s.durability} : {}),
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
  if(previous.durability!==updated.durability)return true;
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

/**
 * What an import says it did. A merge from another cluster can outrank every
 * replica here — export and import are the documented way to migrate between
 * clusters, so it is allowed, but it is named rather than passed over.
 */
function importMessage(applied, fromCluster, currentCluster) {
  const base = `Imported ${applied} entr${applied === 1 ? 'y' : 'ies'}`;
  return fromCluster && fromCluster !== currentCluster ? `${base}, from cluster ${fromCluster}, not ${currentCluster}` : base;
}

module.exports = {
  importMessage,
  refusals,
  DEFAULTS,
  SettingsStore,
  sanitize,
  seedList,
  toNodeConfig,
  toBootstrapConfig,
  nodeNeedsRestart,
  bootstrapNeedsRestart,
};
