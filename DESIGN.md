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
│   └── discovery/discovery.go       -bootstrap/-node/-advertise/-http/-doctor → Controller
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
│   ├── metrics/metrics.go           counters, per-peer traffic, gauges, Prometheus
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
│       ├── node/consistency.go      fingerprint exchange: are the replicas equal?
│       ├── node/diagnose.go         assembles the diagnostics snapshot
│       ├── node/api.go              public operations (Set/CAS/Import/…)
│       ├── topology/topology.go     the cluster graph, from gossiped peer lists
│       ├── diagnostics/diagnostics.go  counters → causes, as pure rules
│       ├── httpapi/httpapi.go       REST gateway (token, TLS, read-only)
│       ├── view/view.go             keys / details / nodes / feed / input
│       └── controller/controller.go TUI glue implementing node.Events
│
├── android/                         Kotlin port: node + bootstrap + Compose UI
├── electron/                        JS port: node + bootstrap + desktop UI
├── e2e/                             containerised cluster + the suite that drives it
├── scripts/                         e2e.sh, render-chart.sh, shell helpers
├── packaging/                       systemd units, /etc/default files, layout.sh
├── DEBIAN/                          Debian packaging metadata + maintainer scripts
├── .github/workflows/ci.yml         Go, cross-build, desktop, Android, e2e
├── Makefile                         cross-build + deb + android + electron targets
├── rezo_agwe.drawio                 the README's picture; `make chart` exports it
├── README.md
├── ROADMAP.md                       the prioritized backlog
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
| 11   | `Fingerprint`        | "summarise your whole store"                     |
| 12   | `FingerprintReply`   | bucket digests over the whole keyspace           |
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
  store larger than one digest is still covered completely — **one cursor
  per peer**, since a shared one divided the ranges among whichever peers
  the random target happened to pick, so covering the whole store against
  any single peer took as many wraps as there were peers.

  The digest is never wider than the repair budget (`DigestBatch` is
  clamped to `min(MaxPush, MaxPull)`). It used to be twice it: a 256-key
  digest, a 128-entry repair cap, and a cursor that advanced by the whole
  digest — so the second half of every badly-diverged range was stranded
  until the cursor wrapped the entire keyspace. On a large store that is
  not a delay, it is a divergence that outlives the process.
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
(temp file + rename), gated on a monotonically increasing generation so a
slow, out-of-order write can never regress the on-disk copy.

Writes are **coalesced**: a mutation marks the state dirty and a flush loop
rewrites the file at most once every 250 ms, with a synchronous flush on a
clean shutdown. It used to be synchronous on every mutation, which meant a
single `PUT` serialised the entire store — and a 128-entry anti-entropy
repair serialised it 128 times, once per applied entry. The trade is up to
250 ms of unflushed work on a `kill -9`; a clean stop loses nothing, and
anything a hard kill does lose is what anti-entropy re-fetches from a peer.
The file is compact rather than indented for the same reason: it is read by
the node, not by a person.

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
  instead of the source — §3.2's message kinds, §8's routes, §15's flags, the
  README's chat commands and hotkeys — still describe the code. Prose rots
  quietly, and this repository has watched it happen. It works: the three routes
  and two message kinds added for §12–§14 all failed this suite before they were
  documented.
* The picture is checked too, which is newer and was overdue: `rezo_agwe.png` is
  exported from `rezo_agwe.drawio` by `make chart`, and the suite asserts the
  drawing names every message kind, makes the claims about the protocol that are
  the reason for having it, and makes none of the ones it outgrew. An image is
  the one document that rots invisibly — the previous export said "protobuf",
  "no auth" and "no anti-entropy" for four releases after all three stopped
  being true, because re-exporting it meant opening a GUI.
