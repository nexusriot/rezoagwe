'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { PassThrough } = require('node:stream');

const {
  frameWithLength, writeFrame, readFrame, FrameReader, MAX_FRAME_SIZE,
} = require('../src/net/framing');
const {
  splitHostPort, validPeerAddr, normalizeAddr, targetOf, localIpv4,
} = require('../src/net/addr');
const { UdpTransport } = require('../src/net/udp');
const { Codec } = require('../src/proto/codec');
const { Kind } = require('../src/proto/wire');

test('a frame carries a big-endian length prefix', () => {
  const framed = frameWithLength(Buffer.from('hello'));
  assert.equal(framed.readUInt32BE(0), 5);
  assert.equal(framed.subarray(4).toString(), 'hello');
});

test('a frame split across chunks is reassembled', () => {
  // TCP does not preserve write boundaries: a reader that assumes one chunk is
  // one frame works locally and fails across a real network.
  const reader = new FrameReader();
  const framed = frameWithLength(Buffer.from('abcdefghij'));
  assert.deepEqual(reader.push(framed.subarray(0, 3)), []);
  assert.deepEqual(reader.push(framed.subarray(3, 9)), []);
  const frames = reader.push(framed.subarray(9));
  assert.equal(frames.length, 1);
  assert.equal(frames[0].toString(), 'abcdefghij');
});

test('two frames in one chunk are both delivered', () => {
  const reader = new FrameReader();
  const chunk = Buffer.concat([frameWithLength(Buffer.from('one')), frameWithLength(Buffer.from('two'))]);
  const frames = reader.push(chunk);
  assert.deepEqual(frames.map((f) => f.toString()), ['one', 'two']);
});

test('an absurd length prefix is refused instead of allocating', () => {
  const reader = new FrameReader();
  const hostile = Buffer.alloc(8);
  hostile.writeUInt32BE(MAX_FRAME_SIZE + 1, 0);
  assert.throws(() => reader.push(hostile), /maximum size/);
  assert.throws(() => frameWithLength(Buffer.alloc(MAX_FRAME_SIZE + 1)), /maximum size/);
});

test('readFrame resolves on the first whole frame', async () => {
  const stream = new PassThrough();
  const pending = readFrame(stream, 1000);
  writeFrame(stream, Buffer.from('payload'));
  assert.equal((await pending).toString(), 'payload');
});

test('readFrame rejects when the stream closes first', async () => {
  const stream = new PassThrough();
  const pending = readFrame(stream, 1000);
  stream.end();
  await assert.rejects(pending, /closed/);
});

test('readFrame gives up rather than waiting forever', async () => {
  const stream = new PassThrough();
  await assert.rejects(readFrame(stream, 20), /timed out/);
});

test('host and port split on the last colon, and brackets come off an IPv6 literal', () => {
  assert.deepEqual(splitHostPort('10.0.0.1:3137'), { host: '10.0.0.1', port: 3137 });
  assert.deepEqual(splitHostPort(':3137'), { host: '', port: 3137 });
  assert.deepEqual(splitHostPort('[::1]:3137'), { host: '::1', port: 3137 });
  assert.equal(splitHostPort('no-port'), null);
  assert.equal(splitHostPort('host:'), null);
  assert.equal(splitHostPort('host:70000'), null);
});

test('a malformed peer address is refused at the door', () => {
  // Gossip is hearsay: anything a peer says about a third party has to be
  // rejected before it can linger in the peer table until eviction.
  assert.ok(validPeerAddr('10.0.0.1:3137'));
  assert.ok(validPeerAddr(':3137'), 'the wildcard address is what a node bound to 0.0.0.0 advertises');
  assert.ok(!validPeerAddr('10.0.0.1'));
  assert.ok(!validPeerAddr('10.0.0.1:abc'));
  assert.ok(!validPeerAddr('bad host:3137'));
  assert.ok(!validPeerAddr(''));
  assert.ok(!validPeerAddr(null));
});

test('the aliases of one address normalise to the same node', () => {
  for (const alias of [':3137', '0.0.0.0:3137', 'localhost:3137', '[::]:3137', ':::3137']) {
    assert.equal(normalizeAddr(alias), '127.0.0.1:3137', `${alias} is this machine`);
  }
  assert.equal(normalizeAddr('10.0.0.2:3137'), '10.0.0.2:3137');
});

test('a datagram to a hostless address goes to loopback', () => {
  assert.deepEqual(targetOf(':3137'), { host: '127.0.0.1', port: 3137 });
  assert.deepEqual(targetOf('10.0.0.1:3137'), { host: '10.0.0.1', port: 3137 });
  assert.equal(targetOf('rubbish'), null);
});

test('the advertised IPv4 address is a real one', () => {
  assert.match(localIpv4(), /^\d+\.\d+\.\d+\.\d+$/);
});

test('a real socket carries datagrams and streams on one port', async () => {
  // The in-memory network cannot prove this: one socket for both directions is
  // exactly what the UDP transport is for.
  const a = await UdpTransport.listen(0);
  const b = await UdpTransport.listen(0);
  assert.ok(a.streamsAvailable, 'the stream listener has to bind the same port');

  const codec = new Codec('k', 'c');
  const received = new Promise((resolve) => a.once('packet', resolve));
  await b.send(`127.0.0.1:${a.port}`, codec.encode(Kind.HELLO, { from: 'b' }));
  const packet = await received;
  assert.equal(codec.decodeJSON(packet.data).body.from, 'b');

  const accepted = new Promise((resolve) => a.once('stream', resolve));
  const conn = await b.dial(`127.0.0.1:${a.port}`);
  const server = await accepted;
  const frame = readFrame(server, 1000);
  writeFrame(conn, Buffer.from('over a stream'));
  assert.equal((await frame).toString(), 'over a stream');

  conn.destroy();
  await a.close();
  await b.close();
});

test('a send to an unusable address fails rather than throwing into a loop', async () => {
  const t = await UdpTransport.listen(0);
  await assert.rejects(t.send('not-an-address', Buffer.from('x')), /unusable/);
  await t.close();
});

test('dialing a port nothing listens on rejects instead of hanging', async () => {
  const t = await UdpTransport.listen(0);
  await assert.rejects(t.dial('127.0.0.1:1'), /ECONNREFUSED|timed out/);
  await t.close();
});
