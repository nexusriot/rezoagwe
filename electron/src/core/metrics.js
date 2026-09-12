'use strict';

const { kindName } = require('../proto/wire');

/**
 * Counters for what a replicating node does. Anti-entropy is invisible when it
 * works, so these are how you tell it is running at all.
 *
 * Traffic is also kept per address rather than only per cluster: "the node sends
 * and receives" hides the case that matters, which is one peer that only ever
 * receives.
 */
class Metrics {
  constructor() {
    this.packetsSent = 0;
    this.packetsReceived = 0;
    this.bytesSent = 0;
    this.bytesReceived = 0;
    this.sendErrors = 0;
    this.lastSendError = null;

    this.authFailures = 0;
    this.replayDrops = 0;
    this.skewDrops = 0;
    this.malformedDrops = 0;

    this.kvApplied = 0;
    this.kvRejectedStale = 0;
    this.kvLocalWrites = 0;
    this.kvCasFailures = 0;
    this.kvExpired = 0;
    this.kvGced = 0;

    this.aeRounds = 0;
    this.aePushed = 0;
    this.aePulled = 0;

    this.stateSyncOut = 0;
    this.stateSyncIn = 0;
    this.streamErrors = 0;

    this.sentByKind = new Map();
    this.recvByKind = new Map();
    this.perAddr = new Map();

    this.lastSampleMs = 0;
    this.lastSent = 0;
    this.lastReceived = 0;
    this.lastBytesSent = 0;
    this.lastBytesReceived = 0;
    this.rates = { windowMs: 0, packetsSent: 0, packetsReceived: 0, bytesSent: 0, bytesReceived: 0 };
  }

  sent(kind, bytes) {
    this.packetsSent++;
    this.bytesSent += bytes;
    this.sentByKind.set(kind, (this.sentByKind.get(kind) || 0) + 1);
  }

  received(kind, bytes) {
    this.packetsReceived++;
    this.bytesReceived += bytes;
    this.recvByKind.set(kind, (this.recvByKind.get(kind) || 0) + 1);
  }

  counters(addr) {
    let c = this.perAddr.get(addr);
    if (!c) {
      c = { packetsOut: 0, packetsIn: 0, bytesOut: 0, bytesIn: 0, sendErrors: 0, rejected: 0 };
      this.perAddr.set(addr, c);
    }
    return c;
  }

  sentTo(addr, bytes) {
    const c = this.counters(addr);
    c.packetsOut++;
    c.bytesOut += bytes;
  }

  receivedFrom(addr, bytes) {
    const c = this.counters(addr);
    c.packetsIn++;
    c.bytesIn += bytes;
  }

  /**
   * A datagram that never left. The reason is kept, not just the count: an
   * unroutable peer and a socket that has been closed under us have nothing in
   * common but the number.
   */
  sendFailed(addr, err) {
    this.sendErrors++;
    this.counters(addr).sendErrors++;
    this.lastSendError = {
      addr,
      cause: (err && (err.code || err.name)) || 'Error',
      message: (err && err.message) || '',
    };
  }

  /** A frame from addr that did not authenticate, parse, or pass the replay guard. */
  rejectedFrom(addr) {
    this.counters(addr).rejected++;
  }

