'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const W = require('../src/proto/wire');

/**
 * What Go actually puts on the wire.
 *
 * Go marshals a nil slice as `null` for any field without `omitempty`, so an
 * empty digest from a Go node is {"from":"…","entries":null}. Refusing one means
 * the repair, roster or gossip it carried is dropped *after* it authenticated —
 * a silent failure that looks exactly like a network problem.
 */
test('an empty Go digest decodes', () => {
  const digest = W.decodeDigest(JSON.parse('{"from":"127.0.0.1:3200","entries":null}'));
  assert.equal(digest.from, '127.0.0.1:3200');
  assert.deepEqual(digest.entries, []);
  assert.equal(digest.hi, '');
});

test('an empty Go pull request, gossip, batch and roster decode', () => {
  assert.deepEqual(W.decodePullRequest(JSON.parse('{"from":"a:1","keys":null}')).keys, []);
  assert.deepEqual(W.decodePeerGossip(JSON.parse('{"from":"a:1","nick":"go","id":"x","peers":null}')).peers, []);
  assert.deepEqual(W.decodeKVBatch(JSON.parse('{"updates":null}')).updates, []);
  assert.deepEqual(W.decodeBootstrapRoster(JSON.parse('{"peers":null}')).peers, []);
  const state = W.decodeStateResponse(JSON.parse('{"kv":null,"chat":null}'));
  assert.deepEqual(state.kv, []);
  assert.deepEqual(state.chat, []);
});

test('we still omit empty collections rather than sending null back', () => {
  // Coercion is for reading. Writing null back would break a Go peer decoding
  // into a typed slice, so an empty list stays omitted or empty.
  assert.equal(JSON.stringify(W.encodeDigest({ from: 'a:1', lo: '', hi: '', entries: [] })),
    '{"from":"a:1","entries":[]}');
  assert.equal(JSON.stringify(W.encodeHello({ from: ':3137' })), '{"from":":3137"}');
});

test('fields Go tags omitempty are not written when empty', () => {
  assert.equal(JSON.stringify(W.encodeKVUpdate({
    action: 'set', key: 'k', value: '', version: W.version(1, 'n'), expiresAt: 0, deletedAt: 0,
  })), '{"action":"set","key":"k","version":{"counter":1,"node":"n"}}');
  assert.equal(JSON.stringify(W.encodeChatMessage({
    sender: 'a:1', nick: '', text: 'hi', ts: 5, action: false, to: '',
  })), '{"sender":"a:1","nick":"","text":"hi","ts":5}');
  assert.equal(JSON.stringify(W.encodeKeyVersion({ key: 'k', version: W.version(2, 'n'), deleted: false })),
    '{"key":"k","version":{"counter":2,"node":"n"}}');
  assert.equal(JSON.stringify(W.encodeKeyVersion({ key: 'k', version: W.version(2, 'n'), deleted: true })),
    '{"key":"k","version":{"counter":2,"node":"n"},"deleted":true}');
});

test('a delete carries its tombstone fields', () => {
  const encoded = W.encodeKVUpdate({
    action: 'delete', key: 'k', value: '', version: W.version(3, 'n'), expiresAt: 0, deletedAt: 1700,
  });
  assert.deepEqual(encoded, {
    action: 'delete', key: 'k', version: { counter: 3, node: 'n' }, deleted_at: 1700,
  });
});

test('an expiry survives the round trip under Go field names', () => {
  const update = W.decodeKVUpdate(JSON.parse(
    '{"action":"set","key":"s","value":"v","version":{"counter":4,"node":"n"},"expires_at":99}',
  ));
  assert.equal(update.expiresAt, 99);
  assert.equal(JSON.parse(JSON.stringify(W.encodeKVUpdate(update))).expires_at, 99);
});

test('wire-v1 chat history of plain strings still reads, with markup stripped', () => {
  // Without this an upgrading node fails to parse its own data file and discards
  // the KV store along with the chat.
  const entry = W.decodeChatEntry('[yellow]alice[white] joined');
  assert.equal(entry.text, 'alice joined');
  assert.equal(entry.kind, 'system');
});

test('an unknown field from a newer peer is ignored, not fatal', () => {
  const hello = W.decodeHello(JSON.parse('{"from":"a:1","nick":"x","id":"y","future_field":42}'));
  assert.equal(hello.nick, 'x');
});

test('version ordering is higher counter, then node id, and equal is not newer', () => {
  const a = W.version(2, 'aaa');
  const b = W.version(2, 'bbb');
  const c = W.version(3, 'aaa');
  assert.ok(W.versionNewer(b, a), 'the node id breaks a counter tie');
  assert.ok(!W.versionNewer(a, b));
  assert.ok(W.versionNewer(c, b), 'a higher counter always wins');
  assert.ok(!W.versionNewer(a, a), 'equal is not newer, so re-delivery is idempotent');
  assert.ok(W.versionZero(W.version(0, '')));
  assert.ok(!W.versionZero(W.version(0, 'n')));
});

test('a malformed update is rejected rather than half-decoded', () => {
  assert.throws(() => W.decodeKVUpdate({ value: 'no key' }));
  assert.throws(() => W.decodeKeyVersion({ version: { counter: 1, node: 'n' } }));
});

test('a non-string entry in a gossiped peer list is dropped, not carried', () => {
  // Gossip is hearsay: anything a peer says about a third party has to survive
  // being wrong without poisoning the peer table.
  const gossip = W.decodePeerGossip({ from: 'a:1', peers: ['b:2', 42, null, 'c:3'] });
  assert.deepEqual(gossip.peers, ['b:2', 'c:3']);
});

test('every message kind has a stable name for the metrics labels', () => {
  assert.equal(W.kindName(W.Kind.KV), 'kv');
  assert.equal(W.kindName(W.Kind.BOOTSTRAP_ROSTER), 'bootstrap_roster');
  assert.equal(W.kindName(200), 'unknown');
});
