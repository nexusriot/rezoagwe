## R3zo Agwe

_PoC distributed key-value store with an embedded chat, written in Go._

Two cooperating binaries:

* **`rezoagwe-bootstrap`** — rendezvous service. Knows only the *set of
  node addresses*; never sees KV data or chat.
* **`rezoagwe-discovery`** — full peer node. Owns a local KV replica,
  participates in chat, gossips peer membership, and runs a `tview` TUI.

A new node contacts bootstrap **once at startup** to learn the initial
roster. From then on, peers gossip among themselves.

For internals, wire protocol, and the failure model, see
[DESIGN.md](DESIGN.md).

### **Why?**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/wtf.png)

### **Design (concept)**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/rezo_agwe.png)
---

### Features

- Eventually-consistent distributed KV, replicated to all peers with
  **per-key last-write-wins**: every write carries a version (Lamport counter
  + node id), so concurrent writes to the same key converge to the same value
  on every node, and deletes are versioned tombstones that a stale write can't
  resurrect
- Local persistence: each node's KV store — values, versions, Lamport clock
  and tombstones — is written atomically to a JSON file after every change and
  reloaded on startup, so both keys *and* write ordering survive a restart
  (`-data`, default `<config dir>/rezoagwe/<node>.json`; `-data -` disables)
- State pulled from a random peer when a node joins and **merged** by version
  (never blindly overwriting newer local data)
- Chat-history sync on join: a joining node prepends the recent chat history
  from the peer it state-syncs with, so its chat pane isn't empty
- Robust join: if bootstrap is unreachable (or a UDP packet is lost) the node
  starts anyway from its persisted store and keeps retrying, instead of
  hanging at startup
- Peer membership maintained by periodic gossip + Hello replies — bootstrap
  is only consulted at join time
- Graceful leave: a node quitting with `Ctrl+Q` broadcasts a `Goodbye` so
  peers drop it and post the *left* line at once, instead of waiting for the
  15 s stale-eviction timeout
- Built-in chat between all live nodes (per-node nickname), with
  per-sender colors and automatic *joined* / *left* system messages
- Per-node TUI built on `tview` / `tcell`: keys list, details, peer list
  (with each peer's nickname), chat pane, message input, and a status
  bar showing connection state, peer count, key count, and uptime
- Cross-build to Linux (amd64/i386/arm64/armv7), FreeBSD, macOS, Windows
- Debian packages (one `.deb` ships both binaries)

---

### Build

```
make x86_64           # linux/amd64, dynamic
make x86_64-static    # linux/amd64, fully static (CGO off)
make all              # every cross-build target into dist/
make debs             # deb-amd64 + deb-i386 + deb-arm64 + deb-armhf
make help             # full list of targets
```

A single `.deb` installs both `/usr/bin/rezoagwe-bootstrap` and
`/usr/bin/rezoagwe-discovery`. To package a single arch directly:

```
./build-deb.sh amd64
./build-deb.sh i386
./build-deb.sh arm64    # or: ./build-deb-arm64.sh
./build-deb.sh armhf
```

### Usage

```
./rezoagwe-bootstrap [-port number]
./rezoagwe-bootstrap -port 9999
```
default port is **9999**

```
./rezoagwe-discovery [-bootstrap addr] [-node addr] [-nick name] [-data file]
./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice
./rezoagwe-discovery -bootstrap :9999 -node :3138 -nick bob
```
defaults: `-bootstrap :9999`, `-node :3137`, `-nick anon`. The KV store is
persisted to `-data` (default `<config dir>/rezoagwe/<node>.json`); pass
`-data -` to disable persistence. On join a node also pulls the peer's recent
chat history so its chat pane starts populated.

The second node will pull alice's KV snapshot on join, both will see
each other in the **Nodes** pane, chat broadcasts in both directions,
and KV writes (see hotkeys) propagate.

### Hotkeys (discovery TUI)

| Key             | Action                                          |
|-----------------|-------------------------------------------------|
| `c`             | Create key (multi-line value supported)         |
| `e` / `Enter`   | Edit key under cursor (multi-line value editor) |
| `d`             | Delete key under cursor                         |
| `/`             | Focus the filter; type to filter keys/values    |
| `Esc` (filter)  | Clear filter and return to keys list            |
| `Tab`           | Cycle focus: Keys → Nodes → Chat → Message      |
| `Enter` (chat)  | Send the chat message                           |
| `Ctrl+Q`        | Quit (announces departure to peers)             |

The bottom status bar shows `CONNECTED` (green) or `DEGRADED` (red, no
peers known), the local node address and nickname, the live peer count,
the number of local keys, and how long this node has been running.

### Status

This is a Pre-PoC. Replication now converges concurrent writes via per-key
last-write-wins, but the transport is still plain UDP with no acks, no auth
and no anti-entropy (a dropped write is never re-sent) — see the *Known
limitations* table in [DESIGN.md](DESIGN.md) for what would have to change
before this is useful in production, and [ROADMAP.md](ROADMAP.md) for the
prioritized plan to get there.
