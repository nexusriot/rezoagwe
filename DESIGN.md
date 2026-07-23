# Rezoagwe — Design & Architecture

This document describes how `rezoagwe` is put together: what each package
does, how the two binaries cooperate at runtime, and the rationale behind
the main design choices. It is aimed at contributors and at users who
want to understand the tool deeply enough to extend it.

For end-user documentation see [README.md](README.md).

---

## 1. High-level overview

`rezoagwe` is a PoC-grade **distributed key-value store with an embedded
chat**. It consists of two cooperating binaries:

* **bootstrap** — a single rendezvous service that helps new nodes find
  the existing cluster. It only knows about the *set of node addresses*;
  it never sees the KV data or the chat.
* **discovery** — a full peer node. It owns a local copy of the KV store,
  participates in chat, gossips peer membership and replicates writes to
  every other node it knows about. Each discovery node also runs a
  `tview`-based TUI.

```
                  ┌──────────────┐
   keyboard ──►   │  Controller  │   ◄── tview events
                  └──────┬───────┘
                         │
            ┌────────────┴────────────┐
            ▼                         ▼
       ┌──────────┐              ┌─────────┐
       │  Model   │ ── UDP ─►    │  View   │ ── tview/tcell ─► terminal
       └────┬─────┘              └─────────┘
            │
            ▼
       other discovery nodes  +  bootstrap (once, at startup)
```

Each discovery node follows a classic **MVC** decomposition:

* **Model** — owns the KV store (with a mutex), the peer set
  (`sync.Map`), per-peer last-seen timestamps and reported nicknames
  (both `sync.Map`s), the chat log, and the node's own identity (UUID,
  address, nickname).
* **View** — owns the `tview` widgets: keys list with a filter field,
  details pane, nodes list (each row labelled `addr — nick`), chat
  pane, chat input, and a one-line status bar at the bottom. Pure UI;
  it does not know anything about the network.
* **Controller** — wires keyboard events to model mutations, opens the
  UDP listener, dispatches incoming packets to handlers, and pushes
  state changes back into the view via `App.QueueUpdateDraw`.

The bootstrap binary uses the same MVC split, but its model is trivial:
just a `map[address]lastSeen` with a mutex and a stale-eviction loop.
Its TUI shows the nodes list (each row carries a "last seen N ago"
sub-line, flagged when a node is about to be evicted) plus a status
bar with the listening port, node count, stale timeout and uptime,
refreshed once per second.

---

## 2. Repository layout

```
rezoagwe/
├── cmd/
│   ├── bootstrap/bootstrap.go       entry point: -port flag → Controller
│   └── discovery/discovery.go       entry point: -bootstrap/-node/-nick → Controller
│
├── pkg/
│   ├── proto/
│   │   ├── rezoagwe.proto           protobuf source (BootstrapMessage, Payload)
│   │   ├── rezoagwe.pb.go           generated; only edited via protoc
│   │   └── wire.go                  1-byte-kind envelope + JSON message types
│   │
│   ├── bootstrap/
│   │   ├── model/model.go           known-nodes table + stale eviction
│   │   ├── view/view.go             nodes list + status bar
│   │   └── controller/controller.go UDP listener for DISCOVER/REGISTER
│   │
│   └── discovery/
│       ├── model/model.go           KV store, peer set, chat log, identity
│       ├── view/view.go             keys / details / nodes / chat / input
│       └── controller/controller.go UDP listener, handlers, gossip, UI glue
│
├── DEBIAN/                          Debian packaging metadata
├── build-deb.sh                     builds a .deb (amd64 by default)
├── build-deb-arm64.sh               builds an arm64 .deb (wrapper)
├── Makefile                         cross-build + deb targets
├── go.mod / go.sum
├── README.md
└── DESIGN.md                        this document
```

---

## 3. Wire protocol

Two protocols coexist:

### 3.1 Bootstrap protocol (node ↔ bootstrap)

Plain protobuf, no envelope. Defined in
[pkg/proto/rezoagwe.proto](pkg/proto/rezoagwe.proto):

```proto
message BootstrapMessage {
  BootstrapAction action = 1;   // DISCOVER | REGISTER
  Host host = 2;                // sender's UDP address
}
```

* **REGISTER**: discovery node tells bootstrap "I exist at `addr`".
  Bootstrap stamps `lastSeen = now()`. No response.
* **DISCOVER**: discovery node asks for the current node roster.
  Bootstrap replies with a comma-separated list of addresses (plain
  bytes, not protobuf — kept that way for simplicity).

Stale nodes (no REGISTER within `NodeTimeout`) are evicted by a
background goroutine on the bootstrap side.

