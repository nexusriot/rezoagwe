'use strict';

const crypto = require('node:crypto');
const { EventEmitter } = require('node:events');

const W = require('../proto/wire');
const { Kind, KVAction, ChatKind } = W;
const { Codec, DecodeError, DecodeException, keyFingerprint } = require('../proto/codec');
const { UdpTransport } = require('../net/udp');
const { writeFrame, readFrame } = require('../net/framing');
const { normalizeAddr, validPeerAddr, localIpv4 } = require('../net/addr');
const { KvStore } = require('./kvstore');
const { Metrics } = require('./metrics');
const { Persister } = require('./persistence');
const { buildTopology, placeGraph } = require('./topology');

const CHAT_RING = 500;
const ACTIVITY_RING = 300;
const STATE_SYNC_CHAT_LINES = 200;

/** Entries in a state response that has to fit one datagram. The stream path has no such limit. */
const DATAGRAM_SYNC_LIMIT = 200;

/** Soft byte budget and entry cap for one anti-entropy batch. */
const BATCH_BYTES = 48 * 1024;
const BATCH_ENTRIES = 64;

const DEFAULTS = {
  advertiseHost: '',
  port: 3137,
  nick: 'desktop',
  seeds: [],
  psk: '',
  cluster: 'rezoagwe',
  gossipIntervalMs: 10_000,
  heartbeatIntervalMs: 5_000,
  evictThresholdMs: 15_000,
  sweepIntervalMs: 5_000,
  tombstoneTtlSec: 0,
  // digestBatch must not exceed either budget: the sender's cursor advances by
  // the whole digest, so a range that identified more repairs than one round can
  // carry has its tail stranded until the cursor wraps the entire keyspace.
  // effectiveDigestBatch() clamps it rather than letting that happen quietly.
  digestBatch: 128,
  maxPush: 128,
  maxPull: 128,
};

/**
 * A rezoagwe peer: membership, replication, chat and anti-entropy.
 *
 * This is a port of the Go node.Node and the Kotlin NodeEngine, and the three
 * have to stay in step — every rule here (last-write-wins merge, digest ranges,
 * version-preserving expiry) is protocol, not implementation detail.
 *
 * It emits 'entries', 'peers', 'chat', 'activity', 'status', 'metrics' and
 * 'topology' so a front end can follow it without polling; it knows nothing
 * about Electron.
 */
class NodeEngine extends EventEmitter {
  constructor(config, dataFile, opts = {}) {
    super();
    this.config = { ...DEFAULTS, ...config };
    this.dataFile = dataFile || null;
    // Injectable so the multi-node tests can run a whole cluster in memory over
    // a lossy network instead of on real sockets.
    this.listen = opts.listen || ((port, o) => UdpTransport.listen(port, o));
    this.now = opts.now || Date.now;

    this.persister = this.dataFile
      ? new Persister(this.dataFile, { onError: (e) => this.logActivity(`persist failed: ${e.message}`) })
      : null;

    let saved = null;
    if (this.persister) {
      try {
        saved = this.persister.load();
      } catch (e) {
        // A corrupt state file must not stop the node: losing the store is bad,
        // refusing to start is worse.
        saved = null;
        this.loadError = e.message;
      }
    }

    // Identity is persisted, not derived from the listen address: a node that
    // moves to another port is still the same writer, and its version tiebreak
    // has to stay stable or its old and new writes sort oddly against each other.
    this.nodeId = (saved && saved.node_id) || crypto.randomUUID();
    this.store = new KvStore(this.nodeId, { nowSec: () => Math.floor(this.now() / 1000) });
    this.metrics = new Metrics();
    this.codec = new Codec(this.config.psk, this.config.cluster, { now: this.now });

    this.transport = null;
    this.timers = [];
    this.peers = new Map();
    this.idToAddr = new Map();
    // What each peer last told us its own peer list was. This is the only source
    // of links between two nodes that are not us, and so the only way the app can
    // draw the cluster rather than just our corner of it.
    this.gossipViews = new Map();
    this.chatLog = [];
    this.activityLog = [];
    this.persistGen = 0;
    // One cursor per peer. A shared cursor divided the keyspace among whichever
    // peers the random target happened to pick, so covering the whole store
    // against any single peer took as many wraps as there were peers.
    this.aeCursors = new Map();
    this.startedAtMs = 0;
    this.nick = this.config.nick;

    if (saved) {
      this.store.loadState(saved.clock, saved.entries);
      for (const entry of saved.chat || []) {
        try {
          this.chatLog.push(W.decodeChatEntry(entry));
        } catch {
          // one unreadable line is not worth discarding the conversation
        }
      }
    }
    this.store.setOnChange(() => this.persist());
    // A freshly generated identity has to reach disk even before the first
    // write, or a restart before any write would mint another one.
    if (this.persister && (!saved || saved.node_id !== this.nodeId)) this.persist();
  }

  /** The address peers are told to reach this node at. */
  get addr() {
    return `${this.config.advertiseHost || localIpv4()}:${this.config.port}`;
  }

  get isRunning() {
    return this.transport !== null;
  }

  // ---- lifecycle -------------------------------------------------------

  /**
   * Binds the sockets and starts the loops, then joins on its own promise.
   *
   * Serving and joining are deliberately separate: it is what lets a test wire a
   * cluster by hand, and it means an unreachable bootstrap can never stall
   * startup.
   */
  async start() {
    if (this.transport) return;
    const transport = await this.listen(this.config.port, {
      onError: (what, e) => this.logActivity(`${what}: ${e.message}`),
    });
    this.transport = transport;
    this.startedAtMs = this.now();
    transport.on('packet', (pkt) => this.handlePacket(pkt));
    transport.on('stream', (conn) => {
      this.serveStream(conn).catch(() => {
        this.metrics.streamErrors++;
      });
    });
    transport.on('error', ({ what, error }) => this.logActivity(`${what}: ${error.message}`));

    this.every(this.config.gossipIntervalMs, () => this.gossipTick());
    this.every(this.config.heartbeatIntervalMs, () => this.heartbeatTick());
    this.every(this.config.heartbeatIntervalMs, () => this.evictTick());
    this.every(this.config.sweepIntervalMs, () => this.sweepTick());

    this.publishStatus();
    this.joining = this.join().catch((e) => this.logActivity(`join: ${e.message}`));
  }

