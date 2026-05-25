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
![Pic](https://github.com/nexusriot/rezoagwe/blob/main/bootstrap.png)
![Pic](https://github.com/nexusriot/rezoagwe/blob/main/discovery.png)

---

### Features

- Eventually-consistent distributed KV (SET / DELETE replicated to all peers)
- State snapshot pulled from a random peer when a node joins
- Peer membership maintained by periodic gossip + Hello replies — bootstrap
  is only consulted at join time
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
./rezoagwe-discovery [-bootstrap addr] [-node addr] [-nick name]
./rezoagwe-discovery -bootstrap :9999 -node :3137 -nick alice
./rezoagwe-discovery -bootstrap :9999 -node :3138 -nick bob
```
defaults: `-bootstrap :9999`, `-node :3137`, `-nick anon`

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
| `Ctrl+Q`        | Quit                                            |

The bottom status bar shows `CONNECTED` (green) or `DEGRADED` (red, no
peers known), the local node address and nickname, the live peer count,
the number of local keys, and how long this node has been running.

### Status

This is a Pre-PoC. The transport is plain UDP with no acks, no auth and
no anti-entropy — see the *Known limitations* table in
[DESIGN.md](DESIGN.md) for what would have to change before this is
useful in production.