* `.github/workflows/ci.yml` runs five jobs on every push: the Go suite under
  the race detector with `gofmt` and `go vet`, every cross-build target and
  every `.deb`, the desktop suite, the Android unit tests and APK, and the
  containerised end-to-end run in a job of its own. Four implementations of one
  protocol went a long time with nothing running any of it, and the two worst
  bugs the project has had — a Go nil slice the Kotlin decoder refused, and a
  UDP send Android silently dropped — were both drift between ports, found by
  hand, months apart.
* The desktop port carries the same vectors, plus a multi-node suite of its own:
  a cluster runs inside the test process over an in-memory network, so the
  partition and packet-loss cases are deterministic there too. Its interop test
  (opt-in via `REZOAGWE_GO_INTEROP`) builds the Go binaries itself.

---

## 8. HTTP gateway

`-http :8080` exposes the store, which is what makes it scriptable — and
what makes a cluster testable end to end without a terminal.

The gateway is a full read/write control surface on a cluster that
authenticates every packet on the wire, so it has its own guards:
`-http-token` requires a bearer token on **every** route (there is no
exemption for `/health` or `/metrics` — an unauthenticated route is one
somebody eventually hangs something else off), `-http-tls-cert` and
`-http-tls-key` serve HTTPS, and `-http-readonly` refuses every mutating
method. Without a token, anything that can reach the port can rewrite the
whole cluster through `POST /import?mode=seed`, so the startup line says so
out loud.

`GET /kv/{key}` honours `If-None-Match` against the same version it puts in
the `ETag`, so a poller asks "has this changed?" instead of re-fetching a
value it holds.

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
| `GET`    | `/metrics`       | Prometheus exposition, counters and gauges         |
| `GET`    | `/topology`      | the cluster graph (§12)                            |
| `GET`    | `/diagnostics`   | health findings; `?format=text` for a report (§13) |
| `GET`    | `/consistency`   | asks every peer what it holds; `409` if they differ (§14) |

---

## 9. Android app

`android/` is a Kotlin port of both roles, so a phone can be a peer, the
rendezvous service, or both at once. It shares no code with the Go
implementation — only the protocol — so the wire rules in §3 and §5 are
duplicated deliberately and pinned by parity tests (§7).

* `proto/` — the frame codec and message bodies, field-for-field with the
  Go structs.
* `core/KvStore.kt` — the same versioned store: LWW, CAS, TTL, digests,
  reconciliation, history, tombstone GC, store limits, and the store
  fingerprint (§14), whose digests are pinned to Go's byte for byte.
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
* `core/Persistence.kt` — the same atomic, generation-guarded state file, and
  `core/Runtime.kt`, the process-wide holder both roles and the settings live
  in, since a foreground service and an Activity have to reach one node.
* `net/Transport.kt` — datagrams and streams on one socket. Android refuses a
  socket write on the main thread and the engine counts the refusal rather than
  raising it, so every UI call that reaches this is wrapped in
  `launch(Dispatchers.IO)` at the call site and `UiThreadingTest` fails the
  build if one is not — see the end of this section for what that cost once.
* `service/NodeService.kt` — a foreground service: a gossip node that only
  runs while its screen is open is not participating in a cluster, since
  peers evict it seconds after the phone sleeps.
* `ui/` — the Compose screens, one per tab, plus `Layout.kt`, which is where
  the window-size tiers below are decided.

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
  reconciliation, history, tombstone GC, and the store fingerprint (§14), whose
  digests are pinned to Go's byte for byte.
* `src/core/node-engine.js` — membership, replication, chat, anti-entropy and
  stream state sync, exposed to the UI as events rather than polling.
* `src/core/bootstrap-server.js` — the rendezvous role.
* `src/core/topology.js` — the cluster graph, derived from the peer lists gossip
  already carries, with the layout computed once so a report and a drawing
  cannot disagree about where a node sits.
* `src/core/diagnostics.js` — the same counters-to-causes rules as the Go
  package (§13), written as pure functions over a snapshot for the same reason,
  and `src/core/metrics.js` behind them.
