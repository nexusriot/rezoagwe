# Rezoagwe — Design & Architecture

This document describes how `rezoagwe` is put together: what each package
does, how the binaries cooperate at runtime, and the rationale behind
the main design choices. It is aimed at contributors and at users who
want to understand the tool deeply enough to extend it.

For end-user documentation see [README.md](README.md).

---

## 1. High-level overview

`rezoagwe` is a **distributed key-value store with an embedded chat**. It
consists of two binaries plus two applications:

* **bootstrap** — a rendezvous service that helps new nodes find the
  existing cluster. It only knows about the *set of node addresses*; it
  never sees the KV data or the chat.
* **discovery** — a full peer node. It owns a local copy of the KV store,
  participates in chat, gossips peer membership, reconciles divergence
  with anti-entropy, and replicates writes to every peer it knows about.
* **android/** — a Kotlin port of both roles with a Compose UI (§9).
* **electron/** — a JavaScript port of both roles with a desktop UI (§10).

```
        ┌───────────┐   node.Events    ┌──────────────┐
        │ Controller│ ◄─────────────── │  node.Node   │ ── transport ─► peers
        │   (TUI)   │ ───────────────► │   (engine)   │                 bootstrap
        └───────────┘   Set/Delete/…   └──────┬───────┘
        ┌───────────┐                         │
        │  httpapi  │ ────────────────────────┘
        └───────────┘
```

The engine (`pkg/discovery/node`) owns the protocol: membership,
replication, chat, anti-entropy and state sync. It knows nothing about
tview. The TUI, the HTTP gateway and the multi-node tests are three front
ends onto the same engine — which is what makes convergence testable
without driving a terminal.

Inside the engine, the **model** owns the KV store, the peer set, the chat
ring and the node's identity; the **view** owns the tview widgets and knows
nothing about the network; the **controller** wires keyboard events to
engine calls and repaints on engine events.

---

## 2. Repository layout

```
rezoagwe/
├── cmd/
│   ├── bootstrap/bootstrap.go       -port/-psk/-cluster/-headless → Controller
│   └── discovery/discovery.go       -bootstrap/-node/-http/-headless → Controller
│
├── pkg/
│   ├── proto/
│   │   ├── wire.go                  message kinds and bodies (all JSON)
│   │   └── codec.go                 framing, HMAC authentication, replay guard
│   │
│   ├── transport/
│   │   ├── transport.go             Transport interface + stream framing
│   │   ├── udp.go                   one shared socket for datagrams + TCP streams
│   │   └── mem.go                   in-process network with loss/dup/delay/partitions
│   │
│   ├── metrics/metrics.go           counters + Prometheus exposition
│   │
│   ├── bootstrap/
│   │   ├── model/model.go           roster + persistence + stale eviction
│   │   ├── server/server.go         REGISTER/DISCOVER over datagrams and streams
│   │   ├── view/view.go             nodes list + status bar
│   │   └── controller/controller.go TUI over the server
│   │
│   └── discovery/
│       ├── model/kvstore.go         versioned store: LWW, CAS, TTL, digest, GC
│       ├── model/model.go           peers, nicks, ids, chat ring, identity
│       ├── model/persist.go         atomic, generation-guarded state file
│       ├── node/node.go             engine lifecycle and loops
│       ├── node/handlers.go         packet dispatch
│       ├── node/antientropy.go      digest exchange and repair
│       ├── node/statesync.go        stream state sync + bootstrap handshake
│       ├── node/chat.go             chat, direct messages, slash commands
│       ├── node/api.go              public operations (Set/CAS/Import/…)
│       ├── httpapi/httpapi.go       REST gateway
│       ├── view/view.go             keys / details / nodes / feed / input
│       └── controller/controller.go TUI glue implementing node.Events
│
├── android/                         Kotlin port: node + bootstrap + Compose UI
├── electron/                        JS port: node + bootstrap + desktop UI
├── e2e/                             containerised cluster + the suite that drives it
├── scripts/                         e2e.sh and its shell helpers
├── DEBIAN/                          Debian packaging metadata
├── Makefile                         cross-build + deb + android + electron targets
├── README.md
└── DESIGN.md                        this document
```

---

## 3. Wire protocol (v2)

One protocol, one framing, for every packet — node-to-node *and*
node-to-bootstrap. Wire v1 had bootstrap speaking raw protobuf on its own
unauthenticated path, which meant two codecs, two framings, and no way to
authenticate a REGISTER. Protobuf is gone; every body is JSON.

### 3.1 Frame

```
byte[0]      MessageKind
byte[1..8]   nonce
byte[9..16]  unix timestamp, big endian
byte[17..48] HMAC-SHA256 tag
byte[49..]   body (JSON)

tag = HMAC-SHA256(key, kind || nonce || timestamp || body)
key = HMAC-SHA256(psk, "rezoagwe/wire/v2" || 0x00 || cluster)
```

Every packet is authenticated: there is no unauthenticated path to keep
tested. With no `-psk` the key is derived from the cluster name alone,
which still keeps two clusters on one LAN apart but — the name being
public — provides no secrecy.

A frame is rejected, and counted, when the tag does not verify, when the
timestamp sits outside ±30 s, or when the nonce has been seen before. The
replay cache is pruned past twice the skew, since such a nonce can never
be accepted again on timestamp grounds anyway.

Streams (TCP) carry exactly the same frames with a 4-byte length prefix,
so a payload larger than a datagram needs no separate chunking protocol.

### 3.2 Message kinds

| Kind | Name                 | Purpose                                          |
|------|----------------------|--------------------------------------------------|
| 0    | `KV`                 | versioned set / delete (LWW)                     |
| 1    | `Chat`               | chat broadcast                                   |
| 2    | `StateRequest`       | "send me your store"                             |
| 3    | `StateResponse`      | versioned entries + recent chat                  |
| 4    | `PeerGossip`         | "here are the peers I know"                      |
| 5    | `Hello`              | "hi, I am `addr`, id `uuid`"                     |
| 6    | `Goodbye`            | "I am shutting down" (clean leave)               |
| 7    | `Digest`             | anti-entropy: what I hold for a key range        |
| 8    | `PullRequest`        | anti-entropy: "(re)send me these keys"           |
| 9    | `KVBatch`            | several updates in one packet (repair traffic)   |
| 10   | `DirectMessage`      | chat to one peer only                            |
| 20   | `BootstrapRegister`  | "I exist at `addr`"                              |
| 21   | `BootstrapDiscover`  | "who is in the cluster?"                         |
| 22   | `BootstrapRoster`    | the roster, with nicknames                       |

---

## 4. Cluster lifecycle

### 4.1 Discovery node startup

`./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice`:

1. **Start serving**: bind the datagram socket and the stream listener,
   start the gossip, heartbeat, eviction and sweep loops.
2. **Join** (on its own goroutine, since it dials hosts that may be down):
   REGISTER with every seed, then DISCOVER the roster — over a stream when
   possible, falling back to datagrams whose reply arrives asynchronously.
3. **Hello-storm**: greet every peer learned, so existing nodes learn the
   new node back without bootstrap having to push anything.
4. **State sync**: ask one random peer for its store. Over a stream this is
   the whole store; the datagram fallback is bounded to what fits. The
   joiner *merges* by version rather than overwriting, so a local edit made
   before the reply arrives is not clobbered. Recent chat history rides
   along — minus direct messages, which are never handed to a joiner.
5. **Run** until `Ctrl+Q` (or a signal, under `-headless`).

Splitting "start serving" from "join" is deliberate: it is what lets a test
wire a cluster by hand, and it means an unreachable bootstrap can never
stall startup.

### 4.2 Steady state

* **KV writes** stamp a per-key version (Lamport counter + this node's
  persistent id) and broadcast a `KVUpdate`. Peers apply it only if the
  version is newer, so concurrent writes converge everywhere; a delete is a
  versioned tombstone, so a stale set cannot resurrect the key.
* **Anti-entropy** rides the gossip tick. The node advertises a `Digest` of
  a contiguous slice of its sorted keyspace to one random peer: versions,
  not values, so a round is cheap. The receiver answers with a `KVBatch`
  of anything it holds newer (or that the sender is missing entirely) and a
  `PullRequest` for anything it lacks. A cursor walks the keyspace so a
  store larger than one digest is still covered completely.
* **Chat** is a `ChatMessage` broadcast; entries are stored structurally
  (sender, nick, text, kind) rather than pre-rendered, so the TUI, the HTTP
  gateway and the Android app each format them their own way.
* **Peer membership**: PeerGossip every 10 s; Hello on any newly learned
  address; a peer with no traffic for 15 s is evicted locally. `Hello` and
  `PeerGossip` carry the sender's nickname *and* node id, which is what lets
  a version be attributed to a name rather than a UUID.
* **TTL sweep** turns expired entries into tombstones, keeping each entry's
  existing version (§5.2), and — when `-tombstone-ttl` is set — reclaims
  tombstones older than that.

### 4.3 Failure model

* **Bootstrap dies after startup**: the cluster keeps working — bootstrap
  is only used at join. Nodes REGISTER every 5 s so bootstrap forgets dead
  clients, and its roster is persisted, so a restart keeps serving the
  cluster it knew instead of making everyone wait out a re-registration.
* **Bootstrap unreachable at join**: nothing blocks. The datagram reply
  arrives asynchronously or not at all; while a node knows no peers, the
  heartbeat loop keeps re-DISCOVERing, so it joins once bootstrap appears.
* **A node dies**: peers evict it after 15 s of silence.
* **A node exits cleanly**: it broadcasts `Goodbye` first, so peers drop it
  and post the "left" line at once.
* **Packet loss**: a lost write is repaired by the next digest exchange
  with any peer that has it — this is the guarantee anti-entropy adds, and
  it is what the lossy-transport tests assert. Lost gossip is resent next
  tick. A lost state-sync reply leaves the joiner on its persisted store
  until anti-entropy fills it in.
* **A hostile packet**: dropped at the codec and counted as an
  authentication failure, before any handler sees it.

### 4.4 Persistence

Each node persists identity, Lamport clock, every entry (tombstones
included) and the chat ring to a JSON file (`-data`, default
`<config dir>/rezoagwe/<node>.json`). The file is rewritten atomically
(temp file + rename) after every change, gated on a monotonically
increasing generation so a slow, out-of-order write can never regress the
on-disk copy.

Identity is persisted rather than derived from the listen address: a node
that moves to a different port is still the same writer, and its version
tiebreak has to stay stable or its old and new writes sort oddly against
each other.

The file also reads the wire-v1 format, where chat history was a list of
pre-rendered strings — otherwise upgrading a node would fail to parse its
own data file and discard the KV store along with the chat.

---

## 5. Replication rules

These are protocol, not implementation detail: the Go, Kotlin and JavaScript
stores must agree on every one of them or two replicas silently disagree —
and disagree quietly, since nothing on the wire reports a divergence.

### 5.1 Last-write-wins

A version is `{counter, node}`. Higher counter wins; ties break on node id.
Equal is *not* newer, so re-delivery is idempotent. Applying a remote
version advances the local Lamport clock past it.

### 5.2 Expiry keeps the version

TTL expiry converts an entry into a tombstone **without bumping the
Lamport clock**. Bumping it would let a sweep outrank a concurrent
legitimate write to the same key, and every replica sweeps independently.
Since expiry is deterministic, replicas reach the same state without
exchanging a message.

### 5.3 Compare-and-swap

A write may carry an expected version; it lands only if the current
version matches. A zero expected version means "the key must be absent",
where missing, tombstoned and expired all count as absent. This is the
primitive a lock or a leader election is built on. CAS is evaluated
against *local* state — this is a PoC, not a consensus system.

### 5.4 Digest ranges

A digest covers `(lo, hi)`, both bounds exclusive. The range is what lets
the receiver tell "the sender has nothing for this key" from "that key was
outside this batch" — without it, keys the sender is missing entirely
would never be repaired.

### 5.5 Tombstone GC

GC is the one operation that can resurrect a key: a peer that never saw the
delete and still holds the value will push it back once the tombstone is
gone. Anti-entropy makes that unlikely rather than impossible, so GC is off
by default and its age must exceed the longest partition expected to heal.

---

## 6. Concurrency model

* `KVStore` is guarded by a `sync.RWMutex`; every mutation notifies a
  change callback *after* releasing the lock, so persistence I/O never
  blocks a writer.
* Peers, nicknames and id mappings live in `sync.Map`s.
* Each inbound datagram is handled on the transport's read goroutine;
  each inbound stream gets its own goroutine.
* The TUI never blocks the engine: `node.Events` callbacks do a
  non-blocking send on a capacity-1 channel, and a single refresh goroutine
  drains those and calls `QueueUpdateDraw`. A direct call would deadlock —
  `QueueUpdateDraw` waits for the tview event loop, and `Ctrl+Q` runs the
  engine shutdown *on* that loop.

---

## 7. Testing

* `pkg/transport` provides an in-process network with configurable loss,
  duplication, delay and partitions. Convergence is asserted, not hoped
  for: a whole cluster reconciles under 30 % packet loss, a dropped write
  is repaired by a digest exchange, and a delete is not resurrected by the
  peer that still holds the value.
* The TUI is driven headlessly through a `tcell` simulation screen.
* `make e2e` is the other half of that: the in-process tests cannot see anything
  that goes wrong *between* processes, so a rendezvous, three peers, a late
  joiner, a leaver, a restarting node and two strangers run as containers on a
  private network while a Go suite drives them through the HTTP gateway. It
  asserts what only exists between nodes — a write reaching a node it was never
  sent to, a stale compare-and-swap refused everywhere rather than locally, a
  departure *announced* rather than merely timed out.
* The Android port carries the Go implementation's own vectors: a frame
  produced by the Go codec is decoded by the Kotlin one, and the derived
  keys are compared against fixed values. A live interop test (opt-in via
  `REZOAGWE_GO_BOOTSTRAP`) joins a running Go cluster and replicates
  through it in both directions.
* The documentation is checked too: `electron/test/docs.test.js` asserts that
  every command the docs offer exists, and that the tables a reader trusts
  instead of the source — §3.2's message kinds, §8's routes, the README's chat
  commands and hotkeys — still describe the code. Prose rots quietly, and this
  repository has watched it happen.
* The desktop port carries the same vectors, plus a multi-node suite of its own:
  a cluster runs inside the test process over an in-memory network, so the
  partition and packet-loss cases are deterministic there too. Its interop test
  (opt-in via `REZOAGWE_GO_INTEROP`) builds the Go binaries itself.

---

## 8. HTTP gateway

`-http :8080` exposes the store, which is what makes it scriptable — and
what makes a cluster testable end to end without a terminal:

| Method   | Path             | Notes                                              |
|----------|------------------|----------------------------------------------------|
| `GET`    | `/kv`            | listing; `?prefix=` filters                        |
| `GET`    | `/kv/{key}`      | value as text; `ETag` carries the version          |
| `PUT`    | `/kv/{key}`      | body is the value; `If-Match` makes it a CAS, `X-Rezoagwe-TTL` sets an expiry |
| `DELETE` | `/kv/{key}`      | `If-Match` supported                               |
| `GET`    | `/history/{key}` | recorded versions with their writers               |
| `GET`    | `/peers`         | known peers                                        |
| `GET`    | `/chat`          | chat log; `POST` sends (slash commands included)   |
| `GET`    | `/activity`      | replication activity feed                          |
| `GET`    | `/export`        | whole store, versions included                     |
| `POST`   | `/import`        | merge by version; `?mode=seed` re-stamps as local  |
| `GET`    | `/health`        | status                                             |
| `GET`    | `/metrics`       | Prometheus exposition                              |

---

## 9. Android app

`android/` is a Kotlin port of both roles, so a phone can be a peer, the
rendezvous service, or both at once. It shares no code with the Go
implementation — only the protocol — so the wire rules in §3 and §5 are
duplicated deliberately and pinned by parity tests (§7).

* `proto/` — the frame codec and message bodies, field-for-field with the
  Go structs.
* `core/KvStore.kt` — the same versioned store: LWW, CAS, TTL, digests,
  reconciliation, history, tombstone GC.
* `core/NodeEngine.kt` — membership, replication, chat, anti-entropy,
  stream state sync, exposed to Compose as `StateFlow`s.
* `core/BootstrapServer.kt` — the rendezvous role.
* `core/Topology.kt` — the cluster graph, derived from the peer lists gossip
  already carries: self/peer/heard-of-only roles, links one end claims against
  links both confirm, and the connected components that make a partition
  visible.
* `core/Diagnostics.kt` + `core/DeviceInfo.kt` — one snapshot of the node and
  the health checks over it, plus the phone's own networking and power state,
  since Doze is a cause of "the cluster forgot me" that no protocol counter
  can explain.
* `core/Metrics.kt` — counters, per-address traffic and sampled rates.
* `service/NodeService.kt` — a foreground service: a gossip node that only
  runs while its screen is open is not participating in a cluster, since
  peers evict it seconds after the phone sleeps.

The UI adapts to the window rather than the device: tabs under 600dp, a
navigation rail above it or in a short landscape window, and two captioned
panes at 880dp when there is also height for them.

Running it on hardware is what proved the port: see `android/README.md` for
what that verified, and for the main-thread send bug it found — every UDP
write issued from a Compose callback was being refused by Android and counted
rather than surfaced, so a UI write reached peers only on the next
anti-entropy round.

---

## 10. Desktop app

`electron/` is a JavaScript port of both roles, so a workstation can be a peer,
the rendezvous service, or both. Like the Android app it shares no code with the
Go implementation — only the protocol — so the wire rules in §3 and the
replication rules in §5 are duplicated deliberately and pinned by parity tests
(§7).

* `src/proto/` — the frame codec and message bodies, field-for-field with the Go
  structs. Go's `omitempty` and its habit of marshalling a nil slice as `null`
  are both handled in one place each, since either one silently drops a packet
  that has already authenticated.
* `src/core/kvstore.js` — the same versioned store: LWW, CAS, TTL, digests,
  reconciliation, history, tombstone GC.
* `src/core/node-engine.js` — membership, replication, chat, anti-entropy and
  stream state sync, exposed to the UI as events rather than polling.
* `src/core/bootstrap-server.js` — the rendezvous role.
* `src/core/topology.js` — the cluster graph, derived from the peer lists gossip
  already carries, with the layout computed once so a report and a drawing
  cannot disagree about where a node sits.
* `src/net/mem.js` — an in-process network with loss, delay and partitions,
  which is what lets the test suite run a whole cluster in one process and
  assert convergence rather than hope for it.
* `renderer/` — the UI: no build step, no framework, a strict CSP, and a
  sandboxed renderer that reaches the engine only through an explicit preload
  bridge.

Packaging has two paths on purpose. `make -C electron deb` uses
electron-builder, which wants `fpm` and can cross-build for arm64 and armhf;
`make -C electron deb-manual` lays an unpacked build out by hand and packs it
with `dpkg-deb`, which needs neither a network nor fpm. From the repository root
`make electron-deb` reaches the first of them. Both write the same package — `rezoagwe-desktop` under
`/opt/Rezoagwe`, separate from the `rezoagwe` package that carries the two Go
binaries — and a test asserts the two descriptions agree, because the failure
mode of a drift is a half-removed installation rather than a build error.

The engine half depends on nothing from Electron. That is what makes the
multi-node tests possible, and it means the same code could drive a headless
desktop node if one were ever wanted.

Two verification paths exist beyond the unit tests: `--selftest` boots the real
window and drives every screen (catching a preload broken by a sandbox change, a
CSP that blocks the scripts, a screen that throws before its first paint), and
`REZOAGWE_GO_INTEROP=1 npm run test:interop` builds the real Go binaries and
replicates through them in both directions.

---

## 11. Known limitations & next steps

| Area             | Limitation                                                        | Possible fix                                  |
|------------------|-------------------------------------------------------------------|-----------------------------------------------|
| Consistency      | Last-write-wins by wall-order, not consensus; CAS is checked locally | Raft/Paxos for a real linearizable store    |
| Liveness         | Heartbeat is best-effort; no quorum membership view                | Acked ping + quorum view                      |
| Security         | HMAC authenticates and separates clusters, but payloads are plaintext and the psk is shared symmetrically | Per-node keys, encryption, key rotation |
| Bootstrap        | Seeds are static                                                   | mDNS / DNS-SD discovery on a LAN              |
| State sync       | Pulls from one random peer                                         | Pull from a quorum                            |
| Anti-entropy     | Digest is per-key, so a huge store costs many rounds               | Merkle tree over key ranges                   |
| Tombstones       | GC is age-based and off by default                                 | Track cluster-wide acknowledgement            |
| Chat             | No history beyond the ring, no attachments                         | Paged history                                 |
| Android          | Runs on hardware; the battery cost of the 10 s gossip tick is unmeasured | Measure it over a night                       |
| Desktop          | Closing the window stops the node on Linux and Windows: no tray icon | Tray icon + close-to-tray                     |

For a prioritized version of this list — with effort estimates and what has
already shipped — see [ROADMAP.md](ROADMAP.md).

---

## 12. Why "rezoagwe"?

[Agwé](https://en.wikipedia.org/wiki/Agw%C3%A9) is the Haitian Vodou
lwa of the sea — a fitting name for a protocol whose packets drift
through the network with no guarantee of arrival. *Rezo* is Haitian
Creole for "network". The PoC is the small boat; production hardening
would be the larger ship.