  /**
   * Announces departure, closes the transport and stops the loops.
   *
   * The goodbye goes first: peers drop this node at once instead of waiting out
   * the eviction timeout. Membership is what the socket learned, so it dies with
   * the socket — keeping it would leave the peer list and the graph claiming a
   * live cluster, frozen at "seen 0s ago", for a node that stopped listening.
   */
  async stop() {
    const transport = this.transport;
    if (!transport) return;
    await this.broadcast(Kind.GOODBYE, W.encodeGoodbye({ from: this.addr, nick: this.nick }));
    for (const t of this.timers) clearInterval(t);
    this.timers = [];
    this.transport = null;
    await transport.close();
    this.peers.clear();
    this.idToAddr.clear();
    this.gossipViews.clear();
    if (this.persister) await this.persister.flush();
    this.publishPeers();
    this.publishStatus();
  }

  every(ms, fn) {
    const t = setInterval(() => {
      Promise.resolve()
        .then(fn)
        .catch((e) => this.logActivity(`loop: ${e.message}`));
    }, ms);
    if (t.unref) t.unref();
    this.timers.push(t);
  }

  // ---- outbound --------------------------------------------------------

  async sendFrame(addr, kind, body) {
    const transport = this.transport;
    if (!transport) return;
    const pkt = this.codec.encodeBody(kind, body);
    try {
      await transport.send(addr, pkt);
      this.metrics.sent(kind, pkt.length);
      this.metrics.sentTo(normalizeAddr(addr), pkt.length);
    } catch (e) {
      this.metrics.sendFailed(normalizeAddr(addr), e);
    }
  }

  send(addr, kind, value) {
    return this.sendFrame(addr, kind, JSON.stringify(value));
  }

  /**
   * Sends one serialised body to every known peer.
   *
   * The body is serialised once but framed per peer, so each gets its own nonce:
   * a shared nonce is harmless (the replay cache is per receiver) but a fresh one
   * costs nothing and keeps the frames independent.
   */
  async broadcast(kind, value) {
    const body = typeof value === 'string' ? value : JSON.stringify(value);
    await Promise.all([...this.peers.keys()].map((addr) => this.sendFrame(addr, kind, body)));
  }

  peerAddrs() {
    return [...this.peers.keys()];
  }

  randomPeer() {
    const peers = this.peerAddrs();
    if (!peers.length) return '';
    return peers[Math.floor(Math.random() * peers.length)];
  }

  helloMessage() {
    return W.encodeHello({ from: this.addr, nick: this.nick, id: this.nodeId });
  }

  helloAllPeers() {
    return this.broadcast(Kind.HELLO, this.helloMessage());
  }

  // ---- membership ------------------------------------------------------

  /** Adds a peer, returning true when it was newly learned. Malformed addresses are refused at the door. */
  addPeer(addr) {
    if (!addr || this.isSelf(addr) || !validPeerAddr(addr)) return false;
    const now = this.now();
    const existing = this.peers.get(addr);
    const added = !existing;
    this.peers.set(addr, {
      addr,
      nick: existing ? existing.nick : '',
      firstSeenMs: existing ? existing.firstSeenMs : now,
      lastSeenMs: now,
    });
    if (added) this.publishPeers();
    return added;
  }

  touchPeer(addr) {
    if (!addr || this.isSelf(addr)) return;
    const existing = this.peers.get(addr);
    if (existing) this.peers.set(addr, { ...existing, lastSeenMs: this.now() });
  }

  setPeerNick(addr, nick) {
    if (!addr || !nick) return;
    const existing = this.peers.get(addr);
    if (existing && existing.nick !== nick) {
      this.peers.set(addr, { ...existing, nick });
      this.publishPeers();
    }
  }

  removePeer(addr) {
    this.peers.delete(addr);
    for (const [id, mapped] of [...this.idToAddr]) if (mapped === addr) this.idToAddr.delete(id);
    this.publishPeers();
    // A departed peer's cursor goes with it, or a long-running node accumulates
    // one per address it has ever spoken to, and a peer that returns resumes a
    // walk from before it left.
    this.aeCursors.delete(addr);
  }

  /**
   * Drops a peer now instead of waiting out the eviction window.
   *
   * Gossip will usually teach it back, which is the point: it is how a link can
   * be cut on purpose to see whether the cluster heals.
   */
  forgetPeer(addr) {
    if (!this.peers.has(addr)) return false;
    const nick = this.nickOf(addr);
    this.removePeer(addr);
    this.announceLeave(addr, nick);
    return true;
  }

  nickOf(addr) {
    const p = this.peers.get(addr);
    return p ? p.nick : '';
  }

  isSelf(candidate) {
    return candidate === this.addr || normalizeAddr(candidate) === normalizeAddr(this.addr);
  }

  /** Renders a version's origin as something a human can read. */
  writerName(id) {
    if (!id) return '?';
    if (id === this.nodeId) return 'you';
    const addr = this.idToAddr.get(id);
    if (!addr) return id.slice(0, 8);
    return this.nickOf(addr) || addr;
  }

  peerName(addr) {
    return this.nickOf(addr) || addr;
  }

  // ---- local operations ------------------------------------------------

  /** Writes a key and replicates it. A non-zero ttl expires it on every replica at the same instant. */
  async set(key, value, ttlSeconds = 0) {
    return this.write(key, value, ttlSeconds, null);
  }

  /**
   * Writes only if the key still holds expect; a zero version requires the key to
   * be absent. This is the primitive a lock or a leader election is built on.
   */
  async compareAndSet(key, value, ttlSeconds, expect) {
    return this.write(key, value, ttlSeconds, expect);
  }

  async write(key, value, ttlSeconds, expect) {
    const expiresAt = ttlSeconds > 0 ? Math.floor(this.now() / 1000) + Number(ttlSeconds) : 0;
    const update = this.store.write(key, value, { expiresAt, expect });
    if (!update) {
      this.metrics.kvCasFailures++;
      return null;
    }
    this.metrics.kvLocalWrites++;
    await this.broadcast(Kind.KV, W.encodeKVUpdate(update));
    this.publishEntries();
    return update;
  }

