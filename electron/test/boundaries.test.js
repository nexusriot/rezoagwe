'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const corpus = require('../../testdata/boundaries.json');
const { KvStore } = require('../src/core/kvstore');
const W = require('../src/proto/wire');
test('shared cross-language boundary corpus preserves data, ordering, and v2 digest', () => {
  const s = new KvStore('test');
  for (const e of corpus.entries) {
    assert.equal(W.escapedLen(e.value),e.escaped_bytes.javascript);
    const u = { action: 'set', key: e.key, value: e.value, version: W.version(e.counter,e.node), expiresAt:e.expires_at };
    assert.ok(s.apply(W.decodeKVUpdate(JSON.parse(JSON.stringify(W.encodeKVUpdate(u))))));
  }
  assert.deepEqual(s.updates().map(e => e.key),corpus.sorted_keys);
  assert.deepEqual(s.fingerprint(16,2).buckets,corpus.buckets);
  const saved=JSON.parse(JSON.stringify(s.snapshotState()));
  const restored=new KvStore('test'); restored.loadState(saved.clock,saved.entries);
  assert.deepEqual(restored.updates(),s.updates());
  assert.equal(restored.write('next','v').version.counter,Number.MAX_SAFE_INTEGER);
});