* `src/core/persistence.js`, `src/core/settings.js` and `src/core/runtime.js` —
  the state file, what the user can configure (and what needs a restart to take
  effect), and the holder that owns both roles for the process.
* `src/net/udp.js` and `src/net/framing.js` — datagrams and length-prefixed
  streams on one socket; `src/net/mem.js` — an in-process network with loss,
  delay and partitions, which is what lets the test suite run a whole cluster in
  one process and assert convergence rather than hope for it.
* `src/main.js` and `src/preload.js` — the window, the menu and the IPC, and
  the one explicit bridge that is the whole renderer-visible API.
* `renderer/` — the UI: no build step, no framework, a strict CSP, and a
  sandboxed renderer that reaches the engine only through that bridge.

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
| Data model       | Every value is an LWW string, so concurrent writes lose data by design | Typed values: PN-counters, OR-sets       |
| Liveness         | Heartbeat is best-effort; no quorum membership view                | Acked ping with a suspicion phase             |
| Security         | The frame is authenticated but the body is plaintext, and one psk means any member can impersonate any other | AEAD payloads; per-node keys |
| Bootstrap        | Seeds are static                                                   | mDNS / DNS-SD discovery on a LAN              |
| State sync       | Pulls from one random peer, so a joiner inherits that peer's gaps  | Pull from several and merge                   |
| Anti-entropy     | Digest is per-key, so a huge store still costs many rounds even now that each one repairs everything it covers | Merkle tree over key ranges |
| Persistence      | A `kill -9` can lose up to 250 ms of writes, and the whole store is rewritten per flush | Append-only log + periodic snapshot |
| Store limits     | `-max-value-bytes` / `-max-keys` refuse a peer's update, which is a deliberate divergence nothing on the wire reports | Advertise limits so peers stop sending |
| Consistency check | Reports *that* two replicas differ and in which bucket, not which key | A per-bucket key listing on request         |
| Tombstones       | GC is age-based and off by default                                 | Track cluster-wide acknowledgement            |
| Chat             | No history beyond the ring, no attachments                         | Paged history                                 |
| Watch            | Every consumer polls, though the engine knows exactly when a key changed | SSE on `/kv?watch=`                     |
| Android          | Runs on hardware; the battery cost of the 10 s gossip tick is still unmeasured | Measure it over a night |
| Store limits UI  | `maxValueBytes` / `maxKeys` are constructor options in the Kotlin and JavaScript engines, but neither Settings screen exposes them; only the Go node has flags | Add the two fields to both settings screens |
| Desktop          | Closing the window stops the node on Linux and Windows: no tray icon | Tray icon + close-to-tray                     |

For a prioritized version of this list — with effort estimates and what has
already shipped — see [ROADMAP.md](ROADMAP.md).

---

## 12. Topology

`pkg/discovery/topology` assembles the cluster graph from state the node
already holds: its own peer table, and the peer lists its peers have
gossiped (`Model.RecordPeerView`). No extra protocol — the picture is
exactly as complete as gossip has made it, which is itself the diagnostic.

Two distinctions carry all the value:

* **Role** — `self`, `direct` (a peer we exchange packets with) or
  `indirect` (a node only ever mentioned by someone else).
* **Link kind** — `direct` (touches this node, so observed first-hand),
  `mutual` (both ends claim it) or `observed` (one end only). A gossip
  packet carries the sender's own peer list, so a link only one end reports
  is a link one end cannot see. Telling that apart from an agreed one is
  what makes a half-open cluster visible instead of looking healthy.

`Components` groups nodes reachable through known links. More than one
group is a partition: each half converges internally and diverges from the
other, and nothing in the KV protocol reports that. Placement is
deterministic — self in the middle, peers on the first shell, hearsay
further out, ordered by sorted address — so the drawing does not reshuffle
between refreshes.

The Android and desktop ports have had this since they were written. The Go
node, which is the one most likely to be running headless on a server,
could only list its peers; a list cannot show a partition.

