'use strict';

const { EventEmitter } = require('node:events');

const W = require('../proto/wire');
const { Kind } = W;
const { Codec } = require('../proto/codec');
const { UdpTransport } = require('../net/udp');
const { writeFrame, readFrame } = require('../net/framing');
const { Persister } = require('./persistence');

/**
 * The rendezvous service: it knows the set of node addresses and nothing else —
 * no KV data, no chat. Running it here makes this machine the meeting point a
 * LAN cluster forms around.
 *
 * A port of the Go bootstrap server and model, including their two rules that
 * are easy to get wrong: a DISCOVER is itself proof of life (so a joiner whose
 * REGISTER was lost still ends up known), and a datagram answer goes to the
 * address the node *advertises*, not the ephemeral port its packet came from.
 */
class BootstrapServer extends EventEmitter {
  constructor(config, dataFile, opts = {}) {
    super();
    this.config = {
      port: 9999,
      nodeTimeoutMs: 30_000,
      psk: '',
      cluster: 'rezoagwe',
      ...config,
    };
    this.dataFile = dataFile || null;
    this.listen = opts.listen || ((port, o) => UdpTransport.listen(port, o));
    this.now = opts.now || Date.now;

    this.nodes = new Map();
    this.codec = new Codec(this.config.psk, this.config.cluster, { now: this.now });
    this.transport = null;
    this.timers = [];
    this.startedAtMs = 0;
    this.persister = this.dataFile ? new Persister(this.dataFile) : null;
    this.load();
  }

  get isRunning() {
    return this.transport !== null;
  }

  async start() {
    if (this.transport) return;
    this.codec = new Codec(this.config.psk, this.config.cluster, { now: this.now });
    const transport = await this.listen(this.config.port, {
      onError: (what, e) => this.emit('warning', `${what}: ${e.message}`),
    });
    this.transport = transport;
    this.startedAtMs = this.now();
    transport.on('packet', (pkt) => this.handlePacket(pkt));
    transport.on('stream', (conn) => {
      this.serveStream(conn).catch(() => {});
    });
    const interval = Math.max(1000, Math.floor(this.config.nodeTimeoutMs / 2));
    const t = setInterval(() => this.removeStale(), interval);
    if (t.unref) t.unref();
    this.timers.push(t);
    this.publish();
  }

  async stop() {
    const transport = this.transport;
    if (!transport) return;
    for (const t of this.timers) clearInterval(t);
    this.timers = [];
    this.transport = null;
    await transport.close();
    if (this.persister) await this.persister.flush();
    this.publish();
  }

  /** Records (or refreshes) a node, reporting whether this was a new arrival. */
  register(addr, nick) {
    if (!addr) return false;
    const previous = this.nodes.get(addr);
    const isNew = !previous;
    const entry = {
      addr,
      nick: nick || (previous ? previous.nick : ''),
      lastSeen: Math.floor(this.now() / 1000),
    };
    this.nodes.set(addr, entry);
    // Only a new arrival or a rename is worth a disk write; a heartbeat is not.
    if (isNew || (previous && previous.nick !== entry.nick)) this.save();
    this.publish();
    return isNew;
  }

  removeStale() {
    const cutoff = Math.floor(this.now() / 1000) - Math.floor(this.config.nodeTimeoutMs / 1000);
    const removed = [];
    for (const [addr, entry] of [...this.nodes]) {
      if (entry.lastSeen < cutoff) {
        this.nodes.delete(addr);
        removed.push(entry);
      }
    }
    if (removed.length) {
      this.save();
      this.publish();
    }
    return removed;
  }

  /** The roster as sent on the wire, minus the requester: a node has no use for its own address. */
  rosterFor(exclude) {
    return {
      peers: [...this.nodes.values()]
        .filter((n) => n.addr !== exclude)
        .sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0))
        .map((n) => ({ addr: n.addr, nick: n.nick, lastSeen: n.lastSeen })),
    };
  }

  roster() {
    return [...this.nodes.values()].sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));
  }

  handlePacket(packet) {
    let frame;
    try {
      frame = this.codec.decode(packet.data);
    } catch {
      // Unauthenticated traffic on an open port is background noise, not a
      // protocol event worth reporting.
      return;
    }
    let body;
    try {
      body = JSON.parse(frame.body.toString('utf8'));
    } catch {
      return;
    }
    this.handle(frame.kind, body, (reply) => {
      const transport = this.transport;
      if (transport) transport.send(this.replyAddr(packet.from, frame.kind, body), reply).catch(() => {});
    });
  }

  async serveStream(conn) {
    try {
      const raw = await readFrame(conn);
      const frame = this.codec.decode(raw);
      const body = JSON.parse(frame.body.toString('utf8'));
      this.handle(frame.kind, body, (reply) => writeFrame(conn, reply));
    } catch {
      // a malformed or unauthenticated stream is dropped in silence
    } finally {
      conn.end();
      if (conn.destroy) setTimeout(() => conn.destroy(), 100).unref?.();
    }
  }

  handle(kind, body, reply) {
    try {
      if (kind === Kind.BOOTSTRAP_REGISTER) {
        const reg = W.decodeBootstrapRegister(body);
        if (reg.from) this.register(reg.from, reg.nick);
      } else if (kind === Kind.BOOTSTRAP_DISCOVER) {
        const req = W.decodeBootstrapDiscover(body);
        // Asking for the roster is itself proof of life, so a joiner whose
        // REGISTER was lost still ends up known.
        if (req.from) this.register(req.from, '');
        reply(this.codec.encode(Kind.BOOTSTRAP_ROSTER, W.encodeBootstrapRoster(this.rosterFor(req.from))));
      }
    } catch {
      // malformed body: drop it
    }
  }

  /**
   * Where a datagram answer goes. The address a node advertises wins: a node
   * bound to a wildcard address is reachable there, while the source port of its
   * datagram may be ephemeral.
   */
  replyAddr(from, kind, body) {
    if (kind === Kind.BOOTSTRAP_DISCOVER) {
      const advertised = W.decodeBootstrapDiscover(body).from;
      if (advertised) return advertised;
    }
    return from;
  }

  load() {
    if (!this.persister) return;
    let saved;
    try {
      saved = this.persister.load();
    } catch {
      return; // a corrupt roster is not worth failing to start over
    }
    if (!saved || !Array.isArray(saved.nodes)) return;
    // Every restored node's last-seen time is reset: the service was down, so
    // nobody could have checked in, and expiring the whole roster the instant it
    // loads would make persistence pointless. A node genuinely gone is dropped
    // after one timeout anyway.
    const now = Math.floor(this.now() / 1000);
    for (const n of saved.nodes) {
      if (n && n.addr) this.nodes.set(n.addr, { addr: n.addr, nick: n.nick || '', lastSeen: now });
    }
    this.publish();
  }

  save() {
    if (!this.persister) return;
    this.persistGen = (this.persistGen || 0) + 1;
    this.persister.save(this.persistGen, { nodes: this.roster() });
  }

  status() {
    return {
      running: this.isRunning,
      port: this.config.port,
      boundPort: this.transport ? this.transport.port : 0,
      nodes: this.nodes.size,
      cluster: this.config.cluster,
      uptimeSec: this.startedAtMs === 0 ? 0 : Math.floor((this.now() - this.startedAtMs) / 1000),
    };
  }

  publish() {
    this.emit('roster', this.roster());
    this.emit('status', this.status());
  }
}

module.exports = { BootstrapServer };
