# Rezoagwe — Roadmap

Open work, implemented foundations and release history. Numbered items use stable
IDs, not a promise of implementation order. The recommended next steps below set
the current suggested order. Effort estimates are rough: **S** is small, **M** is
moderate, and **L** involves substantial design or cross-platform work.

Current behavior and limitations are documented in [MANUAL.md](MANUAL.md) and
[DESIGN.md](DESIGN.md). Suggestions below are not implemented unless explicitly
marked otherwise.

## Working-tree additions (October 25, 2026)

Guarded repair and restore, digest negotiation, persistence health/profiles,
journal modes, resumable Go watches, bounded desktop browsing and bounded repair
sends are implemented but not assigned a new release version. Operating details
are in the [manual](MANUAL.md#13-repair-previews-and-cluster-profiles).

## Recommended next steps

| Order | ID | Feature | Benefit | Effort |
|---|---|---|---|---|
| 1 | 13 | Storage-limit controls and preflight checks | Explain incompatible replica limits before users hit refusals | S |
| 2 | 15 | Automatic watch reconnect | Recover brief outages without custom client retry loops | M |
| 3 | 14 | Android restore preview | Bring selected, guarded restore to the phone | M |
| 4 | 16 | Inspectable restore jobs | Show progress and permit safe cancellation of large restores | M |
| 5 | 17 | Journal inspection and recovery tool | Diagnose corrupt/missing files without hand-editing state | M |
| 6 | 20 | Per-peer repair scheduling | Keep slow peers from monopolizing the repair budget | M |

## Shipped

The repository is at **0.4.8** — the version the Makefile stamps into the Go
binaries and their `.deb`, the desktop client's `package.json`, and the Android
app's `versionName`; a test keeps the four in step.

0.4.8 is a device round: a tablet, the desktop client and two Go peers on one
LAN. It found that the reflexive address 0.4.7 added had shipped in the Go
rendezvous and in neither port, and that the Android app threw away every piece
of typed state on a rotation and kept a refused setting on screen with nothing
in effect behind it. See *0.4.8* below.

0.4.7 releases three rounds. The **Tier 0–3 round** below is eleven items:
eight features off the backlog, and three ahead of them that came out of
reading the code for those eight — a data-loss defect, the cure the
consistency check never had, and the address only the rendezvous service can
see. That round is all Go-side; the Kotlin and JavaScript ports are unchanged
by it and remain at wire parity, because both fields it added are optional and
a port that ignores them simply does not forward a chat line and does not
learn where it is seen from. That last half-sentence was wrong about what it
cost, and 0.4.8 is what it cost: a port that does not *report* where it sees a
node from leaves every joiner guessing, whatever the joiner is written in.

The **hardware round** after it found that one of those eleven had only landed
in the Go implementation. The **device round** that closes 0.4.7 ran a tablet
and the desktop client against a Go cluster at the same time; it left the wire
untouched and fixed what sits either side of it — a port the Android settings
screen should never have accepted, and a packaging script that shipped two of
the three binaries.

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

### 0.4.8 — a second device round, on a different network

The cluster rebuilt from scratch on another LAN with a fresh install, which is
itself worth doing: every address in every config was different, and the round
started by finding that the Go suite had gone red on a clean tree.

**`EscapedLen` over-counted invalid UTF-8, because the toolchain changed under
it.** It charged six bytes per undecodable byte — correct while Go wrote the
escape `\ufffd`, wrong since Go 1.25 writes the three-byte replacement
character itself. `EscapedLen` is what decides whether a value can replicate at
all, so the drift turned into a size check that refuses values which fit. The
file already says its escape table is "derived, not copied" and re-derives it
from the encoder in a test; this was the one cost that was copied instead, and
it is the one that moved. It is measured now, with the fallback biased to the
larger historical cost because over-counting only refuses a value that would
have fit while under-counting strands one that can never replicate. A second
test pins the `/6` bound that `admitLocked` uses as its cheap pre-filter, since
that is the encoder's number too.

**Tombstone GC said nothing once it was switched on.** With GC off all three
ports warn that a node away too long will come back and undo a delete. With GC
on — the same hazard, configured — all three were silent. Reproduced on two
nodes with an eight-second age: the delete was undone on every replica, and
`/consistency` reported the cluster converged, because it was. Nothing is in
the activity feed and nothing is in the findings; only `kv_gc_total` moves.

It cannot be reported after the fact — a node that reclaimed a tombstone and
then accepted a set for that key cannot tell a resurrection from someone
writing it again — so the exposure is named while it is configured, which is
the only honest moment. Added to all three ports at once, with the cross-port
check that keeps it there.

Everything else held: mDNS discovery with no seeds at all (first time on real
hardware), the TLS gateway, periodic snapshots and their retention, multi-seed
failover past a dead first seed, a wrong pre-shared key staying isolated, peer
forget and rediscovery, import merge losing to a newer version while
`?mode=seed` wins, and on the tablet every fix from the previous round —
rotation keeping an open dialog, a chat draft and unsaved settings; a refused
port snapping back with a message; the offline advice staying "reconnect"
rather than naming a stale address. A real Wi-Fi outage again resolved
last-write-wins on all four nodes with the tombstone holding and all six missed
chat lines recovered in order.

One observation, not fixed: the cluster graph is a bare Canvas with no
accessibility semantics, and *forget this peer* is reachable only by tapping a
node on it. It cannot be driven by uiautomator and cannot be reached by a
screen reader at all.

### 0.4.8 — the device round that read the ports back

A PRITOM M10 (Android 16), the Electron client and two Go peers plus a Go
rendezvous on one LAN, cluster `e2elab`. Most of this round is the happy path
and the corner cases holding: a write from each of the three implementations
reaching the other three with all four folding to the same fingerprint, a 2 MB
value over the stream path, emoji, CJK, a 4 KB key, `/ # ? %`, a newline, a
tab, a key of nothing but spaces, an empty value, invalid UTF-8 refused with
`422`, TTL, CAS both ways, a real Wi-Fi outage resolved by last-write-wins with
the tombstone holding and five chat lines detected, pulled and re-inserted in
order, a force-stop and relaunch that restored the node by itself, and forced
deep Doze. Three things did not hold.

**The reflexive address shipped in one implementation of three** (§13). 0.4.7
added it and the note above said the ports were unaffected because the field is
optional. Optional for a *reader*; for a *writer* it is the whole feature. The
Kotlin and JavaScript rendezvous services never filled in `SeenAt`, and neither
node ever read one, so the ERROR that names the address a node should be
advertising — the one check that can say which address is right rather than
that this one looks wrong — simply did not exist outside Go. Measured: a Go
node run with `-advertise auto` against the tablet's rendezvous resolved to
`0.0.0.0:<port>` and published that to the cluster, an address no peer can
dial. Both halves are implemented in both ports now and verified on the LAN in
both directions.

Nothing failed while this was broken, which is the part worth keeping: an
unknown field is ignored by design, so a port that says *less* than the others
is invisible to every suite including the interop one. Three checks now hold
the implementations together — `docs.test.js` on the three services and the
three rules, a `WireCompatTest` case pinning the encoding against the bytes Go
writes, and an interop case that runs a real Go node against *this client's*
rendezvous, which is the direction nothing tested before.

Two harness faults surfaced while writing those. `MemStream` had no
`remoteAddress`, so the in-memory network could not model a connection's source
at all and the desktop suite was structurally unable to see this bug; it now
carries both ends' addresses and hands out a fresh ephemeral source port per
dial, so a rule comparing ports rather than hosts fails there too. And the
first draft of the interop test used `spawnSync` while the rendezvous under
test ran in the same process: the kernel completes the handshake into the
backlog, so Go saw a connected socket, wrote its request and timed out reading
— indistinguishable from a service that accepts a stream and refuses to answer.

**Rotation discarded everything typed into the Android app.** An open key
dialog was dismissed outright, taking the key, the value and the TTL; the chat
composer emptied mid-sentence; unsaved **Settings** edits reverted. The
navigation state was already `rememberSaveable` and only the forms were not, so
the app came back on the right screen with the work gone, which reads as having
thrown it away on purpose. A tablet is the device that gets rotated. The row
being edited is now held by key with the version it opened against beside it —
re-reading the entry on restore would have quietly let through a peer write
made while the screen was turning. `repo.test.js` fails on a screen that holds
typed text in state a rotation discards.

**A state file that could not be read was lost in silence, in two ports of
three.** The desktop client had reported it from the start; Go logged it to a
logger that is discarded without `-logfile`, and the Kotlin port caught the
exception and returned null. The consequence is not obvious from "the file was
bad": the node starts with an empty store *and a freshly minted identity*, and
then rewrites the unreadable file with that new state, so the evidence is gone
by the time anyone looks. Its earlier writes are outranked until anti-entropy
refills it, and every chat line it has ever said now reads as coming from a
different author. Verified on the tablet: the backup held node id `45ae329a…`
and the rewritten file `36c5c470…`. The mirror image of the `seen_at` gap, and
checked the same way.

**And the first fix caused a regression that only the radio could show.**
Reporting the observed address outranks the loopback guess — except with Wi-Fi
off, when a device advertises loopback because it has nowhere else to advertise
and the address a rendezvous saw belongs to the network it has just left.
Switching Wi-Fi off mid-round replaced *"Connect to a network"* with *"set the
advertise host to 192.168.18.60"* — wrong then, and still wrong on the next
network joined. The rule now defers to the `offline` flag the port already
threads through its other findings, which is what that flag is for.

**A refused setting stayed on screen with nothing in effect behind it.**
Typing `0` into *Node port* and pressing *Apply* correctly stored 3137 and left
the field reading `0`, silently: the form re-seeds only when the stored
settings change, so editing *just* the bad field is exactly the case with no
feedback. `Settings.refusals` now names what was not kept, the fields snap back
to what is in effect, and the desktop client — which reverted visibly but
toasted "Settings applied" over it — raises the same thing as a warning.

### 0.4.7 — the Tier 0–3 round

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

### 0.4.7 — the hardware round that followed

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

### 0.4.7 — the device round, and two packaging paths that disagreed

A full end-to-end run on a physical Android tablet and the Electron client at
once, both joined over WiFi to a two-node Go cluster — four peers, three
implementations. The happy path held throughout: writes replicated in every
direction, chat in every direction, unicode, emoji, control characters, an
empty value and a 200 KB value over the stream path, TTL expiring independently
on all four replicas, compare-and-swap returning the documented exit codes, a
delete propagating as a tombstone, and the consistency check agreeing from the
Go CLI, the desktop client and the tablet. A partition — WiFi off, a conflicting
write on each side, WiFi back — healed by itself and converged on one value.

* **The Android settings screen accepted a port no socket can bind.** The
  desktop client refuses anything outside 1..65535 in favour of the value the
  user already had, and a Go node exits with `invalid port`; Kotlin trimmed its
  text fields and range-checked nothing. The two halves fail differently and
  the second is the dangerous one. `99999` throws on bind and leaves the node
  stopped, which at least says so. `0` binds an *ephemeral* port, so the node
  starts, keeps gossiping outward, and reports `CONNECTED` with a full peer
  list — while advertising `host:0`, an address no peer can dial. Every other
  node had already dropped it (all three implementations reject a peer address
  with port 0, which is why the damage stopped there), and the roster it
  registered into handed `host:0` to each new joiner until it aged out. The
  value persisted, so the app stayed broken across restarts. Now refused in
  `Settings.sanitized()`, and repaired on load so a preference written by an
  older build cannot outlive it.
* **`build-deb.sh` shipped two of the three binaries.** `make deb-amd64` built
  `rezoagwectl`; the script beside it did not, so a package made one way was
  missing the client the README says the package installs, and neither path
  errored. Found by listing both `.deb`s. This is the drift
  `packaging/layout.sh` already exists to prevent for the unit files, one
  directory up.
* **The docs had not caught up with the third binary.** The Makefile header,
  `DEBIAN/control` and DESIGN.md §1 all still said the project produces two;
  DESIGN.md §15, the command-line reference whose own note explains that an
  undocumented flag is a flag nobody finds, had no entry for `rezoagwectl` and
  so documented none of its eight. The flag test could not see it because the
  binary's name was not in the test's list.

Three tests close the three gaps: the Android settings suite now pins the port
range the way the desktop suite already did, the §15 flag check reads the
`cmd/` directory instead of a hand-written list and fails when a new binary
appears, and a packaging test holds both Go `.deb` paths to the same set of
commands. The chart was re-checked against the code rather than re-exported:
its bucket count, flush window, length prefix, intervals and architecture count
all still match.

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

### 2. Leases and locks — product decision required
A local CAS-plus-TTL lease can simplify single-replica workflows, but two
partitions can both grant it. A globally exclusive lock needs a consensus
coordinator plus fencing enforced by the protected resource. Decide which product
is wanted before adding endpoints; do not label a local lease a distributed lock.
The existing CAS semantics are already documented and tested.

### 3. Resumable watch — implemented in working tree
Bounded in-memory replay, process/prefix-bound SSE cursors, and explicit resnapshot
signals now ship in the Go gateway/client. History does not survive restart.
See [watch operation](MANUAL.md#resumable-go-watches). Automatic reconnect and
restart-surviving event history remain open as items 15 and 21.

### 4. Mutation subscriptions in Kotlin and JavaScript — **M**
Expose prefix-scoped per-key events with source, version, expiry and explicit
bounded-buffer overflow signals. Existing entry-list/UI notifications are not a
public resumable mutation-stream contract. Define ordering and cancellation first;
add replay parity only if a network/watch API is introduced for these ports.

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

### 9. Append-only log + snapshot — implemented in working tree
Optional journal and journal-sync modes now exist in Go, Electron and Android,
with checksummed deltas, checkpointing, tail recovery and persistence health.
Explicit durable PUT acknowledgements are available in the Go gateway/client/CLI.
See [journal operation](MANUAL.md#journal-durability). Inspection/recovery tooling
and a disk-backed store remain open as items 17 and 22.

### 10. Merkle-tree digests — **M**
Replace the flat per-key digest with a tree over key ranges, so small differences
can be located without scanning every key. Transfer cost still depends on how
much state differs; do not assume logarithmic cost for a full-cluster rebuild.
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

### 11. Backpressure on repair traffic — foundation implemented
Two active repair sends are allowed per node; Go also queues up to 16 jobs.
Oversized values are skipped using advertised peer limits, and deferred work is
retried by anti-entropy. This does not impose byte-rate limits, per-peer fairness,
or limits on every state-sync and ordinary write path. Remaining scheduling work
is item 20.

### 12. Structured logging — **S**
`-logfile` writes logrus text. JSON lines with the node id would make a
multi-node run greppable.

---

## Additional proposals and carried-forward gaps

### 13. Storage-limit controls and preflight checks — **S**, recommended
Expose maximum keys/value bytes in desktop and Android Settings. Before applying,
show local entries that exceed the proposed limits and peers advertising different
limits. Done when settings survive restart, diagnostics explain incompatibility,
and changing a limit never silently deletes data.

### 14. Android restore preview — **M**, recommended
Add file-picker backup inspection, before/after values, selected-key apply and
explicit cross-cluster/delete-missing options. Reuse exact-state guards and keep
selection across rotation. Done when concurrent edits are skipped and the result
separates local application from peer convergence; the current Import remains a
version-merge operation, not a guarded restore wizard.

### 15. Automatic watch reconnect — **M**, recommended
Add a client helper with cancellation, bounded exponential backoff with jitter,
and tracking of the last fully applied cursor. On `resnapshot_required`, replace
the baseline explicitly and report the reset. Done when disconnect/replay tests
show no silent gaps and retries stop immediately when cancelled. Keep the current
manual `WatchFrom` API available.

### 16. Inspectable restore jobs — **M**, recommended
Offer progress, a per-key result view, cancellation between keys, and exportable
results for restores larger than one selected batch. A cancelled operation must
report already-applied writes; it must not promise rollback. Keep jobs bound to
the source profile and reject expired previews. Saved review files may contain
values, so make their storage/export explicit.

### 17. Journal inspection and recovery tool — **M**, recommended
Provide read-only checkpoint/WAL validation, checksum/offset diagnostics and a
list of intact records. An explicit recovery action should write a separate
candidate state and preserve originals. Done when missing checkpoints and corrupt
complete records produce actionable errors, with no claim that delta-only state
can reconstruct omitted checkpoint values.

### 18. Stable key pagination and indexed search — **M**
Add cursor-based listing with a defined snapshot/revision lifetime and expiry
response, plus a key index to avoid sorting the full store for each page. Keep
full values on demand. Done when concurrent inserts/deletes cannot silently skip
or repeat keys within a valid snapshot and retained snapshots have byte/time caps.
The current offset pages remain live views.

### 19. Replication acknowledgement options — **L**, design first
Allow callers to request explicit durable acknowledgement from a stated number
of replicas, reporting which peers acknowledged and what a timeout means. Define
membership, retry/idempotency and version semantics before implementation. An
acknowledged write is still not consensus, linearizability or exclusive ownership.
Do not overload the existing local `-durable` guarantee.

### 20. Per-peer repair scheduling and transfer budgets — **M**, recommended
Add fair per-peer queues, byte-rate limits and cancellation/timeouts, with queue
age, deferred bytes and throughput diagnostics. Extend budgeting to state sync
where needed. Done when a slow peer cannot starve other peers and heartbeat
latency remains bounded in repeatable constrained-bandwidth tests.

### 21. Watch replay across restarts — **L**, optional
Persist an event log with stable cursor epochs, prefix scope and retention caps.
Design it separately from the state WAL, which stores coalesced deltas rather than
every event. Done when checkpoint/rotation/restart tests preserve resume semantics
and expired cursors still trigger an explicit resnapshot.

### 22. Disk-backed storage — **L**, scale-dependent
Move key/value ownership out of the always-resident map when measured datasets
justify it. Preserve versions, tombstones, expiry and guarded writes; define
migration and rollback paths. Benchmark memory use and latency against the
current full-state checkpoint path before selecting a storage engine.

## Release-readiness work carried forward

- Fault-injection and physical power-loss testing for journal/checkpoint fsync
  behavior on supported filesystems; automated unit tests do not establish this.
- Sustained large-store, network-partition and slow-peer load runs with measured
  memory, convergence time and heartbeat latency.
- Android device walkthroughs for durability settings, profile switches,
  import/export and future restore UI, including rotation and process recreation.

These are validation tasks, not unimplemented product guarantees.

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
