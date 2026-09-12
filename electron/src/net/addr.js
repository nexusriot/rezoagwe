'use strict';

const os = require('node:os');

// Address helpers shared by both roles. The rules match the Go model and the
// Kotlin engine: an empty host is the wildcard and resolves to loopback, and a
// node listening on ":3137" recognises "127.0.0.1:3137" as itself.

/** Splits "host:port" on the last colon. IPv6 literals keep their brackets stripped. */
function splitHostPort(addr) {
  const i = addr.lastIndexOf(':');
  if (i < 0) return null;
  let host = addr.slice(0, i);
  const port = Number(addr.slice(i + 1));
  if (!Number.isInteger(port) || port <= 0 || port > 65535 || addr.slice(i + 1).trim() === '') return null;
  if (host.startsWith('[') && host.endsWith(']')) host = host.slice(1, -1);
  return { host, port };
}

/**
 * Whether addr is a usable host:port. Gossip is hearsay at the application
 * level, so malformed entries are refused at the door instead of lingering in
 * the peer list until eviction.
 */
function validPeerAddr(addr) {
  if (typeof addr !== 'string') return false;
  const parts = splitHostPort(addr);
  if (!parts) return false;
  return !/[\s]/.test(parts.host);
}

/** Canonicalises an address so a node recognises the aliases of its own. */
function normalizeAddr(addr) {
  if (typeof addr !== 'string') return '';
  const i = addr.lastIndexOf(':');
  if (i < 0) return addr;
  let host = addr.slice(0, i);
  const port = addr.slice(i + 1);
  if (host === '' || host === '0.0.0.0' || host === 'localhost' || host === '::' || host === '[::]') host = '127.0.0.1';
  return `${host}:${port}`;
}

/** Where a datagram to addr actually goes: a missing host is the loopback interface. */
function targetOf(addr) {
  const parts = splitHostPort(addr);
  if (!parts) return null;
  return { host: parts.host === '' ? '127.0.0.1' : parts.host, port: parts.port };
}

/** The first non-loopback IPv4 address, which is what peers on the LAN can reach. */
function localIpv4() {
  try {
    const ifaces = os.networkInterfaces();
    for (const name of Object.keys(ifaces)) {
      for (const a of ifaces[name] || []) {
        if (!a.internal && (a.family === 'IPv4' || a.family === 4)) return a.address;
      }
    }
  } catch {
    // fall through
  }
  return '127.0.0.1';
}

module.exports = { splitHostPort, validPeerAddr, normalizeAddr, targetOf, localIpv4 };