  async remove(key, expect) {
    const update = this.store.remove(key, { expect });
    if (!update) {
      this.metrics.kvCasFailures++;
      return null;
    }
    this.metrics.kvLocalWrites++;
    await this.broadcast(Kind.KV, W.encodeKVUpdate(update));
    this.publishEntries();
    return update;
  }

  delete(key) {
    return this.remove(key, null);
  }

  compareAndDelete(key, expect) {
    return this.remove(key, expect);
  }

  history(key) {
    return this.store.historyOf(key);
  }

  /** Serialises the whole store, tombstones included, in the Go interchange format. */
  export() {
    return JSON.stringify({
      node: this.addr,
      cluster: this.config.cluster,
      exported_at: Math.floor(this.now() / 1000),
      entries: this.store.updates().map(W.encodeKVUpdate),
    }, null, 2);
  }

  /**
   * Loads an export.
   *
   * Preserving the exported versions merges under the usual last-write-wins rule,
   * so an entry older than what the cluster holds is correctly ignored — a
   * restore. Re-stamping every entry as a fresh local write forces it to win — a
   * seed.
   */
  async import(data, asLocalWrites) {
    const file = JSON.parse(data);
    const entries = Array.isArray(file.entries) ? file.entries : [];
    let applied = 0;
    for (const raw of entries) {
      let u;
      try {
        u = W.decodeKVUpdate(raw);
      } catch {
        continue;
      }
      if (asLocalWrites) {
        if (u.action === KVAction.DELETE) {
          await this.delete(u.key);
        } else {
          let ttl = 0;
          if (u.expiresAt > 0) {
            ttl = u.expiresAt - Math.floor(this.now() / 1000);
            if (ttl <= 0) continue; // already expired; nothing to seed
          }
          await this.set(u.key, u.value, ttl);
        }
        applied++;
      } else if (this.store.apply(u)) {
        applied++;
        await this.broadcast(Kind.KV, W.encodeKVUpdate(u));
      }
    }
    if (applied > 0) {
      this.logActivity(`imported ${applied} ${applied === 1 ? 'entry' : 'entries'}`);
      this.publishEntries();
    }
    return applied;
  }

  // ---- chat ------------------------------------------------------------

  sendChat(text) {
    return this.sendChatMessage(text, false);
  }

  sendAction(text) {
    return this.sendChatMessage(text, true);
  }

  async sendChatMessage(text, action) {
    const ts = Math.floor(this.now() / 1000);
    this.appendChat({
      ts,
      sender: this.addr,
      nick: this.nick,
      text,
      kind: action ? ChatKind.ACTION : ChatKind.MESSAGE,
      to: '',
    });
    await this.broadcast(Kind.CHAT, W.encodeChatMessage({
      sender: this.addr, nick: this.nick, text, ts, action,
    }));
  }

  /** Delivers a message to one peer only, addressed by nickname or address. */
  async sendDirect(target, text) {
    const addr = this.resolvePeer(target);
    if (!addr) return false;
    const ts = Math.floor(this.now() / 1000);
    await this.send(addr, Kind.DIRECT_MESSAGE, W.encodeChatMessage({
      sender: this.addr, nick: this.nick, text, ts, to: addr,
    }));
    this.appendChat({ ts, sender: this.addr, nick: this.nick, text, kind: ChatKind.DIRECT, to: addr });
    return true;
  }

  resolvePeer(target) {
    if (this.peers.has(target)) return target;
    for (const p of this.peers.values()) {
      if (p.nick && p.nick.toLowerCase() === String(target).toLowerCase()) return p.addr;
    }
    return '';
  }

  async renameSelf(nick) {
    const previous = this.nick;
    this.nick = nick;
    this.config = { ...this.config, nick };
    this.system(`${previous} is now known as ${nick}`);
    await this.helloAllPeers();
    this.publishStatus();
    this.publishTopology();
  }

  /**
   * Handles one line of input, slash commands included.
   *
   * Commands live in the engine rather than a front end so the TUI, the HTTP
   * gateway, the Android app and this client all understand the same vocabulary.
   */
  async submit(input) {
    const text = String(input || '').trim();
    if (!text) return;
    if (!text.startsWith('/')) {
      await this.sendChat(text);
      return;
    }
    const [cmd, rest] = split2(text.slice(1));
    switch (cmd.toLowerCase()) {
      case 'help':
      case '?':
        this.system('commands: /nick <name>  /me <text>  /msg <peer> <text>  /peers  '
          + '/keys  /get <key>  /set <key> <value>  /setttl <key> <seconds> <value>  /del <key>');
        break;
      case 'nick':
        if (!rest) this.system('usage: /nick <name>');
        else await this.renameSelf(rest);
        break;
      case 'me':
        if (!rest) this.system('usage: /me <text>');
        else await this.sendAction(rest);
        break;
      case 'msg':
      case 'dm': {
        const [target, body] = split2(rest);
        if (!target || !body) this.system('usage: /msg <peer> <text>');
        else if (!(await this.sendDirect(target, body))) this.system(`no such peer: ${target}`);
        break;
      }
      case 'peers': {
        const known = this.peerAddrs().sort();
        if (!known.length) this.system('no peers known');
        else this.system('peers: ' + known.map((p) => this.peerName(p)).join(', '));
        break;
      }
      case 'keys': {
        const keys = this.store.entries().map((e) => e.key);
        if (!keys.length) this.system('store is empty');
        else this.system('keys: ' + keys.join(', '));
        break;
      }
      case 'get': {
        if (!rest) {
          this.system('usage: /get <key>');
          break;
        }
        const value = this.store.get(rest);
        this.system(value === undefined ? `${rest} is not set` : `${rest} = ${value}`);
        break;
      }
      case 'set': {
        const [key, value] = split2(rest);
        if (!key) this.system('usage: /set <key> <value>');
        else {
          await this.set(key, value);
          this.system(`set ${key}`);
        }
        break;
      }
      case 'setttl': {
        const [key, remainder] = split2(rest);
        const [secs, value] = split2(remainder);
        const ttl = Number(secs);
        if (!key || !Number.isFinite(ttl) || ttl <= 0) this.system('usage: /setttl <key> <seconds> <value>');
        else {
          await this.set(key, value, ttl);
          this.system(`set ${key} (expires in ${ttl}s)`);
        }
        break;
      }
      case 'del':
      case 'delete':
        if (!rest) this.system('usage: /del <key>');
        else {
          await this.delete(rest);
          this.system(`deleted ${rest}`);
        }
        break;
      default:
        this.system(`unknown command: /${cmd} (try /help)`);
    }
  }

