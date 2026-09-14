'use strict';

// Wire protocol v2, field-for-field with the Go structs in pkg/proto/wire.go.
//
// Every packet is kind(1) | nonce(8) | timestamp(8, big endian) | HMAC-SHA256(32) | JSON body.
// The property names below are the Go struct tags: they are the contract, and
// renaming one here silently stops this app from talking to a cluster.
//
// Two asymmetries of Go's encoding/json matter here and are handled in one
// place each:
//   * fields tagged `omitempty` are not written when empty, so the encoders
//     below drop them rather than sending "" / 0 / false;
//   * a nil slice is marshalled as `null` for any field *without* omitempty, so
//     an empty Go node's digest arrives as {"from":"…","entries":null}. The
//     decoders coerce those to [] instead of throwing on a frame that already
//     authenticated.

const Kind = Object.freeze({
  KV: 0,
  CHAT: 1,
  STATE_REQUEST: 2,
  STATE_RESPONSE: 3,
  PEER_GOSSIP: 4,
  HELLO: 5,
  GOODBYE: 6,
  DIGEST: 7,
  PULL_REQUEST: 8,
  KV_BATCH: 9,
  DIRECT_MESSAGE: 10,
  FINGERPRINT: 11,
  FINGERPRINT_REPLY: 12,
  BOOTSTRAP_REGISTER: 20,
  BOOTSTRAP_DISCOVER: 21,
  BOOTSTRAP_ROSTER: 22,
});

const KIND_NAMES = {
  0: 'kv',
  1: 'chat',
  2: 'state_request',
  3: 'state_response',
  4: 'peer_gossip',
  5: 'hello',
  6: 'goodbye',
  7: 'digest',
  8: 'pull_request',
  9: 'kv_batch',
  10: 'direct_message',
  11: 'fingerprint',
  12: 'fingerprint_reply',
  20: 'bootstrap_register',
  21: 'bootstrap_discover',
  22: 'bootstrap_roster',
};

function kindName(kind) {
  return KIND_NAMES[kind] || 'unknown';
}

const KVAction = Object.freeze({ SET: 'set', DELETE: 'delete' });

const ChatKind = Object.freeze({
  MESSAGE: '',
  SYSTEM: 'system',
  ACTION: 'action',
  DIRECT: 'dm',
});

// ---- versions ---------------------------------------------------------------

/** A per-key logical clock: Lamport counter + the writer's node id as tiebreak. */
function version(counter, node) {
  return { counter: Number(counter) || 0, node: node || '' };
}

/** True when a should win over b. Equal is *not* newer, so re-delivery is idempotent. */
function versionNewer(a, b) {
  if (a.counter !== b.counter) return a.counter > b.counter;
  return a.node > b.node;
}

function versionEqual(a, b) {
  return a.counter === b.counter && a.node === b.node;
}

/** The unset version, which compare-and-swap reads as "this key must not exist". */
function versionZero(v) {
  return !v || (v.counter === 0 && (v.node === '' || v.node === undefined));
}

// ---- helpers -----------------------------------------------------------------

const str = (v) => (typeof v === 'string' ? v : v == null ? '' : String(v));
const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : Number(v) || 0);
const list = (v) => (Array.isArray(v) ? v : []);

function decodeVersion(v) {
  if (!v || typeof v !== 'object') return version(0, '');
  return version(v.counter, v.node);
}

/** Writes a Go struct with omitempty on the named fields: those are dropped when empty. */
function withOmitEmpty(obj, omitEmptyFields) {
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    if (omitEmptyFields.includes(k) && (v === '' || v === 0 || v === false || v == null)) continue;
    out[k] = v;
  }
  return out;
}

// ---- message bodies ----------------------------------------------------------

function encodeKVUpdate(u) {
  return withOmitEmpty({
    action: u.action,
    key: u.key,
    value: u.value || '',
    version: { counter: u.version.counter, node: u.version.node },
    expires_at: u.expiresAt || 0,
    deleted_at: u.deletedAt || 0,
  }, ['value', 'expires_at', 'deleted_at']);
}

