'use strict';

const crypto = require('node:crypto');

// Framing, authentication and replay rejection — the twin of pkg/proto/codec.go.
//
// Every packet is authenticated: there is no unauthenticated path. With no
// pre-shared key the key is derived from the cluster name alone, which still
// keeps two clusters sharing a LAN apart but, the name being public, provides no
// secrecy.

const NONCE_LEN = 8;
const TS_LEN = 8;
const MAC_LEN = 32;
const HEADER_LEN = 1 + NONCE_LEN + TS_LEN + MAC_LEN;

const KEY_DOMAIN = 'rezoagwe/wire/v2';
const DEFAULT_CLUSTER = 'rezoagwe';
const DEFAULT_SKEW_MS = 30_000;

/** Why a frame was rejected. Each maps to a counter, so drops are never silent. */
const DecodeError = Object.freeze({
  SHORT_FRAME: 'short_frame',
  BAD_MAC: 'bad_mac',
  CLOCK_SKEW: 'clock_skew',
  REPLAY: 'replay',
  MALFORMED: 'malformed',
});

class DecodeException extends Error {
  constructor(reason) {
    super(reason);
    this.name = 'DecodeException';
    this.reason = reason;
  }
}

/** Turns (psk, cluster) into the per-cluster framing key; matches Go's proto.DeriveKey. */
function deriveKey(psk, cluster) {
  const name = cluster || DEFAULT_CLUSTER;
  const mac = crypto.createHmac('sha256', Buffer.from(psk || '', 'utf8'));
  mac.update(Buffer.from(KEY_DOMAIN, 'utf8'));
  mac.update(Buffer.from([0]));
  mac.update(Buffer.from(name, 'utf8'));
  return mac.digest();
}

/**
 * A short, shareable identity for the framing key. Two nodes that cannot talk
 * compare fingerprints to tell "the key differs" from "the network drops
 * packets", without revealing the key.
 */
function keyFingerprint(psk, cluster) {
  return deriveKey(psk, cluster).subarray(0, 4).toString('hex');
}

class Codec {
  constructor(psk, cluster, opts = {}) {
    this.key = deriveKey(psk, cluster);
    this.skewMs = opts.skewMs || DEFAULT_SKEW_MS;
    this.now = opts.now || Date.now;
    this.seen = new Map();
    this.lastPrune = 0;
  }

  /** Frames and authenticates an already-serialised JSON body. */
  encodeBody(kind, body) {
    const bodyBuf = Buffer.isBuffer(body) ? body : Buffer.from(body, 'utf8');
    const frame = Buffer.alloc(HEADER_LEN + bodyBuf.length);
    frame[0] = kind & 0xff;
    crypto.randomFillSync(frame, 1, NONCE_LEN);
    frame.writeBigUInt64BE(BigInt(Math.floor(this.now() / 1000)), 1 + NONCE_LEN);
    bodyBuf.copy(frame, HEADER_LEN);
    const tag = this.tag(frame[0], frame.subarray(1, 1 + NONCE_LEN),
      frame.subarray(1 + NONCE_LEN, 1 + NONCE_LEN + TS_LEN), bodyBuf);
    tag.copy(frame, 1 + NONCE_LEN + TS_LEN);
    return frame;
  }

  /** Frames a message object, serialising it as JSON. */
  encode(kind, value) {
    return this.encodeBody(kind, JSON.stringify(value));
  }

  /**
   * Authenticates a frame and returns { kind, body }. Frames that fail
   * authentication, sit outside the accepted clock skew, or reuse a nonce throw
   * a DecodeException — the caller drops the packet and counts it.
   */
  decode(frame) {
    if (!Buffer.isBuffer(frame) || frame.length < HEADER_LEN) throw new DecodeException(DecodeError.SHORT_FRAME);
    const kind = frame[0];
    const nonce = frame.subarray(1, 1 + NONCE_LEN);
    const tsBytes = frame.subarray(1 + NONCE_LEN, 1 + NONCE_LEN + TS_LEN);
    const mac = frame.subarray(1 + NONCE_LEN + TS_LEN, HEADER_LEN);
    const body = frame.subarray(HEADER_LEN);

    const expected = this.tag(kind, nonce, tsBytes, body);
    if (!crypto.timingSafeEqual(mac, expected)) throw new DecodeException(DecodeError.BAD_MAC);

    const ts = Number(tsBytes.readBigUInt64BE(0)) * 1000;
    const now = this.now();
    const delta = now - ts;
    if (delta > this.skewMs || delta < -this.skewMs) throw new DecodeException(DecodeError.CLOCK_SKEW);

    if (!this.remember(nonce.toString('hex'), now)) throw new DecodeException(DecodeError.REPLAY);
    return { kind, body };
  }

  /** Decodes a frame and parses its JSON body in one step. */
  decodeJSON(frame) {
    const { kind, body } = this.decode(frame);
    let parsed;
    try {
      parsed = JSON.parse(body.toString('utf8'));
    } catch (e) {
      throw new DecodeException(DecodeError.MALFORMED);
    }
    return { kind, body: parsed };
  }

  tag(kind, nonce, ts, body) {
    const mac = crypto.createHmac('sha256', this.key);
    mac.update(Buffer.from([kind]));
    mac.update(nonce);
    mac.update(ts);
    mac.update(body);
    return mac.digest();
  }

  /**
   * Records a nonce and reports whether it was new. Entries older than twice the
   * skew can never be accepted again on timestamp grounds, so they are pruned
   * rather than kept forever.
   */
  remember(nonce, now) {
    if (now - this.lastPrune > this.skewMs) {
      const cutoff = now - 2 * this.skewMs;
      for (const [k, seenAt] of this.seen) if (seenAt < cutoff) this.seen.delete(k);
      this.lastPrune = now;
    }
    if (this.seen.has(nonce)) return false;
    this.seen.set(nonce, now);
    return true;
  }
}

module.exports = {
  Codec,
  DecodeError,
  DecodeException,
  deriveKey,
  keyFingerprint,
  NONCE_LEN,
  TS_LEN,
  MAC_LEN,
  HEADER_LEN,
  DEFAULT_CLUSTER,
  DEFAULT_SKEW_MS,
};