A discovery node only talks to the bootstrap **at startup**: one
REGISTER + one DISCOVER. From then on, all peer-membership maintenance
happens between discovery nodes themselves.

### 3.2 Node-to-node protocol (discovery ↔ discovery)

To avoid regenerating `rezoagwe.pb.go` whenever a new control message
is added, every node-to-node UDP packet uses a tiny envelope defined in
[pkg/proto/wire.go](pkg/proto/wire.go):

```
byte[0]    = MessageKind
byte[1..]  = body
```

| Kind | Name           | Body encoding | Purpose                               |
|------|----------------|---------------|---------------------------------------|
| 0    | `KV`           | JSON `KVUpdate` | versioned KV set / delete (LWW)     |
| 1    | `Chat`         | JSON `ChatMessage` | chat broadcast                   |
| 2    | `StateRequest` | JSON `StateRequest` | "send me your full KV"          |
| 3    | `StateResponse`| JSON `StateResponse` | versioned KV entries + recent chat |
| 4    | `PeerGossip`   | JSON `PeerGossip` | "here are the peers I know"       |
| 5    | `Hello`        | JSON `Hello`     | "hi, I am `addr`" (peer learning)  |
| 6    | `Goodbye`      | JSON `Goodbye`   | "I am shutting down" (clean leave) |

Every discovery message is a plain Go struct with JSON tags; protobuf is now
used only for the bootstrap handshake. New features ship as new kinds without
regenerating any code, and the envelope costs exactly 1 byte per packet.

---

## 4. Cluster lifecycle

### 4.1 Bootstrap startup

`./rezoagwe-bootstrap -port 9999` opens a UDP listener and starts the
stale-node sweeper. Nothing else happens until someone REGISTERs.

### 4.2 Discovery node startup

`./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice`:

1. **Register** with bootstrap (UDP REGISTER).
2. **Discover** initial roster: ask bootstrap for the comma-separated
   list of known addresses; learn each one as a peer.
3. **Listen** on `-node` UDP address for envelope-prefixed packets.
4. **Hello-storm**: send a `Hello` to every peer learned from
   bootstrap. This causes existing nodes to learn the new node back
   (without requiring bootstrap to push updates).
5. **State sync**: pick one random peer and send `StateRequest`. That
   peer replies with `StateResponse` carrying its full KV store as
   versioned entries (tombstones included) — which the new node
   *merges* under last-write-wins rather than overwriting, so a local
   edit made before the reply arrives isn't clobbered — plus its recent
   chat history, which the new node prepends to its (usually empty)
   chat log so the pane has context on join.
6. **Start gossip loop**: every 10 s, send `PeerGossip` (containing the
   full known-peer list) to one random peer. The receiver learns any
   unknown addresses and `Hello`s back so the relationship is
   symmetric.
7. **Run the TUI** until the user hits `Ctrl+Q`.

### 4.3 Steady state