  addrs() {
    return [...this.perAddr.entries()]
      .map(([addr, c]) => ({ addr, ...c }))
      .sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));
  }

  /**
   * Folds the counters since the previous call into per-second rates. Computed
   * here rather than in the renderer so they keep updating while the window is
   * hidden — which is exactly when a node is still expected to gossip.
   */
  sample(nowMs) {
    const elapsed = nowMs - this.lastSampleMs;
    if (this.lastSampleMs > 0 && elapsed > 0) {
      const perSec = 1000 / elapsed;
      this.rates = {
        windowMs: elapsed,
        packetsSent: (this.packetsSent - this.lastSent) * perSec,
        packetsReceived: (this.packetsReceived - this.lastReceived) * perSec,
        bytesSent: (this.bytesSent - this.lastBytesSent) * perSec,
        bytesReceived: (this.bytesReceived - this.lastBytesReceived) * perSec,
      };
    }
    this.lastSampleMs = nowMs;
    this.lastSent = this.packetsSent;
    this.lastReceived = this.packetsReceived;
    this.lastBytesSent = this.bytesSent;
    this.lastBytesReceived = this.bytesReceived;
  }

  snapshot() {
    const kinds = new Map();
    for (const [k, v] of this.sentByKind) kinds.set(kindName(k), { kind: kindName(k), sent: v, received: 0 });
    for (const [k, v] of this.recvByKind) {
      const name = kindName(k);
      const cur = kinds.get(name) || { kind: name, sent: 0, received: 0 };
      kinds.set(name, { ...cur, received: v });
    }
    return {
      packetsSent: this.packetsSent,
      packetsReceived: this.packetsReceived,
      bytesSent: this.bytesSent,
      bytesReceived: this.bytesReceived,
      sendErrors: this.sendErrors,
      lastSendError: this.lastSendError,
      authFailures: this.authFailures,
      replayDrops: this.replayDrops,
      skewDrops: this.skewDrops,
      malformedDrops: this.malformedDrops,
      kvApplied: this.kvApplied,
      kvRejectedStale: this.kvRejectedStale,
      kvLocalWrites: this.kvLocalWrites,
      kvCasFailures: this.kvCasFailures,
      kvExpired: this.kvExpired,
      kvGced: this.kvGced,
      aeRounds: this.aeRounds,
      aePushed: this.aePushed,
      aePulled: this.aePulled,
      stateSyncOut: this.stateSyncOut,
      stateSyncIn: this.stateSyncIn,
      streamErrors: this.streamErrors,
      kinds: [...kinds.values()].sort((a, b) => (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : 0)),
      rates: { ...this.rates },
    };
  }

  /** The snapshot in the Prometheus text exposition format, matching the Go gateway's names. */
  static prometheus(s) {
    let out = '';
    const counter = (name, help, v) => {
      out += `# HELP rezoagwe_${name} ${help}\n`;
      out += `# TYPE rezoagwe_${name} counter\n`;
      out += `rezoagwe_${name} ${v}\n`;
    };
    counter('packets_sent_total', 'Datagrams handed to the transport.', s.packetsSent);
    counter('packets_received_total', 'Datagrams accepted after authentication.', s.packetsReceived);
    counter('bytes_sent_total', 'Bytes handed to the transport.', s.bytesSent);
    counter('bytes_received_total', 'Bytes accepted after authentication.', s.bytesReceived);
    counter('send_errors_total', 'Transport send failures.', s.sendErrors);
    counter('auth_failures_total', 'Packets dropped with a bad MAC.', s.authFailures);
    counter('replay_drops_total', 'Packets dropped as replays.', s.replayDrops);
    counter('skew_drops_total', 'Packets dropped for timestamp skew.', s.skewDrops);
    counter('malformed_drops_total', 'Packets dropped as unparseable.', s.malformedDrops);
    counter('kv_applied_total', 'Remote updates merged into the store.', s.kvApplied);
    counter('kv_rejected_stale_total', 'Remote updates rejected as not newer.', s.kvRejectedStale);
    counter('kv_local_writes_total', 'Local writes and deletes.', s.kvLocalWrites);
    counter('kv_cas_failures_total', 'Compare-and-swap writes rejected.', s.kvCasFailures);
    counter('kv_expired_total', 'Keys tombstoned by TTL expiry.', s.kvExpired);
    counter('kv_gc_total', 'Tombstones reclaimed.', s.kvGced);
    counter('anti_entropy_rounds_total', 'Digests sent.', s.aeRounds);
    counter('anti_entropy_pushed_total', 'Entries pushed to a lagging peer.', s.aePushed);
    counter('anti_entropy_pulled_total', 'Entries requested from a peer.', s.aePulled);
    counter('state_sync_out_total', 'Snapshots served.', s.stateSyncOut);
    counter('state_sync_in_total', 'Snapshots received.', s.stateSyncIn);
    counter('stream_errors_total', 'Stream (TCP) failures.', s.streamErrors);
    out += '# HELP rezoagwe_messages_total Messages by kind and direction.\n';
    out += '# TYPE rezoagwe_messages_total counter\n';
    for (const k of s.kinds) {
      out += `rezoagwe_messages_total{kind="${k.kind}",direction="sent"} ${k.sent}\n`;
      out += `rezoagwe_messages_total{kind="${k.kind}",direction="received"} ${k.received}\n`;
    }
    return out;
  }
}

module.exports = { Metrics };