  /** Records a local-only notice. System lines carry no sender; they are what makes the pane readable. */
  system(text) {
    this.appendChat({ ts: Math.floor(this.now() / 1000), sender: '', nick: '', text, kind: ChatKind.SYSTEM, to: '' });
  }

  appendChat(entry) {
    this.chatLog.push(entry);
    while (this.chatLog.length > CHAT_RING) this.chatLog.shift();
    this.persist();
    this.publishChat();
  }

  announceJoin(addr, nick) {
    this.system(nick ? `${nick} (${addr}) joined` : `${addr} joined`);
  }

  announceLeave(addr, nick) {
    this.system(nick ? `${nick} (${addr}) left` : `${addr} left`);
  }

  logActivity(text) {
    const stamp = new Date(this.now()).toTimeString().slice(0, 8);
    this.activityLog.push(`${stamp} ${text}`);
    while (this.activityLog.length > ACTIVITY_RING) this.activityLog.shift();
    this.emit('activity', this.activityLog.slice());
  }

  // ---- inbound ---------------------------------------------------------

  handlePacket(packet) {
    const source = normalizeAddr(packet.from);
    let frame;
    try {
      frame = this.codec.decode(packet.data);
    } catch (e) {
      const reason = e instanceof DecodeException ? e.reason : DecodeError.MALFORMED;
      if (reason === DecodeError.BAD_MAC) this.metrics.authFailures++;
      else if (reason === DecodeError.REPLAY) this.metrics.replayDrops++;
      else if (reason === DecodeError.CLOCK_SKEW) this.metrics.skewDrops++;
      else this.metrics.malformedDrops++;
      // Attributed to the socket source: a rejected frame is exactly the case
      // where the sender's claim about who it is cannot be trusted.
      this.metrics.rejectedFrom(source);
      return;
    }
    this.metrics.received(frame.kind, packet.data.length);
    this.metrics.receivedFrom(source, packet.data.length);
    this.dispatch(frame.kind, frame.body).catch((e) => this.logActivity(`dispatch: ${e.message}`));
  }

  async dispatch(kind, bodyBuf) {
    let body;
    try {
      body = JSON.parse(bodyBuf.toString('utf8'));
    } catch {
      this.metrics.malformedDrops++;
      return;
    }
    try {
      switch (kind) {
        case Kind.KV:
          if (this.applyRemote(W.decodeKVUpdate(body))) this.publishEntries();
          break;
        case Kind.KV_BATCH: {
          let changed = false;
          for (const u of W.decodeKVBatch(body).updates) if (this.applyRemote(u)) changed = true;
          if (changed) this.publishEntries();
          break;
        }
        case Kind.CHAT:
          await this.handleChat(W.decodeChatMessage(body));
          break;
        case Kind.DIRECT_MESSAGE:
          this.handleDirect(W.decodeChatMessage(body));
          break;
        case Kind.STATE_REQUEST:
          await this.handleStateRequest(W.decodeStateRequest(body));
          break;
        case Kind.STATE_RESPONSE:
          this.mergeState(W.decodeStateResponse(body));
          break;
        case Kind.PEER_GOSSIP:
          await this.handleGossip(W.decodePeerGossip(body));
          break;
        case Kind.HELLO:
          this.handleHello(W.decodeHello(body));
          break;
        case Kind.GOODBYE:
          this.handleGoodbye(W.decodeGoodbye(body));
          break;
        case Kind.DIGEST:
          await this.handleDigest(W.decodeDigest(body));
          break;
        case Kind.PULL_REQUEST:
          await this.handlePull(W.decodePullRequest(body));
          break;
        case Kind.BOOTSTRAP_ROSTER:
          await this.applyRoster(W.decodeBootstrapRoster(body));
          break;
        case Kind.FINGERPRINT:
          await this.handleFingerprint(W.decodeFingerprint(body));
          break;
        case Kind.FINGERPRINT_REPLY:
          // The checker reads its answers off the stream it asked over, so a
          // datagram reply needs accepting but not correlating.
          this.touchPeer(W.decodeFingerprintReply(body).from);
          break;
        default:
          this.metrics.malformedDrops++;
      }
    } catch (e) {
      this.metrics.malformedDrops++;
    }
  }

  /**
   * How far the anti-entropy sweep has walked, as one line.
   *
   * There is a cursor per peer now, so a single value would name whichever one
   * happened to be read. What matters is how many peers are part-way through a
   * sweep and where the furthest has reached.
   */
  sweepSummary() {
    const known = this.peers.size;
    if (this.aeCursors.size === 0) return `all ${known} peer(s) at the start of the keyspace`;
    const furthest = [...this.aeCursors.values()].sort().pop();
    return `${this.aeCursors.size} of ${known} peer(s) mid-sweep, furthest past "${furthest}"`;
  }