## 13. Diagnostics

`pkg/discovery/diagnostics` turns the counters into the short list of
things actually wrong. Every rule is a claim about the protocol — "auth
failures mean a key mismatch", "one-way traffic is a firewall, not loss" —
and a claim like that is worth a test, which is why `Checks` is a pure
function of a `Snapshot` rather than something the engine does to itself.
`node.Diagnostics()` assembles the snapshot; the package never reaches into
a live node.

What it needed that the engine did not have:

* **Per-peer traffic.** A global counter cannot tell a quiet peer from an
  unreachable one. `metrics` now counts packets and bytes per address in
  both directions, plus per-address send errors and authentication
  rejections, and keeps the last send error whole — "12 send errors" is not
  actionable, "no route to 10.0.0.4:3137" is.
* **The source address.** `handlePacket` takes the address the transport
  saw, not just the body. A node whose advertised address differs from
  where its packets come from is a NAT or a wrong `-advertise`, and it is
  invisible unless unknown sources are recorded (`Node.Strangers`).
* **A key fingerprint.** Two nodes that cannot talk usually differ in the
  psk or the cluster name, and neither is printable. `Codec.KeyFingerprint`
  is comparable at a glance and gives nothing away.

Findings are available three ways: `GET /diagnostics` (JSON, or
`?format=text` for something pasteable), `D` in the TUI, and
`rezoagwe-discovery -doctor`, which starts a node, waits out two heartbeats
and prints the report. The waiting matters: a node seconds old has not
failed to do anything yet, and flagging it turns the first moments of every
start into a red screen.

## 14. Consistency checking

Anti-entropy repairs divergence but never reports it, so a cluster can sit
split — or a peer can quietly refuse everything it is sent — for as long as
nobody looks. `KindFingerprint` / `KindFingerprintReply` (§3.2) is the
looking.

A reply summarises the whole store as 16 bucket digests. A key's bucket
comes from the hash of its **name alone**; what is folded into that bucket
is the hash of the whole entry (key, version, deleted). Both halves are
load-bearing:

* Bucketing on the name keeps a key in one bucket however its value
  changes, so a differing bucket names a stable region of the keyspace —
  the only thing that makes "they differ in bucket 7" more useful than
  "they differ". Bucketing on the whole entry, as the first implementation
  did, relocates a key on every write: one stale value then lights up two
  buckets and neither corresponds to anything you could go and inspect.
* Folding with XOR keeps a bucket independent of the order entries arrived
  in. Two converged replicas that took the same writes by different routes
  must produce identical buckets or the check is worthless.

Tombstones are included: two replicas that disagree about whether a key is
deleted have diverged just as much as two that disagree about its value.

The digests are byte-for-byte identical across the Go and JavaScript
stores, pinned by fixed vectors in both suites — an implementation that
folded differently would report two converged replicas as divergent, which
is the loudest possible false alarm from the one feature whose entire job
is to be believed.

`Node.CheckConsistency` asks every peer in parallel and compares. It goes
over **streams, not datagrams**: a dropped answer would read as a peer that
disagrees, which is exactly the wrong conclusion to draw from packet loss.
A peer that does not answer is reported as unreachable and counted
separately — a silent peer says nothing about whether it agrees.

`GET /consistency` answers `200` when every peer that replied agreed and
`409` when any did not, so a script can branch on the status alone. `v` in
the TUI runs the same check off the event loop, since dialling an
unreachable peer must never freeze the terminal.

All three implementations answer, over the stream path and the datagram
one, and all three initiate: `Node.CheckConsistency` in Go,
`checkConsistency()` in the Kotlin and JavaScript engines. Their bucket
digests are pinned to each other by fixed vectors in all three suites, and
the pairing was exercised on hardware — a Kotlin fold and a Go fold agreeing
byte for byte across a Wi-Fi LAN.

## 15. Command-line reference

