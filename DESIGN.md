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
| 0    | `KV`           | protobuf `Payload` | KV SET / DELETE replication      |
| 1    | `Chat`         | JSON `ChatMessage` | chat broadcast                   |
| 2    | `StateRequest` | JSON `StateRequest` | "send me your full KV"          |
| 3    | `StateResponse`| JSON `StateResponse` | full KV snapshot reply         |
| 4    | `PeerGossip`   | JSON `PeerGossip` | "here are the peers I know"       |
| 5    | `Hello`        | JSON `Hello`     | "hi, I am `addr`" (peer learning)  |

This way the existing protobuf-generated code stays untouched while new
features can ship as ordinary Go structs with JSON tags. The envelope
costs exactly 1 byte per packet.

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
   peer replies with `StateResponse` containing its full KV snapshot,
   which the new node `Replace`s into its own store.
6. **Start gossip loop**: every 10 s, send `PeerGossip` (containing the
   full known-peer list) to one random peer. The receiver learns any
   unknown addresses and `Hello`s back so the relationship is
   symmetric.
7. **Run the TUI** until the user hits `Ctrl+Q`.

### 4.3 Steady state

* **KV writes**: a user creates/deletes a key in the TUI. The local
  store is mutated and a `Payload` (SET or DELETE) is broadcast as a
  `KindKV` envelope to every known peer. Each peer applies it on
  receipt. *No anti-entropy is performed* — if a packet is lost, the
  two stores diverge until the next overlapping write. This is a known
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
  bootstrap is only used at join. New nodes joining while bootstrap is
  down cannot find anyone. Discovery nodes also send a REGISTER
  heartbeat to bootstrap every 5 s so that bootstrap forgets dead
  clients within its 15 s timeout.
* **A discovery node dies**: every discovery node tracks a last-seen
  timestamp per peer (updated on any incoming packet). A peer with no
  traffic for 15 s is evicted locally. Heartbeats are kept fresh by
  the 5 s Hello-fan-out and the 10 s PeerGossip.
* **Packet loss**: lost KV writes diverge silently. Lost gossip is
  resent on the next tick. Lost state-sync response leaves a node with
  an empty store until the next write arrives.

These trade-offs are intentional for a PoC; see §6 for the next-step
items.

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
| State sync         | Asks one random peer; if it lies, lose    | Quorum read or value timestamps             |
| Replication        | Best-effort broadcast, no ordering        | Per-key Lamport clocks; anti-entropy gossip |
| Liveness           | Heartbeat is best-effort UDP; no quorum   | Acked ping + quorum membership view         |
| Security           | Plain UDP, no auth                        | DTLS or pre-shared key + HMAC               |
| Bootstrap          | Single point of failure for new joiners   | Multiple seed addresses; mDNS               |
| Chat history       | In-memory ring of 500 lines per node      | Persistent log; sync on join                |

---

## 7. Why "rezoagwe"?

[Agwé](https://en.wikipedia.org/wiki/Agw%C3%A9) is the Haitian Vodou
lwa of the sea — a fitting name for a protocol whose packets drift
through the network with no guarantee of arrival. *Rezo* is Haitian
Creole for "network". The PoC is the small boat; production hardening
would be the larger ship.