  /**
   * Asks every peer to summarise its store and compares the answers with this
   * node's.
   *
   * Over streams, not datagrams: a dropped answer would read as a peer that
   * disagrees, which is exactly the wrong conclusion to draw from packet loss.
   */
  async checkConsistency() {
    const local = this.store.fingerprint();
    const peers = [...this.peers.keys()];
    const results = await Promise.all(peers.map((addr) => this.fingerprintPeer(addr, local)));
    results.sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));

    return {
      addr: this.addr,
      keys: local.keys,
      tombstones: local.tombstones,
      clock: local.clock,
      checkedAtMs: Date.now(),
      peers: results,
      converged: !results.some((p) => p.reachable && !p.agrees),
      unreachable: results.filter((p) => !p.reachable).length,
    };
  }

  async fingerprintPeer(addr, local) {
    const base = { addr, nick: this.nickOf(addr), reachable: false, error: '', keys: 0, tombstones: 0, clock: 0, agrees: false, differingBuckets: [] };
    const reply = await this.requestFingerprint(addr);
    if (!reply) return { ...base, error: 'no answer' };
    if (reply.buckets.length !== local.buckets.length) {
      // A peer summarising at a different granularity cannot be compared bucket
      // by bucket; say so rather than reporting a false divergence.
      return { ...base, error: `peer reported ${reply.buckets.length} buckets, this node uses ${local.buckets.length}` };
    }
    const differingBuckets = local.buckets
      .map((v, i) => (v === reply.buckets[i] ? -1 : i))
      .filter((i) => i >= 0);
    return {
      ...base,
      nick: reply.nick || base.nick,
      reachable: true,
      keys: reply.keys,
      tombstones: reply.tombstones,
      clock: reply.clock,
      agrees: differingBuckets.length === 0,
      differingBuckets,
    };
  }

  /**
   * One framed round trip that hands the answer back, rather than dispatching it
   * like exchangeStream does. A consistency check needs the reply, not a side
   * effect.
   */
  async requestFingerprint(peer) {
    let conn;
    try {
      conn = await this.transport.dial(peer);
      const pkt = this.codec.encode(Kind.FINGERPRINT, W.encodeFingerprint({ from: this.addr }));
      writeFrame(conn, pkt);
      this.metrics.sent(Kind.FINGERPRINT, pkt.length);

      const raw = await readFrame(conn);
      const frame = this.codec.decode(raw);
      this.metrics.received(frame.kind, raw.length);
      if (frame.kind !== Kind.FINGERPRINT_REPLY) return null;
      return W.decodeFingerprintReply(JSON.parse(frame.body.toString('utf8')));
    } catch (e) {
      this.metrics.streamErrors++;
      return null;
    } finally {
      if (conn) {
        try {
          conn.end();
        } catch (e) { /* already gone */ }
      }
    }
  }

  /** This node's store summary, labelled so a report can name who produced it. */
  fingerprintReply() {
    const r = this.store.fingerprint();
    r.from = this.addr;
    r.nick = this.nick;
    return r;
  }

  /** Answers a summary request that arrived as a datagram. */
  async handleFingerprint(req) {
    if (!req.from) return;
    this.touchPeer(req.from);
    await this.send(req.from, Kind.FINGERPRINT_REPLY, W.encodeFingerprintReply(this.fingerprintReply()));
  }

  /**
   * Merges one remote update, counting and narrating the outcome.
   *
   * A rejected update is not an error — it is a conflict that last-write-wins
   * resolved — but it is invisible without the activity feed, which is exactly
   * when replication bugs hide.
   */
  applyRemote(u) {
    if (this.store.apply(u)) {
      this.metrics.kvApplied++;
      const who = this.writerName(u.version.node);
      if (u.action === KVAction.DELETE) this.logActivity(`${who} deleted ${u.key} (v${u.version.counter})`);
      else this.logActivity(`${who} set ${u.key} = ${preview(u.value)} (v${u.version.counter})`);
      return true;
    }
    this.metrics.kvRejectedStale++;
    this.logActivity(
      `ignored stale ${u.action} for ${u.key} (v${u.version.counter} from ${this.writerName(u.version.node)})`,
    );
    return false;
  }

  async handleChat(m) {
    this.touchPeer(m.sender);
    if (m.nick) this.setPeerNick(m.sender, m.nick);
    this.appendChat({
      ts: m.ts,
      sender: m.sender,
      nick: m.nick,
      text: m.text,
      kind: m.action ? ChatKind.ACTION : ChatKind.MESSAGE,
      to: '',
    });
    // Learn about a chat sender we did not know as a peer.
    if (this.addPeer(m.sender)) {
      this.announceJoin(m.sender, m.nick);
      await this.send(m.sender, Kind.HELLO, this.helloMessage());
    }
  }

  handleDirect(m) {
    this.touchPeer(m.sender);
    if (m.nick) this.setPeerNick(m.sender, m.nick);
    this.appendChat({
      ts: m.ts, sender: m.sender, nick: m.nick, text: m.text, kind: ChatKind.DIRECT, to: this.addr,
    });
  }

  async handleStateRequest(req) {
    this.touchPeer(req.from);
    this.metrics.stateSyncOut++;
    // The datagram path has to stay inside one packet, so it ships a bounded
    // slice of the store. A peer that used the stream path gets everything.
    await this.send(req.from, Kind.STATE_RESPONSE, W.encodeStateResponse(this.stateResponse(DATAGRAM_SYNC_LIMIT)));
  }

  /**
   * Merges a peer snapshot under last-write-wins rather than overwriting, so a
   * local edit made before the sync arrived is not clobbered by a stale value.
   */
  mergeState(resp) {
    this.metrics.stateSyncIn++;
    let changed = false;
    for (const u of resp.kv) {
      if (this.store.apply(u)) {
        this.metrics.kvApplied++;
        changed = true;
      }
    }
    if (resp.chat.length) this.mergeChat(resp.chat);
    if (changed) {
      this.logActivity(`state sync merged ${resp.kv.length} entries`);
      this.publishEntries();
    }
  }

  /**
   * Folds synced history into the log.
   *
   * A snapshot repeats history this node may already hold — a joiner that syncs
   * from two peers is handed the same conversation twice — so entries are merged
   * on identity and put back in time order rather than appended wholesale.
   */
  mergeChat(history) {
    const key = (e) => `${e.ts}\u0001${e.sender}\u0001${e.nick}\u0001${e.text}\u0001${e.kind}\u0001${e.to}`;
    const seen = new Set(this.chatLog.map(key));
    let grew = false;
    for (const entry of history) {
      const k = key(entry);
      if (seen.has(k)) continue;
      seen.add(k);
      this.chatLog.push(entry);
      grew = true;
    }
    if (!grew) return;
    this.chatLog.sort((a, b) => a.ts - b.ts);
    while (this.chatLog.length > CHAT_RING) this.chatLog.shift();
    this.persist();
    this.publishChat();
  }

  async handleGossip(g) {
    this.touchPeer(g.from);
    if (g.nick) this.setPeerNick(g.from, g.nick);
    if (g.id) this.idToAddr.set(g.id, g.from);
    this.recordView(g.from, g.peers);
    if (this.addPeer(g.from)) {
      this.announceJoin(g.from, g.nick);
      await this.send(g.from, Kind.HELLO, this.helloMessage());
    }
    for (const p of g.peers) {
      if (this.addPeer(p)) {
        this.announceJoin(p, this.nickOf(p));
        await this.send(p, Kind.HELLO, this.helloMessage());
      }
    }
  }

  handleHello(h) {
    this.touchPeer(h.from);
    if (h.nick) this.setPeerNick(h.from, h.nick);
    if (h.id) this.idToAddr.set(h.id, h.from);
    if (this.addPeer(h.from)) this.announceJoin(h.from, h.nick);
  }

  /** Keeps the sender's advertised peer list, which is what makes a cluster-wide graph possible. */
  recordView(from, advertised) {
    if (!from) return;
    const owner = normalizeAddr(from);
    if (owner === normalizeAddr(this.addr)) return;
    this.gossipViews.set(owner, {
      peers: [...new Set(advertised.map(normalizeAddr).filter(Boolean))],
      atMs: this.now(),
    });
    this.publishTopology();
  }

  /**
   * Drops a peer that announced a clean shutdown. The nick is resolved before
   * removal (which forgets it) so the "left" line reads the same as an eviction.
   */
  handleGoodbye(g) {
    if (!this.peers.has(g.from)) return;
    const nick = g.nick || this.nickOf(g.from);
    this.removePeer(g.from);
    this.announceLeave(g.from, nick);
  }

  // ---- anti-entropy ----------------------------------------------------

  /**
   * Advertises a slice of the local keyspace to one peer.
   *
   * This closes the last correctness gap in the replication model: a dropped KV
   * datagram is never re-sent by the write path, so two stores stay divergent
   * until the next overlapping write. The digest carries versions, not values,
   * so a round is cheap; the peer answers with exactly the repairs needed in
   * either direction.
   *
   * The cursor walks the sorted keyspace so a store larger than one digest is
   * covered completely, batch by batch, instead of re-comparing the first N keys
   * forever.
   */
  /** The digest width actually used: never wider than one round can repair. */
  effectiveDigestBatch() {
    return Math.min(this.config.digestBatch, this.config.maxPush, this.config.maxPull);
  }

  async antiEntropyRound(target) {
    if (!target) return;
    const digest = this.store.digest(this.aeCursors.get(target) || '', this.effectiveDigestBatch());
    if (digest.hi === '') this.aeCursors.delete(target); // covered the tail; start over
    else if (digest.entries.length) {
      this.aeCursors.set(target, digest.entries[digest.entries.length - 1].key);
    }
    this.metrics.aeRounds++;
    await this.send(target, Kind.DIGEST, W.encodeDigest({ ...digest, from: this.addr }));
  }

  /** Answers a peer's digest: push what this node holds and the peer does not, pull the reverse. */
  async handleDigest(d) {
    this.touchPeer(d.from);
    const { push, pull } = this.store.reconcile(d, this.config.maxPush, this.config.maxPull);
    if (push.length) {
      this.metrics.aePushed += push.length;
      this.logActivity(`anti-entropy: pushing ${push.length} ${plural(push.length)} to ${this.peerName(d.from)}`);
      await this.sendUpdates(d.from, push);
    }
    if (pull.length) {
      this.metrics.aePulled += pull.length;
      this.logActivity(`anti-entropy: pulling ${pull.length} ${plural(pull.length)} from ${this.peerName(d.from)}`);
      await this.send(d.from, Kind.PULL_REQUEST, W.encodePullRequest({ from: this.addr, keys: pull }));
    }
  }

  async handlePull(req) {
    this.touchPeer(req.from);
    const updates = this.store.updatesFor(req.keys);
    if (updates.length) await this.sendUpdates(req.from, updates);
  }

  /** Ships updates as batches, split on a byte budget so a repair of large values still fits a datagram. */
  async sendUpdates(addr, updates) {
    let batch = [];
    let size = 0;
    const flush = async () => {
      if (!batch.length) return;
      const payload = W.encodeKVBatch({ updates: batch });
      batch = [];
      size = 0;
      await this.send(addr, Kind.KV_BATCH, payload);
    };
    for (const u of updates) {
      // Rough per-entry overhead for the JSON keys and the version object.
      const entrySize = u.key.length + (u.value || '').length + 160;
      if (batch.length && (size + entrySize > BATCH_BYTES || batch.length >= BATCH_ENTRIES)) await flush();
      batch.push(u);
      size += entrySize;
    }
    await flush();
  }

  // ---- state sync & bootstrap ------------------------------------------

  /** Builds a snapshot for a joining peer. A limit of 0 means the whole store, safe only on a stream. */
  stateResponse(limit) {
    const updates = this.store.updates();
    // Direct messages are stripped: handing a joining node someone else's private
    // conversation because it asked for history would be a quiet privacy leak.
    const chat = this.chatLog.filter((e) => e.kind !== ChatKind.DIRECT).slice(-STATE_SYNC_CHAT_LINES);
    return { kv: limit > 0 && updates.length > limit ? updates.slice(0, limit) : updates, chat };
  }

  /**
   * Serves one inbound stream. A stream carries exactly one request frame and one
   * response frame, which is all state sync needs and keeps the connection
   * short-lived.
   */
  async serveStream(conn) {
    try {
      const raw = await readFrame(conn);
      const frame = this.codec.decode(raw);
      this.metrics.received(frame.kind, raw.length);
      if (frame.kind === Kind.STATE_REQUEST) {
        const req = W.decodeStateRequest(JSON.parse(frame.body.toString('utf8')));
        this.touchPeer(req.from);
        this.metrics.stateSyncOut++;
        // A stream has no datagram ceiling: send the whole store.
        const pkt = this.codec.encode(Kind.STATE_RESPONSE, W.encodeStateResponse(this.stateResponse(0)));
        writeFrame(conn, pkt);
        this.metrics.sent(Kind.STATE_RESPONSE, pkt.length);
      } else if (frame.kind === Kind.FINGERPRINT) {
        const req = W.decodeFingerprint(JSON.parse(frame.body.toString('utf8')));
        this.touchPeer(req.from);
        const pkt = this.codec.encode(Kind.FINGERPRINT_REPLY, W.encodeFingerprintReply(this.fingerprintReply()));
        writeFrame(conn, pkt);
        this.metrics.sent(Kind.FINGERPRINT_REPLY, pkt.length);
      } else {
        await this.dispatch(frame.kind, frame.body);
      }
    } catch (e) {
      this.metrics.streamErrors++;
    } finally {
      conn.end();
      if (conn.destroy) setTimeout(() => conn.destroy(), 100).unref?.();
    }
  }

  /** One framed request/response round trip. Resolves false when the stream path is unavailable. */
  async exchangeStream(peer, kind, value) {
    const transport = this.transport;
    if (!transport) return false;
    let conn;
    try {
      conn = await transport.dial(peer);
    } catch {
      return false;
    }
    try {
      const pkt = this.codec.encode(kind, value);
      writeFrame(conn, pkt);
      this.metrics.sent(kind, pkt.length);
      const raw = await readFrame(conn);
      const frame = this.codec.decode(raw);
      this.metrics.received(frame.kind, raw.length);
      await this.dispatch(frame.kind, frame.body);
      return true;
    } catch (e) {
      this.metrics.streamErrors++;
      return false;
    } finally {
      try {
        conn.end();
        if (conn.destroy) conn.destroy();
      } catch {
        // already gone
      }
    }
  }

  /**
   * Pulls a peer's snapshot, preferring the stream path so the response is not
   * capped by the datagram size. A store or chat history larger than a datagram
   * used to sync as nothing at all: the send simply failed.
   */
  async requestStateFrom(peer) {
    const req = W.encodeStateRequest({ from: this.addr });
    if (await this.exchangeStream(peer, Kind.STATE_REQUEST, req)) return;
    await this.send(peer, Kind.STATE_REQUEST, req);
  }

  /** Contacts bootstrap, greets everyone learned, and pulls one peer's store. */
  async join() {
    await this.registerWithBootstrap();
    await this.discoverFromBootstrap();
    await this.helloAllPeers();
    const peer = this.randomPeer();
    if (peer) await this.requestStateFrom(peer);
  }

  async registerWithBootstrap() {
    const reg = W.encodeBootstrapRegister({ from: this.addr, nick: this.nick });
    for (const seed of this.config.seeds) {
      if (seed) await this.send(seed, Kind.BOOTSTRAP_REGISTER, reg);
    }
  }

  /**
   * Asks the seeds for the roster: the stream path first (an unbounded roster),
   * then datagrams, whose reply arrives asynchronously through the packet loop —
   * so an unreachable rendezvous service can never stall startup.
   */
  async discoverFromBootstrap() {
    const req = W.encodeBootstrapDiscover({ from: this.addr });
    for (const seed of this.config.seeds) {
      if (!seed) continue;
      if (await this.exchangeStream(seed, Kind.BOOTSTRAP_DISCOVER, req)) return;
    }
    for (const seed of this.config.seeds) {
      if (seed) await this.send(seed, Kind.BOOTSTRAP_DISCOVER, req);
    }
  }

  /** Adopts the peers a bootstrap reported. */
  async applyRoster(roster) {
    let learned = false;
    for (const p of roster.peers) {
      if (this.addPeer(p.addr)) learned = true;
      if (p.nick) this.setPeerNick(p.addr, p.nick);
    }
    if (!learned) return;
    await this.helloAllPeers();
    const peer = this.randomPeer();
    if (peer) await this.requestStateFrom(peer);
  }

  // ---- loops -----------------------------------------------------------

  async gossipTick() {
    const known = this.peerAddrs();
    if (!known.length) return;
    await this.gossipTo(known[Math.floor(Math.random() * known.length)]);
  }

  /**
   * One gossip round to a single peer: our membership view, then a digest.
   *
   * Reconciliation is piggybacked on the same tick — gossip already picked a
   * random peer, and the digest is what makes convergence a mechanism rather
   * than a hope.
   */
  async gossipTo(target) {
    if (!target) return;
    await this.send(target, Kind.PEER_GOSSIP, W.encodePeerGossip({
      from: this.addr, nick: this.nick, id: this.nodeId, peers: this.peerAddrs(),
    }));
    await this.antiEntropyRound(target);
  }

  async heartbeatTick() {
    await this.registerWithBootstrap();
    // Alone? The initial join may have raced bootstrap's startup or lost its
    // packet — re-ask bootstrap.
    if (!this.peerAddrs().length) await this.discoverFromBootstrap();
    await this.helloAllPeers();
    this.publishStatus();
  }

  evictTick() {
    const now = this.now();
    const stale = [...this.peers.values()].filter((p) => now - p.lastSeenMs > this.config.evictThresholdMs);
    for (const peer of stale) {
      this.removePeer(peer.addr);
      this.announceLeave(peer.addr, peer.nick);
    }
    this.pruneViews(now);
  }

  pruneViews(nowMs) {
    const ttl = Math.max(60_000, this.config.gossipIntervalMs * 6);
    let dropped = false;
    for (const [addr, view] of [...this.gossipViews]) {
      if (nowMs - view.atMs > ttl) {
        this.gossipViews.delete(addr);
        dropped = true;
      }
    }
    if (dropped) this.publishTopology();
  }

  sweepTick() {
    const expired = this.store.sweepExpired();
    if (expired > 0) {
      this.metrics.kvExpired += expired;
      this.logActivity(`${expired} key(s) expired`);
      this.publishEntries();
    }
    if (this.config.tombstoneTtlSec > 0) {
      const gced = this.store.gcTombstones(this.config.tombstoneTtlSec);
      if (gced > 0) {
        this.metrics.kvGced += gced;
        this.logActivity(`${gced} tombstone(s) reclaimed`);
      }
    }
    this.metrics.sample(this.now());
    this.publishMetrics();
  }

  // ---- persistence & publishing ----------------------------------------

  persist() {
    if (!this.persister) return;
    this.persistGen++;
    const { clock, entries } = this.store.snapshotState();
    this.persister.save(this.persistGen, {
      node_id: this.nodeId,
      clock,
      entries,
      chat: this.chatLog.map(W.encodeChatEntry),
    });
  }

  publishEntries() {
    this.emit('entries', this.store.entries());
    this.publishStatus();
  }

  publishPeers() {
    this.emit('peers', [...this.peers.values()].sort((a, b) => (a.addr < b.addr ? -1 : 1)));
    this.publishStatus();
    this.publishTopology();
  }

  publishChat() {
    this.emit('chat', this.chatLog.slice());
  }

  publishMetrics() {
    this.emit('metrics', this.metrics.snapshot());
  }

  publishTopology() {
    this.emit('topology', this.topology());
  }

  /**
   * The cluster graph, with each node's place on the canvas already worked out.
   *
   * The layout is computed here rather than in the renderer so there is one
   * implementation of it to keep correct, and so a report and a drawing always
   * agree about where a node sits.
   */
  topology() {
    const topology = buildTopology({
      selfAddr: this.addr,
      selfNick: this.nick,
      peers: [...this.peers.values()],
      views: Object.fromEntries(this.gossipViews),
      nowMs: this.now(),
    });
    const places = placeGraph(topology);
    return {
      ...topology,
      nodes: topology.nodes.map((n) => ({ ...n, ...(places.get(n.addr) || { x: 0, y: 0 }) })),
    };
  }

  status() {
    return {
      addr: this.addr,
      nick: this.nick,
      nodeId: this.nodeId,
      cluster: this.config.cluster,
      peers: this.peers.size,
      keys: this.store.size(),
      tombstones: this.store.tombstones(),
      uptimeSec: this.startedAtMs === 0 ? 0 : Math.floor((this.now() - this.startedAtMs) / 1000),
      running: this.isRunning,
      connected: this.peers.size > 0,
    };
  }

  publishStatus() {
    this.emit('status', this.status());
  }

  chat() {
    return this.chatLog.slice();
  }

  activity() {
    return this.activityLog.slice();
  }

  entries() {
    return this.store.entries();
  }

  peerList() {
    return [...this.peers.values()].sort((a, b) => (a.addr < b.addr ? -1 : 1));
  }

  /**
   * Everything this node knows about itself, in one snapshot.
   *
   * Collected here rather than read field by field from the UI so a report is
   * internally consistent: peers, counters and topology all describe the same
   * instant.
   */
  diagnostics() {
    const snapshot = this.metrics.snapshot();
    const traffic = new Map(this.metrics.addrs().map((t) => [t.addr, t]));
    const selfAddr = normalizeAddr(this.addr);
    const known = new Set([...this.peers.keys()].map(normalizeAddr));

    const peers = this.peerList().map((peer) => {
      const key = normalizeAddr(peer.addr);
      const t = traffic.get(key);
      const view = this.gossipViews.get(key);
      return {
        addr: peer.addr,
        nick: peer.nick,
        known: true,
        firstSeenMs: peer.firstSeenMs,
        lastSeenMs: peer.lastSeenMs,
        packetsOut: t ? t.packetsOut : 0,
        packetsIn: t ? t.packetsIn : 0,
        bytesOut: t ? t.bytesOut : 0,
        bytesIn: t ? t.bytesIn : 0,
        rejected: t ? t.rejected : 0,
        sendErrors: t ? t.sendErrors : 0,
        advertisedPeers: view ? view.peers.length : -1,
        lastGossipMs: view ? view.atMs : 0,
      };
    });

    // An address that sends packets while being no peer of ours is worth showing:
    // it is what a NAT, a stale peer or a wrong advertise host looks like from here.
    const strangers = [...traffic.values()]
      .filter((t) => t.addr !== selfAddr && !known.has(t.addr) && (t.packetsIn > 0 || t.rejected > 0))
      .map((t) => ({
        addr: t.addr,
        nick: '',
        known: false,
        packetsIn: t.packetsIn,
        packetsOut: t.packetsOut,
        bytesIn: t.bytesIn,
        bytesOut: t.bytesOut,
        rejected: t.rejected,
        sendErrors: t.sendErrors,
        advertisedPeers: -1,
        firstSeenMs: 0,
        lastSeenMs: 0,
        lastGossipMs: 0,
      }))
      .sort((a, b) => (a.addr < b.addr ? -1 : 1));

    return {
      addr: this.addr,
      nick: this.nick,
      nodeId: this.nodeId,
      cluster: this.config.cluster,
      keyFingerprint: keyFingerprint(this.config.psk, this.config.cluster),
      pskSet: this.config.psk.length > 0,
      configuredPort: this.config.port,
      boundPort: this.transport ? this.transport.port : 0,
      streamListener: this.transport ? this.transport.streamsAvailable : false,
      advertiseHost: this.config.advertiseHost,
      localIpv4: localIpv4(),
      running: this.isRunning,
      startedAtMs: this.startedAtMs,
      uptimeSec: this.startedAtMs === 0 ? 0 : Math.floor((this.now() - this.startedAtMs) / 1000),
      gossipIntervalMs: this.config.gossipIntervalMs,
      heartbeatIntervalMs: this.config.heartbeatIntervalMs,
      evictThresholdMs: this.config.evictThresholdMs,
      sweepIntervalMs: this.config.sweepIntervalMs,
      tombstoneTtlSec: this.config.tombstoneTtlSec,
      seeds: this.config.seeds,
      peers,
      strangers,
      store: { ...this.store.stats(), digestCursor: this.sweepSummary() },
      metrics: snapshot,
      topology: this.topology(),
      chatLines: this.chatLog.length,
      activityLines: this.activityLog.length,
      dataFile: this.dataFile || '',
      loadError: this.loadError || '',
    };
  }
}

function split2(s) {
  const trimmed = String(s || '').trim();
  const i = trimmed.search(/[ \t]/);
  if (i < 0) return [trimmed, ''];
  return [trimmed.slice(0, i), trimmed.slice(i + 1).trim()];
}

function preview(value) {
  const v = value || '';
  return v.length > 40 ? `${v.slice(0, 37)}...` : v;
}

function plural(n) {
  return n === 1 ? 'entry' : 'entries';
}

module.exports = {
  NodeEngine,
  DEFAULTS,
  split2,
  preview,
  CHAT_RING,
  ACTIVITY_RING,
  DATAGRAM_SYNC_LIMIT,
  BATCH_BYTES,
  BATCH_ENTRIES,
};