Every flag both binaries accept. The README shows the ones a first run needs;
this is the whole surface, because a flag documented nowhere is a flag nobody
finds — `-timeout` and `-version` existed for several releases with no mention
in any document, and a test now fails if that happens again (§7).

### `rezoagwe-discovery`

| Flag                | Default              | What it does                                            |
|---------------------|----------------------|---------------------------------------------------------|
| `-bootstrap`        | `:9999`              | rendezvous address, or a comma-separated list of seeds  |
| `-node`             | `:3137`              | address the sockets bind                                |
| `-advertise`        | `-node`              | address peers are told to use — set this on a LAN (§13) |
| `-nick`             | `anon`               | chat nickname                                           |
| `-data`             | `<config dir>/rezoagwe/<node>.json` | state file; `-` disables persistence     |
| `-psk`              | none                 | pre-shared key authenticating every packet (§3.1)       |
| `-cluster`          | `rezoagwe`           | cluster name; nodes only talk to their own              |
| `-http`             | off                  | serve the HTTP gateway here, e.g. `:8080` (§8)          |
| `-http-token`       | none                 | require this bearer token on **every** gateway route    |
| `-http-tls-cert`    | none                 | serve the gateway over TLS with this certificate        |
| `-http-tls-key`     | none                 | private key for `-http-tls-cert`                        |
| `-http-readonly`    | `false`              | refuse every mutating gateway request                   |
| `-max-value-bytes`  | `0` (unbounded)      | refuse values longer than this, from a peer or a writer |
| `-max-keys`         | `0` (unbounded)      | refuse writes that would exceed this many live keys     |
| `-tombstone-ttl`    | `0` (GC off)         | reclaim tombstones older than this (§5.5)               |
| `-doctor`           | `false`              | join, wait out two heartbeats, print diagnostics, exit  |
| `-headless`         | `false`              | run without the TUI; pair with `-http` to drive it      |
| `-debug`            | `false`              | debug logging — needs `-logfile`                        |
| `-logfile`          | none                 | write logs here instead of discarding them              |
| `-version`          | `false`              | print the version and exit                              |

### `rezoagwe-bootstrap`

| Flag         | Default                                        | What it does                                       |
|--------------|------------------------------------------------|----------------------------------------------------|
| `-port`      | `9999`                                         | listen port, UDP and TCP                           |
| `-timeout`   | `30s`                                          | drop a node that has not registered within this    |
| `-data`      | `<config dir>/rezoagwe/bootstrap-<port>.json`  | roster file; `-` disables persistence              |
| `-psk`       | none                                           | pre-shared key; must match the nodes               |
| `-cluster`   | `rezoagwe`                                     | cluster name; must match the nodes                 |
| `-headless`  | `false`                                        | run without the TUI                                |
| `-debug`     | `false`                                        | debug logging — needs `-logfile`                   |
| `-logfile`   | none                                           | write logs here instead of discarding them         |
| `-version`   | `false`                                        | print the version and exit                         |

`-timeout` is how long the roster keeps a node that has stopped registering;
the sweep that applies it runs every `-timeout`/2. Nodes REGISTER on their own
heartbeat (5 s, §4.2) rather than at a rate derived from this, so the default
30 s tolerates several lost REGISTERs before a live node is dropped. Lowering it
below the heartbeat evicts every node in the cluster on a regular cycle.

`-psk` and `-cluster` have to agree on every process in a cluster, bootstrap
included — they are both folded into the framing key (§3.1), so a mismatch is
not a rejected login but a packet that never authenticates. `-doctor` prints
the key fingerprint precisely so the two ends can be compared without either
printing the key.

---

## 16. Why "rezoagwe"?

[Agwé](https://en.wikipedia.org/wiki/Agw%C3%A9) is the Haitian Vodou
lwa of the sea — a fitting name for a protocol whose packets drift
through the network with no guarantee of arrival. *Rezo* is Haitian
Creole for "network". The PoC is the small boat; production hardening
would be the larger ship.