* **KV writes**: a user creates/deletes a key in the TUI. The local
  store stamps the mutation with a per-key version (a Lamport counter
  plus this node's address as tiebreak) and broadcasts a versioned
  `KVUpdate` as a `KindKV` envelope to every known peer. Each peer
  applies it only if the version is newer than what it holds, so
  concurrent writes to the same key converge to the same value on every
  node; a delete is a versioned tombstone, so a stale set can't
  resurrect the key. *No anti-entropy is performed* — if a packet is
  lost outright, the two stores stay divergent until the next
  overlapping write or a state resync on rejoin. This remains a known
  PoC limitation.
* **Chat**: typed text becomes a `ChatMessage` (sender addr, nick,
  text, unix timestamp), broadcast to every known peer. The receiver
  appends it to its chat log, colored per sender, and if the sender
  was unknown, learns them as a peer and emits a system "joined"
  line into the chat. Peer evictions emit a matching "left" line.
* **Peer membership**: PeerGossip every 10 s; Hello on any newly
  learned address. Both `Hello` and `PeerGossip` carry the sender's
  nickname, so every node can render peers as `addr — nick` without a
  separate lookup round-trip.
* **Status bar**: a 1 s ticker refreshes the bottom line with
  CONNECTED/DEGRADED state, local address/nick, peer count, key count
  and uptime.

### 4.4 Failure model

* **Bootstrap dies after startup**: the cluster keeps working —
  bootstrap is only used at join. Discovery nodes also send a REGISTER
  heartbeat to bootstrap every 5 s so that bootstrap forgets dead
  clients within its 15 s timeout.
* **Bootstrap unreachable at join**: a joining node no longer hangs.
  `DiscoverNodes` reads the roster with a bounded deadline
  (`DiscoverTimeout`, 2 s by default), so a down bootstrap — or a lost
  DISCOVER / reply, which UDP permits — degrades to "no peers learned":
  the node still opens its TUI and serves its persisted store. While it
  knows no peers, the 5 s heartbeat loop keeps re-registering *and*
  re-DISCOVERing, so it joins automatically once bootstrap is reachable.
* **A discovery node dies**: every discovery node tracks a last-seen
  timestamp per peer (updated on any incoming packet). A peer with no
  traffic for 15 s is evicted locally. Heartbeats are kept fresh by
  the 5 s Hello-fan-out and the 10 s PeerGossip.
* **A discovery node exits cleanly** (`Ctrl+Q`): before quitting it
  broadcasts a `Goodbye` to every known peer, so peers drop it and emit
  the "left" chat line immediately instead of waiting out the 15 s
  eviction. Goodbye is best-effort UDP like everything else — if it is
  lost, the peer simply falls back to timeout eviction.
* **Packet loss**: a lost KV write leaves that key divergent until the
  next overlapping write or a state resync — versioning makes the
  eventual reconcile deterministic, but nothing actively re-sends. Lost
  gossip is resent on the next tick. A lost state-sync reply leaves the
  joiner on its persisted store until it retries the join.

These trade-offs are intentional for a PoC; see §6 for the next-step
items.

### 4.5 Persistence

Each discovery node persists its KV store to a JSON file (`-data`, default
`<config dir>/rezoagwe/<node>.json`; the path is derived from the node
address so co-located nodes don't clobber each other). The file is rewritten
atomically (temp file + rename) after every mutation — local writes,
replicated writes from peers, and state-sync merges alike — gated on a
monotonically increasing generation so a slow, out-of-order write can never
regress the on-disk copy. The file records each entry's version and the
node's Lamport clock (tombstones included), so after a restart the node's new
writes still sort after everything it had already seen. On startup the node
loads this file *before* contacting bootstrap, so its keys survive a restart.
Pass `-data -` to disable persistence entirely.

Chat history is deliberately **not** persisted to disk: a rejoining node
repopulates its chat pane from a peer's `StateResponse` (§4.2 step 5)
instead.

---

## 5. Concurrency model

* `KVStore` is guarded by a `sync.RWMutex`. UI iteration goes through
  `Snapshot()` which copies under `RLock`, so the UI never races with
  network writes.
* Peer set lives in `sync.Map` — single-writer-many-reader access from
  goroutines.
* Chat log has its own `sync.Mutex` and `ChatLog()` returns a copy.
* Each incoming UDP packet is handled inline in the listener goroutine.
  Handlers that touch the UI use `App.QueueUpdateDraw` to marshal the
  redraw onto the tview thread.
* A small buffered channel (`updateCh`, cap 16) coalesces KV-related UI
  refresh signals so the listener never blocks on a slow UI tick.

---

## 6. Known limitations & next steps

| Area               | Limitation                                | Possible fix                                |
|--------------------|-------------------------------------------|---------------------------------------------|
| Transport          | UDP, single packet, no acks               | Switch to TCP for state sync; chunk         |
| Replication        | Per-key Lamport-clock LWW converges concurrent writes, but a dropped packet is never re-sent | Anti-entropy / Merkle reconcile on gossip |
| State sync         | Pulls from one random peer and merges by version (can't clobber newer local data), but a peer with gaps yields gaps | Pull from a quorum; anti-entropy |
| Liveness           | Heartbeat is best-effort UDP; no quorum   | Acked ping + quorum membership view         |
| Security           | Plain UDP, no auth                        | DTLS or pre-shared key + HMAC               |
| Bootstrap          | Single point of failure for new joiners   | Multiple seed addresses; mDNS               |
| Bootstrap roster   | DISCOVER reply is one UDP datagram (now ≤64 KB); a very large roster still won't fit | Length-prefix / chunk, or TCP |
| Chat history       | Synced from a peer on join; not persisted | Persist the chat ring to disk too           |
| State sync size    | KV entries + chat ride in one UDP datagram (≤64 KB) | Chunk / switch to TCP for large state |
| Tombstones         | Deleted keys are kept forever as tombstones | GC tombstones once cluster-wide consensus is certain |
| Diagnostics        | `log.Debugf` is inert (level never raised) and stderr logs would scribble over the TUI | Log to a file, gated by a real `-debug` flag |

For a prioritized, actionable version of this list — with effort estimates and
what's already shipped — see [ROADMAP.md](ROADMAP.md).

---

## 7. Why "rezoagwe"?

[Agwé](https://en.wikipedia.org/wiki/Agw%C3%A9) is the Haitian Vodou
lwa of the sea — a fitting name for a protocol whose packets drift
through the network with no guarantee of arrival. *Rezo* is Haitian
Creole for "network". The PoC is the small boat; production hardening
would be the larger ship.