function decodeKVUpdate(o) {
  if (!o || typeof o !== 'object' || typeof o.key !== 'string') throw new TypeError('bad kv update');
  const action = o.action === KVAction.DELETE ? KVAction.DELETE : KVAction.SET;
  return {
    action,
    key: o.key,
    value: str(o.value),
    version: decodeVersion(o.version),
    expiresAt: num(o.expires_at),
    deletedAt: num(o.deleted_at),
  };
}

const kvDeleted = (u) => u.action === KVAction.DELETE;

function encodeKVBatch(b) {
  return { updates: b.updates.map(encodeKVUpdate) };
}

function decodeKVBatch(o) {
  return { updates: list(o && o.updates).map(decodeKVUpdate) };
}

function encodeChatMessage(m) {
  return withOmitEmpty({
    sender: m.sender,
    nick: m.nick || '',
    text: m.text,
    ts: m.ts,
    action: !!m.action,
    to: m.to || '',
  }, ['action', 'to']);
}

function decodeChatMessage(o) {
  if (!o || typeof o !== 'object') throw new TypeError('bad chat message');
  return {
    sender: str(o.sender),
    nick: str(o.nick),
    text: str(o.text),
    ts: num(o.ts),
    action: o.action === true,
    to: str(o.to),
  };
}

function encodeChatEntry(e) {
  return withOmitEmpty({
    ts: e.ts || 0,
    sender: e.sender || '',
    nick: e.nick || '',
    text: e.text || '',
    kind: e.kind || '',
    to: e.to || '',
  }, ['sender', 'nick', 'kind', 'to']);
}

function decodeChatEntry(o) {
  // Wire v1 stored chat as pre-rendered strings; read those as system lines
  // rather than failing to parse the whole state file.
  if (typeof o === 'string') return { ts: 0, sender: '', nick: '', text: stripMarkup(o), kind: ChatKind.SYSTEM, to: '' };
  if (!o || typeof o !== 'object') throw new TypeError('bad chat entry');
  return {
    ts: num(o.ts),
    sender: str(o.sender),
    nick: str(o.nick),
    text: str(o.text),
    kind: str(o.kind),
    to: str(o.to),
  };
}

/** Removes tview colour tags from a legacy pre-rendered chat line. */
function stripMarkup(s) {
  let out = '';
  let depth = 0;
  for (const ch of s) {
    if (ch === '[') depth++;
    else if (ch === ']' && depth > 0) depth--;
    else if (depth === 0) out += ch;
  }
  return out;
}

function encodeStateRequest(r) {
  return { from: r.from };
}

function decodeStateRequest(o) {
  return { from: str(o && o.from) };
}

function encodeStateResponse(r) {
  return withOmitEmpty({
    kv: r.kv.map(encodeKVUpdate),
    chat: (r.chat || []).length ? r.chat.map(encodeChatEntry) : null,
  }, ['chat']);
}

function decodeStateResponse(o) {
  return {
    kv: list(o && o.kv).map(decodeKVUpdate),
    chat: list(o && o.chat).map(decodeChatEntry),
  };
}

function encodePeerGossip(g) {
  return withOmitEmpty({
    from: g.from,
    nick: g.nick || '',
    id: g.id || '',
    peers: g.peers,
  }, ['nick', 'id']);
}

function decodePeerGossip(o) {
  return {
    from: str(o && o.from),
    nick: str(o && o.nick),
    id: str(o && o.id),
    peers: list(o && o.peers).filter((p) => typeof p === 'string'),
  };
}

function encodeHello(h) {
  return withOmitEmpty({ from: h.from, nick: h.nick || '', id: h.id || '' }, ['nick', 'id']);
}

function decodeHello(o) {
  return { from: str(o && o.from), nick: str(o && o.nick), id: str(o && o.id) };
}

function encodeGoodbye(g) {
  return withOmitEmpty({ from: g.from, nick: g.nick || '' }, ['nick']);
}

function decodeGoodbye(o) {
  return { from: str(o && o.from), nick: str(o && o.nick) };
}

function encodeKeyVersion(kv) {
  return withOmitEmpty({
    key: kv.key,
    version: { counter: kv.version.counter, node: kv.version.node },
    deleted: !!kv.deleted,
  }, ['deleted']);
}

