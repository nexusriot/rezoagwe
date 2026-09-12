'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { Codec, DecodeError, deriveKey, keyFingerprint, HEADER_LEN } = require('../src/proto/codec');
const { Kind } = require('../src/proto/wire');

// A frame produced by the *Go* codec, and the second the clock was pinned to.
// The same vectors the Android app is held to: if this file and Go disagree, the
// app cannot talk to any cluster, and no amount of UI testing would show it.
const GO_FRAME = '00f22451949b5ebb9e000000006a7efed20c99f6e1b637e6b1604e08b2168d0cea'
  + 'c41edf41f1e81f52429255b1cf25c7777b22616374696f6e223a22736574222c22'
  + '6b6579223a22636f6c6f7572222c2276616c7565223a22626c7565222c22766572'
  + '73696f6e223a7b22636f756e746572223a372c226e6f6465223a22676f2d6e6f64'
  + '65227d7d';
const GO_FRAME_TS = 1786707666;

const rejects = (t, expected, fn) => {
  try {
    fn();
    assert.fail(`expected ${expected}`);
  } catch (e) {
    assert.equal(e.reason, expected);
  }
};

test('key derivation matches the Go proto.DeriveKey vectors', () => {
  assert.equal(
    deriveKey('', 'rezoagwe').toString('hex'),
    '557630ced9a5d93a06aa171910b5453186a09d1db4cc1f22c16ca0aa6b4b7eec',
  );
  assert.equal(
    deriveKey('s3cret', 'prod').toString('hex'),
    '02e49ce58c95bd03572f543a1a2b99a9bd3d9a5e65e2b9196ea4865898efb43c',
  );
});

test('an empty cluster name falls back to the default, as Go does', () => {
  assert.deepEqual(deriveKey('k', ''), deriveKey('k', 'rezoagwe'));
});

test('a frame produced by the Go node decodes here', () => {
  // The clock is pinned to the frame's own timestamp so the skew check does not
  // give this test a shelf life.
  const codec = new Codec('s3cret', 'prod', { now: () => GO_FRAME_TS * 1000 });
  const { kind, body } = codec.decode(Buffer.from(GO_FRAME, 'hex'));
  assert.equal(kind, Kind.KV);
  const update = JSON.parse(body.toString('utf8'));
  assert.deepEqual(update, {
    action: 'set',
    key: 'colour',
    value: 'blue',
    version: { counter: 7, node: 'go-node' },
  });
});

test('what this app encodes is what Go expects to read', () => {
  const { encodeKVUpdate, version } = require('../src/proto/wire');
  const body = JSON.stringify(encodeKVUpdate({
    action: 'set', key: 'colour', value: 'blue', version: version(7, 'go-node'),
  }));
  assert.equal(body, '{"action":"set","key":"colour","value":"blue","version":{"counter":7,"node":"go-node"}}');
  const frame = new Codec('s3cret', 'prod').encodeBody(Kind.KV, body);
  assert.equal(frame[0], Kind.KV);
  assert.equal(frame.length, HEADER_LEN + Buffer.byteLength(body));
});

test('a frame round-trips through a second codec with the same key', () => {
  const frame = new Codec('secret', 'prod').encode(Kind.HELLO, { from: ':3137', nick: 'desktop' });
  const decoded = new Codec('secret', 'prod').decodeJSON(frame);
  assert.equal(decoded.kind, Kind.HELLO);
  assert.equal(decoded.body.nick, 'desktop');
});

test('a different pre-shared key is rejected', () => {
  const frame = new Codec('secret', 'prod').encode(Kind.HELLO, { from: ':1' });
  rejects(assert, DecodeError.BAD_MAC, () => new Codec('other', 'prod').decode(frame));
});

test('two clusters on one network stay apart even with no key', () => {
  const frame = new Codec('', 'alpha').encode(Kind.HELLO, { from: ':1' });
  rejects(assert, DecodeError.BAD_MAC, () => new Codec('', 'beta').decode(frame));
  new Codec('', 'alpha').decode(frame); // the same cluster still works
});

test('tampering with a byte is detected', () => {
  const frame = new Codec('secret', 'prod').encode(Kind.HELLO, { from: ':1' });
  frame[frame.length - 1] ^= 0xff;
  rejects(assert, DecodeError.BAD_MAC, () => new Codec('secret', 'prod').decode(frame));
});

test('a replayed nonce is refused the second time', () => {
  const receiver = new Codec('secret', 'prod');
  const frame = new Codec('secret', 'prod').encode(Kind.HELLO, { from: ':1' });
  receiver.decode(frame);
  rejects(assert, DecodeError.REPLAY, () => receiver.decode(frame));
});

test('a stale timestamp is refused', () => {
  const past = Date.now() - 10 * 60 * 1000;
  const frame = new Codec('secret', 'prod', { now: () => past }).encode(Kind.HELLO, { from: ':1' });
  rejects(assert, DecodeError.CLOCK_SKEW, () => new Codec('secret', 'prod').decode(frame));
});

test('a frame from the future is refused too', () => {
  const future = Date.now() + 10 * 60 * 1000;
  const frame = new Codec('secret', 'prod', { now: () => future }).encode(Kind.HELLO, { from: ':1' });
  rejects(assert, DecodeError.CLOCK_SKEW, () => new Codec('secret', 'prod').decode(frame));
});

test('a frame shorter than the header is refused', () => {
  rejects(assert, DecodeError.SHORT_FRAME, () => new Codec('', '').decode(Buffer.from([1, 2, 3])));
});

test('the replay cache is pruned rather than grown forever', () => {
  let now = 1_000_000;
  const codec = new Codec('k', 'c', { now: () => now });
  for (let i = 0; i < 5; i++) {
    codec.decode(new Codec('k', 'c', { now: () => now }).encode(Kind.HELLO, { from: `:${i}` }));
  }
  assert.equal(codec.seen.size, 5);
  // Past twice the skew a nonce can never be accepted again on timestamp
  // grounds, so keeping it would only cost memory.
  now += 5 * 60 * 1000;
  codec.decode(new Codec('k', 'c', { now: () => now }).encode(Kind.HELLO, { from: ':late' }));
  assert.equal(codec.seen.size, 1);
});

test('the key fingerprint identifies a key without revealing it', () => {
  assert.equal(keyFingerprint('', 'rezoagwe'), '557630ce');
  assert.notEqual(keyFingerprint('a', 'c'), keyFingerprint('b', 'c'));
  assert.equal(keyFingerprint('a', 'c').length, 8);
});

test('decodeJSON reports a malformed body rather than throwing a parse error', () => {
  const codec = new Codec('k', 'c');
  const frame = codec.encodeBody(Kind.KV, 'not json');
  rejects(assert, DecodeError.MALFORMED, () => new Codec('k', 'c').decodeJSON(frame));
});
