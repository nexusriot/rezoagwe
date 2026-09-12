'use strict';

const { EventEmitter } = require('node:events');
const { normalizeAddr } = require('./addr');

// An in-process network with configurable loss, delay and partitions, so a
// whole cluster can be tested without sockets, sleeps or flakiness — the twin
// of pkg/transport/mem.go. Convergence is asserted, not hoped for.

/** One end of an in-memory stream: whatever is written here is emitted as 'data' on the other end. */
class MemStream extends EventEmitter {
  constructor() {
    super();
    this.peer = null;
    this.ended = false;
    this.destroyed = false;
  }

  write(chunk) {
    if (this.destroyed || !this.peer) return false;
    const peer = this.peer;
    setImmediate(() => {
      if (!peer.destroyed) peer.emit('data', Buffer.from(chunk));
    });
    return true;
  }

  end() {
    if (this.ended) return;
    this.ended = true;
    const peer = this.peer;
    setImmediate(() => {
      if (peer && !peer.destroyed) peer.emit('end');
    });
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    const peer = this.peer;
    setImmediate(() => {
      this.emit('close');
      if (peer && !peer.destroyed) peer.destroy();
    });
  }

  setTimeout() {}

  static pair() {
    const a = new MemStream();
    const b = new MemStream();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }
}

class MemTransport extends EventEmitter {
  constructor(network, addr) {
    super();
    this.network = network;
    this.addr = addr;
    this.port = Number(addr.slice(addr.lastIndexOf(':') + 1)) || 0;
    this.streamsAvailable = network.streams;
    this.closed = false;
  }

  send(addr, data) {
    if (this.closed) return Promise.reject(new Error('transport closed'));
    return this.network.deliver(this.addr, addr, data);
  }

  dial(addr) {
    if (!this.streamsAvailable) return Promise.reject(new Error('stream transport unavailable'));
    const target = this.network.lookup(this.addr, addr);
    if (!target) return Promise.reject(new Error(`no route to ${addr}`));
    const [near, far] = MemStream.pair();
    setImmediate(() => target.emit('stream', far));
    return Promise.resolve(near);
  }

  close() {
    this.closed = true;
    this.network.detach(this.addr);
    return Promise.resolve();
  }
}

class MemNetwork {
  constructor(opts = {}) {
    this.nodes = new Map();
    this.loss = opts.loss || 0; // probability a datagram is dropped
    this.delayMs = opts.delayMs || 0; // fixed delivery delay
    this.streams = opts.streams !== false;
    this.blocked = new Set(); // "a|b" pairs that cannot exchange packets
    this.random = opts.random || Math.random;
    this.dropped = 0;
  }

  attach(addr) {
    const t = new MemTransport(this, addr);
    this.nodes.set(normalizeAddr(addr), t);
    return t;
  }

  detach(addr) {
    this.nodes.delete(normalizeAddr(addr));
  }

  /** Cuts every packet and stream between a and b, in both directions. */
  partition(a, b) {
    this.blocked.add(this.pairKey(a, b));
  }

  heal(a, b) {
    this.blocked.delete(this.pairKey(a, b));
  }

  pairKey(a, b) {
    const x = normalizeAddr(a);
    const y = normalizeAddr(b);
    return x < y ? `${x}|${y}` : `${y}|${x}`;
  }

  lookup(from, to) {
    if (this.blocked.has(this.pairKey(from, to))) return null;
    return this.nodes.get(normalizeAddr(to)) || null;
  }

  deliver(from, to, data) {
    const target = this.lookup(from, to);
    if (!target) return Promise.resolve(); // handed to the network, never delivered
    if (this.loss > 0 && this.random() < this.loss) {
      this.dropped++;
      return Promise.resolve();
    }
    const packet = { from: normalizeAddr(from), data: Buffer.from(data) };
    const fire = () => {
      if (!target.closed) target.emit('packet', packet);
    };
    if (this.delayMs > 0) setTimeout(fire, this.delayMs);
    else setImmediate(fire);
    return Promise.resolve();
  }
}

module.exports = { MemNetwork, MemTransport, MemStream };
