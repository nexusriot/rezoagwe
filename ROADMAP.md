# Rezoagwe — Roadmap

Prioritized backlog. Items are numbered by **global priority** (1 = do next)
and grouped into thematic tiers. Effort is a rough T-shirt size: **S** ≈ a few
hours, **M** ≈ about a day, **L** ≈ multi-day.

This complements the *Known limitations & next steps* table in
[DESIGN.md](DESIGN.md#11-known-limitations--next-steps); each open item notes
the code it touches.

Item 1 is next. The three Tier 0 items that sat ahead of it have shipped — see
*Unreleased* below — so the numbering is once again the whole backlog.

## Shipped

The repository is at **0.4.6** — the version the Makefile stamps into the Go
binaries and their `.deb`, the desktop client's `package.json`, and the Android
app's `versionName`; a test keeps the four in step.

Unreleased on top of it: the **Tier 0–3 round** below — eleven items — and the
**hardware round** after it, which found that one of those eleven had only
landed in the Go implementation. Eight
features off the backlog, and three ahead of them that came out of reading the
code for those eight: a data-loss defect, the cure the consistency check never
had, and the address only the rendezvous service can see.

All Go-side. The Kotlin and JavaScript ports are unchanged and remain at wire
parity: both fields this round added are optional, and a port that ignores them
simply does not forward a chat line and does not learn where it is seen from.

0.4.6 is what a second hardware round found by running two nodes from copies
of one state file: they diverged permanently on a shared key — neither version
newer, so neither replica ever accepting the other — while the consistency
check reported the cluster converged and every tool called the node healthy.
The store fingerprint folded the version and not the value, so the one feature
built to detect divergence could not see it. Fixing that is a protocol change:
the fold now covers a hash of the value in all three ports, and a node on an
older build reports every rebuilt peer as divergent until it is upgraded.

0.4.5 before it is what a hardware round did to that feature: it confirmed sequencing
works over a real radio — a line lost to a genuine Wi-Fi outage was named,
asked for and recovered — and then found three ways the implementation was
wrong, two of which only separate processes on a real network could reach.

0.4.4 before it closed the one gap the earlier hardware run found that was not
a bug in any code: chat had no repair path and, worse, no way to notice it
needed one.

0.4.3 before it was a documentation round over 0.4.2's fixes. The automated checks were
all green and stayed green: what they cannot see is a claim nobody thought to
encode, and five had gone stale — including the picture saying **"four
implementations"** twice while drawing three, with its own footer saying "all
three". The check written to stop that recurring found the fifth, in DESIGN.md,
the same day.

0.4.2 before it was a bug-fix round out of an end-to-end run on a physical Android tablet
with all three implementations in one cluster. Three of the four defects it
found were invisible to every existing test because each needed a second
process, a second implementation, or a force-stop to show itself.

0.4.1 before it was a documentation round: the prose was in good shape because a test suite
holds it there, and the two things that suite could not see — the picture, and
whatever the code grew that no document mentions — were both wrong.

0.4.0 before it was an observability, hardening and parity round: the Go node
caught up with what its own ports could already show, the gateway stopped being
an unauthenticated write surface, and the correctness fixes were carried into
the Kotlin and JavaScript ports — which had been carrying the same anti-entropy
defect all along.

Newest first.

### Unreleased — the Tier 0–3 round

Eleven items in one pass. Several found bugs in themselves on first contact
with something real, which is recorded here because it is the argument for
running a feature against a network rather than only a test.

#### Tier 0 — found while reading the code for the rest

**Nothing is stored that cannot be replicated intact** (§5.9). Bodies are JSON,
and every JSON encoder silently replaces a byte that is not valid UTF-8 with
`U+FFFD`. A binary value was therefore held intact by the node that wrote it
and mangled on every replica, *at the same version* — so anti-entropy compared
versions, found them equal and was right to do nothing. The consistency check
saw it and had nothing to offer; persistence marshalled the same way, so a
restart destroyed the last intact copy. Measured: five bytes in, eleven out.

Reading further found the worse half: a *key* is mangled identically and
becomes a different key, so the entry exists under one name here and another
everywhere else, and a later delete tombstones only one of them. Both are
refused now, on every path including deletes, which also turned up an
unconditional `DELETE` that discarded the store's answer and reported `204`
regardless — the same bug a `PUT` had before `writeRefusal` existed.

The ceiling test that used invalid UTF-8 to trigger six-byte escaping was
rewritten around control characters, which are valid, round-trip exactly and
escape the same way — a better test of a case that is still reachable.

**Repair** (§14.2). The consistency check named the keys replicas disagreed on
and stopped there; the only remedy was a manual delete and rewrite. `POST
/repair` re-stamps this node's copy so last-write-wins carries it everywhere,
tombstones included — the rewrite reads the *replication* view, which is the
only one that contains them. A key this node does not hold is reported rather
than deleted everywhere, and the keys must be named or explicitly derived,
because "repair everything" on the wrong replica overwrites a cluster with
stale data and nothing in the protocol can tell which side was right. The
end-to-end suite runs it against the §14.1 twins, after first asserting that no
amount of anti-entropy resolves their split.

**The reflexive address** (§13). `server.handle` was given the source address
and used it only to pick a reply path — the one component that can see where a
node's packets really come from was throwing it away, while the rule for a
wrong `-advertise` remained a loopback heuristic that could say an address
looked wrong and never which one was right. The roster now carries it back, the
diagnostics name the exact flag value to use, and `-advertise auto` adopts it.

Only hosts are compared: a datagram arrives from the node's own bound socket,
but a stream is dialled from an ephemeral port, and warning about a port the
node never claimed would be worse than silence. `auto` resolves *before* the
node is built — `cfg.AdvertiseAddr` is read without a lock by every loop in the
engine and is the node's identity in its own peer table, so a runtime rewrite
would have been a race in thirty places and would have left `IsSelf` answering
about an address the node had stopped using. Which is, exactly, how the mDNS
work below made a two-node cluster in which both nodes listed themselves.

#### Tiers 1–3

**Watch** (`GET /kv?watch=1`, §16). Every consumer polled, though the engine
knew the instant a key changed. The events are raised by `KVStore` rather than
by the six places that mutate it, so expiry — which replicates nothing — is on
the stream too, and a seventh mutation path cannot be added without one. The
subscription opens before the baseline is read, so a write racing the call is
delivered rather than lost. A subscriber that falls behind is told how many
events it missed: a watch with a silent hole in it is trusted, which makes it
worse than no watch.

**`rezoagwectl`** — a client, not a peer. Every other front end joins the
cluster to look at it; this one holds no replica and appears in nobody's peer
list. Distinct exit codes (2 no such key, 3 version mismatch, 4 replicas
disagree) are the point: a lock loop can tell "someone got there first" from
"this will never work", and a cron job can branch on `check` without reading
the report. `pkg/client` carries the same surface for Go callers.

**LAN discovery** (`-mdns`, §17). The protocol is implemented rather than
imported — one service type, four record types, no cache — so the dependency
list stays at four modules. Run against a real network it immediately found a
phone advertising a nearby-presence service and filed it as a rezoagwe node;
collection is now scoped to our own service type, and a record without a
`cluster=` TXT is not a match. It also exposed that a node publishing a real
address to the LAN while telling peers `:3137` is known by two names, so gossip
hands it its own address back: a two-node cluster in which both nodes listed
themselves. `-mdns` now resolves a wildcard `-advertise`.

**Chat relaying** (§5.8). A broadcast reached only the peers its author knew,
and §12 has always reported that peer tables are asymmetric. The first version
forwarded whenever it could not prove a peer had been reached, and the
end-to-end suite caught it: one line in a four-node mesh produced ten duplicate
deliveries. Forwarding now needs *evidence* of asymmetry from gossip, so a
healthy mesh forwards nothing — and the repair channel is still never a source,
because a repair that introduces a line is the failure §5.7 exists to rule out.

**Timings** (§13). Every measurement here was a counter, and a counter cannot
express a node evicted for being busy rather than gone. Histograms for the
gossip round, state sync and stream exchanges, plus how late each tick fired
against when it was *due* — a ticker drops ticks rather than queueing them, so
consecutive arrivals hide exactly that overrun. The diagnostics rule compares
the worst against `-evict`.

**Snapshots** (`-snapshot-dir`, §18). The state file is a replica, not a
backup: rewritten in place on every flush, so a mistaken delete is gone from it
within 250 ms. Timestamped exports with rotation, and `POST /snapshot` for the
one before a risky change.

**Benchmarks and fuzzing.** There were none of either. The codec parses bytes
from an open UDP port and had never been fed any it did not construct itself;
three fuzz targets now cover decode, the admission size check and the
skew/replay paths, and CI runs them on every push. The benchmarks exist to
settle a backlog argument with a number rather than an intuition — see below.

### Unreleased — the hardware round that followed

A Go node, an Android tablet (PRITOM M10, Android 16) and the desktop client in
one cluster on a real LAN. Pairing, replication, chat, consistency, topology and
diagnostics all three ways; then corner cases. Two bugs, both in the ports, both
from the same root.

**The refusal was Go-only.** §5.9 landed in the Go store last round and nowhere
else, so the Kotlin and JavaScript engines still accepted what the wire cannot
carry. Written in the desktop client's own UI, a value holding an unpaired
surrogate reached the Go node as `U+FFFD` and the tablet as `"?"` — three
implementations, three different values, one version, which anti-entropy
compares and is right to call agreement. Both ports now refuse it, on writes,
deletes and remote applies, and the interop suite asserts the two agree with Go
about which values those are.

**The desktop client's fingerprint described a value it did not hold.**
`valueHash` hashed `value` re-encoded as UTF-8, which substitutes `U+FFFD` for
an unpaired surrogate — so it reported the same hash as the Go node while
holding something else, and the consistency check called that agreement. The
one tool built to find divergence was blind to this one, which is exactly what
0.4.6 was released to fix for a different reason. It now hashes WTF-8: identical
to UTF-8 for every well-formed string, so the three ports stay pinned byte for
byte, and honest about the surrogate.

The round then used `repair -divergent` (§14.2) to heal the three-way split it
had created, across all three implementations, and confirmed the bytes matched
afterwards. Everything else held: 300 KB over the stream path to both ports, TTL
expiring independently on three replicas, a graceful leave dropped inside five
seconds against a fifteen-second eviction window, role restore after a
force-stop, a wrong-key stranger never admitted, and zero auth failures, chat
gaps or lost lines throughout.

Two test-harness gaps were closed on the way: both ceiling tests used unpaired
surrogates to trigger six-byte escaping and now use control characters, which
are valid, round-trip exactly and escape the same way; and the Android interop
harness silently built `http://http://...` from a scheme-prefixed env var, then
reported "the Go node never received the write" twenty seconds later when the
truth was that nothing could ask it.

### 0.4.6 — the divergence the divergence detector could not see

Found by running two Go peers from copies of one state file, which is what a
cloned image or a restored backup produces. Every version is stamped with the
node id, so the twins' writes to one key compared **equal**: neither newer,
neither accepted, 45 seconds of anti-entropy and still split — reported as
`converged: true`, every peer agreeing, `Node looks healthy`.

* **The fingerprint now folds a hash of the value**, in Go, Kotlin and
  JavaScript, re-pinned to new shared vectors. Empty for a tombstone, because
  `Apply` stores whatever value rides along with a remote delete and nothing on
  the wire guarantees that field is cleared. A **protocol change**: mixed builds
  report false divergence until every node is upgraded (§14).
* **The second round of the check names the key** and flags it `same_version` —
  the only difference in a report that replication will never repair.
* **A shared identity is reported directly** (§14.1): a peer announcing this
  node's own id is named outright, and one id claimed by two addresses is
  counted, because a node that moved address rebinds once where twins rebind on
  every heartbeat. Measured: 58 flips against 1.
* **Both GUI ports recommended tombstone GC without the warning that makes it
  safe.** Demonstrated on an isolated pair — a node stopped before a delete came
  back still holding the value, the delete was undone on both sides, and the
  check called them converged. The Go advice named the constraint; Kotlin and
  JavaScript, the two that expose the age as a field, did not. Now all three do,
  and a test in each port fails if the clause is dropped again.

The e2e cast gained two twins in a cluster of their own, seeded with the split
rather than racing for it, so the whole chain — permanent divergence, the named
key, the identity error — is asserted on every run.

### 0.4.5 — what a hardware round did to the finished feature

Chat sequencing was built, tested and documented before any of it had touched a
radio. Driving it on a real cluster — a tablet over Wi-Fi, two Go peers and the
desktop client — confirmed the feature works and then found three ways it was
wrong, two of which no in-process test could reach.

- ✅ **A repair can no longer introduce state** — found by a hardware round
  driving the finished feature. A `ChatBacklog` carrying a line attributed to
  another peer, numbered far ahead, was accepted: that raised the author's
  high-water mark so far that every genuine line they sent afterwards sorted
  below it and was dropped as a duplicate. **One datagram silenced an author at
  that node permanently** — through the mechanism built to stop chat going
  missing — and it spread, because the line landed in a log that every state
  snapshot is built from. A backlog is now accepted only for a gap this node is
  already waiting for, from the author that answered, and nothing claiming to
  be written by this node is taken at all.
- ✅ **A quiet author's lines are recoverable** — the baseline used to be set
  by the first line an author was *heard* to say, which conflates "I was not
  there" with "I was there and did not hear it". Measured: a peer that had
  never spoken said three lines while one node was off the air, and that node
  reported nothing. `Hello` now carries the sender's own chat position on every
  heartbeat, so the baseline is set when two nodes *meet*. Verified on hardware
  by recovering exactly that case with no later line needed to reveal it.
- ✅ **What the tracker accumulates is bounded** — the gaps inside an author
  were capped from the start, the authors were not. An entry is created by
  every *heartbeat*, not only by a line, and a node restarted without a state
  file returns with a new id, so cluster churn grew the set and nothing shrank
  it: the cleanup methods existed in all three ports and were dead code. Now
  capped at the most recently active, with `forget` wired to the explicit
  *forget this peer* action — and deliberately **not** to eviction, since a
  peer that falls quiet for fifteen seconds and comes back is exactly the one
  whose outstanding gaps are still worth asking for.
- ✅ **A flaky test, and the race it was hiding** —
  `NodeEngineTest.aPeerListedByNobodyWeTalkToStaysIndirect` failed once in six
  runs. `handleGossip` records a sender's claimed peers — which already puts a
  gossiped address in the graph — and only *then* adopts them. The test waited
  for mere presence, so `forgetPeer` could land in that window, find nothing to
  forget and return silently, leaving the node DIRECT for good. Waits for the
  adoption now.

### 0.4.4 — chat sequencing

- ✅ **A dropped chat line is detectable** — every broadcast now carries its
  author's own sequence number and persistent node id (wire kinds 13 and 14,
  [DESIGN §5.7](DESIGN.md#57-chat-delivery-and-the-gap-that-used-to-be-invisible)).
  The store is repaired because every entry carries a version a digest can
  compare; a chat line carried nothing, so a dropped one was not merely
  unrecoverable, it was **invisible** — the author's log said sent, every
  receiver's simply never had it, and no counter at either end moved. The
  hardware run lost exactly one line this way and the only reason anyone
  noticed was a human reading both screens.
- ✅ **…and repairable** — `ChatPull` / `ChatBacklog`, addressed to the
  author, because only the author can be sure what its own seq 7 was. The
  answer carries the lines it still holds *and* the ones it does not, which
  is what turns "still waiting" into "gone"; a receiver that is never told
  keeps asking forever.
- ✅ **…without showing anything twice** — a sequence makes a re-delivery
  recognisable, so the cure does not put a second copy of every recovered
  line on screen. Recovered lines are re-inserted where they were said, not
  appended after the lines that followed them.
- ✅ **Keyed on the node id, not the address** — the same choice a KV version
  makes. A node that moves ports stays one author; a node that restarts with
  no state file gets a fresh id and reads as a new one, instead of appearing
  to rewind its whole conversation and having everything after it dropped as
  a duplicate.
- ✅ **Reported** — `chat_gaps_total`, `chat_recovered_total`,
  `chat_lost_total` and `chat_duplicates_total` in all three ports, a *Chat
  delivery* block in the TUI, the Android metrics screen and the desktop
  counters, and a diagnostics finding that separates a gap which may still
  close (info) from one that will not (warning).
- ✅ **The baseline survives a restart** — found by asking what the e2e suite
  could not yet see. The tracker was in memory only, so a node that restarted
  read the first line from any author as its baseline and lost whatever had
  been said while it was down: the one window where a line is most likely to
  have been missed. Each stored line now records its author's id beside its
  sequence, and the tracker is seeded from the state file at startup and from
  every synced snapshot.
- ✅ **…and so does the node's own counter**, for the opposite reason: a node
  that began numbering again at 1 would have its first lines read as
  duplicates and dropped by every peer that still remembers the old process.
  Proved across processes by `TestARestartedNodeContinuesItsChatSequence`,
  which fails by the line never arriving at all — exactly the silent loss the
  feature exists to end.
- Carried into Kotlin and JavaScript in the same round, with the tracker
  rules pinned case-for-case in all three suites and the encoding pinned
  against the bytes Go writes.

### 0.4.3 — the documentation caught up with the fixes

- ✅ **A replication rule that was never written down** — DESIGN §5 lists the
  rules all three stores must agree on, and admission was not among them even
  though a refusal is now a distinguishable outcome with its own counter. New
  **§5.6**, "Admission, and why a refusal is not staleness": limits apply to a
  peer's update as well as a local writer's, a stale rejection means
  last-write-wins is working, and a refusal means this replica can never
  converge until someone moves a limit.
- ✅ **The chart said "four implementations" and drew three** — twice in the
  header and panel title, contradicting its own footer ("all three fold the
  same store") and every other document. Nothing caught it: a wrong *count*
  contradicts no single claim, so the existing chart checks all passed.
  Re-exported, and `docs.test.js` now derives the number from the repository
  and holds the chart and the prose to it. Quoted spans are skipped, so a
  document can still describe the mistake it used to make.
- ✅ **DESIGN.md said it too** — found by that new check within a minute of
  writing it, in §7's note about CI.
- ✅ **The fingerprint vectors were credited to two ports, not three** — §14
  said "identical across the Go and JavaScript stores" while Kotlin's
  `ParityTest` has pinned the same vectors since 0.4.0. Exactly the drift the
  kind-table check was added for in 0.4.1, one section over.
- ✅ **The store-limits and chat rows in §11 were stale** — the first still
  described a refusal as something only the consistency check could notice; the
  second did not mention that chat has **no repair path** at all, which the
  hardware run made visible: a dropped key heals on the next anti-entropy
  round, a dropped line never does, and the sender's own log shows it sent.
- ✅ **The Android role restore was undocumented**, in §9 and in
  `android/README.md`, along with the third hardware round — the one that found
  it.

### 0.4.2 — what a hardware end-to-end run found

- ✅ **A write refused by the store limits was reported as "stale"** — in all
  three ports. `Apply` returned one bool for "I already have something newer"
  and "I will not hold this at all", and every caller read the pair as
  staleness. A node joined to the live cluster with `-max-keys 10` counted 189
  refusals as stale rejections, narrated `ignored stale set for uni-quotes` for
  a key it had never held, and reported **"Node looks healthy"** while
  `/consistency` said it disagreed with all four peers in 9 of 16 buckets. The
  one replica that could never converge was the one claiming nothing was wrong.
  `ApplyWithReason` now carries the reason, a `kv_refused_total` counter stands
  apart from `kv_rejected_stale_total`, the activity feed names the limit that
  refused each entry, and the diagnostics raise an error saying waiting will not
  fix it. Carried into Kotlin and JavaScript the same round.
- ✅ **The Android node never came back when the app was relaunched** — the
  intent was persisted as `nodeWanted`, but its only reader was
  `NodeService.onStartCommand`, which cannot run after a force-stop or a reboot:
  precisely the cases where the node is gone. Opening the app showed a stopped
  node offering "Start node" while `shared_prefs` still said the user wanted it
  up. `MainActivity` now starts the service, which restores every wanted role
  off the main thread.
- ✅ **An empty `If-Match` made a conditional write unconditional** — the
  gateway guarded on `header != ""`, which cannot tell "no If-Match" from
  "If-Match:" with an empty value. `parseVersion` defines the empty string as
  "the key must not exist", so the one request a client writes to avoid
  clobbering a concurrent write was the one that clobbered it. A malformed
  `If-Match` already failed closed; an empty one now does too.
- ✅ **`POST /import?mode=…` ignored a mode it did not recognise** — the two
  modes differ by whether the file overwrites the whole cluster, and
  `?mode=SEED` answered 200 for a restore that never happened. Unknown modes are
  now a 400.

### 0.4.1 — documentation and the chart

- ✅ **The chart redrawn** — `rezo_agwe.png` was exported before wire v2 and had
  been wrong ever since: "protobuf · UDP", "no acks, no auth, no anti-entropy",
  message kinds 0–6 of the 16 that exist, a version tiebreak on the node's
  *address*, and no sign of the HTTP gateway, the TCP state sync, the two ports,
  or anything from §12–§14. It now draws the protocol as it is.
- ✅ **`make chart`** (`scripts/render-chart.sh`) — the export is one command
  rather than a GUI session, which is the actual reason the old one rotted for
  four releases. It quantises the result, so the README's picture costs 400 KB
  instead of 1.2 MB.
- ✅ **The chart is tested** — the suite asserts the drawing names every message
  kind, makes the claims about the protocol that are the point of having it, and
  makes none of the ones it outgrew, and that the `.png` is not older than the
  `.drawio` it came from. An image is the one document that rots invisibly.
- ✅ **A complete command-line reference**
  ([DESIGN.md §15](DESIGN.md#15-command-line-reference)) — every flag both
  binaries accept. `-timeout` and `-version` were documented nowhere at all,
  because the flag test only ever ran README → code and never code → README.
  It now runs both ways.
- ✅ **DESIGN.md §3.2 checked against Go** — the message-kind table was pinned to
  the Kotlin and JavaScript ports and never to the reference implementation,
  which is how `KindFingerprintOK` came to be the only name in the project that
  was not the documented `FingerprintReply`. Renamed, and the third check added.
- ✅ **Smaller corrections** — the desktop README pointed at the wrong roadmap
  item for its own tray-icon entry; §9 and §10 listed the engine files of the
  two ports but not their transports, persistence, runtime or — for the desktop
  — the diagnostics module the README advertises a screen for; and the Android
  section credited `Transport.kt` with the `Dispatchers.IO` dispatch that is
  actually made, and enforced, at every call site in `ui/`.

### Parity across the three implementations

- ✅ **Every correctness fix carried to Kotlin and JavaScript** — the two ports
  shipped with the same anti-entropy defect the Go node had (a 256-key digest
  against a 128-entry repair budget, and one keyspace cursor shared across every
  peer), and with no bound on what a peer could push into their stores. Both now
  clamp the digest to `min(maxPush, maxPull)`, keep a cursor per peer and drop it
  with the peer, and enforce `maxValueBytes` / `maxKeys` against a peer as well
  as a local writer.
- ✅ **Store fingerprint in all three** — `KvStore.fingerprint()` in Kotlin and
  JavaScript alongside Go's, each pinned to the same fixed vectors, so a
  consistency check can be run against any node from any node. A fold that
  differed between ports would report two converged replicas as divergent.
- ✅ **Consistency check initiator in all three** — `checkConsistency()` in the
  Kotlin and JavaScript engines, and a *Verify replicas* action on the Android
  Diagnostics screen next to the Go TUI's `v` and `GET /consistency`.
- ✅ **Verified on hardware, cross-implementation** — see *Verified on hardware*
  in [android/README.md](android/README.md).

### Observability and operability

- ✅ **Cluster graph** (`pkg/discovery/topology`, `g`, `GET /topology`) — roles,
  link kinds and connected components, derived from the peer lists gossip
  already carries. The Android and desktop ports had this; the Go node, the one
  most likely to be running headless, could only list its peers, and a list
  cannot show a partition. See [DESIGN.md §12](DESIGN.md#12-topology).
- ✅ **Diagnostics** (`pkg/discovery/diagnostics`, `D`, `GET /diagnostics`,
  `-doctor`) — the counters turned into causes, as pure rules over a snapshot
  so every claim about the protocol is testable. A key mismatch, a clock out of
  skew, one-way UDP, a loopback address advertised to a LAN, peers about to be
  evicted. See [DESIGN.md §13](DESIGN.md#13-diagnostics).
- ✅ **Consistency check** (`v`, `GET /consistency`, wire kinds 11/12) — asks
  every peer to summarise its whole store as bucket digests and reports who
  disagrees and about which region of the keyspace. Anti-entropy repairs
  divergence but never reports it, so a cluster could sit split for as long as
  nobody looked. Over streams, not datagrams: a dropped answer would read as a
  disagreement. All three implementations answer, and their bucket digests are
  pinned to each other byte for byte.
  See [DESIGN.md §14](DESIGN.md#14-consistency-checking).
- ✅ **Per-peer traffic, send errors and gauges** — packets and bytes per
  address in both directions, per-address authentication rejections, the last
  send error kept whole, and `rezoagwe_peers` / `keys` / `tombstones` /
  `value_bytes` / `lamport_clock` as Prometheus **gauges**. A global counter
  cannot tell a quiet peer from an unreachable one, and an alert is written
  against a level, not a total.
- ✅ **Unknown-source tracking** — `handlePacket` takes the address the
  transport saw, so a node whose advertised address is not where its packets
  come from (a NAT, a wrong `-advertise`) is visible instead of invisible.
- ✅ **CI** — five jobs on every push: the Go suite under the race detector with
  `gofmt` and `go vet`, every cross-build target and every `.deb`, the desktop
  suite, the Android unit tests and APK, and the containerised end-to-end run
  in a job of its own.
- ✅ **systemd units in the `.deb`** — both roles, their `/etc/default` files, a
  `rezoagwe` system user owning `/var/lib/rezoagwe`, and maintainer scripts.
  Neither unit is enabled on install. `packaging/layout.sh` is the one place
  that lays this out, so the Makefile and `build-deb.sh` cannot drift.

### Correctness and security

- ✅ **Repair budget matches the digest** — `DigestBatch` is clamped to
  `min(MaxPush, MaxPull)`. It was twice it: a 256-key digest, a 128-entry
  repair cap, and a cursor that advanced by the whole digest, so the second
  half of every badly-diverged range was stranded until the cursor wrapped the
  entire keyspace.
- ✅ **Per-peer anti-entropy cursors** — a shared cursor divided the ranges
  among whichever peers the random target picked, so covering the whole store
  against any single peer took as many wraps as there were peers.
- ✅ **Gateway token, TLS and read-only mode** — `-http-token` on every route
  (no exemption for `/health` or `/metrics`), `-http-tls-cert`/`-http-tls-key`,
  `-http-readonly`. The gateway was a full read/write control surface on a
  cluster that authenticates every packet on the wire, and it authenticated
  nothing: `POST /import?mode=seed` rewrites everything.
- ✅ **`-advertise`** — what peers are told, separate from what the node binds.
  A node bound to `:3137` told every peer to reach it at `:3137`, which each
  resolved to its own loopback; the diagnostics now flag that too.
- ✅ **Store limits** — `-max-value-bytes` and `-max-keys`, enforced against a
  peer as well as a local writer, off by default and documented as the trade
  they are: a refused update is a deliberate divergence.
- ✅ **Coalesced persistence** — a mutation marks the state dirty and a flush
  loop rewrites the file at most once every 250 ms, with a synchronous flush on
  a clean shutdown, and the file is compact rather than indented. It was
  synchronous on every mutation, so one `PUT` serialised the whole store and a
  128-entry repair serialised it 128 times.
- ✅ **Conditional reads** — `GET /kv/{key}` honours `If-None-Match` against the
  version it already published as the `ETag`.

### Earlier

- ✅ **Android on hardware** — run on a PRITOM M10 tablet against a Go cluster
  on a real LAN. It closed the item and found the bug behind it: every UDP send
  made from a Compose callback was silently dropped. Dispatching to
  `Dispatchers.IO` took a UI write from ~10 s to 234 ms, and `UiThreadingTest`
  now fails the build if the pattern comes back. Battery cost of the 10 s tick
  is still unmeasured.
- ✅ **Hermetic end-to-end suite** (`make e2e`) — a rendezvous, three peers, a
  late joiner, a leaver, a restarting node and two strangers in containers on a
  private network, driven through the HTTP gateway.
- ✅ **Desktop app** — a JavaScript port of both roles in Electron, with the
  cluster drawn as a graph, a diagnostics screen and a split view.
- ✅ **Android app** — Kotlin port of both roles with a Compose UI, a foreground
  service, and parity tests against the Go codec.
- ✅ **Go `null` slices decoded** — Go marshals a nil slice as `null`, so an
  empty node's digest authenticated and then failed to parse in the Kotlin app.
- ✅ **Wire v2** — one authenticated framing for every packet; protobuf removed.
- ✅ **Pre-shared key + HMAC + replay guard**, **anti-entropy reconciliation**,
  **chunked state sync over TCP**, **compare-and-swap**, **key TTL**,
  **tombstone GC**, **HTTP/REST gateway**, **metrics**, **KV activity feed**,
  **key history**, **import/export**, **transport abstraction + lossy in-memory
  network**, **persistent node identity**, **chat enhancements**, **multiple
  bootstrap seeds**, **bootstrap hardening**, **`-debug` with `-logfile`**,
  **headless mode**, **version via ldflags**, **socket reuse and peer-address
  validation**.

Earlier milestones: versioned last-write-wins, robust bootstrap join,
chat-history sync on join, local persistence, graceful leave.

---

## Tier 1 — Data model

### 1. Typed values — **L**
A per-key type tag on `KVUpdate`: `lww` (today, the default), `counter` (a
PN-counter, so concurrent increments are never lost) and `set` (an OR-set, so
concurrent adds both survive).
**Why:** every value is an LWW string, which means concurrent writes *lose
data* by design — the store converges by discarding one of them. This is the
one change that alters what the thing is for, and the machinery is already
there: the store thinks in per-key versions and carries a Lamport clock.
**Notes:** touches `model/kvstore.go`, §3.2 and §5, and all three ports
together. The merge rule per type belongs in DESIGN §5 before any code.

### 2. Leases and locks — **M**
`POST /lock/{name}?ttl=30` → a token, `POST /lock/{name}/renew`,
`DELETE /lock/{name}`, and `GET /election/{name}` for the current holder.
**Why:** the README has said since 0.1.0 that compare-and-swap is "enough to
build a lock or a leader election on". Nobody wants to build it. It is CAS plus
TTL plus a renewal loop, and it turns two primitives into something usable.
**Notes:** must be documented as a *local*-CAS lease, not a consensus lock —
two partitions will each grant it.

### 3. Resumable watch — **M**
A cursor a client can reconnect from, so a dropped connection does not mean a
re-read of the whole store.
**Why:** `?watch=1` ships, and its one honest weakness is that a subscriber
which falls behind loses events. It is *told* how many (§16), which is the
difference between a gap and a lie, but the events are gone — the queue is
bounded because publishing runs on a replication path and must not block.
**Notes:** needs a bounded per-node event log to resume from, which is most of
the append-only-log work at 9. Do that first or do them together.

### 4. Watch in the Kotlin and JavaScript ports — **S**
The Go engine raises mutation events from the store; the other two raise
nothing.
**Why:** the phone and the desktop app both poll their own engines on a timer,
which is the thing this round removed from the Go node. **Notes:** both have
the same single-file store with the same mutation sites, so it is the same
change twice, with no wire implications at all.

## Tier 2 — Reach

### 5. Prefix-scoped state requests — **S**
`/kv?prefix=` filters locally after listing everything. A prefix-scoped state
request would make a large store usable from a small client.

### 6. Desktop background mode — **S**
Closing the window stops the node on Linux and Windows, because there is no
tray icon to keep the app alive. **Why:** the same objection the Android
foreground service answers — a node that only runs while its window is open is
evicted seconds after it is minimised. **Notes:** `electron/src/main.js`
already routes quitting through a `Goodbye`, so this is a tray icon and a
"close to tray" setting, not a lifecycle change.

## Tier 3 — Security

### 7. Encrypted payloads, and an encoding tag — **M–L**
The frame is authenticated but the body is plaintext. Add AEAD — the nonce is
already there. **Notes:** the codec change is contained; the coordination is
not. It is a wire v3 bump across Go, Kotlin and JavaScript together, with a
decision about whether v2 peers are refused or tolerated during a rollout.
Sized for that, not for `codec.go`.

Carry the **encoding tag** in the same bump. A value that is not valid UTF-8
cannot cross a JSON wire intact and is now refused rather than mangled (§5.9),
which is honest but still a refusal: a caller with bytes to store has to encode
them. `"enc":"b64"` on `KVUpdate` would carry them. It is a one-field change
and a three-implementation rollout, so it rides with the one already planned
rather than spending a second one.

### 8. Per-node keys — **L**
One shared psk means any member can impersonate any other. Per-node keypairs
with signed Hellos would close that.

## Tier 4 — Scale

### 9. Append-only log + snapshot — **M**
Coalescing bounded how *often* the store is rewritten; it is still rewritten
whole each time. A log with a periodic snapshot makes a flush proportional to
what changed, and shrinks the 250 ms a hard kill can lose.
**Why, with a number:** `BenchmarkState` is 8.4 ms and 11.5 MB allocated at
100k keys, paid per flush, to persist whatever changed in the last 250 ms.
**Notes:** `writeAtomic` also never fsyncs. The rename is atomic for a reader,
which is what the comment claims and all it claims, but a power loss can still
lose a write the filesystem reported as durable — worth fixing in the same
pass, and cheap. `-snapshot-dir` (§18) is the backup story and is not this:
it answers "the disk is gone", not "the process was killed".

### 10. Merkle-tree digests — **M**
Replace the flat per-key digest with a tree over key ranges, so reconciliation
costs `O(log n)` rounds instead of walking the keyspace.
**Why:** a cluster with a million keys still converges slowly after a long
partition. **Notes:** deliberately below the data-model work now that the
budget clamp and per-peer cursors have fixed the convergence problem clusters
of this size actually hit; `KVStore.Digest`/`Reconcile` isolate the comparison,
so this swaps the summary type without touching the engine.

`make bench` now puts a number on it rather than an intuition, which was the
stated reason this was demoted from priority 1 in the first place. On one
machine, per call:

| | 100 keys | 10k | 100k |
|---|---|---|---|
| `Digest` (a 128-key window) | 28 µs | 1.3 ms | **16 ms** |
| `Reconcile` | 15 µs | 0.19 ms | 2.4 ms |
| `Fingerprint` (whole store) | 87 µs | 6.4 ms | 71 ms |
| `State` (every flush) | 7 µs | 0.59 ms | 8.4 ms |

`Digest` is the one on the gossip loop, and it is linear in the *store*, not in
the window: producing a 128-key digest from 100k keys costs 16 ms because the
keyspace is walked and sorted to find the window. That is still two orders of
magnitude inside a 10 s tick, so this remains correctly ranked here — but it is
also the shape the tree fixes, and the first thing to re-measure at a million
keys, where 16 ms becomes 160 and the lateness rule (§13) starts firing.

`Fingerprint` at 71 ms and 44 MB allocated is the expensive one, and it is
correctly off the gossip path: the consistency check is operator-driven. It
would need the tree too before it could run on a timer.

### 11. Backpressure on repair traffic — **S**
`maxPush`/`maxPull` bound one round, but nothing rate-limits successive rounds;
a node that falls far behind gets a burst per gossip tick.

### 12. Structured logging — **S**
`-logfile` writes logrus text. JSON lines with the node id would make a
multi-node run greppable.

---

## Dropped

Three items from the previous backlog were removed rather than reprioritised.

- **Quorum state sync** (pull from several peers on join and merge). A joiner
  inherits one peer's gaps for exactly as long as it takes anti-entropy to fill
  them, which — with per-peer cursors and a digest that repairs everything it
  covers — is now a round or two. The cost is a joiner that dials N peers and
  merges N snapshots, on the one code path where a slow peer is most visible.
  Still listed as a limitation in DESIGN §11; no longer worth doing.
- **Acked membership** (SWIM-style ping with a suspicion phase). The failure it
  fixes — a busy-but-silent peer looking dead — is now *visible*: per-peer
  traffic and the diagnostics say which peers are going stale and which never
  answered at all. Seeing it is most of the value; replacing the eviction
  mechanism is a protocol change across three implementations for the rest.
- **Merkle digests as priority 1.** Kept, at 10. It was ranked first against a
  convergence problem that turned out to be a two-line budget bug and a shared
  cursor. The tree is the right answer at a million keys and the wrong thing to
  have spent a day on before measuring.
