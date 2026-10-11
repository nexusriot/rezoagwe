'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { ChatTracker, ChatOutcome, MAX_TRACKED_GAPS, MAX_PULL_ATTEMPTS, MAX_TRACKED_AUTHORS } = require('../src/core/chatseq');
const { MAX_CHAT_PULL } = require('../src/proto/wire');

/**
 * The sequence rules are protocol: the Go, Kotlin and JavaScript trackers have
 * to agree or two nodes disagree about whether a line was ever said. These
 * mirror `chatseq_test.go` and `ChatSeqTest.kt` case for case.
 */

const msg = (node, seq, text = 'x') => ({ sender: '10.0.0.2:3137', node, text, seq });

test('numbering in order is all accepted and claims nothing', () => {
  const tr = new ChatTracker();
  for (let s = 1; s <= 5; s++) assert.equal(tr.observe(msg('a', s)), ChatOutcome.ACCEPT);
  assert.equal(tr.outstanding(), 0);
  assert.deepEqual(tr.stats(), []);
});

// The case the whole feature exists for.
test('a line that never arrives is named', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', 4));

  assert.equal(tr.outstanding(), 2);
  const plans = tr.pullPlans();
  assert.equal(plans.length, 1);
  assert.equal(plans[0].addr, '10.0.0.2:3137');
  assert.deepEqual(plans[0].seqs, [2, 3]);
});

test('a reordered line closes its own gap and is still shown', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', 3));

  assert.equal(tr.observe(msg('a', 2)), ChatOutcome.FILLED_GAP);
  assert.equal(tr.outstanding(), 0);
  assert.equal(tr.stats()[0].recovered, 1);
});

// Re-delivery is what the repair path does, so recognising it is not optional.
test('a duplicate is dropped', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', 2));

  assert.equal(tr.observe(msg('a', 2)), ChatOutcome.DUPLICATE);
  assert.equal(tr.observe(msg('a', 1)), ChatOutcome.DUPLICATE);
});

test('joining mid-conversation claims no gap', () => {
  const tr = new ChatTracker();
  assert.equal(tr.observe(msg('a', 97, 'we were already talking')), ChatOutcome.ACCEPT);
  assert.equal(tr.outstanding(), 0);
});

/**
 * Keyed by address alone, a restart would read as the whole conversation
 * rewinding and everything after it would be ignored as a duplicate. The node
 * id is what keeps the two apart.
 */
test('a restart without a state file is a new author', () => {
  const tr = new ChatTracker();
  for (let s = 1; s <= 9; s++) tr.observe(msg('old-id', s));

  assert.equal(tr.observe(msg('new-id', 1, 'hello again')), ChatOutcome.ACCEPT);
  assert.equal(tr.observe(msg('new-id', 2, 'still here')), ChatOutcome.ACCEPT);
  assert.equal(tr.outstanding(), 0);
});

test('an author that moves address is still one author', () => {
  const tr = new ChatTracker();
  tr.observe({ sender: '10.0.0.2:3137', node: 'a', seq: 1 });
  assert.equal(tr.observe({ sender: '10.0.0.2:4000', node: 'a', seq: 2 }), ChatOutcome.ACCEPT);
  assert.equal(tr.outstanding(), 0);

  tr.observe({ sender: '10.0.0.2:4000', node: 'a', seq: 4 });
  assert.equal(tr.pullPlans()[0].addr, '10.0.0.2:4000');
});

test('an unsequenced sender is never gap-checked', () => {
  const tr = new ChatTracker();
  for (let i = 0; i < 5; i++) {
    assert.equal(tr.observe({ sender: '10.0.0.9:3137', text: 'legacy' }), ChatOutcome.ACCEPT);
  }
  assert.equal(tr.outstanding(), 0);
  assert.deepEqual(tr.pullPlans(), []);
});

// "Gone" and "still waiting" are different answers.
test('asking stops once the author says the line is gone', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', 4));

  tr.markUnavailable('10.0.0.2:3137', [2, 3]);
  assert.equal(tr.outstanding(), 0);
  assert.deepEqual(tr.pullPlans(), []);
  assert.equal(tr.stats()[0].lost, 2);
});

test('an author that never answers is eventually given up on', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', 3));

  for (let round = 0; round < MAX_PULL_ATTEMPTS; round++) {
    assert.equal(tr.pullPlans().length, 1, `round ${round}`);
  }
  assert.deepEqual(tr.pullPlans(), []);
  assert.equal(tr.stats()[0].lost, 1);
});

test('one request is one datagram', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', MAX_CHAT_PULL * 3));

  const plan = tr.pullPlans()[0];
  assert.equal(plan.seqs.length, MAX_CHAT_PULL);
  assert.equal(plan.seqs[0], 2);
});

test('the gap set is bounded', () => {
  const tr = new ChatTracker();
  tr.observe(msg('a', 1));
  tr.observe(msg('a', MAX_TRACKED_GAPS * 4));

  assert.ok(tr.outstanding() <= MAX_TRACKED_GAPS);
  assert.ok(tr.stats()[0].lost > 0);
});

const entry = (node, seq, text = 'x') => ({ ts: 1, sender: '10.0.0.1:3137', node, text, seq });

/**
 * The baseline has to survive a restart, because a restart is exactly when a
 * line is most likely to have been missed: the node was not running.
 */
