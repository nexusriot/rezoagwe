'use strict';

const dgram = require('node:dgram');
const net = require('node:net');
const { EventEmitter } = require('node:events');
const { targetOf } = require('./addr');

/** Bounds a stream dial so an unreachable peer cannot stall a joiner. */
const DIAL_TIMEOUT_MS = 5_000;
/** Bounds one request/response exchange on a stream. */
const STREAM_TIMEOUT_MS = 10_000;

/**
 * Datagrams and streams on one port.
 *
 * Datagrams carry the protocol; streams carry payloads that do not fit one — a
 * full state sync or a large bootstrap roster. Both are framed identically, the
 * stream form simply adding a length prefix.
 *
 * One socket for everything matters: a fresh socket per packet burns a
 * descriptor and an ephemeral port per message, and makes every packet appear
 * to come from a different port.
 */
class UdpTransport extends EventEmitter {
  constructor(socket, server, port) {
    super();
    this.socket = socket;
    this.server = server;
    this.port = port;
    this.closed = false;
    socket.on('message', (data, rinfo) => {
      this.emit('packet', { from: `${rinfo.address}:${rinfo.port}`, data: Buffer.from(data) });
    });
    socket.on('error', (e) => {
      if (!this.closed) this.emit('error', { what: 'datagram socket', error: e });
    });
    if (server) {
      server.on('connection', (conn) => {
        conn.setTimeout(STREAM_TIMEOUT_MS, () => conn.destroy());
        conn.on('error', () => {});
        this.emit('stream', conn);
      });
      server.on('error', (e) => {
        if (!this.closed) this.emit('error', { what: 'stream listener', error: e });
      });
    }
  }

  /**
   * Binds port for datagrams and, when possible, the same port for streams. A
   * failure to bind the stream listener is not fatal: the node still works,
   * falling back to datagram state sync.
   */
  static async listen(port, opts = {}) {
    const host = opts.host || '0.0.0.0';
    const socket = dgram.createSocket({ type: 'udp4', reuseAddr: true });
    await new Promise((resolve, reject) => {
      const onError = (e) => {
        socket.removeListener('listening', onListening);
        reject(e);
      };
      const onListening = () => {
        socket.removeListener('error', onError);
        resolve();
      };
      socket.once('error', onError);
      socket.once('listening', onListening);
      socket.bind(port, host);
    });
    const bound = socket.address().port;

    let server = null;
    try {
      server = await new Promise((resolve, reject) => {
        const srv = net.createServer();
        srv.once('error', reject);
        srv.listen(bound, host, () => {
          srv.removeListener('error', reject);
          resolve(srv);
        });
      });
    } catch (e) {
      server = null;
      if (opts.onError) opts.onError('stream listener unavailable', e);
    }
    return new UdpTransport(socket, server, bound);
  }

  /** False when the TCP port was taken, which drops state sync back to datagrams. */
  get streamsAvailable() {
    return this.server !== null;
  }

  /** Hands one datagram to the kernel. Resolves when handed over, never when delivered. */
  send(addr, data) {
    const target = targetOf(addr);
    if (!target) return Promise.reject(new Error(`unusable address: ${addr}`));
    if (this.closed) return Promise.reject(new Error('transport closed'));
    return new Promise((resolve, reject) => {
      this.socket.send(data, 0, data.length, target.port, target.host, (err) => {
        if (err) reject(err);
        else resolve();
      });
    });
  }

  /** Opens a reliable stream to addr, or rejects when the stream path is unavailable. */
  dial(addr) {
    const target = targetOf(addr);
    if (!target) return Promise.reject(new Error(`unusable address: ${addr}`));
    if (!this.server) return Promise.reject(new Error('stream transport unavailable'));
    return new Promise((resolve, reject) => {
      const conn = net.connect({ host: target.host, port: target.port });
      const timer = setTimeout(() => {
        conn.destroy();
        reject(new Error(`dial ${addr} timed out`));
      }, DIAL_TIMEOUT_MS);
      conn.once('connect', () => {
        clearTimeout(timer);
        conn.setTimeout(STREAM_TIMEOUT_MS, () => conn.destroy());
        resolve(conn);
      });
      conn.once('error', (e) => {
        clearTimeout(timer);
        reject(e);
      });
    });
  }

  close() {
    if (this.closed) return Promise.resolve();
    this.closed = true;
    const closing = [];
    closing.push(new Promise((resolve) => {
      try {
        this.socket.close(() => resolve());
      } catch {
        resolve();
      }
    }));
    if (this.server) {
      closing.push(new Promise((resolve) => this.server.close(() => resolve())));
    }
    return Promise.all(closing).then(() => undefined);
  }
}

module.exports = { UdpTransport, DIAL_TIMEOUT_MS, STREAM_TIMEOUT_MS };
