'use strict';

const { EventEmitter } = require('node:events');
const { normalizeAddr } = require('./addr');
const { PacketTooLargeError, MAX_DATAGRAM_PAYLOAD } = require('./udp');

// An in-process network with configurable loss, delay and partitions, so a
// whole cluster can be tested without sockets, sleeps or flakiness — the twin
// of pkg/transport/mem.go. Convergence is asserted, not hoped for.

/** One end of an in-memory stream: whatever is written here is emitted as 'data' on the other end. */
class MemStream extends EventEmitter {
  constructor(remote = '') {
    super();
    this.peer = null;
    this.ended = false;
    this.destroyed = false;
    // The address of the other end, as net.Socket reports it. A stand-in
    // without it cannot exercise anything that reads where a connection came
    // from — which is how a rendezvous service that forgot to report the
    // address it observed passed this whole suite.
    const parts = remote ? remote.slice(0, remote.lastIndexOf(':')) : '';
    this.remoteAddress = remote ? parts : undefined;
    this.remotePort = remote ? Number(remote.slice(remote.lastIndexOf(':') + 1)) : undefined;
  }

  write(chunk) {
    if (this.destroyed || !this.peer) return false;
    const peer = this.peer;
    setImmediate(() => {
      // A destroyed socket discards whatever it had not yet flushed. A
      // stand-in that delivers it anyway hides every write-then-close bug —
      // which is how a truncating one-way send passed this whole suite and
      // only showed up against a real socket carrying tens of megabytes.
      if (this.destroyed) return;
      if (!peer.destroyed) peer.emit('data', Buffer.from(chunk));
    });
    return true;
  }

  /**
   * Matches net.Socket: an optional final chunk, and a callback once it has
   * been handed on. A stand-in that ignored both would let a flush bug
   * through here and only show it against a real socket.
   */
  end(data, cb) {
    const done = typeof data === 'function' ? data : cb;
    if (this.ended) {
      if (done) setImmediate(done);
      return;
    }
    if (data && typeof data !== 'function') this.write(data);
    this.ended = true;
    const peer = this.peer;
    setImmediate(() => {
      if (peer && !peer.destroyed) peer.emit('end');
      if (done) done();
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

  /**
   * Two ends of one connection. Each is told the other's address, so a server
   * reading `remoteAddress` sees the dialler and vice versa.
   */
  static pair(nearRemote = '', farRemote = '') {
    const a = new MemStream(nearRemote);
    const b = new MemStream(farRemote);
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
    // This network has no MTU of its own, but it stands in for the one that
    // does: without the same ceiling the stream fallback is only ever
    // exercised against a live socket.
    if (data.length > MAX_DATAGRAM_PAYLOAD) return Promise.reject(new PacketTooLargeError(data.length));
    return this.network.deliver(this.addr, addr, data);
  }

  dial(addr) {
    if (!this.streamsAvailable) return Promise.reject(new Error('stream transport unavailable'));
    const target = this.network.lookup(this.addr, addr);
    if (!target) return Promise.reject(new Error(`no route to ${addr}`));
    // The dialler's end knows the address it dialled; the listener's end knows
    // the dialler. A real socket's source port is ephemeral, and this models
    // that: anything comparing ports rather than hosts has to fail here too.
    const [near, far] = MemStream.pair(addr, `${this.addrHost()}:${this.network.ephemeralPort()}`);
    setImmediate(() => target.emit('stream', far));
    return Promise.resolve(near);
  }

  /** This transport's host, without its listening port. */
  addrHost() {
    return this.addr.slice(0, this.addr.lastIndexOf(':'));
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
    this.nextEphemeral = 40000;
  }

  /**
   * A source port for an outbound stream. A real one is ephemeral and different
   * every time, which is exactly why the reflexive-address rule compares hosts
   * and not ports; handing out a fixed port here would let a port comparison
   * pass in tests and fail on a socket.
   */
  ephemeralPort() {
    this.nextEphemeral += 1;
    if (this.nextEphemeral > 60000) this.nextEphemeral = 40000;
    return this.nextEphemeral;
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
