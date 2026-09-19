# Rezoagwe — Roadmap

Prioritized backlog. Items are numbered by **global priority** (1 = do next)
and grouped into thematic tiers. Effort is a rough T-shirt size: **S** ≈ a few
hours, **M** ≈ about a day, **L** ≈ multi-day.

This complements the *Known limitations & next steps* table in
[DESIGN.md](DESIGN.md#11-known-limitations--next-steps); each open item notes
the code it touches.

## Shipped

The repository is at **0.4.1** — the version the Makefile stamps into the Go
binaries and their `.deb`, the desktop client's `package.json`, and the Android
app's `versionName`; a test keeps the four in step.

0.4.1 is a documentation round: the prose was in good shape because a test suite
holds it there, and the two things that suite could not see — the picture, and
whatever the code grew that no document mentions — were both wrong.

0.4.0 before it was an observability, hardening and parity round: the Go node
caught up with what its own ports could already show, the gateway stopped being
an unauthenticated write surface, and the correctness fixes were carried into
the Kotlin and JavaScript ports — which had been carrying the same anti-entropy
defect all along.

Newest first.

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

### 3. Watch / subscribe — **S–M**
SSE on `/kv?watch=`, plus a change callback in the engine.
**Why:** every consumer polls, the TUI included, though the engine knows
exactly when a key changed. **Notes:** `ev.KVChanged()` already fires at the
right moment and carries nothing; widening it to `KVChanged(u pb.KVUpdate)` is
most of the work.

## Tier 2 — Reach

### 4. mDNS / DNS-SD discovery — **S–M**
Find peers on a LAN with no seed configured at all — the setup step most likely
to defeat a new user, and the one the Android app feels most.

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

### 7. Encrypted payloads — **M–L**
The frame is authenticated but the body is plaintext. Add AEAD — the nonce is
already there. **Notes:** the codec change is contained; the coordination is
not. It is a wire v3 bump across Go, Kotlin and JavaScript together, with a
decision about whether v2 peers are refused or tolerated during a rollout.
Sized for that, not for `codec.go`.

### 8. Per-node keys — **L**
One shared psk means any member can impersonate any other. Per-node keypairs
with signed Hellos would close that.

## Tier 4 — Scale

### 9. Append-only log + snapshot — **M**
Coalescing bounded how *often* the store is rewritten; it is still rewritten
whole each time. A log with a periodic snapshot makes a flush proportional to
what changed, and shrinks the 250 ms a hard kill can lose.

### 10. Merkle-tree digests — **M**
Replace the flat per-key digest with a tree over key ranges, so reconciliation
costs `O(log n)` rounds instead of walking the keyspace.
**Why:** a cluster with a million keys still converges slowly after a long
partition. **Notes:** deliberately below the data-model work now that the
budget clamp and per-peer cursors have fixed the convergence problem clusters
of this size actually hit; `KVStore.Digest`/`Reconcile` isolate the comparison,
so this swaps the summary type without touching the engine.

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