function decodeKeyVersion(o) {
  if (!o || typeof o.key !== 'string') throw new TypeError('bad key version');
  return { key: o.key, version: decodeVersion(o.version), deleted: o.deleted === true };
}

function encodeDigest(d) {
  return withOmitEmpty({
    from: d.from,
    lo: d.lo || '',
    hi: d.hi || '',
    entries: d.entries.map(encodeKeyVersion),
  }, ['lo', 'hi']);
}

function decodeDigest(o) {
  return {
    from: str(o && o.from),
    lo: str(o && o.lo),
    hi: str(o && o.hi),
    entries: list(o && o.entries).map(decodeKeyVersion),
  };
}

function encodePullRequest(p) {
  return { from: p.from, keys: p.keys };
}

function decodePullRequest(o) {
  return { from: str(o && o.from), keys: list(o && o.keys).filter((k) => typeof k === 'string') };
}

/** How finely a store is summarised for a consistency check. Must match Go. */
const FINGERPRINT_BUCKETS = 16;

function encodeFingerprint(f) {
  return { from: f.from };
}

function decodeFingerprint(o) {
  return { from: str(o && o.from) };
}

function encodeFingerprintReply(r) {
  return withOmitEmpty({
    from: r.from,
    nick: r.nick || '',
    keys: r.keys || 0,
    tombstones: r.tombstones || 0,
    clock: r.clock || 0,
    buckets: r.buckets || [],
  }, ['nick']);
}

function decodeFingerprintReply(o) {
  return {
    from: str(o && o.from),
    nick: str(o && o.nick),
    keys: num(o && o.keys),
    tombstones: num(o && o.tombstones),
    clock: num(o && o.clock),
    // Go marshals a nil slice as null, which has already dropped one packet
    // in this project's history.
    buckets: list(o && o.buckets).filter((b) => typeof b === 'string'),
  };
}

function encodeBootstrapRegister(r) {
  return withOmitEmpty({ from: r.from, nick: r.nick || '' }, ['nick']);
}

function decodeBootstrapRegister(o) {
  return { from: str(o && o.from), nick: str(o && o.nick) };
}

function encodeBootstrapDiscover(r) {
  return { from: r.from };
}

function decodeBootstrapDiscover(o) {
  return { from: str(o && o.from) };
}

function encodeBootstrapPeer(p) {
  return withOmitEmpty({ addr: p.addr, nick: p.nick || '', last_seen: p.lastSeen || 0 }, ['nick', 'last_seen']);
}

function decodeBootstrapPeer(o) {
  return { addr: str(o && o.addr), nick: str(o && o.nick), lastSeen: num(o && o.last_seen) };
}

function encodeBootstrapRoster(r) {
  return { peers: r.peers.map(encodeBootstrapPeer) };
}

function decodeBootstrapRoster(o) {
  return { peers: list(o && o.peers).map(decodeBootstrapPeer) };
}

module.exports = {
  Kind,
  kindName,
  FINGERPRINT_BUCKETS,
  encodeFingerprint,
  decodeFingerprint,
  encodeFingerprintReply,
  decodeFingerprintReply,
  KVAction,
  ChatKind,
  version,
  versionNewer,
  versionEqual,
  versionZero,
  kvDeleted,
  withOmitEmpty,
  stripMarkup,
  encodeKVUpdate,
  decodeKVUpdate,
  encodeKVBatch,
  decodeKVBatch,
  encodeChatMessage,
  decodeChatMessage,
  encodeChatEntry,
  decodeChatEntry,
  encodeStateRequest,
  decodeStateRequest,
  encodeStateResponse,
  decodeStateResponse,
  encodePeerGossip,
  decodePeerGossip,
  encodeHello,
  decodeHello,
  encodeGoodbye,
  decodeGoodbye,
  encodeKeyVersion,
  decodeKeyVersion,
  encodeDigest,
  decodeDigest,
  encodePullRequest,
  decodePullRequest,
  encodeBootstrapRegister,
  decodeBootstrapRegister,
  encodeBootstrapDiscover,
  decodeBootstrapDiscover,
  encodeBootstrapPeer,
  decodeBootstrapPeer,
  encodeBootstrapRoster,
  decodeBootstrapRoster,
};