test('seeding from the stored log survives a restart', () => {
  const tr = new ChatTracker();
  tr.seed([1, 2, 3, 4, 5].map((s) => entry('alice', s)));
  assert.equal(tr.outstanding(), 0, 'seeding invented a gap');

  // Line 6 was said while this node was down; 7 arrives once it is back.
  tr.observe({ sender: '10.0.0.1:3137', node: 'alice', seq: 7 });
  assert.equal(tr.outstanding(), 1);
  assert.deepEqual(tr.pullPlans()[0].seqs, [6]);
});

// A snapshot is a bounded slice of someone else's ring.
test('seeding never invents a gap', () => {
  const tr = new ChatTracker();
  tr.seed([entry('alice', 40), entry('alice', 44)]);
  assert.equal(tr.outstanding(), 0);
  assert.deepEqual(tr.pullPlans(), []);

  tr.observe({ sender: '10.0.0.1:3137', node: 'alice', seq: 45 });
  assert.equal(tr.outstanding(), 0, 'the baseline was not raised to 44');
});

test('seeding closes a gap the snapshot happens to contain', () => {
  const tr = new ChatTracker();
  tr.observe({ sender: '10.0.0.1:3137', node: 'alice', seq: 1 });
  tr.observe({ sender: '10.0.0.1:3137', node: 'alice', seq: 3 });
  assert.equal(tr.outstanding(), 1);

  tr.seed([entry('alice', 2, 'the missing one')]);
  assert.equal(tr.outstanding(), 0);
  assert.equal(tr.stats()[0].recovered, 1);
});

// An entry from before the field carries no author id.
test('seeding tolerates an entry with no author id', () => {
  const tr = new ChatTracker();
  tr.seed([{ ts: 1, sender: '10.0.0.1:3137', text: 'old', seq: 9 }]);
  tr.observe({ sender: '10.0.0.1:3137', seq: 11 });
  assert.equal(tr.outstanding(), 1);
});

/**
 * A heartbeat says where an author's numbering stands, so the baseline is set
 * when two nodes meet rather than when one of them first speaks.
 *
 * Measured on a real cluster: a peer that had never spoken said three lines
 * while one node was off the air, and that node reported nothing — the first
 * line it heard afterwards became the baseline and the three were gone.
 */
test('an announcement sets the baseline on meeting', () => {
  const tr = new ChatTracker();
  tr.announce('bob', '10.0.0.2:3137', 4);
  assert.equal(tr.outstanding(), 0, 'meeting a peer claimed a conversation we were not in');

  tr.observe({ sender: '10.0.0.2:3137', node: 'bob', seq: 5 });
  assert.equal(tr.outstanding(), 0);

  // We go away; bob says three more. The heartbeat alone reveals it.
  tr.announce('bob', '10.0.0.2:3137', 8);
  assert.equal(tr.outstanding(), 3);
  assert.deepEqual(tr.pullPlans()[0].seqs, [6, 7, 8]);
});

test('lines from an author never heard before are still noticed', () => {
  const tr = new ChatTracker();
  tr.announce('bob', '10.0.0.2:3137', 0); // met at join, bob has said nothing
  tr.announce('bob', '10.0.0.2:3137', 3); // three said while we could not hear
  tr.observe({ sender: '10.0.0.2:3137', node: 'bob', seq: 4 });
  assert.equal(tr.outstanding(), 3, "the quiet author's lines are still invisible");
});

test('a stale announcement never rewinds the mark', () => {
  const tr = new ChatTracker();
  tr.announce('bob', '10.0.0.2:3137', 9);
  tr.announce('bob', '10.0.0.2:3137', 4);
  assert.equal(tr.outstanding(), 0);
  tr.observe({ sender: '10.0.0.2:3137', node: 'bob', seq: 10 });
  assert.equal(tr.outstanding(), 0, 'the mark was rewound');
});

/**
 * The gaps inside an author were bounded from the start; the authors were not.
 * Every heartbeat from every peer names one, and a node that restarts without
 * a state file comes back with a new id, so the set grew with cluster churn
 * and nothing in the protocol ever shrank it.
 */
test('the number of authors followed is bounded', () => {
  const tr = new ChatTracker();
  for (let i = 0; i < MAX_TRACKED_AUTHORS * 3; i++) {
    tr.announce(`author-${i}`, `10.0.0.1:${3000 + i}`, 1);
  }
  assert.ok(tr.senders.size <= MAX_TRACKED_AUTHORS, `following ${tr.senders.size} authors`);
  assert.ok(tr.senders.has(`author-${MAX_TRACKED_AUTHORS * 3 - 1}`),
    'the most recently active author was dropped');

  // An author dropped and heard from again is simply met anew.
  tr.announce('author-0', '10.0.0.1:3000', 50);
  assert.equal(tr.outstanding(), 0, 'a re-met author claimed a gap');
});

test('an author that keeps talking is never evicted', () => {
  const tr = new ChatTracker();
  tr.announce('busy', '10.0.0.2:3137', 1);
  for (let i = 0; i < MAX_TRACKED_AUTHORS * 2; i++) {
    tr.announce(`other-${i}`, `10.0.0.1:${3000 + i}`, 1);
    tr.observe({ sender: '10.0.0.2:3137', node: 'busy', seq: i + 2 });
  }
  assert.ok(tr.senders.has('busy'), 'the one author that never stopped talking was evicted');
});
